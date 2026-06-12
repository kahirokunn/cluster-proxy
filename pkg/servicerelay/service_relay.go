package servicerelay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"open-cluster-management.io/cluster-proxy/pkg/constant"
	addonmetrics "open-cluster-management.io/cluster-proxy/pkg/metrics"
	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

// ServiceRelay proxies requests from the hosted service-proxy to in-cluster
// Services on the managed cluster, re-authenticating every caller via
// TokenReview against TrustedCallerUsernames.
type ServiceRelay struct {
	Listen                 string
	AdditionalServiceCA    string
	HealthProbeBindAddress string
	TrustedCallerUsernames []string

	TokenReviewCacheTTL time.Duration

	KubeClientQPS   float32
	KubeClientBurst int

	transport          http.RoundTripper
	authenticateCaller func(ctx context.Context, req *http.Request) error
}

func NewCommand() *cobra.Command {
	relay := &ServiceRelay{
		Listen:                 fmt.Sprintf(":%d", constant.ServiceRelayPort),
		HealthProbeBindAddress: fmt.Sprintf(":%d", constant.ServiceRelayHealthProbePort),
		TokenReviewCacheTTL:    utils.DefaultTokenReviewCacheTTL,
		KubeClientQPS:          utils.DefaultKubeClientQPS,
		KubeClientBurst:        utils.DefaultKubeClientBurst,
	}

	cmd := &cobra.Command{
		Use:   "service-relay",
		Short: "Relay hosted service-proxy requests to managed cluster Services",
		RunE: func(cmd *cobra.Command, args []string) error {
			return relay.Run(cmd.Context())
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&relay.Listen, "listen", relay.Listen, "The HTTP listen address")
	flags.StringVar(&relay.AdditionalServiceCA, "additional-service-ca", relay.AdditionalServiceCA, "The path to the additional CA certificate for services")
	flags.StringVar(&relay.HealthProbeBindAddress, "health-probe-bind-address", relay.HealthProbeBindAddress, "The address the health probe and metrics endpoint binds to")
	flags.StringSliceVar(&relay.TrustedCallerUsernames, "trusted-caller-username", relay.TrustedCallerUsernames,
		"TokenReview usernames permitted to forward requests through the relay. Repeat the flag or use a comma-separated list.")
	flags.DurationVar(&relay.TokenReviewCacheTTL, "token-review-cache-ttl", relay.TokenReviewCacheTTL,
		"TTL for cached TokenReview results. Set to 0 to disable caching.")
	flags.Float32Var(&relay.KubeClientQPS, "kube-api-qps", relay.KubeClientQPS,
		"QPS for the in-cluster kube client used for TokenReview. Increase if client-side throttling is observed under high concurrency.")
	flags.IntVar(&relay.KubeClientBurst, "kube-api-burst", relay.KubeClientBurst,
		"Burst for the in-cluster kube client used for TokenReview.")

	return cmd
}

func (s *ServiceRelay) Run(ctx context.Context) error {
	trustedCallers, err := s.validate()
	if err != nil {
		return err
	}

	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}

	var customChecks []healthz.Checker

	// Trust the managed cluster CA and any additional service CA for outbound
	// HTTPS, wiring each into healthz so the relay restarts on CA rotation.
	serviceCAs := []struct{ name, path string }{
		{"serviceaccount-ca", utils.ServiceAccountCAFile},
		{"additional-service-ca", s.AdditionalServiceCA},
	}
	for _, ca := range serviceCAs {
		if ca.path == "" {
			continue
		}
		check, err := utils.AppendServiceCA(rootCAs, ca.name, ca.path)
		if err != nil {
			return err
		}
		if check != nil {
			customChecks = append(customChecks, check)
		}
	}

	s.transport = utils.NewForwardingTransport(
		&tls.Config{RootCAs: rootCAs, MinVersion: tls.VersionTLS12},
		utils.DefaultMaxIdleConns, utils.DefaultIdleConnTimeout, utils.DefaultTLSHandshakeTimeout, utils.DefaultExpectContinueTimeout)

	if s.authenticateCaller == nil {
		config, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("failed to build in-cluster config for caller authentication: %v", err)
		}
		config.QPS = s.KubeClientQPS
		config.Burst = s.KubeClientBurst
		kubeClient, err := kubernetes.NewForConfig(config)
		if err != nil {
			return fmt.Errorf("failed to build kube client for caller authentication: %v", err)
		}
		s.authenticateCaller = newTokenReviewAuthenticator(kubeClient, trustedCallers, s.TokenReviewCacheTTL)
		klog.Infof("service relay caller authentication enabled for %d trusted username(s), token-review-cache-ttl=%v, kube-api-qps=%v, kube-api-burst=%d",
			len(trustedCallers), s.TokenReviewCacheTTL, s.KubeClientQPS, s.KubeClientBurst)
	}

	go func() {
		if err := utils.ServeHealthProbes(s.HealthProbeBindAddress, nil, customChecks...); err != nil {
			klog.Fatal(err)
		}
	}()

	server := &http.Server{
		Addr:              s.Listen,
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	klog.Infof("service relay listening on %s", s.Listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return ctx.Err()
}

func (s *ServiceRelay) validate() (map[string]struct{}, error) {
	if s.Listen == "" {
		return nil, fmt.Errorf("listen address is required")
	}
	trustedCallers := normalizeTrustedCallers(s.TrustedCallerUsernames)
	if len(trustedCallers) == 0 {
		return nil, fmt.Errorf("at least one --trusted-caller-username is required")
	}
	return trustedCallers, nil
}

func (s *ServiceRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Record the request outcome once, mirroring service-proxy's deferred metric.
	scheme := "unknown"
	result := addonmetrics.ResultError
	defer func() {
		addonmetrics.ObserveServiceRelayRequest(scheme, result)
	}()

	// Enforce the trust boundary before honoring any routing headers.
	if err := s.checkCaller(req); err != nil {
		klog.V(2).Infof("rejecting service relay request: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	target, err := utils.GetTargetServiceURLFromRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		// Use a bounded metric label rather than the caller-controlled scheme.
		scheme = "unsupported"
		http.Error(w, fmt.Sprintf("unsupported target scheme %q", target.Scheme), http.StatusBadRequest)
		return
	}
	scheme = target.Scheme
	if target.Host == utils.KubeAPIServerHost {
		http.Error(w, "service relay does not proxy kube-apiserver requests", http.StatusBadRequest)
		return
	}

	stripClusterProxyHeaders(req)

	status := utils.ServeReverseProxy(w, req, target, s.transport, func(err error) {
		klog.Errorf("service relay proxy error: %v", err)
	})
	result = utils.ResultFromStatus(status)
}

// checkCaller verifies the request comes from a trusted caller, rejecting every
// request when no authenticator has been wired up by Run.
func (s *ServiceRelay) checkCaller(req *http.Request) error {
	if s.authenticateCaller == nil {
		return fmt.Errorf("service relay caller authentication is not configured")
	}
	return s.authenticateCaller(req.Context(), req)
}

// normalizeTrustedCallers trims and deduplicates the trusted caller usernames.
// The flag is a StringSliceVar, so cobra already splits comma-separated values
// before they reach here.
func normalizeTrustedCallers(names []string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, raw := range names {
		if name := strings.TrimSpace(raw); name != "" {
			result[name] = struct{}{}
		}
	}
	return result
}

// newTokenReviewAuthenticator returns a caller-authentication function that
// re-authenticates incoming bearer tokens via TokenReview and rejects callers
// whose authenticated username is not in the trusted set.
func newTokenReviewAuthenticator(client kubernetes.Interface, trusted map[string]struct{}, cacheTTL time.Duration) func(context.Context, *http.Request) error {
	authn := utils.NewCachedTokenAuthenticator(client, "service-relay", cacheTTL)
	return func(ctx context.Context, req *http.Request) error {
		token := utils.BearerTokenFromHeader(req.Header.Get("Authorization"))
		if token == "" {
			token = utils.BearerTokenFromHeader(req.Header.Get(utils.HeaderClusterProxyRelayAuth))
		}
		if token == "" {
			return fmt.Errorf("missing bearer token")
		}
		resp, ok, err := authn.AuthenticateToken(ctx, token)
		if err != nil {
			return fmt.Errorf("token review failed: %v", err)
		}
		if !ok {
			return fmt.Errorf("caller token is not authenticated")
		}
		username := resp.User.GetName()
		if _, allowed := trusted[username]; !allowed {
			return fmt.Errorf("caller %q is not in the trusted set", username)
		}
		return nil
	}
}

// stripClusterProxyHeaders restores the original caller Authorization header from
// Cluster-Proxy-Authorization and removes all Cluster-Proxy-* headers.
func stripClusterProxyHeaders(req *http.Request) {
	authorization := req.Header.Get(utils.HeaderClusterProxyAuthorization)
	req.Header.Del("Authorization")
	for _, header := range utils.ClusterProxyHeaders {
		req.Header.Del(header)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
}
