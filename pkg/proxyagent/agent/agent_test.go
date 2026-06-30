package agent

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	mathrand "math/rand"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	csrv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	fakekube "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/cert"
	"k8s.io/utils/ptr"

	openshiftcrypto "github.com/openshift/library-go/pkg/crypto"
	"github.com/stretchr/testify/assert"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeruntime "sigs.k8s.io/controller-runtime/pkg/client/fake"

	addonv1beta1 "open-cluster-management.io/api/addon/v1beta1"
	fakeaddon "open-cluster-management.io/api/client/addon/clientset/versioned/fake"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	proxyv1alpha1 "open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1"
	"open-cluster-management.io/cluster-proxy/pkg/constant"
	"open-cluster-management.io/cluster-proxy/pkg/proxyserver/operator/authentication/selfsigned"
)

var (
	testscheme   = scheme.Scheme
	nodeSelector = map[string]string{"kubernetes.io/os": "linux"}
	tolerations  = []corev1.Toleration{{Key: "foo", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}}
)

func init() {
	_ = proxyv1alpha1.AddToScheme(testscheme)
	_ = addonv1beta1.Install(testscheme)
}

func TestRemoveDupAndSortservicesToExpose(t *testing.T) {
	testcases := []struct {
		name     string
		services []serviceToExpose
		expected []serviceToExpose
	}{
		{
			name: "remove duplicate and sort other services",
			services: []serviceToExpose{
				{
					Host: "service-3",
				},
				{
					Host: "service-1",
				},
				{
					Host: "service-2",
				},
				{
					Host: "service-1",
				},
			},
			expected: []serviceToExpose{
				{
					Host: "service-1",
				},
				{
					Host: "service-2",
				},
				{
					Host: "service-3",
				},
			},
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			actual := removeDupAndSortServices(testcase.services)
			if len(actual) != len(testcase.expected) {
				t.Errorf("expected %d services, but got %d", len(testcase.expected), len(actual))
			}
			// deep compare actual with expected
			if !reflect.DeepEqual(actual, testcase.expected) {
				t.Errorf("expected %v, but got %v", testcase.expected, actual)
			}
		})
	}
}

func TestAgentAddonRegistrationOption(t *testing.T) {
	cases := []struct {
		name               string
		signerName         string
		cluster            *clusterv1.ManagedCluster
		addon              *addonv1beta1.ManagedClusterAddOn
		expextedCSRConfigs int
		expectedCSRApprove bool
		expectedSignedCSR  bool
	}{
		{
			name:               "install all",
			cluster:            newCluster("cluster", false),
			addon:              newAddOn("addon", "cluster"),
			expextedCSRConfigs: 2,
		},
		{
			name:               "sing csr",
			signerName:         ProxyAgentSignerName,
			cluster:            newCluster("cluster", false),
			addon:              newAddOn("addon", "cluster"),
			expextedCSRConfigs: 2,
			expectedSignedCSR:  true,
		},
		{
			name:               "approve csr",
			cluster:            newCluster("cluster", true),
			addon:              newAddOn("addon", "cluster"),
			expextedCSRConfigs: 2,
			expectedCSRApprove: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeKubeClient := fakekube.NewSimpleClientset()

			agentAddOn, err := NewAgentAddon(
				&fakeSelfSigner{t: t},
				"",
				nil,
				fakeKubeClient,
				true,
				false,
				nil,
			)
			assert.NoError(t, err)

			options := agentAddOn.GetAgentAddonOptions()

			csrConfigs, err := options.Registration.Configurations(context.TODO(), c.cluster, nil)
			assert.NoError(t, err)
			assert.Len(t, csrConfigs, c.expextedCSRConfigs)

			csrApprove := options.Registration.CSRApproveCheck(context.TODO(), c.cluster, nil, nil)
			assert.Equal(t, c.expectedCSRApprove, csrApprove)
			if csrApprove != c.expectedCSRApprove {
				t.Errorf("expect csr approve is %v, but %v", c.expectedCSRApprove, csrApprove)
			}

			err = options.Registration.PermissionConfig(context.TODO(), c.cluster, c.addon)
			assert.NoError(t, err)
			actions := fakeKubeClient.Actions()
			assert.Len(t, actions, 8)

			// Extract RBAC resources from actions
			var role *rbacv1.Role
			var roleBinding *rbacv1.RoleBinding
			var clusterRole *rbacv1.ClusterRole
			var clusterRoleBinding *rbacv1.ClusterRoleBinding

			for _, action := range actions {
				if action.GetVerb() == "create" {
					switch obj := action.(clienttesting.CreateAction).GetObject().(type) {
					case *rbacv1.Role:
						role = obj
					case *rbacv1.RoleBinding:
						roleBinding = obj
					case *rbacv1.ClusterRole:
						clusterRole = obj
					case *rbacv1.ClusterRoleBinding:
						clusterRoleBinding = obj
					}
				}
			}

			// Verify Role was created with correct name and permissions
			assert.NotNil(t, role)
			assert.Equal(t, "cluster-proxy-addon-agent", role.Name)
			assert.Equal(t, []rbacv1.PolicyRule{
				{
					APIGroups: []string{"coordination.k8s.io"},
					Verbs:     []string{"*"},
					Resources: []string{"leases"},
				},
				{
					APIGroups: []string{"addon.open-cluster-management.io"},
					Verbs:     []string{"get"},
					Resources: []string{"managedclusteraddons"},
				},
				{
					APIGroups: []string{"addon.open-cluster-management.io"},
					Verbs:     []string{"update"},
					Resources: []string{"managedclusteraddons/status"},
				},
			}, role.Rules)

			// Verify RoleBinding was created and references the correct subjects
			assert.NotNil(t, roleBinding)
			assert.Equal(t, "cluster-proxy-addon-agent", roleBinding.Name)
			assert.Equal(t, rbacv1.RoleRef{
				Kind:     "Role",
				Name:     "cluster-proxy-addon-agent",
				APIGroup: rbacv1.GroupName,
			}, roleBinding.RoleRef)
			// For token-based registration, subjects come from addon.Status.Registrations
			assert.NotEmpty(t, roleBinding.Subjects)

			// Verify ClusterRole was created with correct permissions
			assert.NotNil(t, clusterRole)
			assert.Equal(t, "cluster-proxy-addon-agent-tokenreview", clusterRole.Name)
			assert.Equal(t, []rbacv1.PolicyRule{
				{
					APIGroups: []string{"authentication.k8s.io"},
					Verbs:     []string{"create"},
					Resources: []string{"tokenreviews"},
				},
			}, clusterRole.Rules)

			// Verify ClusterRoleBinding was created
			assert.NotNil(t, clusterRoleBinding)
			assert.Equal(t, "cluster-proxy-addon-agent-tokenreview", clusterRoleBinding.Name)
			assert.Equal(t, rbacv1.RoleRef{
				Kind:     "ClusterRole",
				Name:     "cluster-proxy-addon-agent-tokenreview",
				APIGroup: rbacv1.GroupName,
			}, clusterRoleBinding.RoleRef)
			assert.NotEmpty(t, clusterRoleBinding.Subjects)

			cert, err := options.Registration.CSRSign(context.TODO(), nil, nil, newCSR(c.signerName))
			assert.NoError(t, err)
			assert.Equal(t, c.expectedSignedCSR, (len(cert) != 0))
		})
	}
}

func TestNewAgentAddon(t *testing.T) {
	addOnName := "open-cluster-management-cluster-proxy"
	clusterName := "cluster"

	managedProxyConfigName := "cluster-proxy"
	addOndDeployConfigName := "deploy-config"

	expectedManifestNames := []string{
		"cluster-proxy-proxy-agent",              // deployment
		"cluster-proxy-addon-agent",              // role
		"cluster-proxy-addon-agent",              // rolebinding
		"cluster-proxy-ca",                       // ca
		clusterName,                              // cluster service
		addOnName,                                // namespace
		"cluster-proxy",                          // service account
		"cluster-proxy-addon-agent-impersonator", // clusterrole for impersonation
		"cluster-proxy-addon-agent-impersonator:open-cluster-management-cluster-proxy", // clusterrolebinding for impersonation
	}

	expectedManifestNamesWithoutClusterService := []string{
		"cluster-proxy-proxy-agent",              // deployment
		"cluster-proxy-addon-agent",              // role
		"cluster-proxy-addon-agent",              // rolebinding
		"cluster-proxy-ca",                       // ca
		addOnName,                                // namespace
		"cluster-proxy",                          // service account
		"cluster-proxy-addon-agent-impersonator", // clusterrole for impersonation
		"cluster-proxy-addon-agent-impersonator:open-cluster-management-cluster-proxy", // clusterrolebinding for impersonation
	}

	expectedManifestNamesWithServiceProxy := append([]string{}, expectedManifestNames...)
	expectedManifestNamesWithServiceProxy = append(expectedManifestNamesWithServiceProxy, "cluster-proxy-service-proxy-server-certificates")

	cases := []struct {
		name                    string
		cluster                 *clusterv1.ManagedCluster
		addon                   *addonv1beta1.ManagedClusterAddOn
		managedProxyConfig      runtimeclient.Object
		addOndDeploymentConfigs []runtime.Object
		kubeObjs                []runtime.Object
		enableKubeApiProxy      bool
		enableServiceProxy      bool
		expectedErrorMsg        string
		verifyManifests         func(t *testing.T, manifests []runtime.Object)
	}{
		{
			name:                    "without default config",
			addon:                   newAddOn(addOnName, clusterName),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			expectedErrorMsg:        "managedproxyconfigurations.proxy.open-cluster-management.io \"cluster-proxy\" not found",
			verifyManifests:         func(t *testing.T, manifests []runtime.Object) {},
		},
		{
			name: "no managed proxy configuration",
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference("none")}
				return addOn
			}(),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			expectedErrorMsg:        "managedproxyconfigurations.proxy.open-cluster-management.io \"cluster-proxy\" not found",
			verifyManifests:         func(t *testing.T, manifests []runtime.Object) {},
		},
		{
			name: "no load balancer service",
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			expectedErrorMsg:        "services \"lbsvc\" not found",
			verifyManifests:         func(t *testing.T, manifests []runtime.Object) {},
		},
		{
			name: "balancer service not ready",
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{newLoadBalancerService("")},
			enableKubeApiProxy:      true,
			expectedErrorMsg:        "the load-balancer service for proxy-server ingress is not yet provisioned",
			verifyManifests:         func(t *testing.T, manifests []runtime.Object) {},
		},
		{
			name:    "balancer service proxy server",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeLoadBalancerService),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{newLoadBalancerService("1.2.3.4")},
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, getProxyServerHost(agentDeploy), "1.2.3.4")
			},
		},
		{
			name:    "hostname proxy server ",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeHostname),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, getProxyServerHost(agentDeploy), "hostname")
			},
		},
		{
			name:    "customized proxy-agent replicas",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      setProxyAgentReplicas(newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeHostname), 2),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, *agentDeploy.Spec.Replicas, int32(2))
			},
		},
		{
			name:    "port forward proxy server",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, getProxyServerHost(agentDeploy), "127.0.0.1")
			},
		},
		{
			name:    "port forward proxy server with service proxy",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{},
			kubeObjs:                []runtime.Object{},
			enableKubeApiProxy:      true,
			enableServiceProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNamesWithServiceProxy))
				assert.ElementsMatch(t, expectedManifestNamesWithServiceProxy, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				serviceProxy := getDeploymentContainer(agentDeploy, "service-proxy")
				if assert.NotNil(t, serviceProxy) &&
					assert.NotNil(t, serviceProxy.ReadinessProbe) &&
					assert.NotNil(t, serviceProxy.ReadinessProbe.TCPSocket) {
					assert.Equal(t, int32(constant.ServiceProxyPort), serviceProxy.ReadinessProbe.TCPSocket.Port.IntVal)
				}
			},
		},
		{
			name:    "with addon deployment config",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfig(addOndDeployConfigName, clusterName)},
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, nodeSelector, agentDeploy.Spec.Template.Spec.NodeSelector)
				assert.Equal(t, tolerations, agentDeploy.Spec.Template.Spec.Tolerations)
				envCount := 0
				for _, container := range agentDeploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-agent" {
						envCount = len(container.Env)
					}
				}
				assert.Equal(t, 1, envCount)
				caSecret := getCASecret(manifests)
				assert.NotNil(t, caSecret)
				caCrt := string(caSecret.Data["ca.crt"])
				count := strings.Count(caCrt, "-----BEGIN CERTIFICATE-----")
				assert.Equal(t, 1, count)

			},
		},
		{
			name:    "with addon deployment config using a customized serviceDomain",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithCustomizedServiceDomain(addOndDeployConfigName, clusterName, "svc.test.com")},
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				externalNameService := getKubeAPIServerExternalNameService(manifests, clusterName)
				assert.NotNil(t, externalNameService)
				assert.Equal(t, "kubernetes.default.svc.test.com", externalNameService.Spec.ExternalName)
			},
		},
		{
			name:    "enable-kube-api-proxy is false",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithCustomizedServiceDomain(addOndDeployConfigName, clusterName, "svc.test.com")},
			enableKubeApiProxy:      false,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				// expect cluster service not created.
				assert.Len(t, manifests, len(expectedManifestNames)-1)
				assert.ElementsMatch(t, expectedManifestNamesWithoutClusterService, manifestNames(manifests))
			},
		},
		{
			name:    "with addon deployment config including https proxy config",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithHttpsProxy(addOndDeployConfigName, clusterName)},
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				envCount := 0
				for _, container := range agentDeploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-agent" {
						envCount = len(container.Env)
					}
				}
				assert.Equal(t, 4, envCount)
				caSecret := getCASecret(manifests)
				assert.NotNil(t, caSecret)
				caCrt := string(caSecret.Data["ca.crt"])
				count := strings.Count(caCrt, "-----BEGIN CERTIFICATE-----")
				assert.Equal(t, 2, count)
			},
		},
		{
			name:    "with addon deployment config including http proxy config",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithHttpProxy(addOndDeployConfigName, clusterName)},
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				envCount := 0
				for _, container := range agentDeploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-agent" {
						envCount = len(container.Env)
					}
				}
				assert.Equal(t, 4, envCount)
				caSecret := getCASecret(manifests)
				assert.NotNil(t, caSecret)
				caCrt := string(caSecret.Data["ca.crt"])
				count := strings.Count(caCrt, "-----BEGIN CERTIFICATE-----")
				assert.Equal(t, 1, count)
			},
		},
		{
			name:    "with addon deployment config including install namespace",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{
				func() *addonv1beta1.AddOnDeploymentConfig {
					config := newAddOnDeploymentConfig(addOndDeployConfigName, clusterName)
					config.Spec.AgentInstallNamespace = "addon-test"
					return config
				}()},
			enableKubeApiProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				newexpectedManifestNames := []string{}
				newexpectedManifestNames = append(newexpectedManifestNames, expectedManifestNames...)
				newexpectedManifestNames[5] = "addon-test"
				newexpectedManifestNames[8] = "cluster-proxy-addon-agent-impersonator:addon-test" // clusterrolebinding
				assert.ElementsMatch(t, newexpectedManifestNames, manifestNames(manifests))
			},
		},
		{
			name:    "with addon deployment config using customized variables",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{
				func() *addonv1beta1.AddOnDeploymentConfig {
					config := newAddOnDeploymentConfig(addOndDeployConfigName, clusterName)
					config.Spec.CustomizedVariables = []addonv1beta1.CustomizedVariable{
						{
							Name:  "replicas",
							Value: "10",
						},
					}
					return config
				}(),
			},
			enableKubeApiProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)
				assert.Equal(t, int32(10), *agentDeploy.Spec.Replicas)
			},
		},
		{
			name:    "with addon deployment config using a customized serviceDomain",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig:      newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{newAddOnDeploymentConfigWithCustomizedServiceDomain(addOndDeployConfigName, clusterName, "svc.test.com")},
			enableKubeApiProxy:      true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))
				externalNameService := getKubeAPIServerExternalNameService(manifests, clusterName)
				assert.NotNil(t, externalNameService)
				assert.Equal(t, "kubernetes.default.svc.test.com", externalNameService.Spec.ExternalName)
			},
		},
		{
			name:    "with addon deployment config using resources requirement",
			cluster: newCluster(clusterName, true),
			addon: func() *addonv1beta1.ManagedClusterAddOn {
				addOn := newAddOn(addOnName, clusterName)
				addOn.Status.ConfigReferences = []addonv1beta1.ConfigReference{
					newManagedProxyConfigReference(managedProxyConfigName),
					newAddOndDeploymentConfigReference(addOndDeployConfigName, clusterName),
				}
				return addOn
			}(),
			managedProxyConfig: newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypePortForward),
			addOndDeploymentConfigs: []runtime.Object{
				newAddOnDeploymentConfigWithResourcesRequirement(
					addOndDeployConfigName,
					clusterName,
					"deployments:cluster-proxy-proxy-agent:proxy-agent",
					corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("150m"),
							corev1.ResourceMemory: resource.MustParse("250Mi"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("150m"),
							corev1.ResourceMemory: resource.MustParse("250Mi"),
						},
					},
				),
			},
			enableKubeApiProxy: true,
			verifyManifests: func(t *testing.T, manifests []runtime.Object) {
				assert.Len(t, manifests, len(expectedManifestNames))
				assert.ElementsMatch(t, expectedManifestNames, manifestNames(manifests))

				// Get the agent deployment and verify resource requirements
				agentDeploy := getAgentDeployment(manifests)
				assert.NotNil(t, agentDeploy)

				// Check if the container has the expected resource requirements
				for _, container := range agentDeploy.Spec.Template.Spec.Containers {
					if container.Name == "proxy-agent" {
						assert.Equal(t, resource.MustParse("150m"), container.Resources.Limits[corev1.ResourceCPU])
						assert.Equal(t, resource.MustParse("250Mi"), container.Resources.Limits[corev1.ResourceMemory])
						assert.Equal(t, resource.MustParse("150m"), container.Resources.Requests[corev1.ResourceCPU])
						assert.Equal(t, resource.MustParse("250Mi"), container.Resources.Requests[corev1.ResourceMemory])
					}
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// add service-proxy secret into kubeObjects
			c.kubeObjs = append(c.kubeObjs, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-proxy-service-proxy-server-cert",
					Namespace: "test",
				},
				Data: map[string][]byte{
					"tls.crt": []byte("testcrt"),
					"tls.key": []byte("testkey"),
				},
			})

			fakeKubeClient := fakekube.NewSimpleClientset(c.kubeObjs...)
			var fakeRuntimeClient runtimeclient.Client
			if c.managedProxyConfig == nil {
				fakeRuntimeClient = fakeruntime.NewClientBuilder().Build()
			} else {
				fakeRuntimeClient = fakeruntime.NewClientBuilder().WithObjects(c.managedProxyConfig).Build()
			}
			fakeAddonClient := fakeaddon.NewSimpleClientset(c.addOndDeploymentConfigs...)

			agentAddOn, err := NewAgentAddon(
				&fakeSelfSigner{t: t},
				"test",
				fakeRuntimeClient,
				fakeKubeClient,
				c.enableKubeApiProxy,
				c.enableServiceProxy,
				fakeAddonClient,
			)
			assert.NoError(t, err)

			manifests, err := agentAddOn.Manifests(context.TODO(), c.cluster, c.addon.DeepCopy())
			if c.expectedErrorMsg != "" {
				assert.ErrorContains(t, err, c.expectedErrorMsg)
				return
			}
			assert.NoError(t, err)
			assertPodSecurityContext(t, getAgentDeployment(manifests))
			c.verifyManifests(t, manifests)
		})
	}
}

func assertPodSecurityContext(t *testing.T, deploy *appsv1.Deployment) {
	t.Helper()

	if !assert.NotNil(t, deploy) {
		return
	}
	expected := &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
	assert.Equal(t, expected, deploy.Spec.Template.Spec.SecurityContext)
}

func TestNewAgentAddonHostedModeManifests(t *testing.T) {
	clusterName := "cluster"
	addOnName := "open-cluster-management-cluster-proxy"
	managedProxyConfigName := "cluster-proxy"

	addon := newAddOn(addOnName, clusterName)
	addon.Annotations = map[string]string{
		addonv1beta1.HostingClusterNameAnnotationKey: "hosting-cluster",
	}
	addon.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}

	fakeKubeClient := fakekube.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-proxy-service-proxy-server-cert",
			Namespace: "test",
		},
		Data: map[string][]byte{
			"tls.crt": []byte("testcrt"),
			"tls.key": []byte("testkey"),
		},
	})
	fakeRuntimeClient := fakeruntime.NewClientBuilder().
		WithObjects(newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeHostname)).
		Build()

	agentAddOn, err := NewAgentAddon(
		&fakeSelfSigner{t: t},
		"test",
		fakeRuntimeClient,
		fakeKubeClient,
		true,
		true,
		fakeaddon.NewSimpleClientset(),
	)
	assert.NoError(t, err)
	assert.True(t, agentAddOn.GetAgentAddonOptions().HostedModeEnabled)

	manifests, err := agentAddOn.Manifests(context.TODO(), newCluster(clusterName, true), addon)
	assert.NoError(t, err)

	agentDeploy := getDeploymentByName(manifests, "cluster-proxy-proxy-agent")
	assert.NotNil(t, agentDeploy)
	assert.Equal(t, "hosting", agentDeploy.Annotations[addonv1beta1.HostedManifestLocationAnnotationKey])
	assert.True(t, deploymentHasVolume(agentDeploy, "managed-kubeconfig"))

	addonAgent := getContainer(agentDeploy, "addon-agent")
	assert.NotNil(t, addonAgent)
	assert.Contains(t, addonAgent.Args, "--spoke-kubeconfig=/etc/managed/kubeconfig")

	serviceProxy := getContainer(agentDeploy, "service-proxy")
	assert.NotNil(t, serviceProxy)
	assert.Contains(t, serviceProxy.Args, "--managed-kubeconfig=/etc/managed/kubeconfig")
	assert.Contains(t, serviceProxy.Args, "--service-relay-name=cluster-proxy-service-relay")
	assert.Contains(t, serviceProxy.Args, "--service-relay-port=7444")

	managedAPIServerProxy := getContainer(agentDeploy, "managed-apiserver-proxy")
	assert.NotNil(t, managedAPIServerProxy)
	assert.Contains(t, managedAPIServerProxy.Args, "--managed-kubeconfig=/etc/managed/kubeconfig")

	provisionerDeploy := getDeploymentByName(manifests, "cluster-proxy-managed-kubeconfig-provisioner")
	assert.NotNil(t, provisionerDeploy)
	assert.Equal(t, "hosting", provisionerDeploy.Annotations[addonv1beta1.HostedManifestLocationAnnotationKey])

	kubeAPIService := getKubeAPIServerExternalNameService(manifests, clusterName)
	assert.NotNil(t, kubeAPIService)
	assert.Equal(t, corev1.ServiceTypeClusterIP, kubeAPIService.Spec.Type)
	assert.Equal(t, "hosting", kubeAPIService.Annotations[addonv1beta1.HostedManifestLocationAnnotationKey])

	serviceRelayDeploy := getDeploymentByName(manifests, "cluster-proxy-service-relay")
	assert.NotNil(t, serviceRelayDeploy)
	assert.NotContains(t, serviceRelayDeploy.Annotations, addonv1beta1.HostedManifestLocationAnnotationKey)

	addonAgentRole := getRoleByName(manifests, "cluster-proxy-addon-agent")
	assert.NotNil(t, addonAgentRole)
	assert.Equal(t, "hosting", addonAgentRole.Annotations[addonv1beta1.HostedManifestLocationAnnotationKey])
}

func TestNewAgentAddonDefaultModeDoesNotRenderHostedResources(t *testing.T) {
	clusterName := "cluster"
	addOnName := "open-cluster-management-cluster-proxy"
	managedProxyConfigName := "cluster-proxy"

	addon := newAddOn(addOnName, clusterName)
	addon.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}

	fakeKubeClient := fakekube.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-proxy-service-proxy-server-cert",
			Namespace: "test",
		},
		Data: map[string][]byte{
			"tls.crt": []byte("testcrt"),
			"tls.key": []byte("testkey"),
		},
	})
	fakeRuntimeClient := fakeruntime.NewClientBuilder().
		WithObjects(newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeHostname)).
		Build()

	agentAddOn, err := NewAgentAddon(
		&fakeSelfSigner{t: t},
		"test",
		fakeRuntimeClient,
		fakeKubeClient,
		true,
		true,
		fakeaddon.NewSimpleClientset(),
	)
	assert.NoError(t, err)

	manifests, err := agentAddOn.Manifests(context.TODO(), newCluster(clusterName, true), addon)
	assert.NoError(t, err)

	for _, manifest := range manifests {
		obj, ok := manifest.(metav1.ObjectMetaAccessor)
		if !ok {
			continue
		}
		assert.NotContains(t, obj.GetObjectMeta().GetAnnotations(), addonv1beta1.HostedManifestLocationAnnotationKey)
	}

	agentDeploy := getDeploymentByName(manifests, "cluster-proxy-proxy-agent")
	assert.NotNil(t, agentDeploy)
	assert.False(t, deploymentHasVolume(agentDeploy, "managed-kubeconfig"))
	assert.Nil(t, getContainer(agentDeploy, "managed-apiserver-proxy"))
	assert.Nil(t, getDeploymentByName(manifests, "cluster-proxy-managed-kubeconfig-provisioner"))
	assert.Nil(t, getDeploymentByName(manifests, "cluster-proxy-service-relay"))
}

func TestNewAgentAddonAgentMetricsDisabledByDefault(t *testing.T) {
	manifests, err := renderAgentManifests(t, false, true, nil)
	assert.NoError(t, err)

	assert.Nil(t, getServiceByName(manifests, "cluster-proxy-agent-metrics"))
	assert.Nil(t, getServiceMonitorByName(manifests, "cluster-proxy-agent-metrics"))
}

func TestNewAgentAddonAgentMetricsServiceOptIn(t *testing.T) {
	manifests, err := renderAgentManifests(t, false, true, []addonv1beta1.CustomizedVariable{
		{Name: "agentMetricsServiceEnabled", Value: "true"},
	})
	assert.NoError(t, err)

	metricsService := getServiceByName(manifests, "cluster-proxy-agent-metrics")
	assert.NotNil(t, metricsService)
	assert.NotContains(t, metricsService.Annotations, addonv1beta1.HostedManifestLocationAnnotationKey)
	assert.ElementsMatch(t, []string{"agent-metrics", "svc-metrics"}, servicePortNames(metricsService))
	assert.Nil(t, getServiceMonitorByName(manifests, "cluster-proxy-agent-metrics"))

	agentDeploy := getDeploymentByName(manifests, "cluster-proxy-proxy-agent")
	assert.True(t, containerHasPort(getContainer(agentDeploy, "addon-agent"), "agent-metrics", 8888))
	assert.True(t, containerHasPort(getContainer(agentDeploy, "service-proxy"), "svc-metrics", 8000))
	assert.Nil(t, getContainer(agentDeploy, "managed-apiserver-proxy"))
}

func TestNewAgentAddonAgentServiceMonitorOptIn(t *testing.T) {
	manifests, err := renderAgentManifests(t, false, true, []addonv1beta1.CustomizedVariable{
		{Name: "agentServiceMonitorEnabled", Value: "true"},
		{Name: "agentServiceMonitorLabels", Value: "team=platform,monitoring.coreos.com/release=ocm"},
	})
	assert.NoError(t, err)

	metricsService := getServiceByName(manifests, "cluster-proxy-agent-metrics")
	assert.NotNil(t, metricsService)
	assert.ElementsMatch(t, []string{"agent-metrics", "svc-metrics"}, servicePortNames(metricsService))

	serviceMonitor := getServiceMonitorByName(manifests, "cluster-proxy-agent-metrics")
	assert.NotNil(t, serviceMonitor)
	assert.Equal(t, map[string]string{
		"team":                          "platform",
		"monitoring.coreos.com/release": "ocm",
	}, serviceMonitor.GetLabels())
	assert.NotContains(t, serviceMonitor.GetAnnotations(), addonv1beta1.HostedManifestLocationAnnotationKey)
	assert.ElementsMatch(t, []string{"agent-metrics", "svc-metrics"}, serviceMonitorEndpointPorts(t, serviceMonitor))
}

func TestNewAgentAddonAgentServiceMonitorMalformedLabels(t *testing.T) {
	manifests, err := renderAgentManifests(t, false, true, []addonv1beta1.CustomizedVariable{
		{Name: "agentServiceMonitorEnabled", Value: "true"},
		{Name: "agentServiceMonitorLabels", Value: "team,monitoring.coreos.com/release=ocm"},
	})
	assert.NoError(t, err)

	serviceMonitor := getServiceMonitorByName(manifests, "cluster-proxy-agent-metrics")
	assert.NotNil(t, serviceMonitor)
	assert.Equal(t, map[string]string{"monitoring.coreos.com/release": "ocm"}, serviceMonitor.GetLabels())
}

func TestNewAgentAddonHostedMetricsServicesOptIn(t *testing.T) {
	manifests, err := renderAgentManifests(t, true, false, []addonv1beta1.CustomizedVariable{
		{Name: "agentMetricsServiceEnabled", Value: "true"},
	})
	assert.NoError(t, err)

	agentMetricsService := getServiceByName(manifests, "cluster-proxy-agent-metrics")
	assert.NotNil(t, agentMetricsService)
	assert.Equal(t, "hosting", agentMetricsService.Annotations[addonv1beta1.HostedManifestLocationAnnotationKey])
	assert.ElementsMatch(t, []string{"agent-metrics", "api-metrics"}, servicePortNames(agentMetricsService))
	assert.Nil(t, getServiceMonitorByName(manifests, "cluster-proxy-agent-metrics"))

	provisionerMetricsService := getServiceByName(manifests, "cluster-proxy-managed-kubeconfig-provisioner-metrics")
	assert.NotNil(t, provisionerMetricsService)
	assert.Equal(t, "hosting", provisionerMetricsService.Annotations[addonv1beta1.HostedManifestLocationAnnotationKey])
	assert.ElementsMatch(t, []string{"metrics"}, servicePortNames(provisionerMetricsService))
	assert.Nil(t, getServiceMonitorByName(manifests, "cluster-proxy-managed-kubeconfig-provisioner-metrics"))

	assert.Nil(t, getServiceByName(manifests, "cluster-proxy-service-relay-metrics"))
	assert.Nil(t, getServiceMonitorByName(manifests, "cluster-proxy-service-relay-metrics"))

	agentDeploy := getDeploymentByName(manifests, "cluster-proxy-proxy-agent")
	assert.True(t, containerHasPort(getContainer(agentDeploy, "addon-agent"), "agent-metrics", 8888))
	assert.True(t, containerHasPort(getContainer(agentDeploy, "managed-apiserver-proxy"), "api-metrics", 8001))

	provisionerDeploy := getDeploymentByName(manifests, "cluster-proxy-managed-kubeconfig-provisioner")
	assert.True(t, containerHasPort(getContainer(provisionerDeploy, "managed-kubeconfig-provisioner"), "metrics", 8000))
}

func TestNewAgentAddonHostedRelayMetricsServicesOnlyOptIn(t *testing.T) {
	manifests, err := renderAgentManifests(t, true, true, []addonv1beta1.CustomizedVariable{
		{Name: "agentMetricsServiceEnabled", Value: "true"},
	})
	assert.NoError(t, err)

	serviceRelayMetricsService := getServiceByName(manifests, "cluster-proxy-service-relay-metrics")
	assert.NotNil(t, serviceRelayMetricsService)
	assert.NotContains(t, serviceRelayMetricsService.Annotations, addonv1beta1.HostedManifestLocationAnnotationKey)
	assert.ElementsMatch(t, []string{"metrics"}, servicePortNames(serviceRelayMetricsService))
	assert.Nil(t, getServiceMonitorByName(manifests, "cluster-proxy-service-relay-metrics"))
}

func TestNewAgentAddonHostedRelayServiceMonitorsOptIn(t *testing.T) {
	manifests, err := renderAgentManifests(t, true, true, []addonv1beta1.CustomizedVariable{
		{Name: "agentServiceMonitorEnabled", Value: "true"},
		{Name: "agentServiceMonitorLabels", Value: "team=platform"},
	})
	assert.NoError(t, err)

	assertHostedServiceMonitor(t, manifests, "cluster-proxy-agent-metrics",
		[]string{"agent-metrics", "svc-metrics", "api-metrics"})
	assertHostedServiceMonitor(t, manifests, "cluster-proxy-managed-kubeconfig-provisioner-metrics",
		[]string{"metrics"})

	serviceRelayMetricsService := getServiceByName(manifests, "cluster-proxy-service-relay-metrics")
	assert.NotNil(t, serviceRelayMetricsService)
	assert.NotContains(t, serviceRelayMetricsService.Annotations, addonv1beta1.HostedManifestLocationAnnotationKey)
	assert.ElementsMatch(t, []string{"metrics"}, servicePortNames(serviceRelayMetricsService))

	serviceRelayServiceMonitor := getServiceMonitorByName(manifests, "cluster-proxy-service-relay-metrics")
	assert.NotNil(t, serviceRelayServiceMonitor)
	assert.Equal(t, map[string]string{"team": "platform"}, serviceRelayServiceMonitor.GetLabels())
	assert.NotContains(t, serviceRelayServiceMonitor.GetAnnotations(), addonv1beta1.HostedManifestLocationAnnotationKey)
	assert.ElementsMatch(t, []string{"metrics"}, serviceMonitorEndpointPorts(t, serviceRelayServiceMonitor))

	serviceRelayDeploy := getDeploymentByName(manifests, "cluster-proxy-service-relay")
	assert.True(t, containerHasPort(getContainer(serviceRelayDeploy, "service-relay"), "metrics", 8000))
}

func TestNewAgentAddonHostedModeServiceProxyUsesRelay(t *testing.T) {
	manifests, err := renderAgentManifests(t, true, true, []addonv1beta1.CustomizedVariable{
		{Name: "externalManagedKubeConfigSecretNamespace", Value: "external-ns"},
		{Name: "externalManagedKubeConfigSecretName", Value: "external-kubeconfig"},
		{Name: "managedKubeConfigSecret", Value: "custom-managed-kubeconfig"},
		{Name: "managedKubeConfigTokenExpiration", Value: "12h"},
		{Name: "managedKubeConfigRefreshBefore", Value: "30m"},
		{Name: "managedKubeConfigSyncInterval", Value: "2m"},
	})
	assert.NoError(t, err)

	agentDeploy := getDeploymentByName(manifests, "cluster-proxy-proxy-agent")
	assert.NotNil(t, agentDeploy)
	assert.Equal(t, "custom-managed-kubeconfig", getVolumeSecretName(agentDeploy, "managed-kubeconfig"))

	serviceProxy := getContainer(agentDeploy, "service-proxy")
	assert.NotNil(t, serviceProxy)
	assert.Contains(t, serviceProxy.Args, "--managed-kubeconfig=/etc/managed/kubeconfig")
	assert.Contains(t, serviceProxy.Args, "--service-relay-name=cluster-proxy-service-relay")
	assert.Contains(t, serviceProxy.Args, "--service-relay-port=7444")

	provisioner := getContainer(getDeploymentByName(manifests, "cluster-proxy-managed-kubeconfig-provisioner"), "managed-kubeconfig-provisioner")
	assert.NotNil(t, provisioner)
	assert.Contains(t, provisioner.Args, "--source-namespace=external-ns")
	assert.Contains(t, provisioner.Args, "--source-name=external-kubeconfig")
	assert.Contains(t, provisioner.Args, "--target-name=custom-managed-kubeconfig")
	assert.Contains(t, provisioner.Args, "--token-expiration=12h")
	assert.Contains(t, provisioner.Args, "--refresh-before=30m")
	assert.Contains(t, provisioner.Args, "--sync-interval=2m")

	serviceProxyServerCertSecret := getSecretByName(manifests, "cluster-proxy-service-proxy-server-certificates")
	assert.NotNil(t, serviceProxyServerCertSecret)
	assert.Equal(t, "hosting", serviceProxyServerCertSecret.Annotations[addonv1beta1.HostedManifestLocationAnnotationKey])

	serviceRelayDeploy := getDeploymentByName(manifests, "cluster-proxy-service-relay")
	assert.NotNil(t, serviceRelayDeploy)
	assert.NotContains(t, serviceRelayDeploy.Annotations, addonv1beta1.HostedManifestLocationAnnotationKey)
}

func TestNewAgentAddonHostedModeRelayServiceProxy(t *testing.T) {
	manifests, err := renderAgentManifests(t, true, true, []addonv1beta1.CustomizedVariable{})
	assert.NoError(t, err)

	agentDeploy := getDeploymentByName(manifests, "cluster-proxy-proxy-agent")
	assert.NotNil(t, agentDeploy)

	serviceProxy := getContainer(agentDeploy, "service-proxy")
	assert.NotNil(t, serviceProxy)
	assert.Contains(t, serviceProxy.Args, "--managed-kubeconfig=/etc/managed/kubeconfig")
	assert.Contains(t, serviceProxy.Args, "--service-relay-name=cluster-proxy-service-relay")
	assert.Contains(t, serviceProxy.Args, "--service-relay-port=7444")

	serviceRelayDeploy := getDeploymentByName(manifests, "cluster-proxy-service-relay")
	assert.NotNil(t, serviceRelayDeploy)
	assert.NotContains(t, serviceRelayDeploy.Annotations, addonv1beta1.HostedManifestLocationAnnotationKey)
	serviceRelay := getContainer(serviceRelayDeploy, "service-relay")
	assert.NotNil(t, serviceRelay)
	assert.Contains(t, serviceRelay.Args, "service-relay")
	assert.Contains(t, serviceRelay.Args, "--listen=:7444")
	assert.Contains(t, serviceRelay.Args, "--trusted-caller-username=system:serviceaccount:open-cluster-management-cluster-proxy:cluster-proxy")
	// The relay calls TokenReview, so its ServiceAccount token must be mounted.
	if serviceRelayDeploy.Spec.Template.Spec.AutomountServiceAccountToken != nil {
		assert.True(t, *serviceRelayDeploy.Spec.Template.Spec.AutomountServiceAccountToken,
			"service-relay must mount the ServiceAccount token to authenticate callers")
	}

	serviceRelayService := getServiceByName(manifests, "cluster-proxy-service-relay")
	assert.NotNil(t, serviceRelayService)
	assert.Equal(t, corev1.ServiceTypeClusterIP, serviceRelayService.Spec.Type)
	assert.NotContains(t, serviceRelayService.Annotations, addonv1beta1.HostedManifestLocationAnnotationKey)

	serviceRelayRole := getRoleByName(manifests, "cluster-proxy-service-relay-proxy")
	assert.NotNil(t, serviceRelayRole)
	assert.NotContains(t, serviceRelayRole.Annotations, addonv1beta1.HostedManifestLocationAnnotationKey)

	// The relay runs under a dedicated least-privilege ServiceAccount, not the
	// impersonation-capable cluster-proxy account.
	assert.Equal(t, "cluster-proxy-service-relay", serviceRelayDeploy.Spec.Template.Spec.ServiceAccountName,
		"service-relay must run under its own least-privilege ServiceAccount, not the impersonation-capable cluster-proxy account")
	assert.NotEqual(t, "cluster-proxy", serviceRelayDeploy.Spec.Template.Spec.ServiceAccountName)

	relaySA := getServiceAccountByName(manifests, "cluster-proxy-service-relay")
	assert.NotNil(t, relaySA, "dedicated relay ServiceAccount must be provisioned on the managed cluster")
	assert.NotContains(t, relaySA.Annotations, addonv1beta1.HostedManifestLocationAnnotationKey)

	relayClusterRole := getClusterRoleByName(manifests, "cluster-proxy-service-relay")
	assert.NotNil(t, relayClusterRole)
	for _, rule := range relayClusterRole.Rules {
		assert.NotContains(t, rule.Verbs, "impersonate",
			"relay ClusterRole must not grant impersonation")
		assert.NotContains(t, rule.Resources, "users")
		assert.NotContains(t, rule.Resources, "groups")
	}
	relayClusterRoleBinding := getClusterRoleBindingByName(manifests, "cluster-proxy-service-relay:open-cluster-management-cluster-proxy")
	assert.NotNil(t, relayClusterRoleBinding)
	assert.Equal(t, "cluster-proxy-service-relay", relayClusterRoleBinding.RoleRef.Name)
	relaySABound := false
	for _, subject := range relayClusterRoleBinding.Subjects {
		if subject.Kind == "ServiceAccount" && subject.Name == "cluster-proxy-service-relay" {
			relaySABound = true
		}
	}
	assert.True(t, relaySABound, "relay ServiceAccount must be bound to its dedicated ClusterRole")

	// The relay SA must NOT be a subject of the impersonator binding.
	impersonatorBinding := getClusterRoleBindingByName(manifests, "cluster-proxy-addon-agent-impersonator:open-cluster-management-cluster-proxy")
	if impersonatorBinding != nil {
		for _, subject := range impersonatorBinding.Subjects {
			assert.NotEqual(t, "cluster-proxy-service-relay", subject.Name,
				"relay ServiceAccount must not be granted impersonation RBAC")
		}
	}

	serviceProxyServerCertSecret := getSecretByName(manifests, "cluster-proxy-service-proxy-server-certificates")
	assert.NotNil(t, serviceProxyServerCertSecret)
	assert.Equal(t, "hosting", serviceProxyServerCertSecret.Annotations[addonv1beta1.HostedManifestLocationAnnotationKey])
}

// TestNewAgentAddonHostedModeRelayServiceProxyCustomRelayNameAndPort asserts that overriding
// serviceRelayName/serviceRelayPort via AddOnDeploymentConfig CustomizedVariables flows through
// the chart consistently: the service-proxy container points at the same Service name and port
// that the relay Service/Deployment/Role are provisioned with, so a rename or port change cannot
// desynchronize the relay and the hosted service-proxy.
func TestNewAgentAddonHostedModeRelayServiceProxyCustomRelayNameAndPort(t *testing.T) {
	const customRelayName = "custom-relay"
	const customRelayPort = "9999"

	manifests, err := renderAgentManifests(t, true, true, []addonv1beta1.CustomizedVariable{
		{Name: "serviceRelayName", Value: customRelayName},
		{Name: "serviceRelayPort", Value: customRelayPort},
	})
	assert.NoError(t, err)

	// service-proxy must be pointed at the customized relay name/port so that it targets
	// the same Service the chart provisions below.
	agentDeploy := getDeploymentByName(manifests, "cluster-proxy-proxy-agent")
	assert.NotNil(t, agentDeploy)
	serviceProxy := getContainer(agentDeploy, "service-proxy")
	assert.NotNil(t, serviceProxy)
	assert.Contains(t, serviceProxy.Args, "--service-relay-name="+customRelayName)
	assert.Contains(t, serviceProxy.Args, "--service-relay-port="+customRelayPort)
	// The default values must NOT be rendered when overridden.
	assert.NotContains(t, serviceProxy.Args, "--service-relay-name=cluster-proxy-service-relay")
	assert.NotContains(t, serviceProxy.Args, "--service-relay-port=7444")

	// The relay Deployment must be named after the override and listen on the override port.
	serviceRelayDeploy := getDeploymentByName(manifests, customRelayName)
	assert.NotNil(t, serviceRelayDeploy, "expected relay Deployment named %q", customRelayName)
	assert.Nil(t, getDeploymentByName(manifests, "cluster-proxy-service-relay"),
		"default-named relay Deployment must not be rendered when overridden")
	serviceRelay := getContainer(serviceRelayDeploy, "service-relay")
	assert.NotNil(t, serviceRelay)
	assert.Contains(t, serviceRelay.Args, "--listen=:"+customRelayPort)

	// The relay Service must be named after the override and expose the override port.
	serviceRelayService := getServiceByName(manifests, customRelayName)
	assert.NotNil(t, serviceRelayService, "expected relay Service named %q", customRelayName)
	assert.Nil(t, getServiceByName(manifests, "cluster-proxy-service-relay"),
		"default-named relay Service must not be rendered when overridden")
	if assert.Len(t, serviceRelayService.Spec.Ports, 1) {
		assert.Equal(t, int32(9999), serviceRelayService.Spec.Ports[0].Port)
	}
}

type fakeSelfSigner struct {
	t *testing.T
}

func (fs *fakeSelfSigner) Sign(cfg cert.Config, expiry time.Duration) (selfsigned.CertPair, error) {
	return selfsigned.CertPair{}, nil
}

func (fs *fakeSelfSigner) CAData() []byte {
	return nil
}

func (fs *fakeSelfSigner) GetSigner() crypto.Signer {
	return nil
}

func (fs *fakeSelfSigner) CA() *openshiftcrypto.CA {
	_, key, err := newRSAKeyPair()
	if err != nil {
		fs.t.Fatal(err)
	}
	caCert, err := cert.NewSelfSignedCACert(cert.Config{CommonName: "open-cluster-management.io"}, key)
	if err != nil {
		fs.t.Fatal(err)
	}

	return &openshiftcrypto.CA{
		Config: &openshiftcrypto.TLSCertificateConfig{
			Certs: []*x509.Certificate{caCert},
			Key:   key,
		},
	}
}

func newRSAKeyPair() (*rsa.PublicKey, *rsa.PrivateKey, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	return &privateKey.PublicKey, privateKey, nil
}

func newCSR(signerName string) *csrv1.CertificateSigningRequest {
	insecureRand := mathrand.New(mathrand.NewSource(0))
	pk, err := ecdsa.GenerateKey(elliptic.P256(), insecureRand)
	if err != nil {
		panic(err)
	}
	csrb, err := x509.CreateCertificateRequest(insecureRand, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   "cn",
			Organization: []string{"org"},
		},
		DNSNames:       []string{},
		EmailAddresses: []string{},
		IPAddresses:    []net.IP{},
	}, pk)
	if err != nil {
		panic(err)
	}
	return &csrv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:         "test",
			GenerateName: "csr-",
		},
		Spec: csrv1.CertificateSigningRequestSpec{
			Username:   "test",
			Usages:     []csrv1.KeyUsage{},
			SignerName: signerName,
			Request:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrb}),
		},
	}
}

func newCluster(name string, accepted bool) *clusterv1.ManagedCluster {
	return &clusterv1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: clusterv1.ManagedClusterSpec{
			HubAcceptsClient: accepted,
		},
	}
}

func newAddOn(name, namespace string) *addonv1beta1.ManagedClusterAddOn {
	return &addonv1beta1.ManagedClusterAddOn{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.ManagedClusterAddOnSpec{},
		Status: addonv1beta1.ManagedClusterAddOnStatus{
			// Simulates what the registration controller sets in production.
			// In v1beta1, Status.Namespace is set by the registration controller
			// before the agentdeploy controller calls Manifests().
			Namespace: name,
			Registrations: []addonv1beta1.RegistrationConfig{
				{
					Type: addonv1beta1.KubeClient,
					KubeClient: &addonv1beta1.KubeClientConfig{
						Subject: addonv1beta1.KubeClientSubject{
							BaseSubject: addonv1beta1.BaseSubject{
								User:   "system:serviceaccount:" + name + ":cluster-proxy",
								Groups: []string{"system:serviceaccounts:" + name},
							},
						},
					},
				},
			},
		},
	}
}

func newManagedProxyConfigReference(name string) addonv1beta1.ConfigReference {
	return addonv1beta1.ConfigReference{
		ConfigGroupResource: addonv1beta1.ConfigGroupResource{
			Group:    "proxy.open-cluster-management.io",
			Resource: "managedproxyconfigurations",
		},
		DesiredConfig: &addonv1beta1.ConfigSpecHash{
			ConfigReferent: addonv1beta1.ConfigReferent{
				Name: name,
			},
			SpecHash: "dummy",
		},
	}
}

func newAddOndDeploymentConfigReference(name, namespace string) addonv1beta1.ConfigReference {
	return addonv1beta1.ConfigReference{
		ConfigGroupResource: addonv1beta1.ConfigGroupResource{
			Group:    "addon.open-cluster-management.io",
			Resource: "addondeploymentconfigs",
		},
		DesiredConfig: &addonv1beta1.ConfigSpecHash{
			ConfigReferent: addonv1beta1.ConfigReferent{
				Name:      name,
				Namespace: namespace,
			},
			SpecHash: "dummy",
		},
	}
}

func newManagedProxyConfig(name string, entryPointType proxyv1alpha1.EntryPointType) *proxyv1alpha1.ManagedProxyConfiguration {
	return &proxyv1alpha1.ManagedProxyConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: proxyv1alpha1.ManagedProxyConfigurationSpec{
			ProxyServer: proxyv1alpha1.ManagedProxyConfigurationProxyServer{
				Entrypoint: &proxyv1alpha1.ManagedProxyConfigurationProxyServerEntrypoint{
					Type: entryPointType,
					LoadBalancerService: &proxyv1alpha1.EntryPointLoadBalancerService{
						Name: "lbsvc",
					},
					Hostname: &proxyv1alpha1.EntryPointHostname{
						Value: "hostname",
					},
				},
				Namespace: "test",
			},
			ProxyAgent: proxyv1alpha1.ManagedProxyConfigurationProxyAgent{
				Image: "quay.io/open-cluster-management.io/cluster-proxy-agent:test",
			},
		},
	}
}

func setProxyAgentReplicas(mpc *proxyv1alpha1.ManagedProxyConfiguration, replicas int32) *proxyv1alpha1.ManagedProxyConfiguration {
	mpc.Spec.ProxyAgent.Replicas = replicas
	return mpc
}

func newAddOnDeploymentConfig(name, namespace string) *addonv1beta1.AddOnDeploymentConfig {
	return &addonv1beta1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.AddOnDeploymentConfigSpec{
			NodePlacement: &addonv1beta1.NodePlacement{
				Tolerations:  tolerations,
				NodeSelector: nodeSelector,
			},
		},
	}
}

func newAddOnDeploymentConfigWithCustomizedServiceDomain(name, namespace, serviceDomain string) *addonv1beta1.AddOnDeploymentConfig {
	return &addonv1beta1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.AddOnDeploymentConfigSpec{
			NodePlacement: &addonv1beta1.NodePlacement{
				Tolerations:  tolerations,
				NodeSelector: nodeSelector,
			},
			CustomizedVariables: []addonv1beta1.CustomizedVariable{
				{
					Name:  "serviceDomain",
					Value: serviceDomain,
				},
			},
		},
	}
}

var fakeCA = "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUM2VENDQWRFQ0ZHSG5lTUpBQ1NjR2lRSnA2K1RYa0NKRVBTVitNQTBHQ1NxR1NJYjNEUUVCQ3dVQU1ERXgKRmpBVUJnTlZCQW9NRFU5d1pXNVRhR2xtZENCQlEwMHhGekFWQmdOVkJBTU1EbmQzZHk1eVpXUm9ZWFF1WTI5dApNQjRYRFRJek1URXhNakV5TURZME4xb1hEVEkwTVRFeE1URXlNRFkwTjFvd01URVdNQlFHQTFVRUNnd05UM0JsCmJsTm9hV1owSUVGRFRURVhNQlVHQTFVRUF3d09kM2QzTG5KbFpHaGhkQzVqYjIwd2dnRWlNQTBHQ1NxR1NJYjMKRFFFQkFRVUFBNElCRHdBd2dnRUtBb0lCQVFEUXZMbHFjYXpYZmxXNXgzcVFDSE52ZjNqTFNCY0QrY3pCczFoMApUV0p2TWEvWVd2T2MrK3VNWXg2OW1RaXRCWEFaMEsyUVpQa1BYK2lEc244Mk9mNklYTUpUSVpmZk1Wb3g4UmtqCkNlQ00vdlNaMzExVGlwa0NkaGVTbnp0WElhek1hN0ZZS3BVT2htYTF3L2RReFcvcnIwandwRG9TMFUvN0xhWGwKNHF2bUF4Wk1iSHVWaFk2S0RZSGJ2MEdKYWdqekJtVkpieTZlMFg3MkozL05ZME1KT2plYklrOTEydjBXZ1pUKwo3UWU0a29scVY1MkQvaUhYV0xFUzhXMWQrMFZUbnlRaFAzY3RvNWp3TFZyWnQ2NDFZL0lRc2ZNQ0w1bGdhVTF0Cm9UMlcvQ3F1amw5aCt0UCt2SG1rNk5JZXk2RUNIdm1MV0xLbU5nblp2M0d0bVdnZEFnTUJBQUV3RFFZSktvWkkKaHZjTkFRRUxCUUFEZ2dFQkFKSjBnd0UxSUR4SlNzaUd1TGxDMlVGV2J3U0RHMUVEK3VlQWYvRDRlV0VSWFZDUAo4aVdZZC9RckdsakYxNGxvZllHb280Vk5PL28xQWJQS2gveXB4UW16REdrVE1NaGg2WFg1bExob3RZWHZERlM2CmlkQXk5TFpiWDFUQnV5UEcwNmorbkI4eEtEY3F4aFNLYTlNb0trck9XcmtGbnFZS2syQzIyZGRvZVlZdlRjR2cKK2JmZ3RSWFJRUFdQRmt2NDR5MGlMZVh0S0VMbHBQMkMyQW5JQkU4b2hzY0JiYnloVmptem5YS1dFSTg3T0xmUgoxNDJBOWoydlVVQW80T0o5d1JCei8raDFXUXkyL3prclVUMW90MFdienY1cy91YmlUQkRpSjlQQ0k4YkZmZXplCnpDbCthbEE5aUFJdGt4OVdZS2pzaDFuVHEzTnJwVWM0MXBJWlFBQT0KLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo="

func newAddOnDeploymentConfigWithHttpsProxy(name, namespace string) *addonv1beta1.AddOnDeploymentConfig {
	rawProxyCaCert, _ := base64.StdEncoding.DecodeString(fakeCA)
	return &addonv1beta1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.AddOnDeploymentConfigSpec{
			NodePlacement: &addonv1beta1.NodePlacement{
				Tolerations:  tolerations,
				NodeSelector: nodeSelector,
			},
			ProxyConfig: addonv1beta1.ProxyConfig{
				HTTPProxy:  "http://192.168.1.1",
				HTTPSProxy: "https://192.168.1.1",
				CABundle:   rawProxyCaCert,
				NoProxy:    "localhost",
			},
		},
	}
}
func newAddOnDeploymentConfigWithHttpProxy(name, namespace string) *addonv1beta1.AddOnDeploymentConfig {
	rawProxyCaCert, _ := base64.StdEncoding.DecodeString(fakeCA)
	return &addonv1beta1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.AddOnDeploymentConfigSpec{
			NodePlacement: &addonv1beta1.NodePlacement{
				Tolerations:  tolerations,
				NodeSelector: nodeSelector,
			},
			ProxyConfig: addonv1beta1.ProxyConfig{
				HTTPProxy:  "http://192.168.1.1",
				HTTPSProxy: "http://192.168.1.1",
				CABundle:   rawProxyCaCert,
				NoProxy:    "localhost",
			},
		},
	}
}

func newAddOnDeploymentConfigWithResourcesRequirement(name, namespace, containerID string,
	resources corev1.ResourceRequirements) *addonv1beta1.AddOnDeploymentConfig {

	return &addonv1beta1.AddOnDeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: addonv1beta1.AddOnDeploymentConfigSpec{
			ResourceRequirements: []addonv1beta1.ContainerResourceRequirements{
				{
					ContainerID: containerID,
					Resources:   resources,
				},
			},
		},
	}
}

func renderAgentManifests(t *testing.T, hosted, enableServiceProxy bool, customizedVariables []addonv1beta1.CustomizedVariable) ([]runtime.Object, error) {
	t.Helper()

	clusterName := "cluster"
	addOnName := "open-cluster-management-cluster-proxy"
	managedProxyConfigName := "cluster-proxy"
	addOnDeploymentConfigName := "deploy-config"

	addon := newAddOn(addOnName, clusterName)
	if hosted {
		addon.Annotations = map[string]string{
			addonv1beta1.HostingClusterNameAnnotationKey: "hosting-cluster",
		}
	}
	addon.Status.ConfigReferences = []addonv1beta1.ConfigReference{newManagedProxyConfigReference(managedProxyConfigName)}

	addOnDeploymentConfigs := []runtime.Object{}
	if customizedVariables != nil {
		addon.Status.ConfigReferences = append(addon.Status.ConfigReferences,
			newAddOndDeploymentConfigReference(addOnDeploymentConfigName, clusterName))
		addOnDeploymentConfig := newAddOnDeploymentConfig(addOnDeploymentConfigName, clusterName)
		addOnDeploymentConfig.Spec.CustomizedVariables = customizedVariables
		addOnDeploymentConfigs = append(addOnDeploymentConfigs, addOnDeploymentConfig)
	}

	agentAddOn, err := NewAgentAddon(
		&fakeSelfSigner{t: t},
		"test",
		fakeruntime.NewClientBuilder().
			WithObjects(newManagedProxyConfig(managedProxyConfigName, proxyv1alpha1.EntryPointTypeHostname)).
			Build(),
		fakekube.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-proxy-service-proxy-server-cert",
				Namespace: "test",
			},
			Data: map[string][]byte{
				"tls.crt": []byte("testcrt"),
				"tls.key": []byte("testkey"),
			},
		}),
		true,
		enableServiceProxy,
		fakeaddon.NewSimpleClientset(addOnDeploymentConfigs...),
	)
	if err != nil {
		return nil, err
	}

	return agentAddOn.Manifests(context.TODO(), newCluster(clusterName, true), addon)
}

func newLoadBalancerService(ingress string) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lbsvc",
			Namespace: "test",
		},
	}
	if len(ingress) != 0 {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ingress}}
	}
	return svc
}

func newAgentClientSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-client",
			Namespace: "test",
		},
		Data: map[string][]byte{
			"tls.crt": []byte("testcrt"),
			"tls.key": []byte("testkey"),
		},
	}
}

func manifestNames(manifests []runtime.Object) []string {
	names := []string{}
	for _, manifest := range manifests {
		obj, ok := manifest.(metav1.ObjectMetaAccessor)
		if !ok {
			continue
		}
		names = append(names, obj.GetObjectMeta().GetName())
	}
	return names
}

func getAgentDeployment(manifests []runtime.Object) *appsv1.Deployment {
	for _, manifest := range manifests {
		switch obj := manifest.(type) {
		case *appsv1.Deployment:
			return obj
		}
	}

	return nil
}

// namedObject is satisfied by the typed API objects produced by Manifests().
type namedObject interface {
	runtime.Object
	GetName() string
}

// getObjectByName returns the first manifest of type T whose name matches, or
// the zero value (typed nil) if none match.
func getObjectByName[T namedObject](manifests []runtime.Object, name string) T {
	for _, manifest := range manifests {
		if obj, ok := manifest.(T); ok && obj.GetName() == name {
			return obj
		}
	}
	var zero T
	return zero
}

func getDeploymentByName(manifests []runtime.Object, name string) *appsv1.Deployment {
	return getObjectByName[*appsv1.Deployment](manifests, name)
}

func getContainer(deploy *appsv1.Deployment, name string) *corev1.Container {
	if deploy == nil {
		return nil
	}
	for i := range deploy.Spec.Template.Spec.Containers {
		if deploy.Spec.Template.Spec.Containers[i].Name == name {
			return &deploy.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

func getDeploymentContainer(deploy *appsv1.Deployment, name string) *corev1.Container {
	return getContainer(deploy, name)
}

func containerHasPort(container *corev1.Container, name string, port int32) bool {
	if container == nil {
		return false
	}
	for _, containerPort := range container.Ports {
		if containerPort.Name == name && containerPort.ContainerPort == port {
			return true
		}
	}
	return false
}

func deploymentHasVolume(deploy *appsv1.Deployment, name string) bool {
	if deploy == nil {
		return false
	}
	for _, volume := range deploy.Spec.Template.Spec.Volumes {
		if volume.Name == name {
			return true
		}
	}
	return false
}

func getVolumeSecretName(deploy *appsv1.Deployment, name string) string {
	if deploy == nil {
		return ""
	}
	for _, volume := range deploy.Spec.Template.Spec.Volumes {
		if volume.Name == name && volume.Secret != nil {
			return volume.Secret.SecretName
		}
	}
	return ""
}

func getRoleByName(manifests []runtime.Object, name string) *rbacv1.Role {
	return getObjectByName[*rbacv1.Role](manifests, name)
}

func getServiceAccountByName(manifests []runtime.Object, name string) *corev1.ServiceAccount {
	return getObjectByName[*corev1.ServiceAccount](manifests, name)
}

func getClusterRoleByName(manifests []runtime.Object, name string) *rbacv1.ClusterRole {
	return getObjectByName[*rbacv1.ClusterRole](manifests, name)
}

func getClusterRoleBindingByName(manifests []runtime.Object, name string) *rbacv1.ClusterRoleBinding {
	return getObjectByName[*rbacv1.ClusterRoleBinding](manifests, name)
}

func getKubeAPIServerExternalNameService(manifests []runtime.Object, clusterName string) *corev1.Service {
	// As the cluster-service.yaml shows, the service name is the cluster name.
	return getServiceByName(manifests, clusterName)
}

func getServiceByName(manifests []runtime.Object, name string) *corev1.Service {
	return getObjectByName[*corev1.Service](manifests, name)
}

func getServiceMonitorByName(manifests []runtime.Object, name string) *unstructured.Unstructured {
	for _, manifest := range manifests {
		obj, ok := manifest.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if obj.GetKind() == "ServiceMonitor" && obj.GetName() == name {
			return obj
		}
	}

	return nil
}

func servicePortNames(service *corev1.Service) []string {
	if service == nil {
		return nil
	}
	names := []string{}
	for _, port := range service.Spec.Ports {
		names = append(names, port.Name)
	}
	return names
}

func serviceMonitorEndpointPorts(t *testing.T, serviceMonitor *unstructured.Unstructured) []string {
	t.Helper()

	endpoints, found, err := unstructured.NestedSlice(serviceMonitor.Object, "spec", "endpoints")
	assert.NoError(t, err)
	assert.True(t, found)

	ports := []string{}
	for _, endpoint := range endpoints {
		endpointMap, ok := endpoint.(map[string]interface{})
		if !assert.True(t, ok) {
			continue
		}
		port, ok := endpointMap["port"].(string)
		if !assert.True(t, ok) {
			continue
		}
		ports = append(ports, port)
	}
	return ports
}

func assertHostedServiceMonitor(t *testing.T, manifests []runtime.Object, name string, expectedPorts []string) {
	t.Helper()

	service := getServiceByName(manifests, name)
	assert.NotNil(t, service)
	assert.Equal(t, "hosting", service.Annotations[addonv1beta1.HostedManifestLocationAnnotationKey])
	assert.ElementsMatch(t, expectedPorts, servicePortNames(service))

	serviceMonitor := getServiceMonitorByName(manifests, name)
	assert.NotNil(t, serviceMonitor)
	assert.Equal(t, map[string]string{"team": "platform"}, serviceMonitor.GetLabels())
	assert.Equal(t, "hosting", serviceMonitor.GetAnnotations()[addonv1beta1.HostedManifestLocationAnnotationKey])
	assert.ElementsMatch(t, expectedPorts, serviceMonitorEndpointPorts(t, serviceMonitor))
}

func getProxyServerHost(deploy *appsv1.Deployment) string {
	args := deploy.Spec.Template.Spec.Containers[0].Args
	for _, arg := range args {
		if strings.HasPrefix(arg, "--proxy-server-host") {
			i := strings.Index(arg, "=") + 1
			return arg[i:]
		}
	}
	return ""
}

func getCASecret(manifests []runtime.Object) *corev1.Secret {
	for _, manifest := range manifests {
		switch obj := manifest.(type) {
		case *corev1.Secret:
			if obj.Name == "cluster-proxy-ca" {
				return obj
			}
		}
	}

	return nil
}

func getSecretByName(manifests []runtime.Object, name string) *corev1.Secret {
	return getObjectByName[*corev1.Secret](manifests, name)
}
