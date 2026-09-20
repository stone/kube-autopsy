# Base images are pinned by digest. The release workflow signs the image with
# cosign and publishes an SBOM; both describe a build whose inputs must be
# identifiable, and a floating tag means the same source can produce a different
# image tomorrow. Update these deliberately, alongside the Go toolchain bump.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS builder
WORKDIR /workspace

# Dependencies are resolved from the committed go.mod/go.sum only; -mod=readonly
# makes a build that would need to change them fail rather than quietly succeed
# against something the repository does not record.
ENV GOFLAGS=-mod=readonly
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETARCH
# VERSION and COMMIT are stamped into the binary so a running agent can be tied
# back to the source it came from. An image tag is not enough: :latest moves.
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w \
      -X github.com/kube-autopsy/kube-autopsy/internal/version.Version=${VERSION} \
      -X github.com/kube-autopsy/kube-autopsy/internal/version.Commit=${COMMIT}" \
    -o /kube-autopsy ./cmd/kube-autopsy/

# Deliberately the root-default variant, not :nonroot. The agent reads the
# kubelet's container log files under /var/log/pods, which are root-owned and
# not world-readable on several distributions, so a non-root default would break
# --capture-logs. The controller does not need root and pins runAsNonRoot with
# runAsUser 65532 in its own securityContext, which is where that belongs.
FROM gcr.io/distroless/static:latest@sha256:58133991db06659feaabe0f4e97a35cebf15ef4ea08f8a4c6d2ee5f75e4aa6a0
COPY --from=builder /kube-autopsy /kube-autopsy
ENTRYPOINT ["/kube-autopsy"]
