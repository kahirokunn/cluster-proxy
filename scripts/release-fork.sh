#!/usr/bin/env bash

# Disable xtrace before handling registry credentials.
set +x
set -euo pipefail

OWNER="${GHCR_OWNER:-kahirokunn}"
REPO="${GITHUB_REPO:-cluster-proxy}"
REMOTE="${GIT_REMOTE:-fork}"
BRANCH="${GIT_BRANCH:-hosted-mode-relay-support}"
REGISTRY="${REGISTRY:-ghcr.io}"
CHART_NAME="cluster-proxy"
CHART_DIR="charts/${CHART_NAME}"
HOST_DOCKER_CONFIG="${DOCKER_CONFIG:-${HOME}/.docker}"

IMAGE="${REGISTRY}/${OWNER}/cluster-proxy"
CHART_REPO="oci://${REGISTRY}/${OWNER}/charts"
CHART_REF="${CHART_REPO}/${CHART_NAME}"
REFRESH_SCOPES_COMMAND="gh auth refresh -h github.com -s read:packages -s write:packages -s delete:packages"

TMP_ROOT=""
TEMP_BUILDX_BUILDER=""

usage() {
  cat >&2 <<EOF
Usage:
  scripts/release-fork.sh release <version>
  scripts/release-fork.sh verify-public <version>

Example:
  scripts/release-fork.sh release v0.10.0-hosted.1
  scripts/release-fork.sh verify-public v0.10.0-hosted.1
EOF
}

log() {
  printf '==> %s\n' "$*" >&2
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [[ -n "${TEMP_BUILDX_BUILDER}" ]]; then
    docker buildx rm -f "${TEMP_BUILDX_BUILDER}" >/dev/null 2>&1 || true
  fi

  if [[ -n "${TMP_ROOT}" && -d "${TMP_ROOT}" ]]; then
    rm -rf "${TMP_ROOT}"
  fi
}
trap cleanup EXIT

validate_version() {
  local version="$1"
  if ! [[ "${version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
    die "version must be SemVer with a leading v, got: ${version}"
  fi
}

chart_version_for() {
  local version="$1"
  printf '%s\n' "${version#v}"
}

require_cmd() {
  local cmd="$1"
  command -v "${cmd}" >/dev/null 2>&1 || die "${cmd} is required"
}

require_tools() {
  require_cmd git
  require_cmd docker
  require_cmd helm
  require_cmd gh
  require_cmd jq

  docker buildx version >/dev/null 2>&1 || die "docker buildx is required"
}

require_clean_tree() {
  if [[ -n "$(git status --porcelain)" ]]; then
    git status --short >&2
    die "release requires a clean git worktree"
  fi
}

require_release_branch() {
  local current_branch
  current_branch="$(git branch --show-current)"
  if [[ "${current_branch}" != "${BRANCH}" ]]; then
    die "release must run from branch ${BRANCH}; current branch is ${current_branch}"
  fi
}

require_tag_available() {
  local version="$1"

  if git rev-parse -q --verify "refs/tags/${version}" >/dev/null; then
    die "local tag ${version} already exists"
  fi

  if git ls-remote --exit-code --tags "${REMOTE}" "refs/tags/${version}" >/dev/null 2>&1; then
    die "remote tag ${version} already exists on ${REMOTE}"
  fi
}

require_release_available() {
  local version="$1"

  if gh release view "${version}" --repo "${OWNER}/${REPO}" >/dev/null 2>&1; then
    die "GitHub Release ${version} already exists in ${OWNER}/${REPO}"
  fi
}

ensure_tmp_root() {
  if [[ -z "${TMP_ROOT}" ]]; then
    TMP_ROOT="$(mktemp -d)"
  fi
}

setup_empty_registry_configs() {
  ensure_tmp_root
  export DOCKER_CONFIG="${TMP_ROOT}/docker"
  export HELM_REGISTRY_CONFIG="${TMP_ROOT}/helm/registry/config.json"
  mkdir -p "${DOCKER_CONFIG}" "$(dirname "${HELM_REGISTRY_CONFIG}")"
  install_docker_buildx_plugin
}

install_docker_buildx_plugin() {
  local plugin
  local candidates=(
    "${HOST_DOCKER_CONFIG}/cli-plugins/docker-buildx"
    "${HOME}/.docker/cli-plugins/docker-buildx"
    "/usr/local/lib/docker/cli-plugins/docker-buildx"
    "/usr/lib/docker/cli-plugins/docker-buildx"
    "/usr/libexec/docker/cli-plugins/docker-buildx"
  )

  if DOCKER_CONFIG="${DOCKER_CONFIG}" docker buildx version >/dev/null 2>&1; then
    return
  fi

  for plugin in "${candidates[@]}"; do
    if [[ -x "${plugin}" ]]; then
      mkdir -p "${DOCKER_CONFIG}/cli-plugins"
      ln -sf "${plugin}" "${DOCKER_CONFIG}/cli-plugins/docker-buildx"
      return
    fi
  done

  die "docker buildx is unavailable with the temporary Docker config"
}

github_token() {
  if [[ -n "${GHCR_TOKEN:-}" ]]; then
    printf '%s\n' "${GHCR_TOKEN}"
    return
  fi

  if [[ -n "${CR_PAT:-}" ]]; then
    printf '%s\n' "${CR_PAT}"
    return
  fi

  gh auth token 2>/dev/null || true
}

github_login_for_token() {
  local token="$1"
  local login

  if [[ -n "${GHCR_USER:-}" ]]; then
    printf '%s\n' "${GHCR_USER}"
    return
  fi

  if login="$(GH_TOKEN="${token}" gh api user --jq .login 2>/dev/null)" && [[ -n "${login}" ]]; then
    printf '%s\n' "${login}"
    return
  fi

  printf '%s\n' "${OWNER}"
}

require_write_packages_scope() {
  local token="$1"
  local headers
  local scopes

  headers="$(GH_TOKEN="${token}" gh api --include user 2>/dev/null || true)"
  scopes="$(printf '%s\n' "${headers}" | awk 'BEGIN { IGNORECASE=1 } /^x-oauth-scopes:/ { sub(/^[^:]*:[[:space:]]*/, ""); print; exit }' | tr '[:upper:]' '[:lower:]' | tr -d '\r')"

  if [[ -z "${scopes}" ]]; then
    warn "could not determine GitHub token scopes; continuing with GHCR login check"
    return
  fi

  if ! printf '%s\n' "${scopes}" | grep -Eq '(^|[ ,])write:packages([ ,]|$)'; then
    die "GitHub token is missing write:packages. Refresh it with: ${REFRESH_SCOPES_COMMAND}"
  fi
}

login_to_registries() {
  local token="$1"
  local login="$2"

  printf '%s\n' "${token}" | docker login "${REGISTRY}" --username "${login}" --password-stdin >/dev/null
  printf '%s\n' "${token}" | helm registry login "${REGISTRY}" --username "${login}" --password-stdin >/dev/null
}

ensure_buildx_builder() {
  local name

  if [[ -n "${BUILDX_BUILDER:-}" ]]; then
    if docker buildx inspect "${BUILDX_BUILDER}" >/dev/null 2>&1; then
      return
    fi
    warn "buildx builder ${BUILDX_BUILDER} is not available with the temporary Docker config; creating a temporary builder"
  fi

  name="cluster-proxy-release-${VERSION//[^0-9A-Za-z]/-}-$$"
  docker buildx create --driver docker-container --name "${name}" >/dev/null
  TEMP_BUILDX_BUILDER="${name}"
  BUILDX_BUILDER="${name}"
}

build_image() {
  local ref="$1"
  local revision="$2"
  local buildx_args=()

  if [[ -n "${BUILDX_BUILDER:-}" ]]; then
    buildx_args+=(--builder "${BUILDX_BUILDER}")
  fi

  docker buildx build "${buildx_args[@]}" \
    --platform linux/amd64,linux/arm64 \
    --push \
    --build-arg "ADDON_AGENT_IMAGE_NAME=${ref}" \
    --label "org.opencontainers.image.source=https://github.com/${OWNER}/${REPO}" \
    --label "org.opencontainers.image.revision=${revision}" \
    --label "org.opencontainers.image.version=${VERSION}" \
    --tag "${ref}" \
    --file cmd/Dockerfile \
    .
}

require_image_platforms() {
  local ref="$1"
  local output

  output="$(docker buildx imagetools inspect "${ref}")"
  printf '%s\n' "${output}" | grep -q 'linux/amd64' || die "${ref} is missing linux/amd64"
  printf '%s\n' "${output}" | grep -q 'linux/arm64' || die "${ref} is missing linux/arm64"
}

package_chart() {
  local version="$1"
  local chart_version="$2"
  local output_dir="$3"
  local chart_copy="${output_dir}/chart/${CHART_NAME}"

  mkdir -p "${output_dir}/chart"
  cp -R "${CHART_DIR}" "${chart_copy}"
  sed -i \
    -e "s#^registry:.*#registry: ${REGISTRY}/${OWNER}#" \
    -e "s#^image:.*#image: cluster-proxy#" \
    -e "s#^tag:.*#tag: ${version}#" \
    -e "s#^proxyServerImage:.*#proxyServerImage: ${IMAGE}#" \
    -e "s#^proxyAgentImage:.*#proxyAgentImage: ${IMAGE}#" \
    "${chart_copy}/values.yaml"

  helm lint "${chart_copy}"
  helm package "${chart_copy}" \
    --version "${chart_version}" \
    --app-version "${version}" \
    --destination "${output_dir}"
}

push_branch_and_tag() {
  local version="$1"

  git tag -a "${version}" -m "Release ${version}"
  git push "${REMOTE}" "HEAD:refs/heads/${BRANCH}"
  git push "${REMOTE}" "refs/tags/${version}"
}

print_public_package_urls() {
  cat >&2 <<EOF

GHCR packages may be private after command-line publishing. Set these packages to Public:
  https://github.com/users/${OWNER}/packages/container/package/cluster-proxy/settings
  https://github.com/users/${OWNER}/packages/container/package/charts%2Fcluster-proxy/settings

After changing visibility, verify anonymous access:
  scripts/release-fork.sh verify-public ${VERSION}
EOF
}

release() {
  VERSION="$1"
  local chart_version
  local token
  local login
  local revision
  local release_dir
  local chart_package

  validate_version "${VERSION}"
  chart_version="$(chart_version_for "${VERSION}")"

  require_tools
  require_clean_tree
  require_release_branch
  require_tag_available "${VERSION}"
  require_release_available "${VERSION}"

  token="$(github_token)"
  [[ -n "${token}" ]] || die "GHCR auth required. Set GHCR_TOKEN or CR_PAT, or authenticate gh with: ${REFRESH_SCOPES_COMMAND}"
  require_write_packages_scope "${token}"
  login="$(github_login_for_token "${token}")"

  setup_empty_registry_configs
  login_to_registries "${token}" "${login}"
  ensure_buildx_builder

  revision="$(git rev-parse HEAD)"

  log "Creating and pushing ${VERSION} from ${revision}"
  push_branch_and_tag "${VERSION}"

  log "Building and pushing ${IMAGE}:${VERSION}"
  build_image "${IMAGE}:${VERSION}" "${revision}"

  log "Verifying multi-arch image manifest"
  require_image_platforms "${IMAGE}:${VERSION}"

  release_dir="${TMP_ROOT}/release"
  log "Packaging Helm chart ${CHART_NAME}-${chart_version}"
  package_chart "${VERSION}" "${chart_version}" "${release_dir}"
  chart_package="${release_dir}/${CHART_NAME}-${chart_version}.tgz"

  log "Pushing Helm chart to ${CHART_REPO}"
  helm push "${chart_package}" "${CHART_REPO}"

  log "Verifying authenticated Helm pull"
  mkdir -p "${TMP_ROOT}/helm-pull"
  helm pull "${CHART_REF}" --version "${chart_version}" --destination "${TMP_ROOT}/helm-pull" >/dev/null

  log "Creating GitHub Release ${VERSION}"
  GH_TOKEN="${token}" gh release create "${VERSION}" "${chart_package}" \
    --repo "${OWNER}/${REPO}" \
    --title "${VERSION}" \
    --notes "Fork release ${VERSION} for hosted mode relay support." \
    --prerelease

  print_public_package_urls
}

verify_public_image() {
  local ref="$1"

  log "Checking anonymous docker pull for ${ref} on linux/amd64"
  docker --config "${DOCKER_CONFIG}" pull --platform linux/amd64 "${ref}" >/dev/null

  log "Checking anonymous docker pull for ${ref} on linux/arm64"
  docker --config "${DOCKER_CONFIG}" pull --platform linux/arm64 "${ref}" >/dev/null
}

verify_public() {
  local version="$1"
  local chart_version

  validate_version "${version}"
  chart_version="$(chart_version_for "${version}")"

  require_tools
  setup_empty_registry_configs

  verify_public_image "${IMAGE}:${version}"

  log "Checking anonymous Helm pull for ${CHART_REF} ${chart_version}"
  mkdir -p "${TMP_ROOT}/public-helm-pull"
  helm pull "${CHART_REF}" --version "${chart_version}" --destination "${TMP_ROOT}/public-helm-pull" >/dev/null

  log "Public verification completed for ${version}"
}

main() {
  local command="${1:-}"
  local version="${2:-}"

  case "${command}" in
    release)
      [[ -n "${version}" ]] || {
        usage
        exit 1
      }
      release "${version}"
      ;;
    verify-public)
      [[ -n "${version}" ]] || {
        usage
        exit 1
      }
      verify_public "${version}"
      ;;
    -h|--help|help)
      usage
      ;;
    *)
      usage
      exit 1
      ;;
  esac
}

main "$@"
