package servicerelay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

func allowAllCallers(context.Context, *http.Request) error { return nil }

func TestServiceRelayRestoresAuthorizationAndStripsInternalHeaders(t *testing.T) {
	var captured *http.Request
	relay := &ServiceRelay{
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req.Clone(req.Context())
			captured.Header = req.Header.Clone()
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     http.Header{"X-Backend": []string{"ok"}},
				Body:       io.NopCloser(strings.NewReader("backend-body")),
				Request:    req,
			}, nil
		}),
		authenticateCaller: allowAllCallers,
	}

	req := httptest.NewRequest("GET", "http://relay/ping?x=1", nil)
	req.Header.Set(utils.HeaderClusterProxyProto, "http")
	req.Header.Set(utils.HeaderClusterProxyNamespace, "default")
	req.Header.Set(utils.HeaderClusterProxyService, "hello")
	req.Header.Set(utils.HeaderClusterProxyPort, "8080")
	req.Header.Set("Authorization", "Bearer managed-token")
	req.Header.Set(utils.HeaderClusterProxyRelayAuth, "Bearer managed-token")
	req.Header.Set(utils.HeaderClusterProxyAuthorization, "Bearer original-token")

	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, recorder.Code)
	}
	if recorder.Header().Get("X-Backend") != "ok" {
		t.Fatalf("expected backend header to be proxied")
	}
	if strings.TrimSpace(recorder.Body.String()) != "backend-body" {
		t.Fatalf("unexpected body %q", recorder.Body.String())
	}
	if captured == nil {
		t.Fatal("expected backend request to be captured")
	}
	if captured.URL.Scheme != "http" || captured.URL.Host != "hello.default.svc:8080" || captured.URL.Path != "/ping" {
		t.Fatalf("unexpected target URL %s", captured.URL.String())
	}
	if captured.Header.Get("Authorization") != "Bearer original-token" {
		t.Fatalf("expected original authorization to be restored, got %q", captured.Header.Get("Authorization"))
	}
	for _, header := range utils.ClusterProxyHeaders {
		if captured.Header.Get(header) != "" {
			t.Fatalf("expected header %s to be stripped, got %q", header, captured.Header.Get(header))
		}
	}
}

func TestServiceRelayRejectsKubeAPIServerTarget(t *testing.T) {
	relay := &ServiceRelay{authenticateCaller: allowAllCallers}
	req := httptest.NewRequest("GET", "http://relay/healthz", nil)
	req.Header.Set(utils.HeaderClusterProxyProto, "https")
	req.Header.Set(utils.HeaderClusterProxyNamespace, "default")
	req.Header.Set(utils.HeaderClusterProxyService, "kubernetes")
	req.Header.Set(utils.HeaderClusterProxyPort, "443")

	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

func TestServiceRelayRejectsUnsupportedTargetScheme(t *testing.T) {
	relay := &ServiceRelay{authenticateCaller: allowAllCallers}
	req := httptest.NewRequest("GET", "http://relay/ping", nil)
	req.Header.Set(utils.HeaderClusterProxyProto, "ftp")
	req.Header.Set(utils.HeaderClusterProxyNamespace, "default")
	req.Header.Set(utils.HeaderClusterProxyService, "hello")
	req.Header.Set(utils.HeaderClusterProxyPort, "8080")

	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, recorder.Code)
	}
}

func TestServiceRelayFailsClosedWhenAuthenticatorMissing(t *testing.T) {
	relay := &ServiceRelay{}
	req := httptest.NewRequest("GET", "http://relay/ping", nil)
	req.Header.Set(utils.HeaderClusterProxyProto, "http")
	req.Header.Set(utils.HeaderClusterProxyNamespace, "default")
	req.Header.Set(utils.HeaderClusterProxyService, "hello")
	req.Header.Set(utils.HeaderClusterProxyPort, "8080")

	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when authenticator missing, got %d", recorder.Code)
	}
}

func TestServiceRelayRejectsUntrustedCallerBeforeHonoringHeaders(t *testing.T) {
	transportCalled := false
	relay := &ServiceRelay{
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			transportCalled = true
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
		}),
		authenticateCaller: func(context.Context, *http.Request) error {
			return fmt.Errorf("denied")
		},
	}

	req := httptest.NewRequest("GET", "http://relay/ping", nil)
	req.Header.Set("Authorization", "Bearer attacker-token")
	req.Header.Set(utils.HeaderClusterProxyAuthorization, "Bearer planted-token")
	req.Header.Set(utils.HeaderClusterProxyRelayAuth, "Bearer attacker-token")
	req.Header.Set(utils.HeaderClusterProxyProto, "http")
	req.Header.Set(utils.HeaderClusterProxyNamespace, "default")
	req.Header.Set(utils.HeaderClusterProxyService, "hello")
	req.Header.Set(utils.HeaderClusterProxyPort, "8080")

	recorder := httptest.NewRecorder()
	relay.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
	if transportCalled {
		t.Fatalf("expected transport to not be invoked for untrusted caller")
	}
}

// newTokenReviewClient returns a fake kubernetes client that answers every
// TokenReview with the given authenticated status and username.
func newTokenReviewClient(authenticated bool, username string) *fake.Clientset {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.TokenReview{Status: authnv1.TokenReviewStatus{
			Authenticated: authenticated,
			User:          authnv1.UserInfo{Username: username},
		}}, nil
	})
	return client
}

func TestTokenReviewAuthenticator(t *testing.T) {
	trustedUser := "system:serviceaccount:open-cluster-management-agent-addon:cluster-proxy"

	tests := []struct {
		name          string
		authenticated bool
		username      string
		header        string
		token         string
		wantErr       bool
	}{
		{name: "trusted token passes", authenticated: true, username: trustedUser, header: "Authorization", token: "valid-token"},
		{name: "unauthenticated token is rejected", authenticated: false, header: "Authorization", token: "attacker-token", wantErr: true},
		{name: "missing bearer token is rejected", wantErr: true},
		{name: "relay authorization header passes when Authorization is stripped", authenticated: true, username: trustedUser, header: utils.HeaderClusterProxyRelayAuth, token: "relay-token"},
		{name: "non-trusted user is rejected", authenticated: true, username: "system:serviceaccount:default:rogue", header: "Authorization", token: "some-token", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTokenReviewClient(tt.authenticated, tt.username)
			auth := newTokenReviewAuthenticator(client, map[string]struct{}{trustedUser: {}}, 0)
			req := httptest.NewRequest("GET", "http://relay/ping", nil)
			if tt.token != "" {
				req.Header.Set(tt.header, "Bearer "+tt.token)
			}
			if err := auth(context.Background(), req); (err != nil) != tt.wantErr {
				t.Fatalf("auth() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeTrustedCallers(t *testing.T) {
	got := normalizeTrustedCallers([]string{" alice ", "bob", "carol", "", "alice"})
	if len(got) != 3 {
		t.Fatalf("expected 3 unique entries, got %d: %v", len(got), got)
	}
	for _, name := range []string{"alice", "bob", "carol"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("expected %q in trusted set", name)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
