package utils

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// NewTLSHealthChecker returns a checker that establishes and verifies a fresh
// TLS connection for every invocation.
func NewTLSHealthChecker(address string, tlsConfig *tls.Config, timeout time.Duration) healthz.Checker {
	dialer := &tls.Dialer{Config: tlsConfig.Clone()}

	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), timeout)
		defer cancel()

		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("tls handshake with %s failed: %w", address, err)
		}
		defer conn.Close()

		return nil
	}
}
