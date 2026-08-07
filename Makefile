# pgtoolbox build entry points.
#

# Image URL to use for all building/pushing image targets
IMG ?= pgtoolbox:latest
PROXY_IMG ?= pgtoolbox-proxy:latest
VERSION ?= development
OLM_VERSION ?= 0.1.0
BUNDLE_IMG ?= pgtoolbox-bundle:v$(OLM_VERSION)
CATALOG_IMG ?= pgtoolbox-catalog:v$(OLM_VERSION)
GO_LDFLAGS ?= -X main.operatorVersion=$(VERSION) -X main.defaultOperatorImage=$(IMG)

CONTROLLER_GEN_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION ?= v2.6.2

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: all
all: build

##@ Development

.PHONY: manifests
manifests: ## Generate CRDs into config/crd/bases.
	$(CONTROLLER_GEN) crd paths="./api/..." \
		output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: ## Generate deepcopy code.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths="./api/..."

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint.
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

.PHONY: test
test: manifests generate fmt vet ## Run unit tests.
	go test ./... -coverprofile cover.out

.PHONY: helm-lint
helm-lint: ## Lint and template-render the Helm chart.
	helm lint deploy/helm/pgtoolbox
	helm template pgtoolbox deploy/helm/pgtoolbox --namespace pgtoolbox > /dev/null

.PHONY: docs
docs: ## Build the documentation site.
	cd web && npm ci && npm run typecheck && npm run build

##@ Build

.PHONY: build
build: generate fmt vet ## Build the manager binary.
	go build -ldflags "$(GO_LDFLAGS)" -o bin/manager ./cmd/manager

.PHONY: run
run: manifests generate fmt vet ## Run the operator against the current kubeconfig context.
	go run -ldflags "$(GO_LDFLAGS)" ./cmd/manager

.PHONY: docker-build
docker-build: ## Build the operator container image.
	docker build --build-arg VERSION=$(VERSION) --build-arg IMG=$(IMG) \
		-f Dockerfile --target manager -t $(IMG) ..

.PHONY: docker-build-proxy
docker-build-proxy: ## Build the pgtoolbox-proxy container image.
	docker build --build-arg VERSION=$(VERSION) --build-arg IMG=$(IMG) \
		-f Dockerfile --target proxy -t $(PROXY_IMG) ..

.PHONY: docker-build-all
docker-build-all: docker-build docker-build-proxy ## Build all container images.

.PHONY: clean
clean: ## Remove build outputs and binaries (bin/, manager, proxy, dist/, coverage, docs build).
	rm -rf $(LOCALBIN) manager proxy dist cover.out web/build web/.docusaurus

##@ Deployment

.PHONY: install
install: manifests ## Install CRDs into the current cluster.
	kubectl apply -k config/crd

.PHONY: uninstall
uninstall: ## Remove CRDs from the current cluster.
	kubectl delete -k config/crd

.PHONY: deploy
deploy: manifests ## Deploy the operator (CRDs, RBAC, manager) into the pgtoolbox namespace.
	kubectl apply -k config/default

.PHONY: undeploy
undeploy: ## Remove the operator deployment, keeping the CRDs.
	kubectl delete -k config/default

##@ OLM

.PHONY: bundle-build
bundle-build: ## Build the OLM bundle image.
	docker build -f deploy/olm/bundle.Dockerfile -t $(BUNDLE_IMG) deploy/olm/bundle

.PHONY: catalog-build
catalog-build: ## Build the OLM catalog index image.
	docker build -f deploy/olm/catalog.Dockerfile -t $(CATALOG_IMG) deploy/olm/catalog

.PHONY: helm-package
helm-package: ## Package the Helm chart into dist/.
	mkdir -p dist
	helm package deploy/helm/pgtoolbox -d dist

##@ Help

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  %-15s %s\n", $$1, $$2 } /^##@/ { printf "\n%s\n", substr($$0, 5) }' $(MAKEFILE_LIST)
