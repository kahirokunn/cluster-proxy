package utils

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

var _ func(string, ...healthz.Checker) error = ServeHealthProbes

var _ func(string, map[string]healthz.Checker, map[string]healthz.Checker) error = ServeHealthProbesWithLivenessAndReadinessChecks

func TestHealthProbeHandler(t *testing.T) {
	failingCheck := func(*http.Request) error {
		return errors.New("check failed")
	}

	tests := []struct {
		name            string
		path            string
		livenessChecks  map[string]healthz.Checker
		readinessChecks map[string]healthz.Checker
		wantStatus      int
	}{
		{
			name:       "healthy liveness",
			path:       "/healthz",
			wantStatus: http.StatusOK,
		},
		{
			name:       "healthy readiness",
			path:       "/readyz",
			wantStatus: http.StatusOK,
		},
		{
			name:       "healthy liveness with trailing slash",
			path:       "/healthz/",
			wantStatus: http.StatusOK,
		},
		{
			name:       "healthy readiness with trailing slash",
			path:       "/readyz/",
			wantStatus: http.StatusOK,
		},
		{
			name:           "unhealthy liveness",
			path:           "/healthz",
			livenessChecks: map[string]healthz.Checker{"config-checksum": failingCheck},
			wantStatus:     http.StatusInternalServerError,
		},
		{
			name:            "unhealthy readiness",
			path:            "/readyz",
			readinessChecks: map[string]healthz.Checker{"service-proxy-tls": failingCheck},
			wantStatus:      http.StatusInternalServerError,
		},
		{
			name:            "failing readiness check does not gate liveness",
			path:            "/healthz",
			livenessChecks:  map[string]healthz.Checker{"config-checksum": healthz.Ping},
			readinessChecks: map[string]healthz.Checker{"service-proxy-tls": failingCheck},
			wantStatus:      http.StatusOK,
		},
		{
			name:            "shared config failure gates service readiness",
			path:            "/readyz",
			livenessChecks:  map[string]healthz.Checker{"config-checksum": failingCheck},
			readinessChecks: map[string]healthz.Checker{"config-checksum": failingCheck, "service-proxy-tls": healthz.Ping},
			wantStatus:      http.StatusInternalServerError,
		},
		{
			name:            "liveness-only check does not gate readiness",
			path:            "/readyz",
			livenessChecks:  map[string]healthz.Checker{"liveness-only": failingCheck},
			readinessChecks: map[string]healthz.Checker{"service-proxy-tls": healthz.Ping},
			wantStatus:      http.StatusOK,
		},
		{
			name:           "named liveness check",
			path:           "/healthz/config-checksum",
			livenessChecks: map[string]healthz.Checker{"config-checksum": failingCheck},
			wantStatus:     http.StatusInternalServerError,
		},
		{
			name:            "named readiness check",
			path:            "/readyz/service-proxy-tls",
			readinessChecks: map[string]healthz.Checker{"service-proxy-tls": failingCheck},
			wantStatus:      http.StatusInternalServerError,
		},
		{
			name:           "liveness-only check is not mounted under readiness",
			path:           "/readyz/liveness-only",
			livenessChecks: map[string]healthz.Checker{"liveness-only": failingCheck},
			wantStatus:     http.StatusNotFound,
		},
		{
			name:            "readiness check is not mounted under liveness",
			path:            "/healthz/service-proxy-tls",
			readinessChecks: map[string]healthz.Checker{"service-proxy-tls": failingCheck},
			wantStatus:      http.StatusNotFound,
		},
		{
			name:       "unknown endpoint",
			path:       "/unknown",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, tt.path, nil)

			newHealthProbeHandler(tt.livenessChecks, tt.readinessChecks).ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("unexpected status: got %d, want %d", recorder.Code, tt.wantStatus)
			}
		})
	}
}

func TestLegacyHealthChecksKeepStableRoutes(t *testing.T) {
	checks := legacyHealthChecks([]healthz.Checker{healthz.Ping})
	handler := newHealthProbeHandler(checks, checks)

	for _, path := range []string{
		"/healthz/healthz-ping",
		"/healthz/custom-healthz-checker-0",
		"/readyz/healthz-ping",
		"/readyz/custom-healthz-checker-0",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody))
			if recorder.Code != http.StatusOK {
				t.Fatalf("unexpected status for %s: got %d, want %d", path, recorder.Code, http.StatusOK)
			}
		})
	}
}
