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
cyclo: gocyclo ## Run cyclomatic complexity analysis.
	@echo "Running cyclomatic complexity analysis (threshold: $(CYCLO_THRESHOLD))..."
	@$(GOCYCLO) -over $(CYCLO_THRESHOLD) -ignore "_test.go|zz_generated" . && echo "✅ All functions are below complexity threshold $(CYCLO_THRESHOLD)" || (echo "❌ Functions above complexity threshold $(CYCLO_THRESHOLD) found!" && $(GOCYCLO) -over $(CYCLO_THRESHOLD) -ignore "_test.go|zz_generated" . && exit 1)

.PHONY: cyclo-report
cyclo-report: gocyclo ## Show full cyclomatic complexity report (including tests).
	@$(GOCYCLO) -top 20 .

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
	$(GOTEST) -v -short -coverprofile=$(COVERAGE_DIR)/unit.out -covermode=atomic -coverpkg=./... ./...

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

.PHONY: test-e2e-stackit
test-e2e-stackit: ## Run E2E tests incl. the cloud suite against a running Kind cluster (operator needs a real SA key).
	@echo "Running E2E tests (cloud suite enabled)..."
	E2E_STACKIT=1 $(GOTEST) -v -tags=e2e -count=1 -timeout=40m ./test/e2e/...

.PHONY: test-helm-render
test-helm-render: ## Render the Helm chart and assert on the user-facing RBAC objects (needs helm on PATH).
	@echo "Running Helm render tests..."
	$(GOTEST) -v -tags=helm -count=1 ./test/helm/...

##@ Release tooling

# npm install, not npm ci: there is no lockfile, and the Semantic Release job
# resolves package.json the same way, so this sees what a release would load.
.PHONY: release-notes-check
release-notes-check: ## Render release notes with the installed semantic-release plugins (needs node and npm).
	@echo "Checking the release tooling..."
	npm install --no-audit --no-fund --no-package-lock
	npm audit signatures
	node hack/release-notes-check.mjs

##@ Security

# gosec is built from its own module in hack/tools rather than installed at a
# release tag: its golang.org/x/tools must be able to read the export data of
# the Go toolchain in use, and a gosec release can lag a Go release. The tool
# module pins gosec and lets Renovate move golang.org/x/tools together with the
# Go version. Rebuilt on every run, so a stale binary on PATH is never used.
.PHONY: gosec
gosec: ## Run gosec security scan.
	@echo "Running gosec security scan..."
	go -C hack/tools build -o $(GOSEC) github.com/securego/gosec/v2/cmd/gosec
	GOFLAGS="-buildvcs=false" $(GOSEC) ./...

.PHONY: vuln
vuln: govulncheck ## Check for vulnerabilities.
	@echo "Checking for vulnerabilities..."
	GOFLAGS="-buildvcs=false" $(GOVULNCHECK) ./...

##@ Code Generation

HELM_CHART_DIR = deploy/helm/stackit-s3-provisioner
CRD_HELM_TARGET = $(HELM_CHART_DIR)/templates/crd.yaml

.PHONY: sync-helm-crd
sync-helm-crd: ## Sync generated CRDs into Helm chart (single source of truth: config/crd/bases/).
	@echo "Syncing CRDs to Helm chart..."
	@echo "# Generated - do not edit manually. Use 'make sync-helm-crd'." > $(CRD_HELM_TARGET)
	@echo '{{- if .Values.crds.install }}' >> $(CRD_HELM_TARGET)
	@for crd in config/crd/bases/*.yaml; do \
		if [ -f "$$crd" ]; then \
			echo "---" >> $(CRD_HELM_TARGET); \
			awk '{print} /^  annotations:$$/{print "    helm.sh/resource-policy: keep"}' "$$crd" >> $(CRD_HELM_TARGET); \
		fi; \
	done
	@echo '{{- end }}' >> $(CRD_HELM_TARGET)
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
	DOCKER_BUILDKIT=1 docker build -f Containerfile -t ${IMG} .

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

# SA_KEY is the STACKIT service-account key handed to the operator by e2e-stackit.
# E2E_BUCKET_PREFIX must match bucketNaming.prefix in test/e2e/helm-values-stackit.yaml:
# it is what the cleanup sweep uses to tell this run's cloud resources from real ones.
SA_KEY ?= account-1.json
E2E_BUCKET_PREFIX ?= s3e2e

# The rclone image the clone tests need inside the Kind node. It is READ OUT of
# the rendered chart rather than pinned here a third time: the tag already lives
# in the chart values (Renovate-managed) and in DefaultCloneImage, and a stale
# copy in the Makefile would preload an image the operator never asks for. The
# image is preloaded so the clone Job never waits on a registry pull mid-test.
E2E_CLONE_IMAGE = $(shell helm template e2e $(HELM_CHART_DIR) --values test/e2e/helm-values-stackit.yaml \
	| grep -m1 -- '--clone-image=' | sed 's/.*--clone-image=//')

.PHONY: e2e-stackit
e2e-stackit: kind-create ## Run E2E against the REAL StackIT API (creates and deletes real buckets/keys; needs $(SA_KEY)).
	@test -f $(SA_KEY) || { echo "ERROR: $(SA_KEY) not found — the cloud E2E run needs a StackIT service-account key"; exit 1; }
	@echo "Building E2E image..."
	docker build -f Containerfile -t $(E2E_IMG) .
	@echo "Loading E2E image into Kind cluster..."
	kind load docker-image $(E2E_IMG) --name $(KIND_CLUSTER)
	@echo "Preloading clone image $(E2E_CLONE_IMAGE) into Kind cluster..."
	docker pull $(E2E_CLONE_IMAGE)
	kind load docker-image $(E2E_CLONE_IMAGE) --name $(KIND_CLUSTER)
	@echo "Creating service-account key Secret..."
	kubectl create namespace stackit-s3-provisioner-system --dry-run=client -o yaml | kubectl apply -f -
	kubectl create secret generic stackit-sa-key \
		--namespace stackit-s3-provisioner-system \
		--from-file=sa-key.json=$(SA_KEY) \
		--dry-run=client -o yaml | kubectl apply -f -
	@echo "Installing operator via Helm (real SA key)..."
	helm install stackit-s3-provisioner $(HELM_CHART_DIR) \
		--namespace stackit-s3-provisioner-system \
		--values test/e2e/helm-values-stackit.yaml \
		--wait \
		--timeout 180s
	@echo "Running E2E tests (cloud suite)..."
	-$(MAKE) test-e2e-stackit
	@echo "Releasing any Bucket left behind (finalizer teardown, cluster still up)..."
	-kubectl delete buckets.stackit-bucket.gtrfc.com --all --all-namespaces --timeout=5m
	@echo "Deleting Kind cluster..."
	$(MAKE) kind-delete
	@echo "Sweeping the project for leftovers..."
	$(MAKE) e2e-stackit-sweep
	@echo "E2E (cloud) done. Re-run the test target above to see failures in isolation."

.PHONY: e2e-stackit-sweep
e2e-stackit-sweep: ## Delete cloud resources a crashed e2e-stackit run left behind (incl. the orphaned admin key).
	go run ./hack/e2ecleanup -key $(SA_KEY) -prefix $(E2E_BUCKET_PREFIX) -delete -admin

.PHONY: e2e-stackit-sweep-dry
e2e-stackit-sweep-dry: ## Report, without deleting, what e2e-stackit-sweep would remove.
	go run ./hack/e2ecleanup -key $(SA_KEY) -prefix $(E2E_BUCKET_PREFIX)

.PHONY: e2e-local
e2e-local: kind-create ## Run full E2E test locally with Kind (skeleton mode, no cloud calls).
	@echo "Building E2E image..."
	docker build -f Containerfile -t $(E2E_IMG) .
	@echo "Loading E2E image into Kind cluster..."
	kind load docker-image $(E2E_IMG) --name $(KIND_CLUSTER)
	@echo "Render-checking Helm chart..."
	$(MAKE) test-helm-render
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
# Each installed binary carries its pinned version and the Go toolchain that built it in its file
# name. A bump of either is then a missing file that make installs, and a binary left over from an
# older pin is never run. gosec needs none of this: it is rebuilt from hack/tools/go.mod on every run.
TOOLS_GO_VERSION := $(shell go env GOVERSION)
KUSTOMIZE ?= $(LOCALBIN)/kustomize-$(KUSTOMIZE_VERSION)-$(TOOLS_GO_VERSION)
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_GEN_VERSION)-$(TOOLS_GO_VERSION)
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)-$(TOOLS_GO_VERSION)
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)-$(TOOLS_GO_VERSION)
GOCYCLO ?= $(LOCALBIN)/gocyclo-$(GOCYCLO_VERSION)-$(TOOLS_GO_VERSION)
GOVULNCHECK ?= $(LOCALBIN)/govulncheck-$(GOVULNCHECK_VERSION)-$(TOOLS_GO_VERSION)
GOSEC ?= $(LOCALBIN)/gosec

## Tool Versions
# renovate: datasource=go depName=sigs.k8s.io/kustomize/kustomize/v5
KUSTOMIZE_VERSION ?= v5.8.3
# renovate: datasource=go depName=sigs.k8s.io/controller-tools/cmd/controller-gen
CONTROLLER_GEN_VERSION ?= v0.23.0
# renovate: datasource=go depName=sigs.k8s.io/controller-runtime/tools/setup-envtest
ENVTEST_VERSION ?= release-0.24
# renovate: datasource=go depName=github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.15.0
# renovate: datasource=go depName=github.com/fzipp/gocyclo/cmd/gocyclo
GOCYCLO_VERSION ?= v0.6.0
# renovate: datasource=go depName=golang.org/x/vuln/cmd/govulncheck
GOVULNCHECK_VERSION ?= v1.9.0

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

.PHONY: gocyclo
gocyclo: $(GOCYCLO) ## Download gocyclo locally if necessary.
$(GOCYCLO): $(LOCALBIN)
	$(call go-install-tool,$(GOCYCLO),github.com/fzipp/gocyclo/cmd/gocyclo,$(GOCYCLO_VERSION))

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Download govulncheck locally if necessary.
$(GOVULNCHECK): $(LOCALBIN)
	$(call go-install-tool,$(GOVULNCHECK),golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))

# go-install-tool installs package $(2) at version $(3) as the binary $(1). go install names the
# binary after the package, so it installs into a scratch GOBIN and the binary is moved to $(1).
define go-install-tool
@[ -f $(1) ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
gobin=$$(mktemp -d) ;\
GOBIN=$$gobin go install $${package} ;\
mv $$gobin/* $(1) ;\
rm -rf $$gobin ;\
}
endef
