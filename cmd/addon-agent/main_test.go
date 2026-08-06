package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	addonutils "open-cluster-management.io/addon-framework/pkg/utils"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

func TestHealthProbeRouteIsolationAndReadinessTransition(t *testing.T) {
	var certificateCalls atomic.Int32
	certificateCheck := func(*http.Request) error {
		certificateCalls.Add(1)
		return nil
	}
	proxyAgentCheck := func(*http.Request) error { return errors.New("proxy-agent unavailable") }
	var startupCalls atomic.Int32
	startupCheck := func(*http.Request) error {
		startupCalls.Add(1)
		return errors.New("proxy-agent has not connected to every server")
	}
	portForwardReady := &atomic.Value{}
	portForwardReady.Store(false)
	handler := newHealthProbeHandler(
		certificateCheck,
		proxyAgentCheck,
		portForwardReadinessChecker(portForwardReady),
		startupCheck,
	)

	assertStatus := func(path string, want int) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
		if response.Code != want {
			t.Fatalf("unexpected status for %s: got %d, want %d", path, response.Code, want)
		}
	}

	assertStatus("/healthz", http.StatusOK)
	assertStatus("/healthz/", http.StatusOK)
	assertStatus("/healthz/ping", http.StatusOK)
	assertStatus("/readyz", http.StatusInternalServerError)
	if certificateCalls.Load() != 0 {
		t.Fatal("certificate checker ran outside the dedicated proxy-agent endpoint")
	}
	portForwardReady.Store(true)
	assertStatus("/readyz/port-forward", http.StatusOK)
	assertStatus("/startupz", http.StatusInternalServerError)
	assertStatus("/startupz/proxy-agent-server-connections", http.StatusInternalServerError)
	if startupCalls.Load() != 2 {
		t.Fatalf("unexpected startup check count: got %d, want 2", startupCalls.Load())
	}
	if certificateCalls.Load() != 0 {
		t.Fatal("certificate checker ran while evaluating startup")
	}
	assertStatus("/proxy-agent-healthz", http.StatusInternalServerError)
	if certificateCalls.Load() != 1 {
		t.Fatalf("unexpected certificate check count: got %d, want 1", certificateCalls.Load())
	}
}

func TestHealthProbeServerLifecycle(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if _, _, err := newHealthProbeServer(occupied.Addr().String(), http.NotFoundHandler()); err == nil {
		t.Fatal("expected an occupied health probe address to fail")
	}

	server, listener, err := newHealthProbeServer("127.0.0.1:0", http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveHealthProbes(ctx, server, listener) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected shutdown error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("health probe server did not stop after cancellation")
	}
}

func TestCertificateChangesAreConsumedOnlyByProxyAgentHealth(t *testing.T) {
	directory := t.TempDir()
	files := []string{
		filepath.Join(directory, "ca.crt"),
		filepath.Join(directory, "tls.crt"),
		filepath.Join(directory, "tls.key"),
	}
	for index, file := range files {
		if err := os.WriteFile(file, []byte(fmt.Sprintf("initial-%d", index)), 0600); err != nil {
			t.Fatal(err)
		}
	}

	checker, err := addonutils.NewConfigChecker("certificates check", files...)
	if err != nil {
		t.Fatal(err)
	}
	checker.SetReload(true)
	handler := newHealthProbeHandler(serializedChecker(checker.Check), healthz.Ping, healthz.Ping, healthz.Ping)

	assertStatus := func(path string, want int) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
		if response.Code != want {
			t.Fatalf("unexpected status for %s: got %d, want %d", path, response.Code, want)
		}
	}

	for index, file := range files {
		if err := os.WriteFile(file, []byte(fmt.Sprintf("changed-%d", index)), 0600); err != nil {
			t.Fatal(err)
		}
		assertStatus("/healthz", http.StatusOK)
		assertStatus("/readyz", http.StatusOK)
		assertStatus("/startupz", http.StatusOK)
		assertStatus("/proxy-agent-healthz", http.StatusInternalServerError)
		assertStatus("/proxy-agent-healthz", http.StatusOK)
	}
}

func TestHTTPHealthChecker(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
		timeout time.Duration
		wantErr bool
	}{
		{
			name:    "success",
			handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusOK) }),
			timeout: time.Second,
		},
		{
			name: "non-OK response",
			handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(http.StatusServiceUnavailable)
			}),
			timeout: time.Second,
			wantErr: true,
		},
		{
			name: "timeout",
			handler: http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				<-request.Context().Done()
			}),
			timeout: 10 * time.Millisecond,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			checker := newHTTPHealthChecker(server.URL, test.timeout)
			err := checker(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody))
			if (err != nil) != test.wantErr {
				t.Fatalf("unexpected checker error: %v", err)
			}
		})
	}
}

func TestSerializedCheckerPreventsConcurrentChecks(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	start := make(chan struct{})
	checker := serializedChecker(func(*http.Request) error {
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return nil
	})

	var waitGroup sync.WaitGroup
	for range 8 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			_ = checker(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody))
		}()
	}
	close(start)
	waitGroup.Wait()
	if maximum.Load() != 1 {
		t.Fatalf("checker ran concurrently: maximum active checks = %d", maximum.Load())
	}
}

func TestCheckProxyAgentStartup(t *testing.T) {
	metric := func(metricType, sample string) string {
		return fmt.Sprintf(
			"# HELP %s Current number of open server connections.\n# TYPE %s %s\n%s %s\n",
			proxyAgentOpenServerConnectionsMetric,
			proxyAgentOpenServerConnectionsMetric,
			metricType,
			proxyAgentOpenServerConnectionsMetric,
			sample,
		)
	}
	padToSize := func(body string, size int) string {
		t.Helper()
		padding := size - len(body)
		if padding < 0 {
			t.Fatalf("metrics body is already %d bytes, cannot pad to %d", len(body), size)
		}
		if padding == 0 {
			return body
		}
		if padding == 1 {
			return body + "\n"
		}
		return body + "#" + strings.Repeat("x", padding-2) + "\n"
	}
	exactLimitBody := padToSize(metric("gauge", "3"), maxProxyAgentMetricsBodyBytes)
	overLimitBody := exactLimitBody + "\n"

	tests := []struct {
		name      string
		status    int
		body      string
		gzip      bool
		expected  int
		wantError string
	}{
		{name: "below expected", status: http.StatusOK, body: metric("gauge", "2"), expected: 3, wantError: "has 2 open"},
		{name: "equal to expected", status: http.StatusOK, body: metric("gauge", "3"), expected: 3},
		{name: "above expected", status: http.StatusOK, body: metric("gauge", "4"), expected: 3},
		{name: "maximum int32", status: http.StatusOK, body: metric("gauge", "2147483647"), expected: math.MaxInt32},
		{
			name:     "optional timestamp",
			status:   http.StatusOK,
			body:     "# TYPE " + proxyAgentOpenServerConnectionsMetric + " gauge\n" + proxyAgentOpenServerConnectionsMetric + " 3 1712345678\n",
			expected: 3,
		},
		{name: "exactly maximum body size", status: http.StatusOK, body: exactLimitBody, expected: 3},
		{name: "missing metric", status: http.StatusOK, body: "# TYPE other gauge\nother 3\n", expected: 3, wantError: "missing a gauge TYPE"},
		{name: "wrong metric type", status: http.StatusOK, body: metric("counter", "3"), expected: 3, wantError: "want gauge"},
		{
			name:   "labeled metric",
			status: http.StatusOK,
			body: "# TYPE " + proxyAgentOpenServerConnectionsMetric + " gauge\n" +
				proxyAgentOpenServerConnectionsMetric + "{server=\"one\"} 3\n",
			expected:  3,
			wantError: "must not have labels",
		},
		{name: "non-200 response", status: http.StatusServiceUnavailable, expected: 3, wantError: "HTTP status 503"},
		{name: "valid metric before oversized padding", status: http.StatusOK, body: overLimitBody, expected: 3, wantError: "exceeds"},
		{
			name:      "compressed response exceeds decompressed body limit",
			status:    http.StatusOK,
			body:      overLimitBody,
			gzip:      true,
			expected:  3,
			wantError: "exceeds",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.gzip {
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.WriteHeader(test.status)
				if test.gzip {
					writer := gzip.NewWriter(w)
					_, _ = writer.Write([]byte(test.body))
					_ = writer.Close()
					return
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			checker := checkProxyAgentStartup(newProxyAgentMetricsClient(), server.URL, test.expected)
			err := checker(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/startupz", http.NoBody))
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("startup check failed: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("startup check error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestCheckProxyAgentStartupWithZeroExpectedConnectionsSkipsHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	checker := checkProxyAgentStartup(newProxyAgentMetricsClient(), server.URL, 0)
	if err := checker(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/startupz", http.NoBody)); err != nil {
		t.Fatalf("zero-connection startup check failed: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("zero-connection startup check made %d HTTP requests, want 0", requests.Load())
	}
}

func TestCheckProxyAgentStartupTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()

	client := newProxyAgentMetricsClient()
	client.Timeout = 10 * time.Millisecond
	checker := checkProxyAgentStartup(client, server.URL, 1)
	err := checker(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/startupz", http.NoBody))
	if err == nil || !strings.Contains(err.Error(), "Client.Timeout") {
		t.Fatalf("startup timeout error = %v", err)
	}
}

func TestCheckProxyAgentStartupPropagatesRequestCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()

	requestContext, cancel := context.WithCancel(t.Context())
	cancel()
	checker := checkProxyAgentStartup(newProxyAgentMetricsClient(), server.URL, 1)
	err := checker(httptest.NewRequestWithContext(requestContext, http.MethodGet, "/startupz", http.NoBody))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("startup cancellation error = %v, want context.Canceled", err)
	}
}

func TestProxyAgentMetricsClientDoesNotFollowRedirects(t *testing.T) {
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer redirect.Close()

	checker := checkProxyAgentStartup(newProxyAgentMetricsClient(), redirect.URL, 1)
	err := checker(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/startupz", http.NoBody))
	if err == nil || !strings.Contains(err.Error(), "HTTP status 307") {
		t.Fatalf("redirect startup check error = %v", err)
	}
	if redirectedRequests.Load() != 0 {
		t.Fatalf("metrics client followed redirect %d times", redirectedRequests.Load())
	}
}

func TestParseOpenServerConnectionsRejectsInvalidMetrics(t *testing.T) {
	metric := proxyAgentOpenServerConnectionsMetric
	tests := []struct {
		name      string
		input     string
		wantError string
	}{
		{name: "missing type", input: metric + " 3\n", wantError: "missing a gauge TYPE"},
		{name: "missing sample", input: "# TYPE " + metric + " gauge\n", wantError: "is missing"},
		{name: "duplicate type", input: "# TYPE " + metric + " gauge\n# TYPE " + metric + " gauge\n" + metric + " 3\n", wantError: "duplicate TYPE"},
		{name: "invalid type declaration", input: "# TYPE " + metric + " gauge extra\n" + metric + " 3\n", wantError: "invalid TYPE"},
		{name: "duplicate sample", input: "# TYPE " + metric + " gauge\n" + metric + " 3\n" + metric + " 3\n", wantError: "appears more than once"},
		{name: "negative", input: "# TYPE " + metric + " gauge\n" + metric + " -1\n", wantError: "invalid value"},
		{name: "fractional", input: "# TYPE " + metric + " gauge\n" + metric + " 1.5\n", wantError: "invalid value"},
		{name: "not a number", input: "# TYPE " + metric + " gauge\n" + metric + " NaN\n", wantError: "invalid value"},
		{name: "infinite", input: "# TYPE " + metric + " gauge\n" + metric + " +Inf\n", wantError: "invalid value"},
		{name: "above int32", input: "# TYPE " + metric + " gauge\n" + metric + " 2147483648\n", wantError: "invalid value"},
		{name: "too many fields", input: "# TYPE " + metric + " gauge\n" + metric + " 3 123 extra\n", wantError: "invalid sample"},
		{name: "invalid timestamp", input: "# TYPE " + metric + " gauge\n" + metric + " 3 yesterday\n", wantError: "invalid timestamp"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseOpenServerConnections([]byte(test.input))
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("parse error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestValidateProxyAgentHealthPort(t *testing.T) {
	for _, port := range []int{1, defaultProxyAgentHealthPort, 65535} {
		if err := validateProxyAgentHealthPort(port); err != nil {
			t.Errorf("expected port %d to be valid: %v", port, err)
		}
	}
	for _, port := range []int{-1, 0, 65536} {
		if err := validateProxyAgentHealthPort(port); err == nil {
			t.Errorf("expected port %d to be invalid", port)
		}
	}
}

func TestValidateExpectedProxyServerConnections(t *testing.T) {
	for _, expected := range []int{0, 1, math.MaxInt32} {
		if err := validateExpectedProxyServerConnections(expected); err != nil {
			t.Errorf("expected connection count %d to be valid: %v", expected, err)
		}
	}
	for _, expected := range []int{-1, math.MaxInt32 + 1} {
		if err := validateExpectedProxyServerConnections(expected); err == nil {
			t.Errorf("expected connection count %d to be invalid", expected)
		}
	}
}
