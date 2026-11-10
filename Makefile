# pgToolBox: the one Makefile. `make check` runs what CI runs without Docker or
# kind, so a green local run is a green pull request. Shared pieces
# (help, doctor, tool resolution against mise.toml, the fuzz pattern) live in
# hack/common.mk.

include hack/common.mk
-include hack/mk/*.mk

# ---------------------------------------------------------------- variables

VERSION ?= development
# Local image names; published as ghcr.io/fyannk/<name>.
OPERATOR_IMG ?= pgtoolbox-operator:dev
CONSOLE_IMG ?= pgtoolbox-console:dev
EVIDENCE_IMG ?= pgtoolbox-evidence-agent:dev
# The chart is the one place the product version is written by hand; the bundle
# and catalog tags follow it (`make validate-packaging` compares the rest).
OLM_VERSION ?= $(shell sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\} *$$/\1/p' charts/pgtoolbox/Chart.yaml)
BUNDLE_IMG ?= pgtoolbox-bundle:$(OLM_VERSION)
CATALOG_IMG ?= pgtoolbox-catalog:$(OLM_VERSION)
PLATFORMS ?= linux/amd64,linux/arm64

OPERATOR_LDFLAGS = -X main.operatorVersion=$(VERSION) -X main.defaultOperatorImage=$(OPERATOR_IMG)
VERSION_LDFLAGS = -X main.version=$(VERSION)

# Tools: the binary on PATH (mise), else the pinned version through `go run`.
GOLANGCI_LINT_MODULE = go:github.com/golangci/golangci-lint/v2/cmd/golangci-lint
GOLANGCI_LINT ?= $(call tool,golangci-lint,go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(call mise_v,$(GOLANGCI_LINT_MODULE)))
GOVULNCHECK ?= $(call tool,govulncheck,go run golang.org/x/vuln/cmd/govulncheck@v$(call mise_v,go:golang.org/x/vuln/cmd/govulncheck))
CONTROLLER_GEN ?= $(call tool,controller-gen,go run sigs.k8s.io/controller-tools/cmd/controller-gen@v$(call mise_v,go:sigs.k8s.io/controller-tools/cmd/controller-gen))
SETUP_ENVTEST ?= $(call tool,setup-envtest,go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v$(call mise_v,go:sigs.k8s.io/controller-runtime/tools/setup-envtest))

# envtest control planes. The API CEL rules must pass on the oldest Kubernetes
# the OLM bundle declares (minKubeVersion) and the charts' kubeVersion; the
# console's kube layer is exercised on the newest the e2e uses.
ENVTEST_K8S_VERSION_API ?= 1.34.1
ENVTEST_K8S_VERSION_CONSOLE ?= 1.34.1

# Go modules in this tree: the root and the nested evidence module (go.work).
EVIDENCE_MODULE = pkg/evidence

# ------------------------------------------------------------------ develop

##@ Development

.PHONY: manifests
manifests: ## Generate CRDs and the manager ClusterRole, then sync the packaging.
	$(CONTROLLER_GEN) crd paths="./api/v1alpha1/..." output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths="./internal/operator/..." output:rbac:artifacts:config=config/rbac
	./hack/sync-manifests.sh

.PHONY: verify-manifests
verify-manifests: manifests ## Fail when the checked-in manifests are stale.
	@git diff --exit-code -- config deploy charts || { \
		echo "generated manifests are stale; run 'make manifests' and commit the result"; exit 1; }

# Every file a generator owns, regenerated, then the tree compared with the
# commit: a stale or hand-edited generated file, or a new one nobody committed,
# fails here. Run it on a clean tree; it reports any local change as drift.
.PHONY: verify-generated
verify-generated: generate manifests openapi catalog-docs crd-ref-docs insight-pack ## Regenerate deepcopy code, manifests, OpenAPI, check and API references; fail on any difference.
	@test -z "$$(git status --porcelain)" || { \
		git status --short; git diff --stat; \
		echo "generated files are stale or the tree is dirty; run 'make generate manifests openapi catalog-docs crd-ref-docs insight-pack' and commit the result"; exit 1; }

.PHONY: generate
generate: ## Generate deepcopy code.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate/boilerplate.go.txt paths="./api/..."

.PHONY: generate-evidence-artifacts
generate-evidence-artifacts: ## Regenerate the evidence wire schemas and goldens (pkg/evidence v1alpha1).
	cd $(EVIDENCE_MODULE) && go run ./cmd/evidence-artifacts

.PHONY: fmt
fmt: ## Format Go sources (gofumpt + gci, as configured in .golangci.yml).
	$(GOLANGCI_LINT) fmt ./...
	cd $(EVIDENCE_MODULE) && $(GOLANGCI_LINT) fmt --config ../../.golangci.yml ./...

.PHONY: fmt-check
fmt-check: ## Fail when a Go source is not gofumpt/gci-clean.
	$(GOLANGCI_LINT) fmt --diff ./... || { echo "run 'make fmt'"; exit 1; }
	cd $(EVIDENCE_MODULE) && $(GOLANGCI_LINT) fmt --diff --config ../../.golangci.yml ./... || { echo "run 'make fmt'"; exit 1; }

.PHONY: vet
vet: ## Run go vet on every module.
	go vet ./...
	cd $(EVIDENCE_MODULE) && go vet ./...

##@ Build

.PHONY: build
build: ## Build every binary into bin/.
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(OPERATOR_LDFLAGS)" -o bin/operator ./cmd/operator
	CGO_ENABLED=0 go build -trimpath -ldflags "$(VERSION_LDFLAGS)" -o bin/console ./cmd/console
	CGO_ENABLED=0 go build -trimpath -ldflags "$(VERSION_LDFLAGS)" -o bin/evidence-agent ./cmd/evidence-agent
	cd $(EVIDENCE_MODULE) && go build ./...

.PHONY: clean
clean: ## Remove build outputs.
	rm -rf bin dist artifacts cover.out docs/site/build docs/site/.docusaurus

# ------------------------------------------------------------------- test

##@ Test

.PHONY: test
test: ## Unit tests of every module.
	go test ./... -coverprofile cover.out
	cd $(EVIDENCE_MODULE) && go test ./...

.PHONY: test-race
# hack/ holds build-time tools; their tests run in `test`. readonlycheck loads
# packages concurrently through golang.org/x/tools/go/packages, and the race
# detector reports a race inside go/types itself (Named.unpack), not in our code.
test-race: ## Unit tests with the race detector.
	go test -race $$(go list ./... | grep -v '/hack/')
	cd $(EVIDENCE_MODULE) && go test -race ./...

# The parsers on the trust boundary, each fuzzed against the invariant it owns
# rather than against "does not panic": a barman destination must round-trip
# or be refused; a credential must reach libpq's service and password files
# exactly or not at all; the level parser stays closed; the redaction boundary
# emits a category and never a cause; every evidence format parser returns a
# typed error rather than a panic.
.PHONY: test-fuzz
test-fuzz: ## Fuzz the trust-boundary parsers (FUZZ_TIME per target).
	$(call fuzz,./internal/backupresolve,FuzzParseDestination)
	$(call fuzz,./internal/backupresolve,FuzzPgBackRestEndpoint)
	$(call fuzz,./internal/operator/adminsync,FuzzServiceFile)
	$(call fuzz,./internal/operator/adminsync,FuzzPgpassLine)
	$(call fuzz,./internal/operator/pgadmin/offer,FuzzParseOffer)
	$(call fuzz,./internal/operator/pgadmin/declared,FuzzParseDeclared)
	$(call fuzz,./internal/operator/configuration,FuzzLoadEnvironment)
	$(call fuzz,./internal/console/mcpoauth,FuzzRegistration)
	$(call fuzz,./pkg/redact,FuzzSafe)
	$(call fuzz,./pkg/redact,FuzzPayload)
	$(call fuzz,./pkg/cnpgstatus/insight,FuzzParse)
	$(call fuzz,./pkg/cnpgstatus/pgconf,FuzzParse)
	$(call fuzz,./pkg/cnpgstatus/scrape,FuzzParseCatalog)
	$(call fuzz,./pkg/cnpgstatus/scrape,FuzzParseLabels)
	$(call fuzz,./pkg/cnpgstatus/logstream,FuzzMatcherObserve)
	$(call fuzz,./internal/evidence/formats/barmancloud,FuzzParseBackupInfo)
	$(call fuzz,./internal/evidence/formats/barmancloud,FuzzParseTimelineHistory)
	$(call fuzz,./internal/evidence/formats/barmancloud,FuzzParseWALName)
	$(call fuzz,./internal/evidence/formats/pgbackrest,FuzzParseINI)
	$(call fuzz,./internal/evidence/formats/pgbackrest,FuzzDecrypt)
	$(call fuzz,./internal/evidence/formats/pgbackrest,FuzzParseWALName)
	$(call fuzz,./internal/evidence/formats/pgbackrest,FuzzParseHistory)
	$(call fuzz,./internal/evidence/formats/pgbackrest,FuzzParseLSN)
	$(call fuzz,./internal/evidence/formats/timeline,FuzzLatest)
	$(call fuzz,./internal/evidence/provider/cursor,FuzzCodec)
	$(call fuzz,./internal/evidence/evidenceapi,FuzzBackupInfoProjection)
	$(call fuzz,./internal/evidence/config,FuzzCheckConnectionString)
	cd $(EVIDENCE_MODULE) && $(call fuzz,./v1alpha1,FuzzParseS3Destination)
	cd $(EVIDENCE_MODULE) && $(call fuzz,./v1alpha1,FuzzParseAzureDestination)
	cd $(EVIDENCE_MODULE) && $(call fuzz,./v1alpha1,FuzzParseGCSDestination)

# Tests that need a real API server (CEL rules, defaulting) on the oldest
# supported control plane.
.PHONY: test-envtest
test-envtest: manifests ## API-server-backed tests of the CRDs (CEL, defaulting, pruning) on envtest.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_API) -p path)" \
		go test ./api/... -run 'CEL|Schema' -count=1 -v

.PHONY: test-envtest-evidenceagent
test-envtest-evidenceagent: manifests ## The EvidenceAgent reconciler on envtest, as the generated manager ClusterRole (SSA, RBAC escalation, status).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_API) -p path)" \
		go test ./internal/operator/evidenceagent/ -run Envtest -count=1 -v

.PHONY: test-envtest-admissionpolicy
test-envtest-admissionpolicy: ## The operator chart's ValidatingAdmissionPolicy (helm template) evaluated on envtest as different users (needs helm; Kubernetes 1.30+ policies).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_CONSOLE) -p path)" \
		go test ./internal/operator/admissionpolicy/ -run Envtest -count=1 -v

.PHONY: test-envtest-pgtoolboxconsole
test-envtest-pgtoolboxconsole: manifests ## The PgToolBoxConsole reconciler on envtest, as the generated manager ClusterRole (bind-only console roles, lifecycle keeper).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_API) -p path)" \
		go test ./internal/operator/console/ -run Envtest -count=1 -v

.PHONY: test-fuzz-evidence-agent
test-fuzz-evidence-agent: ## Fuzz the evidence agent channel parsers (bound-token and shared-token headers, agent handler).
	$(call fuzz,./internal/evidence/evidenceapi,FuzzAgentHandlerRequest)
	cd $(EVIDENCE_MODULE) && $(call fuzz,./v1alpha1,FuzzParseAuthorization)
	cd $(EVIDENCE_MODULE) && $(call fuzz,./v1alpha1,FuzzParseSharedAuthorization)

.PHONY: test-e2e-evidenceagent
test-e2e-evidenceagent: ## The EvidenceAgent scenario on kind (agent published, bound-token client, refusals). Docker + kind.
	./test/e2e/evidenceagent.sh

.PHONY: test-e2e-team
test-e2e-team: ## Team mode on kind: charts/pgtools installed by a namespace admin, no operator, a real CloudNativePG + barman-cloud + MinIO (console, evidence agent, pgAdmin). Docker + kind.
	./test/e2e/team/run.sh

.PHONY: test-e2e-pgbackrest
test-e2e-pgbackrest: ## pgBackRest on kind, one cell per plugin (Dalibo v0.0.3, Opera v0.9.0), MinIO over TLS, both agent modes, the console, a restore drill and the broken cases. Heavy tier. Docker + kind.
	./test/e2e/pgbackrest/run.sh

.PHONY: pentest
pentest: ## Live-fire attack scripts (test/pentest) against a running stack; set KUBECONFIG. Does not provision a cluster.
	./test/pentest/run.sh

.PHONY: test-stress
test-stress: ## The evidence agent's concurrency tests, repeated.
	go test -race ./internal/evidence/evidenceapi -run '^(TestEngine|TestNewEngine|TestAgentHandler)' -count=10
	go test -race ./internal/evidence/inventory -run '^(TestScanner)' -count=10
	go test -race ./internal/evidence/application ./internal/evidence/config -run '^(TestAgent|TestLoad)' -count=10

.PHONY: test-evidence-integration
test-evidence-integration: ## Barman and pgBackRest fixtures, S3/Azure/GCS provider parity and the pgBackRest provider journeys (Docker).
	./hack/evidence/test-barman-fixtures.sh
	./hack/evidence/test-pgbackrest-fixtures.sh
	./hack/evidence/test-provider-parity.sh

.PHONY: test-pgbackrest-providers
test-pgbackrest-providers: ## Real pgBackRest 2.60.0 writes to MinIO and Azurite over TLS; the evidence reader reads it back (Docker).
	./hack/evidence/test-pgbackrest-providers.sh

.PHONY: test-pgbackrest-fixtures
test-pgbackrest-fixtures: ## Regenerate the pgBackRest 2.60.0 fixtures in Docker and compare their shape with the committed ones.
	./hack/evidence/test-pgbackrest-fixtures.sh

.PHONY: pgbackrest-fixtures
pgbackrest-fixtures: ## Regenerate the pgBackRest 2.60.0 fixtures in Docker and replace the committed ones (then go test -update).
	./hack/evidence/test-pgbackrest-fixtures.sh --update

.PHONY: test-container
test-container: docker-build-console docker-build-evidence ## Restricted-runtime profile of the console and evidence images (Docker).
	./hack/console/test-container.sh $(CONSOLE_IMG)
	./hack/evidence/test-container.sh $(EVIDENCE_IMG)
	./hack/evidence/test-agent-container.sh $(EVIDENCE_IMG)

.PHONY: test-multiarch
test-multiarch: ## Every image builds for PLATFORMS (amd64+arm64), without loading or pushing.
	for target in operator console evidence-agent; do \
		docker buildx build --platform $(PLATFORMS) --build-arg VERSION=$(VERSION) --target $$target . || exit 1; \
	done

.PHONY: test-e2e-console
test-e2e-console: docker-build-console ## Console-only e2e on kind: the binary boots against pinned CNPG, token sign-in, SAR filter.
	IMAGE=$(CONSOLE_IMG) ./hack/console/test-e2e.sh

# ----------------------------------------------------------------- quality

##@ Quality

# Every build tag the tree gates code behind (grep -rh '^//go:build' --include=*.go),
# so that `make lint` type-checks and lints the e2e, envtest and integration code
# too: it cannot rot unseen between runs of the suites that need a cluster.
LINT_TAGS = e2e,e2echart,e2econsole,e2epgbackrest,envtest,integration,scale,catalogpins

.PHONY: lint
lint: ## golangci-lint on every module, build-tagged e2e/envtest/integration code included, plus the repository checks.
	$(GOLANGCI_LINT) run --build-tags $(LINT_TAGS) --timeout 10m ./...
	cd $(EVIDENCE_MODULE) && $(GOLANGCI_LINT) run --config ../../.golangci.yml --timeout 10m ./...
	$(MAKE) --no-print-directory fmt-check vet check-repo

.PHONY: check-repo
check-repo: ## Repository invariants: headers, read-only boundary, dependency closure, layering, toolchain, documented settings.
	./hack/check-boilerplate.sh
	./hack/check-readonly.sh
	./hack/check-deps.sh
	./hack/check-layering.sh
	./hack/check-toolchain.sh
	./hack/check-docs-env.sh

.PHONY: vuln
vuln: ## Scan the Go dependency graph for known vulnerabilities.
	$(GOVULNCHECK) ./...
	cd $(EVIDENCE_MODULE) && $(GOVULNCHECK) ./...

# govulncheck reads the Go tree; the npm trees are the other dependency
# surface that ships. They resolve from the lockfile alone, so this installs
# nothing. The wrapper subtracts the advisories reviewed and accepted in
# hack/npm-audit-accepted.txt; anything unlisted still fails.
.PHONY: audit
audit: ## npm audit of the docs site.
	./hack/check-npm-audit.sh docs/site $(NPM_AUDIT_LEVEL)

# The catalog's version pins are claims about specific operator releases. This
# fetches each verified release's source through the Go module proxy and greps
# every pinned rule's strings in it, so a reworded upstream message fails here
# instead of silently never matching; the inverse runs beside it.
.PHONY: verify-pins
verify-pins: ## Verify the diagnostics catalog's CNPG pins against upstream sources.
	go test -tags catalogpins -count=1 -run 'TestPinnedStringsExistInVerifiedReleases|TestEveryUpstreamSignalIsDecided|TestCronParserMatchesTheOperator' ./pkg/diagnose/catalog/

.PHONY: catalog-docs
catalog-docs: ## Regenerate docs/site/docs/reference/checks.md from the catalog.
	UPDATE_CATALOG_DOCS=1 go test -count=1 -run TestCheckReferenceIsCurrent ./pkg/diagnose/catalog/

.PHONY: insight-pack
insight-pack: ## Regenerate deploy/insight-pack/insight-pack.yaml from the chart's pack.
	UPDATE_INSIGHT_PACK=1 go test -count=1 -run TestPlainManifestIsCurrent ./pkg/cnpgstatus/insight/

.PHONY: rules-test
rules-test: ## promtool: check the MCP alert rules and run their unit tests (hack/rules/).
	@command -v promtool >/dev/null || { echo "promtool is not on PATH (it ships in the Prometheus release archive)" >&2; exit 2; }
	promtool check rules charts/pgtools/files/alerts/mcp.rules.yaml
	promtool test rules hack/rules/mcp.rules.test.yaml

.PHONY: dashboards-check
dashboards-check: ## The Grafana dashboard JSON parses and every panel has a title and a query.
	python3 -c 'import json,sys; [(d:=json.load(open(f)), [sys.exit(f+": panel without title/target") for p in d["panels"] if p["type"]!="row" and not (p.get("title") and p.get("targets"))]) for f in sys.argv[1:]]' charts/pgtools/files/dashboards/*.json

.PHONY: helm-lint
helm-lint: ## Lint and template-render the two Helm charts.
	helm lint charts/pgtoolbox charts/pgtools
	helm template pgtoolbox charts/pgtoolbox --namespace pgtoolbox > /dev/null
	helm template pgtoolbox-console charts/pgtools --namespace team-a > /dev/null

.PHONY: validate-packaging
validate-packaging: ## Compare Helm, kustomize and OLM field by field (CRDs, RBAC, versions, images).
	./hack/validate-packaging.sh

.PHONY: test-hack
test-hack: ## Tests of the hack/ scripts (the security report generator; throwaway gpg keys).
	python3 hack/security_report_test.py
	./hack/evidence/test-verify-signing-key.sh
	./hack/lib/test-net.sh
	./hack/test-release.sh

.PHONY: lint-workflows
lint-workflows: ## actionlint and zizmor on .github (workflows and the local action).
	./hack/lint-workflows.sh

.PHONY: lint-shell
lint-shell: ## shellcheck (warnings and errors fail) on every shell script in the tree.
	@command -v shellcheck >/dev/null || { echo "shellcheck is not on PATH (mise install; see mise.toml)" >&2; exit 2; }
	git ls-files -z '*.sh' | xargs -0 shellcheck --severity=warning

.PHONY: lint-docker
lint-docker: ## hadolint on every Dockerfile (warnings and errors fail; info-level style hints do not).
	hadolint --failure-threshold warning Dockerfile deploy/olm/bundle.Dockerfile deploy/olm/catalog.Dockerfile

.PHONY: tokens
tokens: ## Test the design-tokens package.
	cd ui/tokens && npm test

.PHONY: check
check: lint test test-race test-fuzz test-envtest test-envtest-admissionpolicy test-console-core vuln audit verify-pins verify-generated validate-packaging helm-lint tokens docs lint-workflows lint-docker lint-shell ## Everything CI runs without Docker or kind.

# ----------------------------------------------------------------- images

##@ Images

# DOCKER_BUILD_EXTRA carries anything the caller adds (a layer cache, a
# platform): make docker-build-console DOCKER_BUILD_EXTRA='--platform linux/arm64'.
DOCKER_BUILD_EXTRA ?=
DOCKER_BUILD = docker buildx build --load $(DOCKER_BUILD_EXTRA) --build-arg VERSION=$(VERSION) --build-arg IMG=$(OPERATOR_IMG)
# PREBUILT_IMAGES=1: the images were built once for the whole CI run and loaded
# into the daemon as pgtoolbox-<target>:prebuilt (ci.yml's e2e-images job); the
# docker-build targets then only tag them, so every e2e scenario runs the same
# bytes and none rebuilds them. The e2e manifests pass --operator-image, so the
# IMG default baked into the operator binary is not what they run.
image_build = $(if $(PREBUILT_IMAGES),docker image inspect pgtoolbox-$(1):prebuilt >/dev/null && docker tag pgtoolbox-$(1):prebuilt $(2),$(DOCKER_BUILD) --target $(1) -t $(2) .)

.PHONY: docker-build
docker-build: ## Build the operator image for the host platform (loaded locally).
	$(call image_build,operator,$(OPERATOR_IMG))

.PHONY: docker-build-console
docker-build-console: ## Build the console image.
	$(call image_build,console,$(CONSOLE_IMG))

.PHONY: docker-build-evidence
docker-build-evidence: ## Build the evidence agent image.
	$(call image_build,evidence-agent,$(EVIDENCE_IMG))

.PHONY: docker-build-all
docker-build-all: docker-build docker-build-console docker-build-evidence ## Build every Go image for the host platform.

.PHONY: bundle-build
bundle-build: ## Build the OLM bundle image.
	docker build -f deploy/olm/bundle.Dockerfile -t $(BUNDLE_IMG) deploy/olm/bundle

.PHONY: catalog-build
catalog-build: ## Build the OLM catalog index image.
	docker build -f deploy/olm/catalog.Dockerfile -t $(CATALOG_IMG) deploy/olm/catalog

.PHONY: helm-package
helm-package: ## Package the two Helm charts into dist/.
	mkdir -p dist
	helm package charts/pgtoolbox charts/pgtools -d dist

# ----------------------------------------------------------------- cluster

##@ Cluster

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

# ----------------------------------------------------------------- release

##@ Release

# The release path (.github/workflows/release.yml, hack/release.sh). The workflow
# builds, pushes, signs and attests; these targets run the checks and the
# dry run on a laptop. Nothing here pushes, tags or publishes.
GITHUB_REPOSITORY ?= fyannk/pgToolBox
TAG ?=
REQUIRE_TAG ?= 0
IMAGE ?=
# What a signature made by this repository's release workflow looks like.
COSIGN_IDENTITY_REGEXP ?= ^https://github.com/fyannk/pgToolBox/\.github/workflows/release\.yml@refs/tags/v
COSIGN_OIDC_ISSUER ?= https://token.actions.githubusercontent.com

.PHONY: release-check
release-check: ## Check semver, annotated tag, chart versions and CHANGELOG for TAG (default: the chart's version). REQUIRE_TAG=1 insists the tag exists.
	REQUIRE_TAG=$(REQUIRE_TAG) ./hack/release.sh check $(TAG)

.PHONY: release-check-module
release-check-module: ## Check a nested-module tag (pkg/evidence/vX.Y.Z): module path, semver, annotation, same commit rules.
	./hack/release.sh check-module $(TAG)

.PHONY: release-dry-run
release-dry-run: ## Build every release image (amd64+arm64) into OCI archives, pin and package the charts and manifests into dist/release; nothing is pushed or signed. DRY_RUN_FAKE_DIGESTS=1 skips the image builds; TAG=v0.1.0-rc.1 rehearses a release candidate.
	GITHUB_REPOSITORY=$(GITHUB_REPOSITORY) DRY_RUN_TAG=$(TAG) ./hack/release.sh dry-run

.PHONY: verify-image
verify-image: ## Verify the keyless signature and the provenance of IMAGE (a tag or, better, a digest) with cosign and gh.
	@test -n "$(IMAGE)" || { echo "usage: make verify-image IMAGE=ghcr.io/fyannk/pgtoolbox-operator:X.Y.Z"; exit 2; }
	@command -v cosign >/dev/null || { echo "cosign is not on PATH (mise install; see mise.toml)" >&2; exit 2; }
	cosign verify --certificate-identity-regexp '$(COSIGN_IDENTITY_REGEXP)' --certificate-oidc-issuer '$(COSIGN_OIDC_ISSUER)' "$(IMAGE)" >/dev/null && echo "signature ok: $(IMAGE)"
	@if command -v gh >/dev/null; then \
		gh attestation verify "oci://$(IMAGE)" --repo $(GITHUB_REPOSITORY) >/dev/null && echo "provenance ok"; \
	else echo "gh is not on PATH: provenance not checked"; fi

# ------------------------------------------------------------ console API

##@ Console API

# The committed OpenAPI document and the UI's MSW fixtures are generated from
# the Huma registrations in internal/console/api. `make test`
# already fails when they drift (TestGeneratedArtifactsUpToDate); the CI job
# `generated` runs the explicit check.
.PHONY: openapi
openapi: ## Regenerate internal/console/api/openapi.yaml and its fixtures.
	go run ./internal/console/api/gen -dir internal/console/api

.PHONY: verify-openapi
verify-openapi: ## Fail when the committed OpenAPI document or fixtures are stale.
	go run ./internal/console/api/gen -dir internal/console/api -check

# The console's trust-boundary parsers: a sealed session opens only if this
# codec sealed it; a redirect never leaves the origin; the CSRF rules hold for
# any header combination; the revocation list is refused whole or well-formed;
# a pasted token cannot split a header; a cluster path reaches the read model
# only as valid Kubernetes names.
.PHONY: test-fuzz-console-api
test-fuzz-console-api: ## Fuzz the console's sign-in and API parsers.
	$(call fuzz,./internal/console/auth,FuzzOpenSession)
	$(call fuzz,./internal/console/auth,FuzzSafeRedirect)
	$(call fuzz,./internal/console/auth,FuzzCheckRequest)
	$(call fuzz,./internal/console/auth,FuzzParseRevocationList)
	$(call fuzz,./internal/console/auth,FuzzNormalizePastedToken)
	$(call fuzz,./internal/console/api,FuzzClusterPath)

test-fuzz: test-fuzz-console-api test-fuzz-evidence-agent test-fuzz-operator

# ------------------------------------------------------------------- chart

##@ Charts (charts/pgtoolbox, charts/pgtools, hack/chart-gates.sh)

.PHONY: helm-unittest
helm-unittest: ## helm-unittest: the console's RBAC, availability, NetworkPolicy and exposure rules.
	./hack/chart-gates.sh unittest

.PHONY: chart-lint
chart-lint: ## ct lint, once per charts/*/ci/*-values.yaml.
	./hack/chart-gates.sh lint

.PHONY: chart-kubeconform
chart-kubeconform: ## Every chart rendering against the Kubernetes (and Gateway API) schemas.
	./hack/chart-gates.sh kubeconform

.PHONY: chart-kube-linter
chart-kube-linter: ## Every chart rendering against kube-linter.
	./hack/chart-gates.sh kube-linter

.PHONY: chart-docs
chart-docs: ## Regenerate the Values reference of both chart READMEs (helm-docs, from values.yaml).
	./hack/chart-gates.sh docs-write

.PHONY: chart-docs-check
chart-docs-check: ## Fail when a chart README is stale or a value has no `# --` description.
	./hack/chart-gates.sh docs

.PHONY: chart-gates
chart-gates: helm-lint dashboards-check rules-test helm-unittest chart-lint chart-kubeconform chart-kube-linter chart-docs-check validate-packaging ## All the static chart gates, in CI's order.

# `make check` runs the chart gates too (a prerequisite added here, not an edit of
# the target above).
check: chart-gates test-hack

# The two chart e2e jobs build the console image from the working tree (CONSOLE_IMG)
# and load it into a kind cluster named after the run.

.PHONY: test-e2e-chart
test-e2e-chart: docker-build-console docker-build ## Two team consoles in two namespaces on kind: RBAC proof by SubjectAccessReview, then ct install of the chart (scope e2e).
	CONSOLE_IMG=$(CONSOLE_IMG) OPERATOR_IMG=$(OPERATOR_IMG) ./hack/e2e-chart.sh scope

.PHONY: test-e2e-chart-netpol
test-e2e-chart-netpol: docker-build-console ## The chart with networkPolicy.enabled on kind + Calico: the console works, cannot reach the other namespace.
	CONSOLE_IMG=$(CONSOLE_IMG) ./hack/e2e-chart.sh netpol
# ------------------------------------------------------- console core

##@ Console core

.PHONY: sync-crds
sync-crds: ## Vendor the CNPG CRDs per supported minor into test/crds/cnpg-<minor> (CNPG_SRC=checkout).
	CNPG_SRC=$(or $(CNPG_SRC),../cloudnative-pg) ./hack/sync-crds.sh

.PHONY: test-envtest-console
test-envtest-console: ## The console's informer cache on envtest, once per vendored CNPG minor (RBAC-starved user, probes, re-probe rebuild).
	set -e; assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_CONSOLE) -p path)"; \
	for crds in test/crds/cnpg-*; do \
		echo "== $$crds"; \
		KUBEBUILDER_ASSETS="$$assets" CNPG_CRDS="$$PWD/$$crds" go test -tags envtest -count=1 ./internal/console/cache/; \
	done

.PHONY: test-fuzz-console
test-fuzz-console: ## Fuzz the console's trust-boundary parsers (/pg/status body, LSN text).
	$(call fuzz,./internal/console/sweep,FuzzDecodeStatus)
	$(call fuzz,./internal/console/sweep,FuzzParseLSN)

.PHONY: verify-pins-cnpgstatus
verify-pins-cnpgstatus: ## Verify the read model's per-minor tables against the verified CNPG releases (module cache).
	go test -tags catalogpins -count=1 -run 'TestTablePinsExistInVerifiedReleases|TestEveryUpstreamPhaseIsInTheTable|TestFeatureFloorsAreAbsentBelowTheFloor' ./pkg/cnpgstatus/

.PHONY: test-console-core
test-console-core: test-envtest-console test-fuzz-console verify-pins-cnpgstatus ## Everything the console core adds beyond make test (envtest per minor, fuzz, pins).

# ----------------------------------------------------------------- console UI

##@ Console UI

# The console SPA. Node and pnpm come from mise.toml.
PNPM ?= pnpm
UI_DIR := ui

.PHONY: ui-install
ui-install: ## Install the SPA's dependencies from the lockfile (no install scripts run).
	cd $(UI_DIR) && $(PNPM) install --frozen-lockfile

.PHONY: ui-dev
ui-dev: ui-install ## Run the SPA on Vite with MSW serving the Go view-model goldens (no cluster needed).
	cd $(UI_DIR) && $(PNPM) i18n && $(PNPM) dev

.PHONY: ui-check
ui-check: ui-install ## The SPA gate: generated drift, Biome, styling rules, knip, tsc, tokens, Vitest, stories (axe), build, size budgets.
	cd $(UI_DIR) && $(PNPM) check

.PHONY: ui-check-light
ui-check-light: ui-install ## The SPA gate of a pull request: ui-check without the stories (axe) and the Storybook build.
	cd $(UI_DIR) && $(PNPM) check:light

.PHONY: ui-build
ui-build: ui-install ## Build ui/dist (hashed assets + .br/.gz siblings) for the Go console to embed.
	cd $(UI_DIR) && $(PNPM) i18n && $(PNPM) build

.PHONY: ui-e2e
ui-e2e: ui-install ## Playwright + axe + screenshots against the harness build, in the image the baselines were rendered in (Docker).
	cd $(UI_DIR) && $(PNPM) i18n && ./scripts/e2e-docker.sh

.PHONY: ui-e2e-update
ui-e2e-update: ui-install ## Re-render the screenshot baselines in the CI image; review the image diff before committing.
	cd $(UI_DIR) && $(PNPM) i18n && ./scripts/e2e-docker.sh --update

.PHONY: ui-audit
ui-audit: ## pnpm audit of the SPA's runtime dependencies.
	cd $(UI_DIR) && $(PNPM) audit --prod --audit-level high

# The PgAdmin controller against a real API server with CloudNativePG's CRDs,
# as the operator identity bound to the generated manager ClusterRole: RBAC
# escalation prevention, schemas, server-side apply and the no-op reconcile.
.PHONY: test-envtest-operator
test-envtest-operator: manifests ## The PgAdmin reconciler and the pgtools sidecar's name-scoped Role on envtest (RBAC, SSA, no-op reconcile).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_CONSOLE) -p path)" \
		go test ./internal/operator/pgadmin/ ./internal/operator/pgadmin/passfilesync/ -run 'Envtest' -count=1 -v

.PHONY: test-e2e-pgadmin
test-e2e-pgadmin: ## The PgAdmin scenario of the operator on kind. Docker + kind.
	./test/e2e/pgadmin/run.sh
