# Fork Release

This fork publishes hosted-mode cluster-proxy artifacts to GHCR from a local
machine. There is no GitHub Actions release workflow for this fork release.

Target artifacts for `v0.10.0-hosted.1`:

- `ghcr.io/kahirokunn/cluster-proxy:v0.10.0-hosted.1`
- `oci://ghcr.io/kahirokunn/charts/cluster-proxy --version 0.10.0-hosted.1`

## Prerequisites

Install `docker` with `buildx`, `helm`, `gh`, and `jq`. Authenticate GitHub CLI
with package write access:

```sh
gh auth refresh -h github.com -s read:packages -s write:packages -s delete:packages
```

Alternatively, set `GHCR_TOKEN` or `CR_PAT` to a token with `write:packages`.
The release script logs in to GHCR with temporary Docker and Helm registry
configs and removes those configs when it exits.

If `GH_TOKEN` is set in the environment, GitHub CLI uses that value instead of
stored credentials. Unset `GH_TOKEN` before running `gh auth refresh`, or set
`GHCR_TOKEN`/`CR_PAT` directly for the release script.

## Publish

Commit the release tooling first, then publish from a clean
`hosted-mode-relay-support` worktree:

```sh
bash -n scripts/release-fork.sh
git diff --check
scripts/release-fork.sh release v0.10.0-hosted.1
```

The script creates an annotated `v0.10.0-hosted.1` tag on the current commit,
pushes `hosted-mode-relay-support` and the tag to the `fork` remote, builds and
pushes the multi-arch image, pushes the Helm OCI chart, and creates a GitHub
Release with the packaged chart attached.

The chart package is built from a temporary copy with:

- chart `version: 0.10.0-hosted.1`
- chart `appVersion: v0.10.0-hosted.1`
- default `registry: ghcr.io/kahirokunn`
- default `image: cluster-proxy`
- default `proxyServerImage: ghcr.io/kahirokunn/cluster-proxy`
- default `proxyAgentImage: ghcr.io/kahirokunn/cluster-proxy`
- default `tag: v0.10.0-hosted.1`

No registry token is passed as a Docker build argument, image label, build
environment variable, or Helm value.

## Public Visibility

Container packages published from the command line may default to private. After
the release command completes, set these GHCR packages to Public from package
settings:

- <https://github.com/users/kahirokunn/packages/container/package/cluster-proxy/settings>
- <https://github.com/users/kahirokunn/packages/container/package/charts%2Fcluster-proxy/settings>

GitHub warns that public package visibility cannot be changed back to private.

Then verify anonymous pulls with empty Docker and Helm registry configs:

```sh
scripts/release-fork.sh verify-public v0.10.0-hosted.1
```

## Install

```sh
helm upgrade --install cluster-proxy \
  oci://ghcr.io/kahirokunn/charts/cluster-proxy \
  --version 0.10.0-hosted.1 \
  -n open-cluster-management-addon \
  --create-namespace
```
