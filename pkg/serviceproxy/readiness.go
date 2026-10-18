package serviceproxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

const tlsHealthCheckTimeout = 500 * time.Millisecond

func newTLSHealthChecker(address string, tlsConfig *tls.Config, timeout time.Duration) healthz.Checker {
	config := tlsConfig.Clone()

	return func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), timeout)
		defer cancel()

		dialer := tls.Dialer{
			NetDialer: &net.Dialer{},
			Config:    config.Clone(),
		}
		conn, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("tls handshake with %s failed: %w", address, err)
		}
		defer conn.Close()

		return nil
	}
}
