package e2e

import (
	"context"
	"fmt"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	addonapiv1alpha1 "open-cluster-management.io/api/addon/v1alpha1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/common"
	"open-cluster-management.io/cluster-proxy/pkg/config"
)

var _ = Describe("Install Test", Label("install", "deployment"),
	func() {
		It("ClusterProxy configuration conditions should be okay", Label("configuration", "conditions"),
			func() {
				By("Polling configuration conditions")
				Eventually(
					func() (bool, error) {
						proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
						err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
							Name: "cluster-proxy",
						}, proxyConfiguration)
						if err != nil {
							return false, err
						}
						isDeployed := meta.IsStatusConditionTrue(proxyConfiguration.Status.Conditions,
							proxyv1alpha1.ConditionTypeProxyServerDeployed)
						isAgentServerSigned := meta.IsStatusConditionTrue(proxyConfiguration.Status.Conditions,
							proxyv1alpha1.ConditionTypeProxyServerSecretSigned)
						isProxyServerSigned := meta.IsStatusConditionTrue(proxyConfiguration.Status.Conditions,
							proxyv1alpha1.ConditionTypeAgentServerSecretSigned)
						ready := isDeployed && isAgentServerSigned && isProxyServerSigned
						return ready, nil
					}).
					WithTimeout(time.Minute).
					Should(BeTrue())
			})

		It("ManagedClusterAddon should be available", Label("addon", "health"), func() {
			By("Polling addon healthiness")
			Eventually(
				func() (bool, error) {
					addon := &addonapiv1alpha1.ManagedClusterAddOn{}
					if err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
						Namespace: managedClusterName,
						Name:      "cluster-proxy",
					}, addon); err != nil {
						if apierrors.IsNotFound(err) {
							return false, nil
						}
						return false, err
					}
					return meta.IsStatusConditionTrue(
						addon.Status.Conditions,
						addonapiv1alpha1.ManagedClusterAddOnConditionAvailable), nil
				}).
				WithTimeout(time.Minute).
				Should(BeTrue())
		})

		It("ManagedClusterAddon should be configured with AddOnDeployMentConfig", Label("addon", "config", "deployment"), func() {
			deployConfigName := "deploy-config"
			nodeSelector := map[string]string{"kubernetes.io/os": "linux"}
			tolerations := []corev1.Toleration{{Key: "node-role.kubernetes.io/infra", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}

			By("Cleanup existing AddOnDeploymentConfig if any")
			_ = hubRuntimeClient.Delete(context.TODO(), &addonapiv1alpha1.AddOnDeploymentConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deployConfigName,
					Namespace: managedClusterName,
				},
			})

			waitProxyAgentDeploymentRolledOut()
			originalConfigs, err := getManagedClusterAddonConfigs()
			Expect(err).ToNot(HaveOccurred())

			originalDeployment := &appsv1.Deployment{}
			err = hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
				Namespace: config.DefaultAddonInstallNamespace,
				Name:      "cluster-proxy-proxy-agent",
			}, originalDeployment)
			Expect(err).ToNot(HaveOccurred())
			originalNodeSelector := cloneStringMap(originalDeployment.Spec.Template.Spec.NodeSelector)
			originalTolerations := cloneTolerations(originalDeployment.Spec.Template.Spec.Tolerations)
			originalReplicas := deploymentReplicas(originalDeployment)

			DeferCleanup(func() {
				By("Restore cluster-proxy addon config after test")
				Eventually(func() error {
					return setManagedClusterAddonConfigs(originalConfigs)
				}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

				By("Cleanup AddOnDeploymentConfig after test")
				_ = hubRuntimeClient.Delete(context.TODO(), &addonapiv1alpha1.AddOnDeploymentConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name:      deployConfigName,
						Namespace: managedClusterName,
					},
				})

				By("Wait for cluster-proxy deployment to return to the previous placement")
				waitProxyAgentDeploymentConfigured(originalNodeSelector, originalTolerations, originalReplicas)
				waitManagedClusterAddonAvailable()
			})

			By("Prepare a AddOnDeployMentConfig for cluster-proxy")
			Eventually(func() error {
				return hubRuntimeClient.Create(context.TODO(), &addonapiv1alpha1.AddOnDeploymentConfig{
					ObjectMeta: metav1.ObjectMeta{
						Name:      deployConfigName,
						Namespace: managedClusterName,
					},
					Spec: addonapiv1alpha1.AddOnDeploymentConfigSpec{
						NodePlacement: &addonapiv1alpha1.NodePlacement{
							NodeSelector: nodeSelector,
							Tolerations:  tolerations,
						},
						AgentInstallNamespace: config.DefaultAddonInstallNamespace,
					},
				})
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			By("Add the config to cluster-proxy")
			Eventually(func() error {
				return setManagedClusterAddonConfigs([]addonapiv1alpha1.AddOnConfig{
					{
						ConfigGroupResource: addonapiv1alpha1.ConfigGroupResource{
							Group:    "addon.open-cluster-management.io",
							Resource: "addondeploymentconfigs",
						},
						ConfigReferent: addonapiv1alpha1.ConfigReferent{
							Namespace: managedClusterName,
							Name:      deployConfigName,
						},
					},
				})
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			By("Ensure the config is referenced")
			Eventually(func() error {
				addon := &addonapiv1alpha1.ManagedClusterAddOn{}
				if err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: managedClusterName,
					Name:      "cluster-proxy",
				}, addon); err != nil {
					return err
				}

				if len(addon.Status.ConfigReferences) == 0 {
					return fmt.Errorf("no config references in addon status")
				}
				for _, cr := range addon.Status.ConfigReferences {
					if cr.Name == deployConfigName {
						return nil
					}
				}
				return fmt.Errorf("unexpected config references %v", addon.Status.ConfigReferences)
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			By("Ensure the cluster-proxy is configured")
			waitProxyAgentDeploymentConfigured(nodeSelector, tolerations, originalReplicas)

			By("Ensure the cluster-proxy is available")
			waitManagedClusterAddonAvailable()
		})

		It("ClusterProxy configuration - check configuration generation", Label("configuration", "generation"), func() {
			proxyConfiguration := &proxyv1alpha1.ManagedProxyConfiguration{}
			err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
				Name: "cluster-proxy",
			}, proxyConfiguration)
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() error {
				expectedGeneration := proxyConfiguration.Generation
				proxyServerDeploy := &appsv1.Deployment{}
				err = hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: proxyConfiguration.Spec.ProxyServer.Namespace,
					Name:      "cluster-proxy",
				}, proxyServerDeploy)
				if err != nil {
					return err
				}
				if proxyServerDeploy.Annotations[common.AnnotationKeyConfigurationGeneration] != strconv.Itoa(int(expectedGeneration)) {
					return fmt.Errorf("proxy server deployment is not updated")
				}

				proxyAgentDeploy := &appsv1.Deployment{}
				err = hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
					Namespace: config.DefaultAddonInstallNamespace,
					Name:      proxyConfiguration.Name + "-" + common.ComponentNameProxyAgent,
				}, proxyAgentDeploy)
				if err != nil {
					return err
				}
				if proxyAgentDeploy.Annotations[common.AnnotationKeyConfigurationGeneration] != strconv.Itoa(int(expectedGeneration)) {
					return fmt.Errorf("proxy agent deployment is not updated")
				}

				return nil
			}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())

			waitAgentReady(proxyConfiguration, hubKubeClient)
		})
	})

func waitAgentReady(proxyConfiguration *proxyv1alpha1.ManagedProxyConfiguration, client kubernetes.Interface) {
	waitProxyAgentDeploymentGenerationRolledOut(proxyConfiguration.Generation, proxyConfiguration.Spec.ProxyAgent.Replicas)

	Eventually(
		func() int {
			podList, err := client.CoreV1().
				Pods(config.DefaultAddonInstallNamespace).
				List(context.TODO(), metav1.ListOptions{
					LabelSelector: common.LabelKeyComponentName + "=" + common.ComponentNameProxyAgent,
				})
			Expect(err).NotTo(HaveOccurred())
			matchedGeneration := 0
			for _, pod := range podList.Items {
				if pod.DeletionTimestamp != nil {
					continue
				}
				allReady := true
				for _, st := range pod.Status.ContainerStatuses {
					if !st.Ready {
						allReady = false
					}
				}
				if allReady &&
					pod.Annotations[common.AnnotationKeyConfigurationGeneration] == strconv.Itoa(int(proxyConfiguration.Generation)) {
					matchedGeneration++
				}
			}
			return matchedGeneration
		}).
		WithTimeout(time.Second * 30).
		Should(Equal(int(proxyConfiguration.Spec.ProxyAgent.Replicas)))
}

func waitManagedClusterAddonAvailable() {
	Eventually(func() error {
		addon := &addonapiv1alpha1.ManagedClusterAddOn{}
		if err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
			Namespace: managedClusterName,
			Name:      "cluster-proxy",
		}, addon); err != nil {
			return err
		}

		if !meta.IsStatusConditionTrue(
			addon.Status.Conditions,
			addonapiv1alpha1.ManagedClusterAddOnConditionAvailable) {
			return fmt.Errorf("addon is unavailable")
		}

		return nil
	}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())
}

func getManagedClusterAddonConfigs() ([]addonapiv1alpha1.AddOnConfig, error) {
	addon := &addonapiv1alpha1.ManagedClusterAddOn{}
	if err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
		Namespace: managedClusterName,
		Name:      "cluster-proxy",
	}, addon); err != nil {
		return nil, err
	}

	return cloneAddOnConfigs(addon.Spec.Configs), nil
}

func setManagedClusterAddonConfigs(configs []addonapiv1alpha1.AddOnConfig) error {
	addon := &addonapiv1alpha1.ManagedClusterAddOn{}
	if err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
		Namespace: managedClusterName,
		Name:      "cluster-proxy",
	}, addon); err != nil {
		return err
	}

	if equality.Semantic.DeepEqual(addon.Spec.Configs, configs) {
		return nil
	}

	addon.Spec.Configs = cloneAddOnConfigs(configs)
	return hubRuntimeClient.Update(context.TODO(), addon)
}

func waitProxyAgentDeploymentConfigured(
	expectedNodeSelector map[string]string,
	expectedTolerations []corev1.Toleration,
	expectedReplicas int32,
) {
	Eventually(func() error {
		deploy, err := getProxyAgentDeployment()
		if err != nil {
			return err
		}

		if deploy.Status.ObservedGeneration < deploy.Generation {
			return fmt.Errorf("proxy agent deployment generation %d has not been observed: %v", deploy.Generation, deploy.Status)
		}

		if deploymentReplicas(deploy) != expectedReplicas {
			return fmt.Errorf("unexpected proxy agent spec replicas %d", deploymentReplicas(deploy))
		}

		if deploy.Status.Replicas != expectedReplicas ||
			deploy.Status.UpdatedReplicas != expectedReplicas ||
			deploy.Status.ReadyReplicas != expectedReplicas ||
			deploy.Status.AvailableReplicas != expectedReplicas ||
			deploy.Status.UnavailableReplicas != 0 {
			return fmt.Errorf("proxy agent deployment rollout is incomplete: %v", deploy.Status)
		}

		if !equality.Semantic.DeepEqual(deploy.Spec.Template.Spec.NodeSelector, expectedNodeSelector) {
			return fmt.Errorf("unexpected nodeSelector %v", deploy.Spec.Template.Spec.NodeSelector)
		}

		if !equality.Semantic.DeepEqual(deploy.Spec.Template.Spec.Tolerations, expectedTolerations) {
			return fmt.Errorf("unexpected tolerations %v", deploy.Spec.Template.Spec.Tolerations)
		}

		return nil
	}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())
}

func waitProxyAgentDeploymentGenerationRolledOut(expectedGeneration int64, expectedReplicas int32) {
	Eventually(func() error {
		deploy, err := getProxyAgentDeployment()
		if err != nil {
			return err
		}

		expectedGenerationAnnotation := strconv.Itoa(int(expectedGeneration))
		if deploy.Annotations[common.AnnotationKeyConfigurationGeneration] != expectedGenerationAnnotation {
			return fmt.Errorf("proxy agent deployment generation annotation is not updated")
		}
		if deploy.Spec.Template.Annotations[common.AnnotationKeyConfigurationGeneration] != expectedGenerationAnnotation {
			return fmt.Errorf("proxy agent pod template generation annotation is not updated")
		}

		if deploymentReplicas(deploy) != expectedReplicas {
			return fmt.Errorf("unexpected proxy agent spec replicas %d", deploymentReplicas(deploy))
		}

		if deploy.Status.ObservedGeneration < deploy.Generation {
			return fmt.Errorf("proxy agent deployment generation %d has not been observed: %v", deploy.Generation, deploy.Status)
		}

		if deploy.Status.Replicas != expectedReplicas ||
			deploy.Status.UpdatedReplicas != expectedReplicas ||
			deploy.Status.ReadyReplicas != expectedReplicas ||
			deploy.Status.AvailableReplicas != expectedReplicas ||
			deploy.Status.UnavailableReplicas != 0 {
			return fmt.Errorf("proxy agent deployment rollout is incomplete: %v", deploy.Status)
		}

		return nil
	}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())
}

func waitProxyAgentDeploymentRolledOut() {
	Eventually(func() error {
		deploy, err := getProxyAgentDeployment()
		if err != nil {
			return err
		}

		expectedReplicas := deploymentReplicas(deploy)
		if deploy.Status.ObservedGeneration < deploy.Generation {
			return fmt.Errorf("proxy agent deployment generation %d has not been observed: %v", deploy.Generation, deploy.Status)
		}

		if deploy.Status.Replicas != expectedReplicas ||
			deploy.Status.UpdatedReplicas != expectedReplicas ||
			deploy.Status.ReadyReplicas != expectedReplicas ||
			deploy.Status.AvailableReplicas != expectedReplicas ||
			deploy.Status.UnavailableReplicas != 0 {
			return fmt.Errorf("proxy agent deployment rollout is incomplete: %v", deploy.Status)
		}

		return nil
	}).WithTimeout(time.Minute).ShouldNot(HaveOccurred())
}

func getProxyAgentDeployment() (*appsv1.Deployment, error) {
	deploy := &appsv1.Deployment{}
	err := hubRuntimeClient.Get(context.TODO(), types.NamespacedName{
		Namespace: config.DefaultAddonInstallNamespace,
		Name:      "cluster-proxy-proxy-agent",
	}, deploy)
	return deploy, err
}

func deploymentReplicas(deploy *appsv1.Deployment) int32 {
	if deploy.Spec.Replicas == nil {
		return 1
	}
	return *deploy.Spec.Replicas
}

func cloneAddOnConfigs(configs []addonapiv1alpha1.AddOnConfig) []addonapiv1alpha1.AddOnConfig {
	if configs == nil {
		return nil
	}
	return append([]addonapiv1alpha1.AddOnConfig{}, configs...)
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneTolerations(in []corev1.Toleration) []corev1.Toleration {
	if in == nil {
		return nil
	}
	return append([]corev1.Toleration{}, in...)
}
