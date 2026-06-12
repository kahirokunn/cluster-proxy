package utils

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGetTargetServiceConfigForKubeAPIServer(t *testing.T) {
	testcases := []struct {
		requestURL string
		expect     TargetServiceConfig
		err        error
	}{
		{
			requestURL: "route-domain/cluster1/api/pods?timeout=32s",
			expect: TargetServiceConfig{
				Cluster:   "cluster1",
				Proto:     "https",
				Service:   "kubernetes",
				Namespace: "default",
				Port:      "443",
				Path:      "api/pods",
			},
		},
		{
			requestURL: "route-domain/cluster1",
			expect: TargetServiceConfig{
				Cluster:   "cluster1",
				Proto:     "https",
				Service:   "kubernetes",
				Namespace: "default",
				Port:      "443",
				Path:      "api/pods",
			},
			err: fmt.Errorf("requestURL format not correct, path more than 2: route-domain/cluster1"),
		},
	}
	for _, tc := range testcases {
		actual, err := GetTargetServiceConfigForKubeAPIServer(tc.requestURL)
		if err != nil {
			if tc.err == nil {
				t.Fatalf("except no err, but got %v", err)
			}
			continue
		}

		// compare every field in targetServiceConfig
		if actual.Cluster != tc.expect.Cluster {
			t.Errorf("expected cluster: %v, got: %v", tc.expect.Cluster, actual.Cluster)
		}
		if actual.Proto != tc.expect.Proto {
			t.Errorf("expected proto: %v, got: %v", tc.expect.Proto, actual.Proto)
		}
		if actual.Service != tc.expect.Service {
			t.Errorf("expected service: %v, got: %v", tc.expect.Service, actual.Service)
		}
		if actual.Namespace != tc.expect.Namespace {
			t.Errorf("expected namespace: %v, got: %v", tc.expect.Namespace, actual.Namespace)
		}
		if actual.Port != tc.expect.Port {
			t.Errorf("expected port: %v, got: %v", tc.expect.Port, actual.Port)
		}
		if actual.Path != tc.expect.Path {
			t.Errorf("expected path: %v, got: %v", tc.expect.Path, actual.Path)
		}
	}
}

func TestGetProxyType(t *testing.T) {
	testcases := []struct {
		requestURL string
		proxyType  int
	}{
		{
			requestURL: "route-domain/cluster1/api?timeout=32s",
			proxyType:  ProxyTypeKubeAPIServer,
		},
		{
			requestURL: "route-domain/cluster1/api/pods?timeout=32s",
			proxyType:  ProxyTypeKubeAPIServer,
		},
		{
			requestURL: "route-domain/cluster1/api/v1/namespaces/default/services/https:nginx:80/proxy-service/hello",
			proxyType:  ProxyTypeService,
		},
	}

	for _, tc := range testcases {
		pt := GetProxyType(tc.requestURL)
		if pt != tc.proxyType {
			t.Errorf("expected isProxy: %v, got: %v", tc.proxyType, pt)
		}
	}
}

func TestParseServiceRequestURL(t *testing.T) {
	testcases := []struct {
		requestURL string
		expect     TargetServiceConfig
		err        error
	}{
		{
			requestURL: "route-domain/cluster1/api/v1/namespaces/default/services/http:nginx:80/proxy-service/hello?timeout=32s",
			expect: TargetServiceConfig{
				Cluster:   "cluster1",
				Proto:     "http",
				Service:   "nginx",
				Namespace: "default",
				Port:      "80",
				Path:      "hello",
			},
			err: nil,
		},
		{
			requestURL: "route-domain/cluster1/api/v1/namespaces/default/services/https:nginx:443/proxy-service",
			expect: TargetServiceConfig{
				Cluster:   "cluster1",
				Proto:     "https",
				Service:   "nginx",
				Namespace: "default",
				Port:      "443",
				Path:      "",
			},
		},
		{
			requestURL: "route-domain/cluster1/proxy-service/hello?timeout=32s",
			expect:     TargetServiceConfig{},
			err:        fmt.Errorf("requestURL format not correct, path less than 9: route-domain/cluster1/proxy-service/hello?timeout=32s"),
		},
	}

	for _, tc := range testcases {
		actual, err := GetTargetServiceConfig(tc.requestURL)
		if err != nil {
			if tc.err == nil {
				t.Fatalf("except no err, but got %v", err)
			}
			continue
		}

		// compare every field in targetServiceConfig
		if actual.Cluster != tc.expect.Cluster {
			t.Errorf("expected cluster: %v, got: %v", tc.expect.Cluster, actual.Cluster)
		}
		if actual.Proto != tc.expect.Proto {
			t.Errorf("expected proto: %v, got: %v", tc.expect.Proto, actual.Proto)
		}
		if actual.Service != tc.expect.Service {
			t.Errorf("expected service: %v, got: %v", tc.expect.Service, actual.Service)
		}
		if actual.Namespace != tc.expect.Namespace {
			t.Errorf("expected namespace: %v, got: %v", tc.expect.Namespace, actual.Namespace)
		}
		if actual.Port != tc.expect.Port {
			t.Errorf("expected port: %v, got: %v", tc.expect.Port, actual.Port)
		}
		if actual.Path != tc.expect.Path {
			t.Errorf("expected path: %v, got: %v", tc.expect.Path, actual.Path)
		}
	}
}

func TestUpdateRequest(t *testing.T) {
	tsc := TargetServiceConfig{
		Cluster:   "cluster1",
		Proto:     "https",
		Service:   "hello-world",
		Namespace: "default",
		Port:      "9091",
		Path:      "/hello?timeout=3s",
	}

	testcases := []struct {
		req      *http.Request
		userType string
		expect   *http.Request
	}{
		{
			req: &http.Request{
				Header: make(http.Header),
				URL:    &url.URL{},
			},
			expect: &http.Request{
				Header: map[string][]string{
					HeaderClusterProxyProto:     {"https"},
					HeaderClusterProxyPort:      {"9091"},
					HeaderClusterProxyNamespace: {"default"},
					HeaderClusterProxyService:   {"hello-world"},
				},
				URL: &url.URL{
					Path: "/hello?timeout=3s",
				},
			},
		},
	}

	for _, tc := range testcases {
		actual := UpdateRequest(tsc, tc.req)
		if actual.Header.Get(HeaderClusterProxyProto) != tc.expect.Header.Get(HeaderClusterProxyProto) {
			t.Errorf("expected proto: %v, got: %v", tc.expect.Header.Get(HeaderClusterProxyProto), actual.Header.Get(HeaderClusterProxyProto))
		}
		if actual.Header.Get(HeaderClusterProxyPort) != tc.expect.Header.Get(HeaderClusterProxyPort) {
			t.Errorf("expected port: %v, got: %v", tc.expect.Header.Get(HeaderClusterProxyPort), actual.Header.Get(HeaderClusterProxyPort))
		}
		if actual.Header.Get(HeaderClusterProxyNamespace) != tc.expect.Header.Get(HeaderClusterProxyNamespace) {
			t.Errorf("expected namespace: %v, got: %v", tc.expect.Header.Get(HeaderClusterProxyNamespace), actual.Header.Get(HeaderClusterProxyNamespace))
		}
		if actual.Header.Get(HeaderClusterProxyService) != tc.expect.Header.Get(HeaderClusterProxyService) {
			t.Errorf("expected service: %v, got: %v", tc.expect.Header.Get(HeaderClusterProxyService), actual.Header.Get(HeaderClusterProxyService))
		}
		if actual.URL.Path != tc.expect.URL.Path {
			t.Errorf("expected path: %v, got: %v", tc.expect.URL.Path, actual.URL.Path)
		}
	}
}

func TestGetTargetServiceURLFromRequest(t *testing.T) {
	testcases := []struct {
		name   string
		req    *http.Request
		expect string
		err    error
	}{
		{
			name: "short for parameters",
			req: &http.Request{
				Header: map[string][]string{
					HeaderClusterProxyProto: {"https"},
					HeaderClusterProxyPort:  {"9091"},
				},
			},
			err: errors.New("invalid request headers"),
		},
		{
			name: "kubernetes apiserver",
			req: &http.Request{
				Header: map[string][]string{
					HeaderClusterProxyProto:     {"https"},
					HeaderClusterProxyPort:      {"443"},
					HeaderClusterProxyService:   {"kubernetes"},
					HeaderClusterProxyNamespace: {"default"},
				},
			},
			expect: "https://kubernetes.default.svc",
		},
		{
			name: "other services",
			req: &http.Request{
				Header: map[string][]string{
					HeaderClusterProxyProto:     {"https"},
					HeaderClusterProxyPort:      {"9091"},
					HeaderClusterProxyService:   {"hello-world"},
					HeaderClusterProxyNamespace: {"default"},
				},
			},
			expect: "https://hello-world.default.svc:9091",
		},
	}

	for _, tc := range testcases {
		actual, err := GetTargetServiceURLFromRequest(tc.req)
		if err != nil {
			if tc.err == nil {
				t.Errorf("expected: %v, got: %v", tc.err, err)
			} else if tc.err.Error() != err.Error() {
				t.Errorf("expected: %v, got: %v", tc.err, err)
			}
		} else {
			expectURL, err := url.Parse(tc.expect)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if actual.Scheme != expectURL.Scheme {
				t.Errorf("expected: %v, got: %v", expectURL.Scheme, actual.Scheme)
			}
			if actual.Host != expectURL.Host {
				t.Errorf("expected: %v, got: %v", expectURL.Host, actual.Host)
			}
			if actual.Path != expectURL.Path {
				t.Errorf("expected: %v, got: %v", expectURL.Path, actual.Path)
			}
		}
	}
}

func TestServeReverseProxy(t *testing.T) {
	target, _ := url.Parse("http://backend.svc")

	t.Run("returns backend status code", func(t *testing.T) {
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusCreated,
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		})
		req := httptest.NewRequest("GET", "http://proxy/ping", nil)
		recorder := httptest.NewRecorder()

		var logged error
		status := ServeReverseProxy(recorder, req, target, transport, func(err error) { logged = err })

		if status != http.StatusCreated {
			t.Fatalf("expected status %d, got %d", http.StatusCreated, status)
		}
		if logged != nil {
			t.Fatalf("expected no logged error, got %v", logged)
		}
	})

	t.Run("returns generic bad gateway on transport error", func(t *testing.T) {
		transportErr := errors.New("dial tcp backend.svc:80: connection refused")
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, transportErr
		})
		req := httptest.NewRequest("GET", "http://proxy/ping", nil)
		recorder := httptest.NewRecorder()

		var logged error
		status := ServeReverseProxy(recorder, req, target, transport, func(err error) { logged = err })

		if status != http.StatusBadGateway {
			t.Fatalf("expected status %d, got %d", http.StatusBadGateway, status)
		}
		if !errors.Is(logged, transportErr) {
			t.Fatalf("expected transport error to be logged, got %v", logged)
		}
		if body := strings.TrimSpace(recorder.Body.String()); body != "bad gateway" {
			t.Fatalf("expected generic body %q, got %q", "bad gateway", body)
		}
	})
}

func TestBearerTokenFromHeader(t *testing.T) {
	testcases := []struct {
		header string
		want   string
	}{
		{header: "", want: ""},
		{header: "Bearer", want: ""},
		{header: "Bearer ", want: ""},
		{header: "Bearer abc", want: "abc"},
		{header: "bearer abc", want: "abc"},
		{header: "BEARER abc", want: "abc"},
		{header: "Bearer   xyz  ", want: "xyz"},
		{header: "Basic abc", want: ""},
		{header: "Bearertoken", want: ""},
	}
	for _, tc := range testcases {
		if got := BearerTokenFromHeader(tc.header); got != tc.want {
			t.Errorf("BearerTokenFromHeader(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func TestBearerTokenToHeader(t *testing.T) {
	testcases := []struct {
		token string
		want  string
	}{
		{token: "", want: "Bearer "},
		{token: "abc", want: "Bearer abc"},
	}
	for _, tc := range testcases {
		if got := BearerTokenToHeader(tc.token); got != tc.want {
			t.Errorf("BearerTokenToHeader(%q) = %q, want %q", tc.token, got, tc.want)
		}
		// BearerTokenToHeader is the inverse of BearerTokenFromHeader for non-empty tokens.
		if tc.token != "" {
			if got := BearerTokenFromHeader(tc.want); got != tc.token {
				t.Errorf("BearerTokenFromHeader(%q) = %q, want %q", tc.want, got, tc.token)
			}
		}
	}
}
