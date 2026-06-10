**Table of Contents**

- [Contributing guidelines](#contributing-guidelines)
    - [Terms](#terms)
    - [Certificate of Origin](#certificate-of-origin)
    - [DCO Sign Off](#dco-sign-off)
    - [Code of Conduct](#code-of-conduct)
    - [Contributing a patch](#contributing-a-patch)
    - [Issue and pull request management](#issue-and-pull-request-management)
    - [Pre-check before submitting a PR](#pre-check-before-submitting-a-pr)

# Contributing guidelines

## Terms

All contributions to the repository must be submitted under the terms of the [Apache Public License 2.0](https://www.apache.org/licenses/LICENSE-2.0).

## Certificate of Origin

By contributing to this project, you agree to the Developer Certificate of Origin (DCO). This document was created by the Linux Kernel community and is a simple statement that you, as a contributor, have the legal right to make the contribution. See the [DCO](https://github.com/open-cluster-management/community/blob/main/DCO) file for details.

## DCO Sign Off

You must sign off your commit to state that you certify the [DCO](https://github.com/open-cluster-management/community/blob/main/DCO). To certify your commit for DCO, add a line like the following at the end of your commit message:

```
Signed-off-by: John Smith <john@example.com>
```

This can be done with the `--signoff` option to `git commit`. See the [Git documentation](https://git-scm.com/docs/git-commit#Documentation/git-commit.txt--s) for details.

## Code of Conduct

The Open Cluster Management project has adopted the CNCF Code of Conduct. Refer to our [Community Code of Conduct](https://github.com/open-cluster-management/community/blob/main/CODE_OF_CONDUCT.md) for details.

## Contributing a patch

1. Submit an issue describing your proposed change to the repository in question. The repository owners will respond to your issue promptly.
2. Fork the desired repository, then develop and test your code changes.
3. Submit a pull request.

## Issue and pull request management

Anyone can comment on issues and submit reviews for pull requests. In order to be assigned an issue or pull request, you can leave a `/assign <your Github ID>` comment on the issue or pull request (PR).

## Pre-check before submitting a PR 
<!-- Customize this template for your repository -->

Before submitting a PR, please perform the following steps:

- Run `make build`.
- Run `make verify`.
- Run `make test`.
- Run `make test-integration` for controller or manifest behavior changes.
- Run `make test-e2e` for user-facing proxy behavior changes.
- Run `make test-e2e-hosted` for hosted-mode behavior changes.
- Run `make test-e2e-hosted-no-worker` when changing how the agent behaves on a
  managed cluster that has no schedulable worker nodes.

Use these make targets as the official test interface. A raw `go test ./...`
does not include generated manifests, envtest asset setup, linting, or the e2e
packaging used by CI.

### Hosted e2e prerequisites

`make test-e2e-hosted` provisions three kind clusters (hub, hosting, managed),
builds the cluster-proxy container image via `make images`, and drives the
hosted addon flow end-to-end. It runs the non-destructive hosted specs first
and then runs the destructive `hosted-cleanup` spec as a final phase. Before
running it locally, install:

- `docker` with BuildKit enabled (the `cmd/Dockerfile` uses `--platform=$BUILDPLATFORM`,
  which requires BuildKit; on Docker 23+ BuildKit is the default builder, on older
  Docker daemons export `DOCKER_BUILDKIT=1`)
- `kind`, `kubectl`, `helm`, `jq`, and `clusteradm`

The target removes any leftover hosted kind clusters and the working directory
before each run, so it is safe to re-invoke.

### Hosted e2e knobs

The following environment variables override defaults consumed by
`make test-e2e-hosted` and the scripts under `test/e2e/env/`:

- `IMAGE_REGISTRY_NAME`, `IMAGE_NAME`, `IMAGE_TAG`: image coordinates loaded
  into every hosted kind cluster (defaults: `quay.io/open-cluster-management`,
  `cluster-proxy`, `latest`)
- `CONTAINER_ENGINE`: container build tool used by `make images` (default `docker`)
- `E2E_HOSTED_HUB_CLUSTER_NAME`, `E2E_HOSTED_HOSTING_CLUSTER_NAME`,
  `E2E_HOSTED_MANAGED_CLUSTER_NAME`: kind cluster names (defaults
  `cluster-proxy-hosted-hub`, `cluster-proxy-hosted-hosting`,
  `cluster-proxy-hosted-managed`)
- `E2E_HOSTED_WORK_DIR`: scratch directory for kubeconfigs and the generated
  `env` file (default `_output/e2e-hosted`)
- `E2E_HOSTED_PROXY_ENTRYPOINT_LOCAL_PORT`, `E2E_HOSTED_USER_SERVER_LOCAL_PORT`:
  local ports used by the `kubectl port-forward` driving the hub services
  (defaults `18090`, `19092`)
- `HOSTED_LABEL_FILTER`: Ginkgo v2 label-filter expression passed to
  `go test ./test/e2e` (default `hosted && !hosted-cleanup`); set it to a more
  specific label such as `hosted-relay` to scope the run to a subset of hosted
  specs, or `hosted-cleanup` to run only the final deletion/cleanup check.

Use `make clean-e2e-hosted` to tear down hosted kind clusters and the working
directory between runs.

### Hosted no-worker e2e

`make test-e2e-hosted-no-worker` runs the hosted flow against a managed cluster
whose worker workloads are unschedulable, validating that the hosted
kube-apiserver proxy stays available when the managed cluster exposes no
schedulable worker nodes. It reuses the same prerequisites and scripts as
`make test-e2e-hosted`, but provisions a dedicated set of kind clusters so it
can run alongside the standard hosted target without colliding. Internally it
sets `MANAGED_CLUSTER_WORKLOADS_UNSCHEDULABLE=true` for `init-hosted.sh` and
scopes the Ginkgo run to the `hosted-no-worker` label.

The no-worker target reads its own override knobs (separate from the standard
hosted target so both can coexist):

- `E2E_HOSTED_NO_WORKER_HUB_CLUSTER_NAME`,
  `E2E_HOSTED_NO_WORKER_HOSTING_CLUSTER_NAME`,
  `E2E_HOSTED_NO_WORKER_MANAGED_CLUSTER_NAME`: kind cluster names (defaults
  `cluster-proxy-hosted-no-worker-hub`,
  `cluster-proxy-hosted-no-worker-hosting`,
  `cluster-proxy-hosted-no-worker-managed`)
- `E2E_HOSTED_NO_WORKER_WORK_DIR`: scratch directory for kubeconfigs and the
  generated `env` file (default `_output/e2e-hosted-no-worker`)
- `E2E_HOSTED_NO_WORKER_PROXY_ENTRYPOINT_LOCAL_PORT`,
  `E2E_HOSTED_NO_WORKER_USER_SERVER_LOCAL_PORT`: local ports used by the
  `kubectl port-forward` driving the hub services (defaults `18091`, `19093`)

The shared `IMAGE_REGISTRY_NAME`, `IMAGE_NAME`, `IMAGE_TAG`, and
`CONTAINER_ENGINE` knobs documented above apply here as well.

Use `make clean-e2e-hosted-no-worker` to tear down the no-worker kind clusters
and its working directory between runs.
