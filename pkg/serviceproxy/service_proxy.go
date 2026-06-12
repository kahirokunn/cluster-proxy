package serviceproxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"sigs.k8s.io/controller-runtime/pkg/healthz"

	addonutils "open-cluster-management.io/addon-framework/pkg/utils"
	"open-cluster-management.io/cluster-proxy/pkg/constant"
	addonmetrics "open-cluster-management.io/cluster-proxy/pkg/metrics"
	"open-cluster-management.io/cluster-proxy/pkg/utils"
	sdktls "open-cluster-management.io/sdk-go/pkg/tls"
)

func NewServiceProxyCommand() *cobra.Command {
	serviceProxyServer := newServiceProxy()

	cmd := &cobra.Command{
		Use:   "service-proxy",
		Short: "service-proxy",
		Long:  `A http proxy server, receives http requests from proxy-agent and forwards to the target service.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return serviceProxyServer.Run(cmd.Context())
		},
	}

	serviceProxyServer.AddFlags(cmd)
	return cmd
}

type serviceProxy struct {
	cert, key           string
	additionalServiceCA string
	rootCAs             *x509.CertPool

	maxIdleConns          int
	idleConnTimeout       time.Duration
	tLSHandshakeTimeout   time.Duration
	expectContinueTimeout time.Duration
	transport             http.RoundTripper

	tokenReviewCacheTTL time.Duration
	kubeClientQPS       float32
	kubeClientBurst     int

	hubKubeConfig               string
	managedKubeConfig           string
	managedAPIServerURLTemplate *url.URL
	managedAPIServerTLSConfig   *tls.Config
	hubKubeClient               kubernetes.Interface
	managedClusterKubeClient    kubernetes.Interface

	serviceRelayName string
	serviceRelayPort int
	relayURLTemplate *url.URL

	enableImpersonation bool

	managedClusterAuthenticator authenticator.Token
	hubAuthenticator            authenticator.Token

	// getImpersonateTokenFunc reads the service account token used for impersonation.
	// Defaults to reading the mounted service account token file, and Run swaps in
	// the managed-kubeconfig reader when --managed-kubeconfig is set.
	getImpersonateTokenFunc func() (string, error)
}

func newServiceProxy() *serviceProxy {
	s := &serviceProxy{
		tokenReviewCacheTTL: utils.DefaultTokenReviewCacheTTL,
		kubeClientQPS:       utils.DefaultKubeClientQPS,
		kubeClientBurst:     utils.DefaultKubeClientBurst,
		serviceRelayName:    constant.ServiceRelayName,
		serviceRelayPort:    constant.ServiceRelayPort,
	}
	s.getImpersonateTokenFunc = s.readImpersonateTokenFromFile
	return s
}

func (s *serviceProxy) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	flags.StringVar(&s.cert, "cert", s.cert, "The path to the certificate of the service proxy server")
	flags.StringVar(&s.key, "key", s.key, "The path to the key of the service proxy server")
	flags.StringVar(&s.additionalServiceCA, "additional-service-ca", s.additionalServiceCA, "The path to the additional CA certificate for services")

	// hubKubeConfig is the kubeconfig file for connecting to the hub cluster
	flags.StringVar(&s.hubKubeConfig, "hub-kubeconfig", "", "The kubeconfig file for connecting to the hub cluster")
	flags.StringVar(&s.managedKubeConfig, "managed-kubeconfig", "", "The kubeconfig file for connecting to the managed cluster. If empty, in-cluster config is used")
	flags.StringVar(&s.serviceRelayName, "service-relay-name", s.serviceRelayName, "Name of the service-relay Service on the managed cluster, used with --managed-kubeconfig to build the managed-apiserver proxy URL")
	flags.IntVar(&s.serviceRelayPort, "service-relay-port", s.serviceRelayPort, "Port of the service-relay Service on the managed cluster, used with --managed-kubeconfig to build the managed-apiserver proxy URL")

	// proxy related flags
	flags.IntVar(&s.maxIdleConns, "max-idle-conns", utils.DefaultMaxIdleConns, "The maximum number of idle (keep-alive) connections across all hosts.")
	flags.DurationVar(&s.idleConnTimeout, "idle-conn-timeout", utils.DefaultIdleConnTimeout, "The maximum amount of time an idle (keep-alive) connection will remain idle before closing itself.")
	flags.DurationVar(&s.tLSHandshakeTimeout, "tls-handshake-timeout", utils.DefaultTLSHandshakeTimeout, "The maximum amount of time waiting to wait for a TLS handshake.")
	flags.DurationVar(&s.expectContinueTimeout, "expect-continue-timeout", utils.DefaultExpectContinueTimeout, "The amount of time to wait for a server's first response headers after fully writing the request headers if the request has an \"Expect: 100-continue\" header.")
	flags.BoolVar(&s.enableImpersonation, "enable-impersonation", true, "Whether to enable impersonation")

	// token review cache flags
	flags.DurationVar(&s.tokenReviewCacheTTL, "token-review-cache-ttl", utils.DefaultTokenReviewCacheTTL, "TTL for cached TokenReview results. Set to 0 to disable caching.")

	// kube client rate limiting flags
	flags.Float32Var(&s.kubeClientQPS, "kube-api-qps", utils.DefaultKubeClientQPS, "QPS for kube API clients (managed cluster and hub). Increase if client-side throttling is observed under high concurrency.")
	flags.IntVar(&s.kubeClientBurst, "kube-api-burst", utils.DefaultKubeClientBurst, "Burst for kube API clients (managed cluster and hub).")
}

func (s *serviceProxy) Run(ctx context.Context) error {
	var err error
	customChecks := []healthz.Checker{}

	cc, err := addonutils.NewConfigChecker("cert", s.cert, s.key, utils.ServiceAccountCAFile, s.hubKubeConfig)
	if err != nil {
		return err
	}
	customChecks = append(customChecks, cc.Check)

	// In hosted mode, restart the Pod when the managed kubeconfig's endpoint or
	// TLS material changes (see managedKubeconfigReloadChecksum).
	if s.hostedMode() {
		managedKubeConfigChecker, err := newManagedKubeconfigReloadChecker(s.managedKubeConfig)
		if err != nil {
			return err
		}
		customChecks = append(customChecks, managedKubeConfigChecker)
	}

	if err := s.validate(); err != nil {
		return err
	}

	podNamespace := os.Getenv("POD_NAMESPACE")
	if len(podNamespace) == 0 {
		klog.Fatalf("Pod namespace is empty, please set the ENV for POD_NAMESPACE")
	}

	// get root CAs
	s.rootCAs = x509.NewCertPool()
	// ca for accessing apiserver

	apiserverPem, err := os.ReadFile(utils.ServiceAccountCAFile)
	if err != nil {
		return err
	}
	s.rootCAs.AppendCertsFromPEM(apiserverPem)

	// ca for accessing additional services
	if s.additionalServiceCA != "" {
		check, err := utils.AppendServiceCA(s.rootCAs, "additional-service-ca", s.additionalServiceCA)
		if err != nil {
			return err
		}
		if check != nil {
			customChecks = append(customChecks, check)
		}
	}

	// init managedClusterKubeClient
	managedConfig, err := s.managedRESTConfig()
	if err != nil {
		return err
	}
	managedConfig.QPS = s.kubeClientQPS
	managedConfig.Burst = s.kubeClientBurst

	s.managedClusterKubeClient, err = kubernetes.NewForConfig(managedConfig)
	if err != nil {
		return err
	}
	if s.hostedMode() {
		managedAPIServerURL := managedConfig.Host
		s.managedAPIServerURLTemplate, err = parseManagedAPIServerURL(managedAPIServerURL)
		if err != nil {
			return err
		}
		s.getImpersonateTokenFunc = s.readImpersonateTokenFromManagedKubeconfig
		// Reuse the managed kubeconfig's TLS settings for outbound calls to the
		// managed apiserver (see outboundTLSConfig). rest.TLSConfigFor already
		// builds the RootCAs pool from the kubeconfig's CA, so no separate CA
		// wiring is needed here.
		s.managedAPIServerTLSConfig, err = rest.TLSConfigFor(managedConfig)
		if err != nil {
			return fmt.Errorf("failed to build managed apiserver TLS config: %v", err)
		}
		if s.managedAPIServerTLSConfig != nil && s.managedAPIServerTLSConfig.MinVersion < tls.VersionTLS12 {
			s.managedAPIServerTLSConfig.MinVersion = tls.VersionTLS12
		}
		s.relayURLTemplate = buildServiceRelayURL(s.managedAPIServerURLTemplate, podNamespace, s.serviceRelayName, s.serviceRelayPort)
	}

	// get hubKubeConfig
	hubConfig, err := clientcmd.BuildConfigFromFlags("", s.hubKubeConfig)
	if err != nil {
		return err
	}
	hubConfig.QPS = s.kubeClientQPS
	hubConfig.Burst = s.kubeClientBurst
	s.hubKubeClient, err = kubernetes.NewForConfig(hubConfig)
	if err != nil {
		return err
	}

	// Impersonation mode reviews every hub token against the managed cluster
	// first, where it always fails, so caching unauthenticated results too is
	// critical to avoid an API call per request under high concurrency.
	s.managedClusterAuthenticator = utils.NewCachedTokenAuthenticator(s.managedClusterKubeClient, "managed cluster", s.tokenReviewCacheTTL)
	s.hubAuthenticator = utils.NewCachedTokenAuthenticator(s.hubKubeClient, "hub", s.tokenReviewCacheTTL)
	if s.tokenReviewCacheTTL > 0 {
		klog.Infof("TokenReview cache enabled with TTL %v", s.tokenReviewCacheTTL)
	} else {
		klog.Infof("TokenReview cache disabled")
	}

	// The TLS profile ConfigMap lives in POD_NAMESPACE on the cluster the Pod
	// runs on. In hosted mode that is the hosting cluster, not the managed
	// cluster s.managedClusterKubeClient targets, so watch it with an in-cluster
	// client; in non-hosted mode the managed client already points there.
	hostingKubeClient := s.managedClusterKubeClient
	if s.hostedMode() {
		hostingConfig, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("failed to get in-cluster config for hosting cluster TLS ConfigMap watcher: %v", err)
		}
		hostingConfig.QPS = s.kubeClientQPS
		hostingConfig.Burst = s.kubeClientBurst
		hostingKubeClient, err = kubernetes.NewForConfig(hostingConfig)
		if err != nil {
			return fmt.Errorf("failed to create hosting cluster client for TLS ConfigMap watcher: %v", err)
		}
	}

	sdkTLSConfig, err := sdktls.StartTLSConfigMapWatcher(ctx, hostingKubeClient, podNamespace, func() {
		klog.Info("TLS ConfigMap changed, restarting")
		os.Exit(0)
	})
	if err != nil {
		klog.Fatalf("failed to start TLS ConfigMap watcher: %v", err)
	}
	klog.Infof("TLS config loaded: minVersion=%s, ciphersuites=%s", sdktls.VersionToString(sdkTLSConfig.MinVersion),
		sdktls.CipherSuitesToString(sdkTLSConfig.CipherSuites))

	tlsConfig := &tls.Config{
		MinVersion:   sdkTLSConfig.MinVersion,
		CipherSuites: sdkTLSConfig.CipherSuites,
	}

	go func() {
		// Currently ServeHealthProbes uses HTTP so our tlsConfig is not needed, however passing through for
		// consistency and in case it's ever updated to use HTTPS in the future
		if err = utils.ServeHealthProbes(":8000", tlsConfig, customChecks...); err != nil {
			klog.Fatal(err)
		}
	}()

	// Build the outbound forwarding transport once so its keep-alive connection
	// pool is reused across requests (see outboundTLSConfig for the stable TLS
	// material it depends on).
	s.transport = utils.NewForwardingTransport(s.outboundTLSConfig(), s.maxIdleConns, s.idleConnTimeout, s.tLSHandshakeTimeout, s.expectContinueTimeout)

	httpserver := &http.Server{
		Addr:      fmt.Sprintf(":%d", constant.ServiceProxyPort),
		TLSConfig: tlsConfig,
		Handler:   s,
	}

	return httpserver.ListenAndServeTLS(s.cert, s.key)
}

func (s *serviceProxy) ServeHTTP(wr http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	logger := klog.FromContext(ctx)
	targetKind := "unknown"
	result := addonmetrics.ResultError
	defer func() {
		addonmetrics.ObserveServiceProxyRequest(s.metricsMode(), targetKind, result)
	}()

	if klog.V(4).Enabled() {
		dump, err := httputil.DumpRequest(req, true)
		if err != nil {
			http.Error(wr, err.Error(), http.StatusBadRequest)
			return
		}
		klog.V(4).Infof("request:\n %s", string(dump))
	}

	targetURL, err := utils.GetTargetServiceURLFromRequest(req)
	if err != nil {
		http.Error(wr, err.Error(), http.StatusBadRequest)
		logger.Error(err, "failed to get target service url from request")
		return
	}
	isKubeAPIServer := targetURL.Host == utils.KubeAPIServerHost
	targetKind = "service"
	if isKubeAPIServer {
		targetKind = "kube-apiserver"
	}
	if s.hostedMode() {
		if isKubeAPIServer {
			clone := *s.managedAPIServerURLTemplate
			targetURL = &clone
		} else {
			clone := *s.relayURLTemplate
			targetURL = &clone
			if err := s.prepareRelayRequest(req); err != nil {
				http.Error(wr, err.Error(), http.StatusBadRequest)
				logger.Error(err, "failed to prepare service relay request")
				return
			}
		}
	}

	logger = logger.WithValues(
		"targetHost", targetURL.Host,
		"method", req.Method,
		"path", req.URL.Path,
	)
	ctx = klog.NewContext(ctx, logger)

	logger.V(4).Info("service proxy received request",
		"targetScheme", targetURL.Scheme,
		"enableImpersonation", s.enableImpersonation,
		"isKubeAPIServer", isKubeAPIServer,
	)

	if isKubeAPIServer && s.enableImpersonation {
		if err := s.processAuthentication(ctx, req); err != nil {
			logger.Error(err, "authentication failed")
			http.Error(wr, err.Error(), http.StatusUnauthorized)
			return
		}
	}

	logger.V(6).Info("forwarding request to reverse proxy",
		"targetURL", targetURL.String(),
	)

	status := utils.ServeReverseProxy(wr, req, targetURL, s.transport, func(err error) {
		logger.Error(err, "service proxy reverse proxy error")
	})
	result = utils.ResultFromStatus(status)
}

func (s *serviceProxy) validate() error {
	if s.cert == "" {
		return fmt.Errorf("cert is required")
	}
	if s.key == "" {
		return fmt.Errorf("key is required")
	}
	return nil
}

// hostedMode reports whether the service-proxy runs in hosted mode, where a
// managed kubeconfig is set and requests are relayed to the managed cluster
// instead of served directly from the in-cluster config.
func (s *serviceProxy) hostedMode() bool {
	return s.managedKubeConfig != ""
}

func (s *serviceProxy) metricsMode() string {
	if s.hostedMode() {
		return "relay"
	}
	return "direct"
}

func (s *serviceProxy) managedRESTConfig() (*rest.Config, error) {
	if !s.hostedMode() {
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get in-cluster config: %v", err)
		}
		return config, nil
	}

	config, err := clientcmd.BuildConfigFromFlags("", s.managedKubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build managed kubeconfig: %v", err)
	}
	return config, nil
}

// newManagedKubeconfigReloadChecker returns a health checker that reports
// unhealthy once the managed kubeconfig's endpoint or TLS material changes from
// the baseline captured at startup, restarting the Pod to pick it up. The
// baseline is never updated (matching the reload=false config-checker semantics)
// and excludes the bearer token (see managedKubeconfigReloadChecksum).
func newManagedKubeconfigReloadChecker(path string) (healthz.Checker, error) {
	baseline, err := managedKubeconfigReloadChecksum(path)
	if err != nil {
		return nil, err
	}
	return func(_ *http.Request) error {
		current, err := managedKubeconfigReloadChecksum(path)
		if err != nil {
			return err
		}
		if current != baseline {
			return fmt.Errorf("managed kubeconfig endpoint or TLS material changed")
		}
		return nil
	}, nil
}

// managedKubeconfigReloadChecksum hashes the parts of the managed kubeconfig
// whose change requires a restart — the apiserver endpoint and the TLS trust
// material (CA bundle, server name, client certs) — while excluding the bearer
// token, which is refreshed routinely and read fresh on every request.
func managedKubeconfigReloadChecksum(path string) ([32]byte, error) {
	config, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return [32]byte{}, err
	}
	for _, authInfo := range config.AuthInfos {
		if authInfo == nil {
			continue
		}
		authInfo.Token = ""
		authInfo.TokenFile = ""
	}
	raw, err := clientcmd.Write(*config)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

// outboundTLSConfig returns the TLS config used for proxied outbound calls.
// In hosted mode every outbound call reaches the managed cluster, so the managed
// kubeconfig's TLS settings (ServerName, client cert, InsecureSkipVerify) are
// reused to keep hostname verification and mTLS working. Otherwise it falls back
// to the in-cluster trust pool augmented with the additional service CA.
func (s *serviceProxy) outboundTLSConfig() *tls.Config {
	if s.managedAPIServerTLSConfig != nil {
		return s.managedAPIServerTLSConfig
	}
	return &tls.Config{
		RootCAs:    s.rootCAs,
		MinVersion: tls.VersionTLS12,
	}
}

func parseManagedAPIServerURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("managed apiserver URL %q must include scheme and host", rawURL)
	}
	return parsed, nil
}

// buildServiceRelayURL derives the relay proxy URL from the already-parsed
// managed apiserver URL template, leaving the template untouched so it can still
// be cloned for direct kube-apiserver requests.
func buildServiceRelayURL(template *url.URL, namespace, relayName string, relayPort int) *url.URL {
	relayURL := *template
	relayURL.Path = fmt.Sprintf(
		"/api/v1/namespaces/%s/services/http:%s:%d/proxy",
		url.PathEscape(namespace),
		relayName,
		relayPort,
	)
	relayURL.RawQuery = ""
	return &relayURL
}

func (s *serviceProxy) prepareRelayRequest(req *http.Request) error {
	authorization := req.Header.Get("Authorization")
	req.Header.Del(utils.HeaderClusterProxyAuthorization)
	if authorization != "" {
		req.Header.Set(utils.HeaderClusterProxyAuthorization, authorization)
	}

	token, err := s.impersonateToken()
	if err != nil {
		return fmt.Errorf("failed to get managed kubeconfig token: %v", err)
	}
	managedAuthorization := utils.BearerTokenToHeader(token)
	req.Header.Set("Authorization", managedAuthorization)
	req.Header.Set(utils.HeaderClusterProxyRelayAuth, managedAuthorization)
	return nil
}

// impersonateToken returns the service-account bearer token used to talk to the
// managed cluster, trimming surrounding whitespace and rejecting empty tokens so
// callers do not have to repeat that validation.
func (s *serviceProxy) impersonateToken() (string, error) {
	token, err := s.getImpersonateTokenFunc()
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("impersonate token is empty")
	}
	return token, nil
}

func (s *serviceProxy) readImpersonateTokenFromFile() (string, error) {
	// Read the latest token from the mounted file
	token, err := os.ReadFile(utils.ServiceAccountTokenFile)
	if err != nil {
		return "", err
	}
	return string(token), nil
}

func (s *serviceProxy) readImpersonateTokenFromManagedKubeconfig() (string, error) {
	config, err := clientcmd.LoadFromFile(s.managedKubeConfig)
	if err != nil {
		return "", err
	}

	authInfo, err := utils.CurrentAuthInfo(config)
	if err != nil {
		return "", err
	}
	if authInfo.Token != "" {
		return authInfo.Token, nil
	}
	if authInfo.TokenFile != "" {
		token, err := os.ReadFile(authInfo.TokenFile)
		if err != nil {
			return "", err
		}
		return string(token), nil
	}
	return "", fmt.Errorf("managed kubeconfig does not contain a bearer token")
}

// processAuthentication handles the authentication flow for both managed cluster and hub users.
// It tries managed cluster TokenReview first; if unauthenticated, falls back to hub TokenReview.
func (s *serviceProxy) processAuthentication(ctx context.Context, req *http.Request) error {
	logger := klog.FromContext(ctx)
	token := utils.BearerTokenFromHeader(req.Header.Get("Authorization"))

	logger.V(6).Info("processing authentication for request",
		"tokenPresent", token != "",
		"tokenLength", len(token),
	)

	// try managed cluster authentication first
	managedClusterResp, managedClusterAuthenticated, err := s.managedClusterAuthenticator.AuthenticateToken(ctx, token)
	if err != nil {
		return fmt.Errorf("managed cluster authentication failed: %v", err)
	}

	if managedClusterAuthenticated {
		logger.V(4).Info("managed cluster authentication succeeded",
			"username", managedClusterResp.User.GetName(),
		)
		return nil
	}
	logger.V(4).Info("managed cluster authentication result", "authenticated", false)

	// try hub authentication
	hubResp, hubAuthenticated, err := s.hubAuthenticator.AuthenticateToken(ctx, token)
	if err != nil {
		return fmt.Errorf("hub authentication failed: %v", err)
	}
	logger.V(4).Info("hub cluster authentication result", "authenticated", hubAuthenticated)

	if !hubAuthenticated {
		return fmt.Errorf("the token is not authenticated")
	}

	if err := s.processHubUser(ctx, req, hubResp.User); err != nil {
		return fmt.Errorf("failed to process hub user: %v", err)
	}

	return nil
}

// processHubUser handles the hub user specific operations including impersonation
func (s *serviceProxy) processHubUser(ctx context.Context, req *http.Request, hubUser user.Info) error {
	logger := klog.FromContext(ctx)

	// set impersonate group header
	for _, group := range hubUser.GetGroups() {
		// Here using `Add` instead of `Set` to support multiple groups
		req.Header.Add("Impersonate-Group", group)
	}

	// check if the hub user is serviceaccount kind, if so, add "cluster:hub:" prefix to the username
	username := hubUser.GetName()
	isServiceAccount := strings.HasPrefix(username, "system:serviceaccount:")
	impersonateUser := username
	if isServiceAccount {
		impersonateUser = fmt.Sprintf("cluster:hub:%s", username)
	}
	req.Header.Set("Impersonate-User", impersonateUser)

	logger.V(4).Info("impersonation headers set for hub user",
		"impersonateUser", impersonateUser,
		"impersonateGroups", hubUser.GetGroups(),
		"isServiceAccount", isServiceAccount,
	)

	// replace the original token with cluster-proxy service-account token which has impersonate permission
	token, err := s.impersonateToken()
	if err != nil {
		return fmt.Errorf("failed to get impersonate token: %v", err)
	}

	req.Header.Set("Authorization", utils.BearerTokenToHeader(token))

	logger.V(6).Info("original bearer token replaced with service account impersonation token")

	return nil
}
