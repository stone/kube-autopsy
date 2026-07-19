# kube-autopsy Makefile
IMG ?= kube-autopsy:latest
BINARY = kube-autopsy
.DEFAULT_GOAL := build

##@ Build
.PHONY: build
build: ## Build the binary.
	go build -o bin/$(BINARY) ./cmd/kube-autopsy/

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: ## Run tests with race detection.
	go test ./... -v -race

.PHONY: lint
lint: ## Run golangci-lint.
	golangci-lint run

##@ Generate
.PHONY: generate
generate: ## Generate CRD manifests with controller-gen.
	controller-gen crd paths=./api/... output:crd:dir=deploy/base

##@ Docker
.PHONY: docker-build
docker-build: ## Build the Docker image.
	docker build -t $(IMG) .

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
deploy: ## Deploy the full stack via Kustomize.
	kubectl apply -k deploy/overlays/default

.PHONY: undeploy
undeploy: ## Remove the full stack.
	kubectl delete -k deploy/overlays/default

##@ E2E Testing

.PHONY: e2e
e2e: ## Run end-to-end tests using kind (creates/destroys cluster).
	./test/e2e/run.sh

.PHONY: e2e-no-cleanup
e2e-no-cleanup: ## Run e2e tests but keep the kind cluster for debugging.
	./test/e2e/run.sh --no-cleanup
