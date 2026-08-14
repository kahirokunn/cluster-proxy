package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"

	"open-cluster-management.io/addon-framework/pkg/lease"
	addonutils "open-cluster-management.io/addon-framework/pkg/utils"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/util"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

var (
	hubKubeconfig                  string
	clusterName                    string
	proxyServerNamespace           string
	enablePortForwardProxy         bool
	enableProxyAgentHealthCheck    bool
	proxyAgentHealthPort           int
	expectedProxyServerConnections int
)

// envKeyPodNamespace represents the environment variable key for the addon agent namespace.
const envKeyPodNamespace = "POD_NAMESPACE"

// proxyAgentHealthAddr is the address of the proxy-agent health server.
// The addon-agent and proxy-agent containers run in the same Pod and share the network namespace,
// so we can access the proxy-agent's health server via localhost.
const (
	defaultProxyAgentHealthPort           = 8093
	healthProbeAddress                    = ":8888"
	healthCheckTimeout                    = time.Second
	healthShutdownTimeout                 = 5 * time.Second
	proxyAgentOpenServerConnectionsMetric = "konnectivity_network_proxy_agent_open_server_connections"
	maxProxyAgentMetricsBodyBytes         = 1 << 20
)

// checkProxyAgentReadiness returns a health check function that checks if the proxy-agent
// is connected to the proxy-server by querying the proxy-agent's /readyz endpoint.
// Since both containers share the same network namespace within the Pod, this function
// can reach the proxy-agent's health server at localhost:8093.
func checkProxyAgentReadiness(address string) func() bool {
	client := &http.Client{Timeout: 5 * time.Second}
	return func() bool {
		resp, err := client.Get(fmt.Sprintf("http://%s/readyz", address))
		if err != nil {
			klog.V(4).Infof("Failed to check proxy-agent readiness: %v", err)
			return false
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return true
		}
		klog.V(4).Infof("Proxy-agent not ready, status code: %d", resp.StatusCode)
		return false
	}
}

func main() {

	logger := textlogger.NewLogger(textlogger.NewConfig())
	klog.SetOutput(os.Stdout)
	klog.InitFlags(flag.CommandLine)
	flag.StringVar(&hubKubeconfig, "hub-kubeconfig", "",
		"The kubeconfig to talk to hub cluster")
	flag.StringVar(&clusterName, "cluster-name", "",
		"The name of the managed cluster")
	flag.StringVar(&proxyServerNamespace, "proxy-server-namespace", "open-cluster-management-addon",
		"The namespace where proxy-server pod lives")
	flag.BoolVar(&enablePortForwardProxy, "enable-port-forward-proxy", false,
		"If true, running a local server forwarding tunnel shakes to proxy-server pods")
	flag.BoolVar(&enableProxyAgentHealthCheck, "enable-proxy-agent-health-check", true,
		"If true, check proxy-agent connection status before updating lease")
	flag.IntVar(&proxyAgentHealthPort, "proxy-agent-health-port", defaultProxyAgentHealthPort,
		"The local proxy-agent health server port")
	flag.IntVar(&expectedProxyServerConnections, "expected-proxy-server-connections", 1,
		"Number of proxy-server connections required before the proxy-agent startup probe succeeds")
	flag.Parse()

	// pipe controller-runtime logs to klog
	ctrl.SetLogger(logger)
	if err := run(ctrl.SetupSignalHandler()); err != nil {
		klog.Fatalf("addon agent failed: %v", err)
	}
}

func run(ctx context.Context) error {
	if err := validateProxyAgentHealthPort(proxyAgentHealthPort); err != nil {
		return err
	}
	if err := validateExpectedProxyServerConnections(expectedProxyServerConnections); err != nil {
		return err
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", hubKubeconfig)
	if err != nil {
		return err
	}
	cfg.UserAgent = "proxy-agent-addon-agent"

	spokeConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("get spoke config: %w", err)
	}
	spokeClient, err := kubernetes.NewForConfig(spokeConfig)
	if err != nil {
		return fmt.Errorf("failed to create spoke client: %w", err)
	}
	addonAgentNamespace := os.Getenv("POD_NAMESPACE")
	if len(addonAgentNamespace) == 0 {
		return fmt.Errorf("pod namespace is empty, please set the ENV for %s", envKeyPodNamespace)
	}
	proxyAgentHealthAddress := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", proxyAgentHealthPort))

	var healthCheckFuncs []func() bool
	if enableProxyAgentHealthCheck {
		klog.Infof("Proxy-agent health check enabled, lease will only update when proxy-agent is connected")
		healthCheckFuncs = []func() bool{checkProxyAgentReadiness(proxyAgentHealthAddress)}
	}
	leaseUpdater := lease.NewLeaseUpdater(
		spokeClient,
		common.AddonName,
		addonAgentNamespace,
		healthCheckFuncs...,
	).WithHubLeaseConfig(cfg, clusterName)

	readiness := &atomic.Value{}
	readiness.Store(true)

	// This checker intentionally lives only behind /proxy-agent-healthz. It is
	// a one-shot restart trigger for the sibling proxy-agent container and must
	// not make the add-on agent's own liveness or readiness fail.
	cc, err := addonutils.NewConfigChecker(
		"certificates check",
		"/etc/ca/ca.crt",
		"/etc/tls/tls.crt",
		"/etc/tls/tls.key",
	)
	if err != nil {
		return fmt.Errorf("create certificates checker: %w", err)
	}
	cc.SetReload(true)

	healthHandler := newHealthProbeHandler(
		serializedChecker(cc.Check),
		newHTTPHealthChecker("http://"+proxyAgentHealthAddress+"/healthz", healthCheckTimeout),
		portForwardReadinessChecker(readiness),
		checkProxyAgentStartup(
			newProxyAgentMetricsClient(),
			"http://"+proxyAgentHealthAddress+"/metrics",
			expectedProxyServerConnections,
		),
	)
	healthServer, listener, err := newHealthProbeServer(healthProbeAddress, healthHandler)
	if err != nil {
		return err
	}
	defer listener.Close()

	stopPortForward := func() {}
	if enablePortForwardProxy {
		readiness.Store(false)
		klog.Infof("Running local port-forward proxy")
		rr := util.NewRoundRobinLocalProxy(
			cfg,
			readiness,
			proxyServerNamespace,
			common.LabelKeyComponentName+"="+common.ComponentNameProxyServer,
			8091,
		)
		stopPortForward, err = rr.Listen(ctx)
		if err != nil {
			return err
		}
	}
	defer stopPortForward()

	klog.Infof("Starting lease updater")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go leaseUpdater.Start(runCtx)
	return serveHealthProbes(runCtx, healthServer, listener)
}

func validateProxyAgentHealthPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("proxy-agent-health-port must be between 1 and 65535, got %d", port)
	}
	return nil
}

func validateExpectedProxyServerConnections(expected int) error {
	if expected < 0 || expected > math.MaxInt32 {
		return fmt.Errorf("expected-proxy-server-connections must be between 0 and %d, got %d", math.MaxInt32, expected)
	}
	return nil
}

func newHealthProbeHandler(certificates, proxyAgent, portForward, startup healthz.Checker) http.Handler {
	mux := http.NewServeMux()
	mountHealthHandler(mux, "/healthz", map[string]healthz.Checker{"ping": healthz.Ping})
	mountHealthHandler(mux, "/readyz", map[string]healthz.Checker{"port-forward": portForward})
	mountHealthHandler(mux, "/proxy-agent-healthz", map[string]healthz.Checker{
		"certificates": certificates,
		"proxy-agent":  proxyAgent,
	})
	mountHealthHandler(mux, "/startupz", map[string]healthz.Checker{
		"proxy-agent-server-connections": startup,
	})
	return mux
}

func mountHealthHandler(mux *http.ServeMux, basePath string, checks map[string]healthz.Checker) {
	handler := http.StripPrefix(basePath, &healthz.Handler{Checks: checks})
	mux.Handle(basePath, handler)
	mux.Handle(basePath+"/", handler)
}

func serializedChecker(check healthz.Checker) healthz.Checker {
	var mutex sync.Mutex
	return func(request *http.Request) error {
		mutex.Lock()
		defer mutex.Unlock()
		return check(request)
	}
}

func newHTTPHealthChecker(url string, timeout time.Duration) healthz.Checker {
	client := &http.Client{Timeout: timeout}
	return func(request *http.Request) error {
		req, err := http.NewRequestWithContext(request.Context(), http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("check %s: %w", url, err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("check %s returned status %d", url, response.StatusCode)
		}
		return nil
	}
}

func newProxyAgentMetricsClient() *http.Client {
	return &http.Client{
		Timeout: healthCheckTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func checkProxyAgentStartup(client *http.Client, metricsURL string, expectedConnections int) healthz.Checker {
	return func(request *http.Request) error {
		if expectedConnections == 0 {
			return nil
		}

		metricsRequest, err := http.NewRequestWithContext(request.Context(), http.MethodGet, metricsURL, http.NoBody)
		if err != nil {
			return fmt.Errorf("create proxy-agent metrics request: %w", err)
		}

		response, err := client.Do(metricsRequest)
		if err != nil {
			return fmt.Errorf("get proxy-agent metrics: %w", err)
		}
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("get proxy-agent metrics: unexpected HTTP status %d", response.StatusCode)
		}

		body, err := io.ReadAll(io.LimitReader(response.Body, maxProxyAgentMetricsBodyBytes+1))
		if err != nil {
			return fmt.Errorf("read proxy-agent metrics: %w", err)
		}
		if len(body) > maxProxyAgentMetricsBodyBytes {
			return fmt.Errorf("proxy-agent metrics response exceeds %d bytes", maxProxyAgentMetricsBodyBytes)
		}

		connections, err := parseOpenServerConnections(body)
		if err != nil {
			return err
		}
		if int(connections) < expectedConnections {
			return fmt.Errorf("proxy-agent has %d open proxy-server connections, want at least %d", connections, expectedConnections)
		}
		return nil
	}
}

func parseOpenServerConnections(metrics []byte) (int32, error) {
	scanner := bufio.NewScanner(bytes.NewReader(metrics))
	scanner.Buffer(make([]byte, 64*1024), maxProxyAgentMetricsBodyBytes+1)

	foundType := false
	foundSample := false
	var connections int32
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		targetType, err := parseOpenServerConnectionsType(fields)
		if err != nil {
			return 0, err
		}
		if targetType {
			if foundType {
				return 0, fmt.Errorf("proxy-agent metric %q has duplicate TYPE declarations", proxyAgentOpenServerConnectionsMetric)
			}
			foundType = true
			continue
		}

		targetSample, value, err := parseOpenServerConnectionsSample(fields)
		if err != nil {
			return 0, err
		}
		if !targetSample {
			continue
		}
		if foundSample {
			return 0, fmt.Errorf("proxy-agent metric %q appears more than once", proxyAgentOpenServerConnectionsMetric)
		}
		connections = value
		foundSample = true
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read proxy-agent metrics: %w", err)
	}
	if !foundType {
		return 0, fmt.Errorf("proxy-agent metric %q is missing a gauge TYPE declaration", proxyAgentOpenServerConnectionsMetric)
	}
	if !foundSample {
		return 0, fmt.Errorf("proxy-agent metric %q is missing", proxyAgentOpenServerConnectionsMetric)
	}
	return connections, nil
}

func parseOpenServerConnectionsType(fields []string) (bool, error) {
	if len(fields) < 3 || fields[0] != "#" || fields[1] != "TYPE" || fields[2] != proxyAgentOpenServerConnectionsMetric {
		return false, nil
	}
	if len(fields) != 4 {
		return false, fmt.Errorf("proxy-agent metric %q has an invalid TYPE declaration", proxyAgentOpenServerConnectionsMetric)
	}
	if fields[3] != "gauge" {
		return false, fmt.Errorf("proxy-agent metric %q has type %q, want gauge", proxyAgentOpenServerConnectionsMetric, fields[3])
	}
	return true, nil
}

func parseOpenServerConnectionsSample(fields []string) (matched bool, connections int32, err error) {
	if len(fields) == 0 {
		return false, 0, nil
	}
	if strings.HasPrefix(fields[0], proxyAgentOpenServerConnectionsMetric+"{") {
		return false, 0, fmt.Errorf("proxy-agent metric %q must not have labels", proxyAgentOpenServerConnectionsMetric)
	}
	if fields[0] != proxyAgentOpenServerConnectionsMetric {
		return false, 0, nil
	}
	if len(fields) < 2 || len(fields) > 3 {
		return false, 0, fmt.Errorf("proxy-agent metric %q has an invalid sample", proxyAgentOpenServerConnectionsMetric)
	}
	if len(fields) == 3 {
		if _, parseErr := strconv.ParseInt(fields[2], 10, 64); parseErr != nil {
			return false, 0, fmt.Errorf("proxy-agent metric %q has invalid timestamp %q", proxyAgentOpenServerConnectionsMetric, fields[2])
		}
	}

	value, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value != math.Trunc(value) || value > math.MaxInt32 {
		return false, 0, fmt.Errorf("proxy-agent metric %q has invalid value %q", proxyAgentOpenServerConnectionsMetric, fields[1])
	}
	return true, int32(value), nil
}

func portForwardReadinessChecker(readiness *atomic.Value) healthz.Checker {
	return func(_ *http.Request) error {
		if !readiness.Load().(bool) {
			return fmt.Errorf("port-forward proxy is not ready")
		}
		return nil
	}
}

func newHealthProbeServer(address string, handler http.Handler) (*http.Server, net.Listener, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, fmt.Errorf("listen for health probes on %s: %w", address, err)
	}
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}, listener, nil
}

func serveHealthProbes(ctx context.Context, server *http.Server, listener net.Listener) error {
	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if err == http.ErrServerClosed {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), healthShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down health probe server: %w", err)
	}
	return <-serveErr
}
