FROM golang:1-alpine AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /kube-autopsy ./cmd/kube-autopsy/

# Minimal runtime image.
FROM gcr.io/distroless/static:latest
COPY --from=builder /kube-autopsy /kube-autopsy
# Default to root for DaemonSet; Controller Deployment will override via runAsUser: 65532
ENTRYPOINT ["/kube-autopsy"]
