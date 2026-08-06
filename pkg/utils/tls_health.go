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
	return NewDynamicTLSHealthChecker(address, func() *tls.Config {
		return tlsConfig.Clone()
	}, timeout)
}

// NewDynamicTLSHealthChecker verifies a fresh TLS connection using a current
// config snapshot for every invocation.
func NewDynamicTLSHealthChecker(address string, tlsConfig func() *tls.Config, timeout time.Duration) healthz.Checker {

	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), timeout)
		defer cancel()

		dialer := &tls.Dialer{Config: tlsConfig()}
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("tls handshake with %s failed: %w", address, err)
		}
		defer conn.Close()

		return nil
	}
}
