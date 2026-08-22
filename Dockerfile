# Base images are pinned by digest. The release workflow signs the image with
# cosign and publishes an SBOM; both describe a build whose inputs must be
# identifiable, and a floating tag means the same source can produce a different
# image tomorrow. Update these deliberately, alongside the Go toolchain bump.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS builder
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
FROM gcr.io/distroless/static:latest@sha256:f2ea2709ac8db56323cbd7d014277f32cb572d9ea124b0076f7aafe5980678fe
COPY --from=builder /kube-autopsy /kube-autopsy
ENTRYPOINT ["/kube-autopsy"]
