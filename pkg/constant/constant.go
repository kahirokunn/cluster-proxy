package constant

const (
	AgentInstallNamespace = "open-cluster-management-agent-addon"

	ServiceProxyPort = 7443

	ServiceRelayPort = 7444

	// The health-probe and managed-apiserver-proxy ports below are hardcoded in
	// the addon-agent deployment template and must stay in sync with it.
	// ServiceRelay and ManagedKubeconfigProvisioner share 8000 because they run
	// in separate containers.
	ServiceRelayHealthProbePort = 8000

	ManagedAPIServerProxyPort            = 8443
	ManagedAPIServerProxyHealthProbePort = 8001

	ManagedKubeconfigProvisionerHealthProbePort = 8000

	ServerCertSecretName = "cluster-proxy-service-proxy-server-cert"

	ServiceProxyName = "cluster-proxy-service-proxy"

	ServiceRelayName = "cluster-proxy-service-relay"

	// UserServerSecretName is the fixed secret name for user server certificates.
	// This is used both by controller-generated certificates and external certificate generators
	// to ensure consistency.
	UserServerSecretName = "cluster-proxy-user-serving-cert"

	// UserServerServiceName is the fixed service name for user server.
	UserServerServiceName = "cluster-proxy-addon-user"
)
