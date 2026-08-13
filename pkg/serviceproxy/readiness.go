package serviceproxy

import (
	"crypto/tls"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

const tlsHealthCheckTimeout = 500 * time.Millisecond

func newTLSHealthChecker(address string, tlsConfig *tls.Config, timeout time.Duration) healthz.Checker {
	return utils.NewTLSHealthChecker(address, tlsConfig, timeout)
}
