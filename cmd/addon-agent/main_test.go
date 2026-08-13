package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	portForwardReady := &atomic.Value{}
	portForwardReady.Store(false)
	handler := newHealthProbeHandler(certificateCheck, proxyAgentCheck, portForwardReadinessChecker(portForwardReady))

	assertStatus := func(path string, want int) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
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
	handler := newHealthProbeHandler(serializedChecker(checker.Check), healthz.Ping, healthz.Ping)

	assertStatus := func(path string, want int) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
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
			err := checker(httptest.NewRequest(http.MethodGet, "/", nil))
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
			_ = checker(httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	close(start)
	waitGroup.Wait()
	if maximum.Load() != 1 {
		t.Fatalf("checker ran concurrently: maximum active checks = %d", maximum.Load())
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
