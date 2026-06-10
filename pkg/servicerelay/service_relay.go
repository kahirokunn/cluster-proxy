package servicerelay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	addonutils "open-cluster-management.io/addon-framework/pkg/utils"
	"open-cluster-management.io/cluster-proxy/pkg/constant"
	addonmetrics "open-cluster-management.io/cluster-proxy/pkg/metrics"
	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

const (
	// defaultTokenReviewCacheTTL deduplicates concurrent TokenReviews for the same
	// bearer token; a short TTL still picks up token revocation quickly.
	defaultTokenReviewCacheTTL = 10 * time.Second

	// defaultKubeClientQPS and defaultKubeClientBurst raise the client-go defaults,
	// which are too low for the relay's per-request TokenReview path.
	defaultKubeClientQPS   = 50.0
	defaultKubeClientBurst = 100
)

// ServiceRelay proxies requests from the hosted service-proxy to other
// in-cluster Services on the managed cluster. To avoid being an open proxy, it
// re-authenticates every request via TokenReview and rejects callers whose
// username is not in TrustedCallerUsernames.
type ServiceRelay struct {
	Listen                 string
	AdditionalServiceCA    string
	HealthProbeBindAddress string
	TrustedCallerUsernames []string

	TokenReviewCacheTTL time.Duration

	KubeClientQPS   float32
	KubeClientBurst int

	rootCAs            *x509.CertPool
	transport          http.RoundTripper
	authenticateCaller func(ctx context.Context, req *http.Request) error
}

func NewCommand() *cobra.Command {
	relay := &ServiceRelay{
		Listen:                 fmt.Sprintf(":%d", constant.ServiceRelayPort),
		HealthProbeBindAddress: ":8000",
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
		"TokenReview usernames permitted to forward requests through the relay. Repeat the flag or use a comma-separated list. Requests are rejected if the authenticated caller is not in this set.")
	flags.DurationVar(&relay.TokenReviewCacheTTL, "token-review-cache-ttl", relay.TokenReviewCacheTTL,
		"TTL for cached TokenReview results used to authenticate inbound relay callers. Set to 0 to disable caching and TokenReview the managed kube-apiserver on every request.")
	flags.Float32Var(&relay.KubeClientQPS, "kube-api-qps", relay.KubeClientQPS,
		"QPS for the in-cluster kube client used to call TokenReview against the managed kube-apiserver. Increase if the relay is throttled under high request rates.")
	flags.IntVar(&relay.KubeClientBurst, "kube-api-burst", relay.KubeClientBurst,
		"Burst for the in-cluster kube client used to call TokenReview against the managed kube-apiserver.")

	return cmd
}

func (s *ServiceRelay) Run(ctx context.Context) error {
	if s.Listen == "" {
		return fmt.Errorf("listen address is required")
	}

	s.rootCAs, _ = x509.SystemCertPool()
	if s.rootCAs == nil {
		s.rootCAs = x509.NewCertPool()
	}

	var customChecks []healthz.Checker
	if s.AdditionalServiceCA != "" {
		caData, err := os.ReadFile(s.AdditionalServiceCA)
		if err != nil {
			if os.IsNotExist(err) {
				klog.Infof("additional-service-ca file not found: %s", s.AdditionalServiceCA)
			} else {
				return err
			}
		} else if ok := s.rootCAs.AppendCertsFromPEM(caData); !ok {
			return fmt.Errorf("failed to parse additional service CA %s", s.AdditionalServiceCA)
		} else {
			// Wire the additional-service-ca file into the healthz endpoint
			// so the kubelet's liveness probe restarts the relay when the
			// mounted CA bundle changes. Without this, hosted Relay HTTPS
			// backend CA rotations are not picked up until the Pod is
			// restarted out-of-band, because s.rootCAs is captured once at
			// startup into s.transport's TLS config.
			cc, err := addonutils.NewConfigChecker("additional-service-ca", s.AdditionalServiceCA)
			if err != nil {
				return err
			}
			customChecks = append(customChecks, cc.Check)
		}
	}

	if s.transport == nil {
		s.transport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			TLSClientConfig: &tls.Config{
				RootCAs:    s.rootCAs,
				MinVersion: tls.VersionTLS12,
			},
			ForceAttemptHTTP2: false,
		}
	}

	if s.authenticateCaller == nil {
		trusted := normalizeTrustedCallers(s.TrustedCallerUsernames)
		if len(trusted) == 0 {
			return fmt.Errorf("at least one --trusted-caller-username is required")
		}
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
		s.authenticateCaller = newTokenReviewAuthenticator(kubeClient, trusted, s.TokenReviewCacheTTL)
		klog.Infof("service relay caller authentication enabled for %d trusted username(s), token-review-cache-ttl=%v, kube-api-qps=%v, kube-api-burst=%d",
			len(trusted), s.TokenReviewCacheTTL, s.KubeClientQPS, s.KubeClientBurst)
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

func (s *ServiceRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Enforce the trust boundary before honoring any routing headers.
	if err := s.checkCaller(req); err != nil {
		klog.V(2).Infof("rejecting service relay request: %v", err)
		rejectRequest(w, "unknown", "unauthorized", http.StatusUnauthorized)
		return
	}

	target, err := utils.GetTargetServiceURLFromRequest(req)
	if err != nil {
		rejectRequest(w, "unknown", err.Error(), http.StatusBadRequest)
		return
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		// Use a bounded label rather than target.Scheme so a hostile or
		// misconfigured caller cannot inflate Prometheus label cardinality
		// by sending arbitrary Cluster-Proxy-Proto values.
		rejectRequest(w, "unsupported", fmt.Sprintf("unsupported target scheme %q", target.Scheme), http.StatusBadRequest)
		return
	}
	if target.Host == utils.KubeAPIServerHost {
		rejectRequest(w, target.Scheme, "service relay does not proxy kube-apiserver requests", http.StatusBadRequest)
		return
	}

	stripClusterProxyHeaders(req)

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = s.transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		// Log the transport error server-side but return a generic body: the raw
		// error often embeds backend hostnames, ClusterIPs, or TLS details that
		// should not be exposed to callers.
		klog.Errorf("service relay proxy error: %v", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}

	recorder := &utils.StatusRecorder{ResponseWriter: w, StatusCode: http.StatusOK}
	proxy.ServeHTTP(recorder, req)
	addonmetrics.ObserveServiceRelayRequest(target.Scheme, utils.ResultFromStatus(recorder.StatusCode))
}

// rejectRequest writes an HTTP error response and records a failed-request
// metric under the given scheme label.
func rejectRequest(w http.ResponseWriter, scheme, msg string, code int) {
	http.Error(w, msg, code)
	addonmetrics.ObserveServiceRelayRequest(scheme, "error")
}

// checkCaller verifies the request comes from a trusted caller, rejecting every
// request when no authenticator has been wired up by Run.
func (s *ServiceRelay) checkCaller(req *http.Request) error {
	if s.authenticateCaller == nil {
		return fmt.Errorf("service relay caller authentication is not configured")
	}
	return s.authenticateCaller(req.Context(), req)
}

func normalizeTrustedCallers(names []string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, raw := range names {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				result[part] = struct{}{}
			}
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

// clusterProxyHeaders are the routing and auth headers the service-proxy adds;
// the relay strips them all before forwarding the request to the backend Service.
var clusterProxyHeaders = []string{
	utils.HeaderClusterProxyAuthorization,
	utils.HeaderClusterProxyRelayAuth,
	utils.HeaderClusterProxyProto,
	utils.HeaderClusterProxyNamespace,
	utils.HeaderClusterProxyService,
	utils.HeaderClusterProxyPort,
}

// stripClusterProxyHeaders restores the original caller Authorization header from
// Cluster-Proxy-Authorization and removes all Cluster-Proxy-* headers.
func stripClusterProxyHeaders(req *http.Request) {
	authorization := req.Header.Get(utils.HeaderClusterProxyAuthorization)
	req.Header.Del("Authorization")
	for _, header := range clusterProxyHeaders {
		req.Header.Del(header)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
}
