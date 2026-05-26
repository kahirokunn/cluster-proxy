# Cluster Proxy

[![License](https://img.shields.io/:license-apache-blue.svg)](http://www.apache.org/licenses/LICENSE-2.0.html)
[![Go](https://github.com/open-cluster-management-io/cluster-proxy/actions/workflows/go-presubmit.yml/badge.svg)](https://github.com/open-cluster-management-io/cluster-proxy/actions/workflows/go-presubmit.yml)

## Overview

Cluster Proxy enables secure network connectivity between hub clusters and managed clusters in Open Cluster Management (OCM) environments. It provides a solution for accessing services in managed clusters from the hub cluster, even when clusters are deployed in different networks or VPCs.

## What is Cluster Proxy?

Cluster Proxy is a pluggable addon for Open Cluster Management (OCM) built on the extensibility
provided by [addon-framework](https://github.com/open-cluster-management-io/addon-framework) 
that automates the installation of [apiserver-network-proxy](https://github.com/kubernetes-sigs/apiserver-network-proxy)
on both hub cluster and managed clusters. The network proxy establishes
reverse proxy tunnels from the managed cluster to the hub cluster, enabling 
clients from the hub network to access services in the managed clusters'
network even when all the clusters are isolated in different VPCs.

Cluster Proxy consists of two components:

- **Addon-Manager**: Manages the installation of proxy servers (proxy ingress)
  in the hub cluster.
  
- **Addon-Agent**: Manages the installation of proxy agents for each managed 
  cluster.

The overall architecture is shown below:

![Arch](./hack/picture/arch.png)


## Getting started

### Prerequisites

- Open Cluster Management (OCM) registration component (>= 0.5.0)
- A Kubernetes cluster serving as the hub cluster
- One or more managed Kubernetes clusters registered with the hub

### Steps

#### Installing via Helm Chart

1. Add the OCM Helm repository:

```shell
helm repo add ocm https://open-cluster-management.io/helm-charts/
helm repo update
helm search repo ocm/cluster-proxy
```

Expected output:
```
NAME                       	CHART VERSION	APP VERSION	DESCRIPTION                   
ocm/cluster-proxy          	<..>       	    1.0.0      	A Helm chart for Cluster-Proxy
```

2. Install the Helm chart:

```shell
helm install \
    -n open-cluster-management-addon --create-namespace \
    cluster-proxy ocm/cluster-proxy 
```

3. Verify the installation:

```shell
kubectl get pods -n open-cluster-management-addon
```

Expected output:
```
NAME                                           READY   STATUS        RESTARTS   AGE
cluster-proxy-5d8db7ddf4-265tm                 1/1     Running       0          12s
cluster-proxy-addon-manager-778f6d679f-9pndv   1/1     Running       0          33s
...
```

4. The addon will be automatically installed to your registered clusters. 
   Verify the addon installation:

```shell
kubectl get managedclusteraddon -A | grep cluster-proxy
```

Expected output:
```
NAMESPACE         NAME                     AVAILABLE   DEGRADED   PROGRESSING
<your cluster>    cluster-proxy            True                   
```

### Usage

By default, the proxy servers are running in gRPC mode so the proxy clients 
are expected to proxy through the tunnels by the [konnectivity-client](https://github.com/kubernetes-sigs/apiserver-network-proxy#clients).
Konnectivity is the underlying technique of Kubernetes' [egress-selector](https://kubernetes.io/docs/tasks/extend-kubernetes/setup-konnectivity/)
feature and an example of konnectivity client is visible [here](https://github.com/open-cluster-management-io/cluster-proxy/tree/main/examples/test-client).

In code, proxying to the managed cluster is simply a matter of overriding the 
dialer of the Kubernetes client config object, e.g.:

```go
  // instantiate a gRPC proxy dialer
  tunnel, err := konnectivity.CreateSingleUseGrpcTunnel(
      context.TODO(),
      <proxy service>,
      grpc.WithTransportCredentials(grpccredentials.NewTLS(proxyTLSCfg)),
  )
  cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
  if err != nil {
      return err
  }
  // The managed cluster's name.
  cfg.Host = clusterName
  // Override the default TCP dialer
  cfg.Dial = tunnel.DialContext 
```

### Hosted mode

Cluster Proxy supports addon-framework hosted mode when the `ManagedClusterAddOn`
has the `addon.open-cluster-management.io/hosting-cluster-name` annotation. In
hosted mode the proxy-agent deployment runs on the hosting cluster, together
with the addon namespace, the hosted `cluster-proxy` service account, and the
Role/RoleBinding that grants leader-election access to leases and ConfigMaps in
that namespace. The managed cluster keeps its own `cluster-proxy` service
account plus a ClusterRole/Binding granting TokenReview and user/group
impersonation against the managed apiserver, which the service-proxy and
service relay rely on. Short-lived tokens for the managed service account are
minted by the hosted provisioner via the external admin kubeconfig's
TokenRequest subresource.

The hosting cluster must contain an external managed-cluster kubeconfig Secret.
By default the addon reads `external-managed-kubeconfig` from the namespace named
after the managed cluster, creates short-lived tokens for the managed
`cluster-proxy` service account, and writes a generated kubeconfig Secret named
`cluster-proxy-managed-kubeconfig` in the addon install namespace. The generated
kubeconfig is mounted read-only by the hosted agent containers; the external
admin kubeconfig is mounted only by the provisioner.

#### External managed kubeconfig Secret contract

The operator is expected to provision the external managed-cluster kubeconfig
Secret on the hosting cluster before the hosted `cluster-proxy` addon comes
online. The provisioner reads the Secret named by
`externalManagedKubeConfigSecretName` (default `external-managed-kubeconfig`)
in the namespace named by `externalManagedKubeConfigSecretNamespace` (default:
the managed cluster name) and applies the following contract:

- **Secret type**: `Opaque` (any type whose `data` map can carry the key
  below).
- **Required key**: `kubeconfig`. The value must be a complete, self-contained
  kubeconfig (PEM-inlined CA data or a CA reference that resolves inside the
  provisioner pod) pointing at the managed cluster's kube-apiserver. A missing
  or empty `kubeconfig` key causes the provisioner to fail the sync and patch
  `ManagedKubeconfigReady=False` on the hub `ManagedClusterAddOn`.
- **Embedded credential**: must authenticate as an identity on the managed
  cluster that is authorized to `create` the `serviceaccounts/token`
  subresource for the `cluster-proxy` ServiceAccount in the addon install
  namespace (the namespace named by the addon's
  `InstallStrategy`/`InstallSpec`, typically
  `open-cluster-management-cluster-proxy`). The provisioner calls
  `TokenRequest` against that ServiceAccount; any other verbs (`get`, `list`,
  ...) on the managed cluster are not required.
- **Lifecycle**: the Secret is read-only as far as the addon is concerned;
  rotating the embedded credential is the operator's responsibility. When the
  Secret contents change the provisioner detects the new SHA-256 hash and
  re-issues a managed token within `managedKubeConfigSyncInterval`.

The `cluster-proxy` ServiceAccount on the managed cluster is created by the
addon-agent's own manifests and does not need to be pre-provisioned; only the
permission to mint tokens for it must already be granted to the identity
embedded in the external kubeconfig.

Example one-shot provisioning of the Secret from a managed-cluster kubeconfig
file:

```shell
kubectl --kubeconfig <hosting-kubeconfig> -n <managed-cluster-name> \
  create secret generic external-managed-kubeconfig \
  --from-file=kubeconfig=<path-to-managed-kubeconfig>
```

Hosted mode supports the managed Kubernetes API proxy path. When
`enableServiceProxy=true`, regular Service proxy traffic is always relayed
through a managed-side `cluster-proxy-service-relay` Service via the managed
kube-apiserver Service proxy subresource. This is required because the
service-proxy container runs on the hosting cluster and cannot use managed
cluster Service DNS names or ClusterIPs directly.

| Mode | Kube API proxy | Regular Service proxy |
|------|----------------|-----------------------|
| Default | Supported | Supported when service proxy is enabled |
| Hosted, `enableServiceProxy=false` | Supported | Disabled |
| Hosted, `enableServiceProxy=true` | Supported | Supported through the managed-side `cluster-proxy-service-relay` Deployment and Service |

The following `AddOnDeploymentConfig.spec.customizedVariables` are available for
hosted mode:

- `externalManagedKubeConfigSecretNamespace`: defaults to the managed cluster name
- `externalManagedKubeConfigSecretName`: defaults to `external-managed-kubeconfig`
- `managedKubeConfigSecret`: defaults to `cluster-proxy-managed-kubeconfig`
- `managedKubeConfigTokenExpiration`: defaults to `24h`
- `managedKubeConfigRefreshBefore`: defaults to `1h`
- `managedKubeConfigSyncInterval`: defaults to `5m`
- `serviceRelayName`: name of the managed-side relay Service/Deployment provisioned when `enableServiceProxy=true`; defaults to `cluster-proxy-service-relay`. The hosted service-proxy uses this name to build the managed-apiserver service-proxy URL, so it must match the relay Service name.
- `serviceRelayPort`: port of the managed-side relay Service provisioned when `enableServiceProxy=true`; defaults to `7444`. The hosted service-proxy uses this port to build the managed-apiserver service-proxy URL, so it must match the relay Service port.

The hosted provisioner patches `ManagedKubeconfigReady` on the hub
`ManagedClusterAddOn` and exposes health and metrics on `:8000`. The
managed-apiserver raw TCP relay exposes health and metrics on `:8001`; the
service relay exposes health and metrics on `:8000`.

#### Recommended NetworkPolicy for the managed service-relay

When hosted Service proxy is enabled, the managed-side `cluster-proxy-service-relay`
Service is reachable inside the managed cluster on its ClusterIP. The relay
itself enforces the trust boundary by TokenReview-ing the inbound caller token
against the managed kube-apiserver and rejecting callers not in
`--trusted-caller-username`, so a managed-cluster Pod that reaches the relay
ClusterIP directly cannot use it as an open HTTP proxy. As a belt-and-braces
defense, operators are expected to also apply a `NetworkPolicy` that restricts
ingress to the relay Pod to traffic arriving via the managed kube-apiserver's
`services/proxy` subresource. The chart does not ship this policy because the
allowed source depends on how the managed cluster runs its kube-apiserver
(host-network on control plane nodes vs. a labeled Pod in `kube-system`), and on
which CNI is in use.

A typical policy for a managed cluster where the kube-apiserver runs as
host-network on control plane nodes selects the relay Pod by its component
label and allows ingress only from the control plane node CIDR (substitute the
real CIDR and addon install namespace for your environment):

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: cluster-proxy-service-relay
  namespace: open-cluster-management-cluster-proxy
spec:
  podSelector:
    matchLabels:
      open-cluster-management.io/addon: cluster-proxy
      proxy.open-cluster-management.io/component-name: service-relay
  policyTypes:
    - Ingress
  ingress:
    - from:
        - ipBlock:
            cidr: 10.0.0.0/24 # control plane node CIDR
      ports:
        - protocol: TCP
          port: 7444 # matches serviceRelayPort
```

If the managed kube-apiserver runs as a regular Pod (for example in
`kube-system` with a `component=kube-apiserver` label), replace the `ipBlock`
peer with the corresponding `namespaceSelector` + `podSelector` peer. Adjust the
addon install namespace, the `port` to match the configured `serviceRelayPort`,
and any additional egress rules required by your CNI.

### Metrics

The proxy-agent Pod exposes Prometheus metrics on the `addon-agent` container's
`agent-metrics` port (`8888`). When the regular service-proxy container is
enabled, it exposes metrics on the `svc-metrics` port (`8000`). In hosted mode
with the managed-apiserver proxy enabled, the `managed-apiserver-proxy`
container additionally exposes metrics on the `api-metrics` port (`8001`).

A metrics-only ClusterIP Service named `cluster-proxy-agent-metrics` and a
matching `monitoring.coreos.com/v1` `ServiceMonitor` of the same name are
rendered into the addon install namespace when opted in via the following
`AddOnDeploymentConfig.spec.customizedVariables`:

- `agentMetricsServiceEnabled`: `"true"` to render the agent metrics Service;
  defaults to `"false"`. Enabling the ServiceMonitor below implicitly enables
  this Service.
- `agentServiceMonitorEnabled`: `"true"` to render the `ServiceMonitor`;
  defaults to `"false"`. Enabling this requires the
  `monitoring.coreos.com/v1` `ServiceMonitor` CRD (from prometheus-operator) to
  be installed on the cluster that hosts the proxy-agent Pod — the addon
  install namespace on the managed cluster in Default mode, or the hosting
  cluster in Hosted mode. The Service is enabled automatically when this is
  enabled.
- `agentServiceMonitorLabels`: optional comma-separated `key=value` list of
  labels added to the rendered `ServiceMonitor` (typically to match a
  Prometheus `serviceMonitorSelector`). Each key must be a valid Kubernetes
  label key and each value a valid label value; defaults to empty.

### Performance

The following table shows network bandwidth benchmarking results via [goben](https://github.com/udhos/goben)
comparing direct connections with connections through Cluster-Proxy (Apiserver-Network-Proxy). 
The proxying through the tunnel involves approximately 50% performance overhead, so it's recommended 
to avoid transferring data-intensive traffic over the proxy when possible.

|  Bandwidth  |   Direct   | over Cluster-Proxy |
|-------------|------------|--------------------|
|  Read/Mbps  |  902 Mbps  |     461 Mbps       |
|  Write/Mbps |  889 Mbps  |     428 Mbps       |



## References

- Design: [https://github.com/open-cluster-management-io/enhancements/tree/main/enhancements/sig-architecture/14-addon-cluster-proxy](https://github.com/open-cluster-management-io/enhancements/tree/main/enhancements/sig-architecture/14-addon-cluster-proxy)
- Addon-Framework: [https://github.com/open-cluster-management-io/addon-framework](https://github.com/open-cluster-management-io/addon-framework)
