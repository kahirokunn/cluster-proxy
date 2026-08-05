package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

func TestCheckProxyAgentStartup(t *testing.T) {
	metric := func(value string) string {
		return fmt.Sprintf("# TYPE %s gauge\n%s %s\n", proxyAgentOpenServerConnectionsMetric, proxyAgentOpenServerConnectionsMetric, value)
	}

	tests := []struct {
		name        string
		status      int
		body        string
		expected    int
		delay       time.Duration
		clientLimit time.Duration
		wantError   string
	}{
		{name: "below expected", status: http.StatusOK, body: metric("2"), expected: 3, wantError: "has 2 open"},
		{name: "equal to expected", status: http.StatusOK, body: metric("3"), expected: 3},
		{name: "above expected", status: http.StatusOK, body: metric("4"), expected: 3},
		{name: "missing metric", status: http.StatusOK, body: "other_metric 3\n", expected: 3, wantError: "is missing"},
		{
			name:      "duplicate metric",
			status:    http.StatusOK,
			body:      metric("3") + metric("3"),
			expected:  3,
			wantError: "appears more than once",
		},
		{name: "malformed metric", status: http.StatusOK, body: metric("not-a-number"), expected: 3, wantError: "invalid value"},
		{name: "non-200 response", status: http.StatusServiceUnavailable, expected: 3, wantError: "HTTP status 503"},
		{
			name:        "request timeout",
			status:      http.StatusOK,
			body:        metric("3"),
			expected:    3,
			delay:       50 * time.Millisecond,
			clientLimit: 5 * time.Millisecond,
			wantError:   "Client.Timeout",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(test.delay)
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			timeout := time.Second
			if test.clientLimit > 0 {
				timeout = test.clientLimit
			}
			checker := checkProxyAgentStartup(&http.Client{Timeout: timeout}, server.URL, test.expected)
			err := checker(httptest.NewRequest(http.MethodGet, "/startupz", nil))
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

func TestParseOpenServerConnectionsRejectsInvalidSamples(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantError string
	}{
		{name: "negative", value: "-1", wantError: "invalid value"},
		{name: "fractional", value: "1.5", wantError: "invalid value"},
		{name: "not a number", value: "NaN", wantError: "invalid value"},
		{name: "infinite", value: "+Inf", wantError: "invalid value"},
		{name: "too many fields", value: "3 123 extra", wantError: "invalid sample"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := fmt.Sprintf("%s %s\n", proxyAgentOpenServerConnectionsMetric, test.value)
			_, err := parseOpenServerConnections(strings.NewReader(input))
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("parse error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestHealthAndStartupRoutesUseSeparateChecks(t *testing.T) {
	var healthCalls atomic.Int32
	var startupCalls atomic.Int32
	handler := newHealthProbeHandler(
		map[string]healthz.Checker{
			"addon": func(*http.Request) error {
				healthCalls.Add(1)
				return nil
			},
		},
		func(*http.Request) error {
			startupCalls.Add(1)
			return nil
		},
	)

	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if healthResponse.Code != http.StatusOK {
		t.Fatalf("health response status = %d", healthResponse.Code)
	}
	if healthCalls.Load() != 1 || startupCalls.Load() != 0 {
		t.Fatalf("health request calls: health=%d startup=%d", healthCalls.Load(), startupCalls.Load())
	}

	startupResponse := httptest.NewRecorder()
	handler.ServeHTTP(startupResponse, httptest.NewRequest(http.MethodGet, "/startupz", nil))
	if startupResponse.Code != http.StatusOK {
		t.Fatalf("startup response status = %d", startupResponse.Code)
	}
	if healthCalls.Load() != 1 || startupCalls.Load() != 1 {
		t.Fatalf("startup request calls: health=%d startup=%d", healthCalls.Load(), startupCalls.Load())
	}
}
