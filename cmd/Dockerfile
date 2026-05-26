# Build the manager binary
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder

WORKDIR /workspace

ARG TARGETOS
ARG TARGETARCH
ARG OS
ARG ARCH
ARG APISERVER_NETWORK_PROXY_VERSION=0.30.2
ARG KUBECTL_VERSION=v1.30.2
ARG ADDON_AGENT_IMAGE_NAME

# Build Apiserver-network-proxy binaries
RUN build_os="${OS:-${TARGETOS:-linux}}" \
      && build_arch="${ARCH:-${TARGETARCH:-amd64}}" \
      && wget https://github.com/kubernetes-sigs/apiserver-network-proxy/archive/refs/tags/v${APISERVER_NETWORK_PROXY_VERSION}.tar.gz \
      && tar xzf v${APISERVER_NETWORK_PROXY_VERSION}.tar.gz \
      && cd apiserver-network-proxy-${APISERVER_NETWORK_PROXY_VERSION} \
      && CGO_ENABLED=0 GOOS="${build_os}" GOARCH="${build_arch}" go build -o /workspace/proxy-server ./cmd/server/ \
      && CGO_ENABLED=0 GOOS="${build_os}" GOARCH="${build_arch}" go build -o /workspace/proxy-agent ./cmd/agent/ \
      && CGO_ENABLED=0 GOOS="${build_os}" GOARCH="${build_arch}" go build -o /workspace/proxy-test-client ./cmd/test-client/ \
      && CGO_ENABLED=0 GOOS="${build_os}" GOARCH="${build_arch}" go build -o /workspace/proxy-test-server ./cmd/test-server/ \
      && cd /workspace \
      && curl -LO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${build_os}/${build_arch}/kubectl" \
      && chmod a+x kubectl

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY pkg pkg/

# Build addons
RUN build_os="${OS:-${TARGETOS:-linux}}" \
      && build_arch="${ARCH:-${TARGETARCH:-amd64}}" \
      && CGO_ENABLED=0 GOOS="${build_os}" GOARCH="${build_arch}" go build -a -o agent cmd/addon-agent/main.go
RUN build_os="${OS:-${TARGETOS:-linux}}" \
      && build_arch="${ARCH:-${TARGETARCH:-amd64}}" \
      && CGO_ENABLED=0 GOOS="${build_os}" GOARCH="${build_arch}" go build -a \
        -ldflags="-X 'open-cluster-management.io/cluster-proxy/pkg/config.AgentImageName=${ADDON_AGENT_IMAGE_NAME}'" \
        -o manager cmd/addon-manager/main.go
RUN build_os="${OS:-${TARGETOS:-linux}}" \
      && build_arch="${ARCH:-${TARGETARCH:-amd64}}" \
      && CGO_ENABLED=0 GOOS="${build_os}" GOARCH="${build_arch}" go build -a -o cluster-proxy cmd/cluster-proxy/main.go

FROM alpine:3.21

WORKDIR /
COPY --from=builder /workspace/kubectl /workspace/proxy-server /workspace/proxy-agent /workspace/proxy-test-client /workspace/proxy-test-server ./
COPY --from=builder /workspace/agent /workspace/manager ./
COPY --from=builder /workspace/cluster-proxy ./
USER 65532:65532
