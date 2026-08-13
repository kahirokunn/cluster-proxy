package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // Ginkgo tests conventionally use the DSL as a dot import.
	. "github.com/onsi/gomega"    //nolint:revive // Gomega matchers are the test assertion DSL.

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/config"
)

const (
	proxyAgentNativeHealthPort  = 8093
	addonAgentHealthPort        = 8888
	openServerConnectionsMetric = "konnectivity_network_proxy_agent_open_server_connections"
)

var _ = Describe("Proxy-agent rollout safety", Serial, Label("rollout", "ha", "connectivity"), func() {
	var originalConfiguration *proxyv1alpha1.ManagedProxyConfiguration
	var baselineGeneration int64

	BeforeEach(func() {
		By("Saving the ManagedProxyConfiguration")
		current := &proxyv1alpha1.ManagedProxyConfiguration{}
		Expect(hubRuntimeClient.Get(context.Background(), types.NamespacedName{Name: "cluster-proxy"}, current)).To(Succeed())
		originalConfiguration = current.DeepCopy()

		DeferCleanup(func() {
			By("Restoring the ManagedProxyConfiguration after the rollout test")
			restored, err := updateManagedProxyConfiguration(func(configuration *proxyv1alpha1.ManagedProxyConfiguration) {
				configuration.Spec = originalConfiguration.Spec
			})
			Expect(err).ToNot(HaveOccurred())
			waitForProxyServerReplicas(restored.Spec.ProxyServer.Replicas)
			waitForProxyAgentRollout(restored.Generation, restored.Spec.ProxyAgent.Replicas)
		})

		By("Preparing two proxy-servers and one proxy-agent")
		configuration, err := updateManagedProxyConfiguration(func(configuration *proxyv1alpha1.ManagedProxyConfiguration) {
			configuration.Spec.ProxyServer.Replicas = 2
			configuration.Spec.ProxyAgent.Replicas = 1
			configuration.Spec.ProxyAgent.AdditionalArgs = withoutProxyAgentSyncIntervalArgs(
				configuration.Spec.ProxyAgent.AdditionalArgs,
			)
		})
		Expect(err).ToNot(HaveOccurred())
		waitForProxyServerReplicas(2)
		waitForProxyAgentRollout(configuration.Generation, 1)
		baselineGeneration = configuration.Generation
	})

	It("keeps the old agent running until the replacement connects to every server", func() {
		var oldPod *corev1.Pod
		Eventually(func() error {
			var err error
			oldPod, err = readyProxyAgentPodForGeneration(baselineGeneration)
			return err
		}).WithTimeout(30 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())

		By("Making one of the two proxy-servers unavailable")
		Expect(setProxyServerDeploymentReplicas(1)).To(Succeed())
		waitForProxyServerReplicas(1)

		By("Starting a replacement while only one proxy-server is available")
		Expect(triggerProxyAgentRollout()).To(Succeed())

		By("Observing the startup gate while exactly one of two server connections exists")
		Eventually(func() error {
			return expectProxyAgentStartupGateWindow(baselineGeneration, oldPod.UID)
		}).WithTimeout(90 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())

		By("Restoring the second proxy-server")
		Expect(setProxyServerDeploymentReplicas(2)).To(Succeed())
		waitForProxyServerReplicas(2)

		By("Waiting for the replacement to connect to both servers and complete its rollout")
		waitForProxyAgentRollout(baselineGeneration, 1)

		By("Verifying traffic through the replacement agent")
		Eventually(probeRolloutTraffic).
			WithTimeout(30 * time.Second).
			WithPolling(100 * time.Millisecond).
			Should(Succeed())
	})
})

func updateManagedProxyConfiguration(
	mutate func(*proxyv1alpha1.ManagedProxyConfiguration),
) (*proxyv1alpha1.ManagedProxyConfiguration, error) {

	var updated *proxyv1alpha1.ManagedProxyConfiguration
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configuration := &proxyv1alpha1.ManagedProxyConfiguration{}
		if err := hubRuntimeClient.Get(context.Background(), types.NamespacedName{Name: "cluster-proxy"}, configuration); err != nil {
			return err
		}
		mutate(configuration)
		if err := hubRuntimeClient.Update(context.Background(), configuration); err != nil {
			return err
		}
		updated = configuration.DeepCopy()
		return nil
	})
	return updated, err
}

func withoutProxyAgentSyncIntervalArgs(args []string) []string {
	filtered := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--sync-interval" || arg == "--sync-interval-cap" {
			if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
				index++
			}
			continue
		}
		if strings.HasPrefix(arg, "--sync-interval=") || strings.HasPrefix(arg, "--sync-interval-cap=") {
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered
}

func setProxyServerDeploymentReplicas(replicas int32) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := getProxyServerDeployment()
		if err != nil {
			return err
		}
		deployment.Spec.Replicas = ptr.To(replicas)
		return hubRuntimeClient.Update(context.Background(), deployment)
	})
}

func triggerProxyAgentRollout() error {
	rolloutID := time.Now().UTC().Format(time.RFC3339Nano)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := getProxyAgentDeployment()
		if err != nil {
			return err
		}
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = map[string]string{}
		}
		deployment.Spec.Template.Annotations["proxy.open-cluster-management.io/e2e-rollout-id"] = rolloutID
		return hubRuntimeClient.Update(context.Background(), deployment)
	})
}

func waitForProxyServerReplicas(expected int32) {
	Eventually(func() error {
		deployment, err := getProxyServerDeployment()
		if err != nil {
			return err
		}
		if ptr.Deref(deployment.Spec.Replicas, 1) != expected {
			return fmt.Errorf("proxy-server replicas = %d, want %d", ptr.Deref(deployment.Spec.Replicas, 1), expected)
		}
		return expectProxyServerReady(deployment, 0)
	}).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
}

func waitForProxyAgentRollout(expectedGeneration int64, expectedReplicas int32) {
	expectedGenerationAnnotation := strconv.FormatInt(expectedGeneration, 10)
	Eventually(func() error {
		deployment, err := getProxyAgentDeployment()
		if err != nil {
			return err
		}
		if deployment.Spec.Template.Annotations[common.AnnotationKeyConfigurationGeneration] != expectedGenerationAnnotation {
			return fmt.Errorf("proxy-agent Pod template has not observed configuration generation %d", expectedGeneration)
		}
		return proxyAgentRolledOut(deployment, expectedReplicas)
	}).WithTimeout(5 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
}

func readyProxyAgentPodForGeneration(expectedGeneration int64) (*corev1.Pod, error) {
	expectedGenerationAnnotation := strconv.FormatInt(expectedGeneration, 10)
	pods, err := hubKubeClient.CoreV1().Pods(config.DefaultAddonInstallNamespace).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: common.LabelKeyComponentName + "=" + common.ComponentNameProxyAgent},
	)
	if err != nil {
		return nil, err
	}
	var readyPods []*corev1.Pod
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.DeletionTimestamp == nil &&
			pod.Annotations[common.AnnotationKeyConfigurationGeneration] == expectedGenerationAnnotation &&
			proxyAgentContainerReady(pod) {

			readyPods = append(readyPods, pod)
		}
	}
	if len(readyPods) != 1 {
		return nil, fmt.Errorf(
			"found %d ready proxy-agent Pods for configuration generation %d, want exactly one",
			len(readyPods),
			expectedGeneration,
		)
	}
	return readyPods[0].DeepCopy(), nil
}

func expectProxyAgentStartupGateWindow(expectedGeneration int64, oldPodUID types.UID) error {
	expectedGenerationAnnotation := strconv.FormatInt(expectedGeneration, 10)
	pods, err := hubKubeClient.CoreV1().Pods(config.DefaultAddonInstallNamespace).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: common.LabelKeyComponentName + "=" + common.ComponentNameProxyAgent},
	)
	if err != nil {
		return err
	}

	var oldPod *corev1.Pod
	var newPods []*corev1.Pod
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.UID == oldPodUID {
			oldPod = pod
		}
		if pod.UID != oldPodUID && pod.Annotations[common.AnnotationKeyConfigurationGeneration] == expectedGenerationAnnotation {
			newPods = append(newPods, pod)
		}
	}
	if oldPod == nil {
		return fmt.Errorf("old proxy-agent Pod %s no longer exists", oldPodUID)
	}
	if oldPod.DeletionTimestamp != nil {
		return fmt.Errorf("old proxy-agent Pod %s is terminating before the replacement passed startup", oldPod.Name)
	}
	if !proxyAgentContainerReady(oldPod) {
		return fmt.Errorf("old proxy-agent Pod %s is not Ready", oldPod.Name)
	}
	if len(newPods) == 0 {
		return fmt.Errorf("no replacement proxy-agent Pod for configuration generation %d", expectedGeneration)
	}

	var observations []string
	for _, newPod := range newPods {
		err := expectReplacementProxyAgentGated(newPod)
		if err == nil {
			return nil
		}
		observations = append(observations, fmt.Sprintf("%s: %v", newPod.Name, err))
	}
	return fmt.Errorf("replacement proxy-agent has not entered the expected startup gate window: %s", strings.Join(observations, "; "))
}

func expectReplacementProxyAgentGated(pod *corev1.Pod) error {
	var proxyAgentStatus *corev1.ContainerStatus
	for index := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[index].Name == "proxy-agent" {
			proxyAgentStatus = &pod.Status.ContainerStatuses[index]
			break
		}
	}
	if proxyAgentStatus == nil {
		return errors.New("proxy-agent container has no status")
	}
	if ptr.Deref(proxyAgentStatus.Started, false) {
		return errors.New("proxy-agent container is already Started")
	}
	if proxyAgentStatus.Ready {
		return errors.New("proxy-agent container is unexpectedly Ready")
	}

	requestContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	nativeStatus, nativeBody, err := podProxyResponse(requestContext, pod, proxyAgentNativeHealthPort, "readyz")
	if err != nil {
		return fmt.Errorf("query native readyz: %w", err)
	}
	if nativeStatus != http.StatusOK {
		return fmt.Errorf("native readyz status = %d, want %d: %s", nativeStatus, http.StatusOK, nativeBody)
	}

	metricsStatus, metricsBody, err := podProxyResponse(requestContext, pod, proxyAgentNativeHealthPort, "metrics")
	if err != nil {
		return fmt.Errorf("query native metrics: %w", err)
	}
	if metricsStatus != http.StatusOK {
		return fmt.Errorf("native metrics status = %d, want %d: %s", metricsStatus, http.StatusOK, metricsBody)
	}
	connections, err := openServerConnectionsFromMetrics(metricsBody)
	if err != nil {
		return err
	}
	if connections != 1 {
		return fmt.Errorf("open proxy-server connections = %d, want exactly 1", connections)
	}

	_, startupBody, startupErr := podProxyResponse(
		requestContext,
		pod,
		addonAgentHealthPort,
		"startupz/proxy-agent-server-connections",
	)
	if startupErr == nil || !apierrors.IsInternalError(startupErr) {
		return fmt.Errorf("add-on startupz did not return an internal error: body=%q, error=%v", startupBody, startupErr)
	}
	expectedFailure := "proxy-agent has 1 open proxy-server connections, want at least 2"
	startupResponse := string(startupBody) + " " + startupErr.Error()
	if !strings.Contains(startupResponse, expectedFailure) {
		return fmt.Errorf("add-on startupz response %q does not contain %q", startupResponse, expectedFailure)
	}
	return nil
}

func proxyAgentContainerReady(pod *corev1.Pod) bool {
	for index := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[index]
		if status.Name == "proxy-agent" {
			return ptr.Deref(status.Started, false) && status.Ready
		}
	}
	return false
}

func podProxyResponse(ctx context.Context, pod *corev1.Pod, port int, path string) (int, []byte, error) {
	statusCode := 0
	result := hubKubeClient.CoreV1().RESTClient().Get().
		Namespace(pod.Namespace).
		Resource("pods").
		Name(pod.Name + ":" + strconv.Itoa(port)).
		SubResource("proxy").
		Suffix(path).
		Do(ctx).
		StatusCode(&statusCode)
	body, err := result.Raw()
	return statusCode, body, err
}

func openServerConnectionsFromMetrics(body []byte) (int, error) {
	found := false
	connections := 0
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if strings.HasPrefix(fields[0], openServerConnectionsMetric+"{") {
			return 0, fmt.Errorf("metric %q unexpectedly has labels", openServerConnectionsMetric)
		}
		if fields[0] != openServerConnectionsMetric {
			continue
		}
		if found || len(fields) < 2 || len(fields) > 3 {
			return 0, fmt.Errorf("metric %q is duplicate or malformed", openServerConnectionsMetric)
		}
		value, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, fmt.Errorf("parse metric %q value %q: %w", openServerConnectionsMetric, fields[1], err)
		}
		found = true
		connections = value
	}
	if !found {
		return 0, fmt.Errorf("metric %q is missing", openServerConnectionsMetric)
	}
	return connections, nil
}

func probeKubernetesAPIThroughClusterProxy(ctx context.Context) error {
	_, err := clusterProxyKubeClient.CoreV1().Pods(hubInstallNamespace).List(ctx, metav1.ListOptions{Limit: 1})
	return err
}

func probeServiceThroughClusterProxy(ctx context.Context) error {
	target := fmt.Sprintf(
		"https://%s/%s/api/v1/namespaces/default/services/http:hello-world:8000/proxy-service/index.html",
		userServerServiceAddress,
		managedClusterName,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return err
	}
	response, err := clusterProxyHttpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d: %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), "Hello from hello-world") {
		return fmt.Errorf("unexpected response body: %s", body)
	}
	return nil
}

func probeRolloutTraffic() error {
	probes := []struct {
		name  string
		probe func(context.Context) error
	}{
		{name: "kubernetes-api", probe: probeKubernetesAPIThroughClusterProxy},
		{name: "service-proxy", probe: probeServiceThroughClusterProxy},
	}
	for _, item := range probes {
		requestContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := item.probe(requestContext)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", item.name, err)
		}
	}
	return nil
}
