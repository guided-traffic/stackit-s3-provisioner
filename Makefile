# Image URL to use for building/pushing image targets
IMG ?= guidedtraffic/stackit-s3-provisioner:latest

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.33.0

# Go commands
GOCMD = go
GOTEST = $(GOCMD) test
GOFMT = gofmt

# Coverage directory
COVERAGE_DIR = coverage

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands like 'source' to be used
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	@echo "Formatting code..."
	$(GOFMT) -s -w .

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: golangci-lint ## Run linting.
	@echo "Running static analysis..."
	go vet ./...
	$(GOFMT) -l .
	$(GOLANGCI_LINT) run --timeout=5m

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes.
	$(GOLANGCI_LINT) run --fix

.PHONY: cyclo
cyclo: ## Run cyclomatic complexity analysis.
	@echo "Running cyclomatic complexity analysis (threshold: $(CYCLO_THRESHOLD))..."
	@which gocyclo > /dev/null || (echo "Installing gocyclo..." && go install github.com/fzipp/gocyclo/cmd/gocyclo@$(GOCYCLO_VERSION))
	@gocyclo -over $(CYCLO_THRESHOLD) -ignore "_test.go|zz_generated" . && echo "✅ All functions are below complexity threshold $(CYCLO_THRESHOLD)" || (echo "❌ Functions above complexity threshold $(CYCLO_THRESHOLD) found!" && gocyclo -over $(CYCLO_THRESHOLD) -ignore "_test.go|zz_generated" . && exit 1)

.PHONY: cyclo-report
cyclo-report: ## Show full cyclomatic complexity report (including tests).
	@which gocyclo > /dev/null || (echo "Installing gocyclo..." && go install github.com/fzipp/gocyclo/cmd/gocyclo@$(GOCYCLO_VERSION))
	@gocyclo -top 20 .

.PHONY: test
test: fmt vet envtest ## Run all tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -coverprofile cover.out

.PHONY: test-unit
test-unit: ## Run unit tests only.
	@echo "Running unit tests..."
	$(GOTEST) -v -short ./...

.PHONY: test-unit-coverage
test-unit-coverage: ## Run unit tests with coverage.
	@echo "Running unit tests with coverage..."
	@mkdir -p $(COVERAGE_DIR)
	$(GOTEST) -v -short -coverprofile=$(COVERAGE_DIR)/unit.out -covermode=atomic ./...

.PHONY: test-integration
test-integration: envtest ## Run integration tests (envtest) only.
	@echo "Running integration tests..."
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" $(GOTEST) -v -tags=integration -count=1 -timeout=20m ./test/integration/...

.PHONY: test-integration-coverage
test-integration-coverage: envtest ## Run integration tests (envtest) with coverage.
	@echo "Running integration tests with coverage..."
	@mkdir -p $(COVERAGE_DIR)
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" $(GOTEST) -v -tags=integration -count=1 -timeout=20m -coverprofile=$(COVERAGE_DIR)/integration.out -covermode=atomic -coverpkg=./... ./test/integration/...

.PHONY: test-e2e
test-e2e: ## Run E2E smoke tests against a running Kind cluster.
	@echo "Running E2E tests..."
	$(GOTEST) -v -tags=e2e -count=1 -timeout=20m ./test/e2e/...

##@ Security

.PHONY: gosec
gosec: ## Run gosec security scan.
	@echo "Running gosec security scan..."
	@which gosec > /dev/null || (echo "Installing gosec..." && go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION))
	GOFLAGS="-buildvcs=false" gosec ./...

.PHONY: vuln
vuln: ## Check for vulnerabilities.
	@echo "Checking for vulnerabilities..."
	@which govulncheck > /dev/null || (echo "Installing govulncheck..." && go install golang.org/x/vuln/cmd/govulncheck@latest)
	GOFLAGS="-buildvcs=false" govulncheck ./...

##@ Code Generation

HELM_CHART_DIR = deploy/helm/stackit-s3-provisioner
CRD_HELM_TARGET = $(HELM_CHART_DIR)/templates/crd.yaml

.PHONY: sync-helm-crd
sync-helm-crd: ## Sync generated CRDs into Helm chart (single source of truth: config/crd/bases/).
	@echo "Syncing CRDs to Helm chart..."
	@echo "# Generated - do not edit manually. Use 'make sync-helm-crd'." > $(CRD_HELM_TARGET)
	@for crd in config/crd/bases/*.yaml; do \
		if [ -f "$$crd" ]; then \
			echo "---" >> $(CRD_HELM_TARGET); \
			cat "$$crd" >> $(CRD_HELM_TARGET); \
		fi; \
	done
	@echo "CRDs synced to $(CRD_HELM_TARGET)"

##@ Build

.PHONY: build
build: fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: fmt vet ## Run a controller from your host.
	go run ./cmd/main.go --zap-log-level=debug

.PHONY: generate-all
generate-all: manifests generate sync-helm-crd ## Regenerate CRD, DeepCopy code, and sync Helm chart. Run this after any api/v1/ type change.

.PHONY: docker-build
docker-build: generate-all ## Build docker image with the manager (regenerates CRD and DeepCopy first).
	docker build -f Containerfile -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push ${IMG}

.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for cross-platform support.
	- docker buildx create --name project-builder
	docker buildx use project-builder
	docker buildx build -f Containerfile --push --platform=linux/amd64,linux/arm64 --tag ${IMG} .
	docker buildx rm project-builder

##@ Local cluster (Kind)

KIND_CLUSTER ?= stackit-s3-provisioner-test
E2E_IMG ?= stackit-s3-provisioner:test

.PHONY: kind-create
kind-create: ## Create a Kind cluster for local testing.
	@echo "Creating Kind cluster..."
	@printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nname: %s\nnodes:\n- role: control-plane\n' "$(KIND_CLUSTER)" > /tmp/kind-config.yaml
	kind create cluster --config /tmp/kind-config.yaml --wait 120s

.PHONY: kind-delete
kind-delete: ## Delete the Kind test cluster.
	@echo "Deleting Kind cluster..."
	-kind delete cluster --name $(KIND_CLUSTER)

.PHONY: e2e-local
e2e-local: kind-create ## Run full E2E test locally with Kind.
	@echo "Building E2E image..."
	docker build -f Containerfile -t $(E2E_IMG) .
	@echo "Loading E2E image into Kind cluster..."
	kind load docker-image $(E2E_IMG) --name $(KIND_CLUSTER)
	@echo "Installing operator via Helm..."
	helm install stackit-s3-provisioner $(HELM_CHART_DIR) \
		--namespace stackit-s3-provisioner-system \
		--create-namespace \
		--values test/e2e/helm-values.yaml \
		--wait \
		--timeout 120s
	@echo "Running E2E tests..."
	$(MAKE) test-e2e
	@echo "Cleaning up..."
	$(MAKE) kind-delete

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

## Tool Versions
# renovate: datasource=go depName=sigs.k8s.io/kustomize/kustomize/v5
KUSTOMIZE_VERSION ?= v5.8.1
# renovate: datasource=go depName=sigs.k8s.io/controller-tools/cmd/controller-gen
CONTROLLER_GEN_VERSION ?= v0.21.0
# renovate: datasource=go depName=sigs.k8s.io/controller-runtime/tools/setup-envtest
ENVTEST_VERSION ?= release-0.24
# renovate: datasource=go depName=github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2
# renovate: datasource=go depName=github.com/fzipp/gocyclo/cmd/gocyclo
GOCYCLO_VERSION ?= v0.6.0
# renovate: datasource=go depName=github.com/securego/gosec/v2/cmd/gosec
GOSEC_VERSION ?= v2.22.9

# Cyclomatic complexity threshold (recommended: 10-15)
CYCLO_THRESHOLD ?= 15

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_GEN_VERSION))

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: manifests
manifests: controller-gen ## Generate ClusterRole and CustomResourceDefinition objects, then sync into the Helm chart.
	$(CONTROLLER_GEN) rbac:roleName=stackit-s3-provisioner-role crd paths="./..." output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac
	$(MAKE) sync-helm-crd

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and target path
define go-install-tool
@[ -f $(1) ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
}
endef
