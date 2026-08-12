# Cross-compile from the build platform rather than emulating the target, so
# multi-arch builds do not run the Go toolchain under QEMU.
FROM --platform=$BUILDPLATFORM golang:1-alpine AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# TARGETARCH is supplied by buildx for each platform being built. The agent
# embeds an architecture-specific eBPF object selected by build tag, so this
# must match the platform the image will actually run on.
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /kube-autopsy ./cmd/kube-autopsy/

# Minimal runtime image.
FROM gcr.io/distroless/static:latest
COPY --from=builder /kube-autopsy /kube-autopsy
# Default to root for DaemonSet; Controller Deployment will override via runAsUser: 65532
ENTRYPOINT ["/kube-autopsy"]
