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

## Architecture

See [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) for the component layout,
default mode and hosted mode request paths, ClusterProfile integration, and the
expected hosted behavior before a managed cluster has schedulable workload
nodes.

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
has the `addon.open-cluster-management.io/hosting-cluster-name` annotation. See
the [architecture guide](./docs/ARCHITECTURE.md#hosted-mode) for the hosted
component placement and kube-apiserver versus Service request flow.

The hosting cluster must contain an external managed-cluster kubeconfig Secret.
By default the addon reads `external-managed-kubeconfig` from the namespace named
after the managed cluster, creates short-lived tokens for the managed
`cluster-proxy` service account, and writes a generated kubeconfig Secret named
`cluster-proxy-managed-kubeconfig` in the addon install namespace. The generated
kubeconfig is mounted read-only by the hosted agent containers. The external
admin kubeconfig is mounted only by the provisioner.

The external Secret contract is:

- type `Opaque`
- data key `kubeconfig`
- a complete kubeconfig for the managed kube-apiserver
- credentials authorized to `create` the `serviceaccounts/token` subresource for
  the managed cluster `cluster-proxy` ServiceAccount in the addon install
  namespace
- operator-owned credential rotation; the addon detects Secret content changes
  and refreshes the generated kubeconfig within `managedKubeConfigSyncInterval`

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

The following `AddOnDeploymentConfig.spec.customizedVariables` are available for
hosted mode:

- `externalManagedKubeConfigSecretNamespace`: defaults to the managed cluster name
- `externalManagedKubeConfigSecretName`: defaults to `external-managed-kubeconfig`
- `managedKubeConfigSecret`: defaults to `cluster-proxy-managed-kubeconfig`
- `managedKubeConfigTokenExpiration`: defaults to `24h`
- `managedKubeConfigRefreshBefore`: defaults to `1h`
- `managedKubeConfigSyncInterval`: defaults to `5m`
- `serviceRelayName`: name of the managed-side relay Service/Deployment
  provisioned when `enableServiceProxy=true`; defaults to
  `cluster-proxy-service-relay`
- `serviceRelayPort`: port of the managed-side relay Service provisioned when
  `enableServiceProxy=true`; defaults to `7444`

The hosted provisioner patches `ManagedKubeconfigReady` on the hub
`ManagedClusterAddOn` and exposes health and metrics on `:8000`. The
managed-apiserver raw TCP relay exposes health and metrics on `:8001`; the
service relay exposes health and metrics on `:8000`.

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
