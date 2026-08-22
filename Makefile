# kube-autopsy Makefile
IMG ?= kube-autopsy:latest
BINARY = kube-autopsy
.DEFAULT_GOAL := build

# Stamped into the binary so a running agent can be traced back to its source;
# an image tag is not enough, since :latest moves.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
VERSION_PKG = github.com/kube-autopsy/kube-autopsy/internal/version
LDFLAGS = -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT)

##@ Build
.PHONY: build
build: ## Build the binary.
	go build -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/kube-autopsy/

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: ## Run tests with race detection.
	go test ./... -v -race

.PHONY: cover
cover: ## Run tests and report per-package coverage.
	go test ./... -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run

##@ Generate
.PHONY: generate
generate: ## Generate deepcopy functions and CRD manifests with controller-gen.
	controller-gen object:headerFile="" paths=./api/...
	# controller-gen names its output <group>_<plural>.yaml, so it is renamed to
	# the path the kustomization and Makefile install target actually reference.
	# Without the rename, deploy/base/crd.yaml silently goes stale.
	controller-gen crd paths=./api/... output:crd:dir=deploy/base
	mv deploy/base/autopsy.tty.se_podcrashreports.yaml deploy/base/crd.yaml

.PHONY: generate-bpf
generate-bpf: ## Regenerate the eBPF objects and Go bindings (needs clang and libbpf headers).
	go generate ./...

##@ Docker
.PHONY: docker-build
docker-build: ## Build the Docker image.
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the Docker image.
	docker push $(IMG)

##@ Deploy
.PHONY: install
install: ## Install CRDs into the cluster.
	kubectl apply -f deploy/base/crd.yaml

.PHONY: uninstall
uninstall: ## Remove CRDs from the cluster.
	kubectl delete -f deploy/base/crd.yaml

.PHONY: deploy
deploy: ## Deploy the quickstart stack (privileged agent, plain-HTTP metrics, logs on).
	kubectl apply -k deploy/overlays/quickstart

.PHONY: deploy-hardened
deploy-hardened: ## Deploy the least-privilege stack (needs Linux 5.8+ on every node).
	kubectl apply -k deploy/overlays/hardened

.PHONY: undeploy
undeploy: ## Remove the quickstart stack.
	kubectl delete -k deploy/overlays/quickstart

.PHONY: undeploy-hardened
undeploy-hardened: ## Remove the hardened stack.
	kubectl delete -k deploy/overlays/hardened

##@ E2E Testing

.PHONY: e2e
e2e: ## Run end-to-end tests using kind (creates/destroys cluster).
	./test/e2e/run.sh

.PHONY: e2e-no-cleanup
e2e-no-cleanup: ## Run e2e tests but keep the kind cluster for debugging.
	./test/e2e/run.sh --no-cleanup

BTF_FIXTURE_DIR = internal/agent/bpf/testdata/btf

.PHONY: btf-fixtures
btf-fixtures: ## Fetch kernel BTF blobs used by the CO-RE relocation tests.
	@# One kernel from each side of the Linux 6.2 mm_struct rss_stat layout
	@# change, so the tests can prove the eBPF program resolves against both.
	@# The blobs are several MB each, so they are fetched rather than committed.
	mkdir -p $(BTF_FIXTURE_DIR)
	curl -sSL "https://github.com/aquasecurity/btfhub-archive/raw/main/ubuntu/20.04/x86_64/5.8.0-63-generic.btf.tar.xz" \
		| tar xJ -O > $(BTF_FIXTURE_DIR)/linux-5.8-x86_64.btf
	gzip -dc "$$(go list -m -f '{{.Dir}}' github.com/cilium/ebpf)/btf/testdata/vmlinux.btf.gz" \
		> $(BTF_FIXTURE_DIR)/linux-6x-x86_64.btf
