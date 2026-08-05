package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/config"
)

var _ = Describe("Proxy-agent rollout safety", Serial, Label("rollout", "ha", "connectivity"), func() {
	var originalConfiguration *proxyv1alpha1.ManagedProxyConfiguration

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

		By("Preparing three proxy-servers and one proxy-agent")
		configuration, err := updateManagedProxyConfiguration(func(configuration *proxyv1alpha1.ManagedProxyConfiguration) {
			configuration.Spec.ProxyServer.Replicas = 3
			configuration.Spec.ProxyAgent.Replicas = 1
		})
		Expect(err).ToNot(HaveOccurred())
		waitForProxyServerReplicas(3)
		waitForProxyAgentRollout(configuration.Generation, 1)
	})

	It("keeps API and service traffic available while replacing the agent", func() {
		monitor := startRolloutTrafficMonitor()
		DeferCleanup(monitor.stop)

		By("Establishing continuous traffic before the rollout")
		Eventually(monitor.successfulRequests).
			WithTimeout(30 * time.Second).
			WithPolling(100 * time.Millisecond).
			Should(And(
				HaveKeyWithValue("kubernetes-api", BeNumerically(">", 0)),
				HaveKeyWithValue("service-proxy", BeNumerically(">", 0)),
			))

		By("Changing a harmless proxy-agent argument to trigger a rollout")
		configuration, err := updateManagedProxyConfiguration(func(configuration *proxyv1alpha1.ManagedProxyConfiguration) {
			configuration.Spec.ProxyAgent.AdditionalArgs = append(
				configuration.Spec.ProxyAgent.AdditionalArgs,
				"--v=4",
			)
		})
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for the replacement agent to pass its all-server startup probe")
		waitForProxyAgentPodStarted(configuration.Generation)
		waitForProxyAgentRollout(configuration.Generation, 1)

		By("Continuing traffic after the old agent has terminated")
		Consistently(func() int64 {
			return monitor.errorCount()
		}).WithTimeout(5 * time.Second).WithPolling(100 * time.Millisecond).Should(BeZero())

		monitor.stop()
		attempts, successes, errorCount, samples := monitor.results()
		Expect(attempts["kubernetes-api"]).To(BeNumerically(">", 0))
		Expect(attempts["service-proxy"]).To(BeNumerically(">", 0))
		Expect(successes["kubernetes-api"]).To(Equal(attempts["kubernetes-api"]))
		Expect(successes["service-proxy"]).To(Equal(attempts["service-proxy"]))
		Expect(errorCount).To(BeZero(), "continuous traffic failures: %s", strings.Join(samples, "; "))
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
	expectedGenerationAnnotation := fmt.Sprintf("%d", expectedGeneration)
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

func waitForProxyAgentPodStarted(expectedGeneration int64) {
	expectedGenerationAnnotation := fmt.Sprintf("%d", expectedGeneration)
	Eventually(func() error {
		pods, err := hubKubeClient.CoreV1().Pods(config.DefaultAddonInstallNamespace).List(
			context.Background(),
			metav1.ListOptions{LabelSelector: common.LabelKeyComponentName + "=" + common.ComponentNameProxyAgent},
		)
		if err != nil {
			return err
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.DeletionTimestamp != nil ||
				pod.Annotations[common.AnnotationKeyConfigurationGeneration] != expectedGenerationAnnotation {
				continue
			}
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == "proxy-agent" && ptr.Deref(status.Started, false) && status.Ready {
					return nil
				}
			}
		}
		return fmt.Errorf("no proxy-agent Pod for generation %d has passed startup and readiness probes", expectedGeneration)
	}).WithTimeout(5 * time.Minute).WithPolling(500 * time.Millisecond).Should(Succeed())
}

type rolloutTrafficMonitor struct {
	cancel    context.CancelFunc
	stopOnce  sync.Once
	waitGroup sync.WaitGroup

	mutex     sync.Mutex
	attempts  map[string]int
	successes map[string]int
	errors    int64
	samples   []string
}

func startRolloutTrafficMonitor() *rolloutTrafficMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	monitor := &rolloutTrafficMonitor{
		cancel:    cancel,
		attempts:  map[string]int{},
		successes: map[string]int{},
	}
	monitor.run(ctx, "kubernetes-api", probeKubernetesAPIThroughClusterProxy)
	monitor.run(ctx, "service-proxy", probeServiceThroughClusterProxy)
	return monitor
}

func (m *rolloutTrafficMonitor) run(ctx context.Context, name string, probe func(context.Context) error) {
	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			requestContext, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := probe(requestContext)
			cancel()
			if ctx.Err() != nil {
				return
			}

			m.mutex.Lock()
			m.attempts[name]++
			if err == nil {
				m.successes[name]++
			} else {
				m.errors++
				if len(m.samples) < 20 {
					m.samples = append(m.samples, fmt.Sprintf("%s: %v", name, err))
				}
			}
			m.mutex.Unlock()

			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
}

func (m *rolloutTrafficMonitor) stop() {
	m.stopOnce.Do(func() {
		m.cancel()
		m.waitGroup.Wait()
	})
}

func (m *rolloutTrafficMonitor) successfulRequests() map[string]int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return map[string]int{
		"kubernetes-api": m.successes["kubernetes-api"],
		"service-proxy":  m.successes["service-proxy"],
	}
}

func (m *rolloutTrafficMonitor) errorCount() int64 {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.errors
}

func (m *rolloutTrafficMonitor) results() (map[string]int, map[string]int, int64, []string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return map[string]int{
			"kubernetes-api": m.attempts["kubernetes-api"],
			"service-proxy":  m.attempts["service-proxy"],
		}, map[string]int{
			"kubernetes-api": m.successes["kubernetes-api"],
			"service-proxy":  m.successes["service-proxy"],
		}, m.errors, append([]string(nil), m.samples...)
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
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
