package utils

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	addonutils "open-cluster-management.io/addon-framework/pkg/utils"

	addonmetrics "open-cluster-management.io/cluster-proxy/pkg/metrics"
)

const (
	HEADERSERVICECA   = "Service-Root-Ca"
	HEADERSERVICECERT = "Service-Client-Cert"
	HEADERSERVICEKEY  = "Service-Client-Key"

	// Cluster-Proxy custom headers for service proxy
	HeaderClusterProxyProto         = "Cluster-Proxy-Proto"
	HeaderClusterProxyNamespace     = "Cluster-Proxy-Namespace"
	HeaderClusterProxyService       = "Cluster-Proxy-Service"
	HeaderClusterProxyPort          = "Cluster-Proxy-Port"
	HeaderClusterProxyAuthorization = "Cluster-Proxy-Authorization"
	HeaderClusterProxyRelayAuth     = "Cluster-Proxy-Relay-Authorization"

	// KubeAPIServerHost is the in-cluster host of the managed cluster's
	// kube-apiserver. GetTargetServiceURLFromRequest produces it, and both
	// service-proxy and service-relay branch on it to recognize kube-apiserver
	// targets.
	KubeAPIServerHost = "kubernetes.default.svc"

	// ServiceAccountCAFile and ServiceAccountTokenFile are the in-cluster paths
	// of the mounted ServiceAccount CA bundle and bearer token.
	ServiceAccountCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	ServiceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

// ClusterProxyHeaders enumerates every Cluster-Proxy-* routing and auth header
// the service-proxy adds to a request. It lives next to the header constants so
// that adding a new one here keeps the relay's strip set complete: the relay
// removes all of these before forwarding to the managed cluster's backend.
var ClusterProxyHeaders = []string{
	HeaderClusterProxyAuthorization,
	HeaderClusterProxyRelayAuth,
	HeaderClusterProxyProto,
	HeaderClusterProxyNamespace,
	HeaderClusterProxyService,
	HeaderClusterProxyPort,
}

const bearerPrefix = "Bearer "

// BearerTokenFromHeader extracts the token from an Authorization-style header
// value of the form "Bearer <token>". It returns an empty string when the value
// is missing the bearer prefix. Both service-proxy and service-relay use it to
// read incoming bearer tokens.
func BearerTokenFromHeader(value string) string {
	if len(value) <= len(bearerPrefix) || !strings.EqualFold(value[:len(bearerPrefix)], bearerPrefix) {
		return ""
	}
	return strings.TrimSpace(value[len(bearerPrefix):])
}

// BearerTokenToHeader builds the Authorization-style header value of the form
// "Bearer <token>" for the given token. It is the inverse of
// BearerTokenFromHeader.
func BearerTokenToHeader(token string) string {
	return bearerPrefix + token
}

// TargetServiceConfig is a collection of data extrict from the request URL description the target service we can to access on the managed cluster.
// There are 2 usages of it:
// 1. used in function `ServiceProxyURL` to construct the target service URL.
// 2. used in function `UpdateRequest` to update the request object.
type TargetServiceConfig struct {
	Cluster   string
	Proto     string
	Service   string
	Namespace string
	Port      string
	Path      string
}

func UpdateRequest(t TargetServiceConfig, req *http.Request) *http.Request {
	// update request URL path
	req.URL.Path = t.Path

	// populate proto, namespace, service, and port to request headers
	req.Header.Set(HeaderClusterProxyProto, t.Proto)
	req.Header.Set(HeaderClusterProxyNamespace, t.Namespace)
	req.Header.Set(HeaderClusterProxyService, t.Service)
	req.Header.Set(HeaderClusterProxyPort, t.Port)

	return req
}

// GetTargetServiceConfig extrict the target service config from requestURL
// input: https://<route location cluster-proxy>/cluster1/api/v1/namespaces/default/services/<https:helloworld:8080>/proxy-service/ping?time-out=32s
// output: TargetServiceConfig{Cluster: cluster1, Proto: https, Service: helloworld, Namespace: default, Port: 8080, Path: /ping}
func GetTargetServiceConfig(requestURL string) (ts TargetServiceConfig, err error) {
	urlparams := strings.Split(requestURL, "/")
	if len(urlparams) < 9 {
		err = fmt.Errorf("requestURL format not correct, path less than 9: %s", requestURL)
		return
	}

	namespace := urlparams[5]

	proto, service, port, valid := utilnet.SplitSchemeNamePort(urlparams[7])
	if !valid {
		return TargetServiceConfig{}, fmt.Errorf("invalid service name %q", urlparams[7])
	}
	if proto == "" {
		proto = "https" // set a default to https
	}

	servicePath := strings.Join(urlparams[9:], "/")
	servicePath = strings.Split(servicePath, "?")[0] //we only need path here, the proxy pkg would add params back

	return TargetServiceConfig{
		Cluster:   urlparams[1],
		Proto:     proto,
		Service:   service,
		Namespace: namespace,
		Port:      port,
		Path:      servicePath,
	}, nil
}

// GetTargetServiceConfigForKubeAPIServer extrict the kube apiserver config from requestURL
// input: https://<route location cluster-proxy>/cluster1/api/pods?timeout=32s
// output: TargetServiceConfig{Cluster: cluster1, Proto: https, Service: kubernetes, Namespace: default, Port: 443, Path: api/pods}
func GetTargetServiceConfigForKubeAPIServer(requestURL string) (ts TargetServiceConfig, err error) {
	ts = TargetServiceConfig{
		Proto:     "https",
		Service:   "kubernetes",
		Namespace: "default",
		Port:      "443",
	}

	paths := strings.Split(requestURL, "/")
	if len(paths) <= 2 {
		err = fmt.Errorf("requestURL format not correct, path more than 2: %s", requestURL)
		return
	}
	kubeAPIPath := strings.Join(paths[2:], "/")      // api/pods?timeout=32s
	kubeAPIPath = strings.Split(kubeAPIPath, "?")[0] // api/pods note: we only need path here, the proxy pkg would add params back

	ts.Cluster = paths[1]
	ts.Path = kubeAPIPath
	return ts, nil
}

// GetTargetServiceURLFromRequest is used on the agent side, the service-proxy agent received a request from the proxy-agent, and need to know the target service URL to do further proxy.
func GetTargetServiceURLFromRequest(req *http.Request) (*url.URL, error) {
	// get proto, namespace, service, and port from request headers
	proto := req.Header.Get(HeaderClusterProxyProto)
	namespace := req.Header.Get(HeaderClusterProxyNamespace)
	service := req.Header.Get(HeaderClusterProxyService)
	port := req.Header.Get(HeaderClusterProxyPort)

	// validate proto, namespace, service, and port
	if proto == "" || namespace == "" || service == "" || port == "" {
		return nil, fmt.Errorf("invalid request headers")
	}

	var targetServiceURL string
	// check if the request is meant to proxy to kube-apiserver
	if proto == "https" && service == "kubernetes" && namespace == "default" && port == "443" {
		targetServiceURL = "https://" + KubeAPIServerHost
	} else {
		targetServiceURL = fmt.Sprintf("%s://%s.%s.svc:%s", proto, service, namespace, port)
	}

	url, err := url.Parse(targetServiceURL)
	if err != nil {
		return nil, err
	}

	return url, nil
}

const (
	ProxyTypeService = iota
	ProxyTypeKubeAPIServer
)

// GetProxyType determines whether a request meant to proxy to a regular service or the kube-apiserver of the managed cluster.
// An example of service: https://<route location cluster-proxy>/<managed_cluster_name>/api/v1/namespaces/<namespace_name>/services/<[https:]service_name[:port_name]>/proxy-service/<service_path>
// An example of kube-apiserver: https://<route location cluster-proxy>/<managed_cluster_name>/api/pods?timeout=32s
func GetProxyType(reqURI string) int {
	urlparams := strings.Split(reqURI, "/")
	if len(urlparams) > 9 && urlparams[8] == "proxy-service" {
		return ProxyTypeService
	}
	return ProxyTypeKubeAPIServer
}

// ServeHealthProbes serves health probes and configchecker.
func ServeHealthProbes(healthProbeBindAddress string, tlsConfig *tls.Config, customChecks ...healthz.Checker) error {
	mux := http.NewServeMux()

	checks := map[string]healthz.Checker{
		"healthz-ping": healthz.Ping,
	}

	for i, check := range customChecks {
		checks[fmt.Sprintf("custom-healthz-checker-%d", i)] = check
	}

	mux.Handle("/healthz", http.StripPrefix("/healthz", &healthz.Handler{Checks: checks}))
	mux.Handle("/metrics", legacyregistry.Handler())
	server := http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		Addr:              healthProbeBindAddress,
		// TLS config is currently unused, as below we are using ListenAndServe() which will serve HTTP
		TLSConfig: tlsConfig,
	}
	klog.Infof("heath probes server is running...")
	return server.ListenAndServe()
}

// AppendServiceCA loads a PEM CA bundle from path into pool and, when the file
// exists, returns a config checker so the process is restarted on rotation. A
// missing file is logged and skipped (returns nil, nil).
func AppendServiceCA(pool *x509.CertPool, name, path string) (healthz.Checker, error) {
	caData, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			klog.Infof("%s file not found, skipping: %s", name, path)
			return nil, nil
		}
		return nil, err
	}
	if !pool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("failed to parse %s %s", name, path)
	}
	cc, err := addonutils.NewConfigChecker(name, path)
	if err != nil {
		return nil, err
	}
	return cc.Check, nil
}

// ResultFromStatus classifies an HTTP status code as a success or error
// metric label value.
func ResultFromStatus(statusCode int) string {
	if statusCode >= http.StatusBadRequest {
		return addonmetrics.ResultError
	}
	return addonmetrics.ResultSuccess
}

const (
	// DefaultMaxIdleConns, DefaultIdleConnTimeout, DefaultTLSHandshakeTimeout and
	// DefaultExpectContinueTimeout tune the forwarding transport built by
	// NewForwardingTransport. service-proxy exposes them as flags and service-relay
	// uses them directly, so they share these defaults.
	DefaultMaxIdleConns          = 100
	DefaultIdleConnTimeout       = 90 * time.Second
	DefaultTLSHandshakeTimeout   = 10 * time.Second
	DefaultExpectContinueTimeout = 1 * time.Second
)

// NewForwardingTransport builds the http.Transport that service-proxy and
// service-relay use to forward requests to managed-cluster backends. HTTP/2
// auto-upgrade is disabled because the golang http pkg automatically upgrades to
// http2, but http2 connections cannot be upgraded to the SPDY protocol used by
// "kubectl exec"; forcing HTTP/1.1 keeps streaming and connection upgrades
// working.
func NewForwardingTransport(tlsConfig *tls.Config, maxIdleConns int, idleConnTimeout, tlsHandshakeTimeout, expectContinueTimeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          maxIdleConns,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     false,
	}
}

// ServeReverseProxy forwards req to target through transport and returns the
// captured response status code so callers can record a per-request metric via
// ResultFromStatus. On a transport-level failure it reports err through logError
// and returns a generic "bad gateway" to the caller, never leaking backend
// hostnames or TLS details. Both service-proxy and service-relay proxy through
// it; logError lets each keep its own request-scoped logger.
func ServeReverseProxy(w http.ResponseWriter, req *http.Request, target *url.URL, transport http.RoundTripper, logError func(error)) int {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		logError(err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
	recorder := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
	proxy.ServeHTTP(recorder, req)
	return recorder.statusCode
}

// statusRecorder wraps http.ResponseWriter to capture the response status
// code while transparently forwarding Flush and Hijack so reverse proxies
// can still stream responses and upgrade connections.
type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}
