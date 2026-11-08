# Packaging targets: the OLM bundle validation and scorecard, the kustomize
# renderings against the Kubernetes schemas, the per-image gates and the kind x
# CNPG matrix. Included by the Makefile (`-include hack/mk/*.mk`); uses tool/mise_v from hack/common.mk.

# Not buildable with `go run` (cgo, libgpgme): the binary comes from mise.toml.
OPERATOR_SDK ?= operator-sdk
# The Kubernetes version the schemas are taken from (as hack/chart-gates.sh).
K8S_SCHEMA_VERSION ?= 1.34.0

##@ Packaging (OLM, kustomize, matrix)

.PHONY: bundle-validate
bundle-validate: ## operator-sdk bundle validate (core + operatorframework suite) on deploy/olm/bundle. The scorecard is `make scorecard`.
	@command -v $(OPERATOR_SDK) >/dev/null || { echo "operator-sdk is not on PATH (mise install; see mise.toml)" >&2; exit 2; }
	$(OPERATOR_SDK) bundle validate deploy/olm/bundle --select-optional suite=operatorframework

.PHONY: scorecard
scorecard: ## operator-sdk scorecard (basic + olm suites) on deploy/olm/bundle, on a kind cluster of its own. Docker + kind.
	./hack/scorecard.sh

.PHONY: kustomize-validate
kustomize-validate: ## Render config/default and config/console and validate them against the Kubernetes schemas.
	@command -v kubeconform >/dev/null || { echo "kubeconform is not on PATH (mise install; see mise.toml)" >&2; exit 2; }
	@for dir in config/default config/console; do \
		echo "== kubeconform $$dir"; \
		kubectl kustomize $$dir | kubeconform -strict -summary -kubernetes-version $(K8S_SCHEMA_VERSION) \
			-skip CustomResourceDefinition -schema-location default || exit 1; \
	done

.PHONY: matrix-plan
matrix-plan: ## Print the kind x CNPG matrix as JSON (KUBERNETES=1.34|1.36|all CNPG=1.30.1|main|all NIGHTLY=true).
	@./hack/matrix.sh plan

.PHONY: test-e2e-matrix
test-e2e-matrix: ## One matrix cell by hand: make test-e2e-matrix K8S=1.34 CNPG=1.30.1 [SCENARIO=console|evidenceagent|pgadmin|all]. Docker + kind.
	./hack/matrix.sh run $(or $(SCENARIO),all)

##@ Packaging: images

.PHONY: test-image-profiles
test-image-profiles: docker-build-all ## Per-image gate: non-root numeric USER, exec ENTRYPOINT, OCI labels, licence, restricted runtime, no shell (Docker).
	./hack/image-profile.sh $(OPERATOR_IMG)
	./hack/image-profile.sh $(CONSOLE_IMG)
	./hack/image-profile.sh $(EVIDENCE_IMG)

##@ Packaging: documentation hygiene

DOC_PROSE = docs/site/docs README.md CONTRIBUTING.md SECURITY.md CHANGELOG.md charts/pgtoolbox/README.md charts/pgtools/README.md

.PHONY: docs-spell
docs-spell: ## codespell over the documentation prose (.codespellrc).
	@command -v codespell >/dev/null || { echo "codespell is not on PATH (mise install; see mise.toml)" >&2; exit 2; }
	codespell $(DOC_PROSE)

.PHONY: docs-links
docs-links: ## lychee over the external links of the documentation (lychee.toml). Informational: a dead third-party link is reported, not failed on.
	@command -v lychee >/dev/null || { echo "lychee is not on PATH (mise install; see mise.toml)" >&2; exit 2; }
	lychee --config lychee.toml $(DOC_PROSE)

##@ Bounded-resource tests

# The bounded stores of the read model: history (per-object cap, global cap, byte
# budget, diff bounds), the metrics ring and instance cap, the log buffer and matcher.
.PHONY: test-bounds
test-bounds: ## Bounded-resource tests under the race detector: history, metrics ring, log buffer; and the evidence inventory scale test (-tags=scale).
	go test -race ./pkg/cnpgstatus/history ./pkg/cnpgstatus/logstream ./pkg/cnpgstatus/metrics -run '^Test(PerObjectCapEvictsThatObjectsOldest|GlobalRevisionCapEvictsOldest|ByteBudgetEvictsOldest|DiffBoundsEntriesAndValues|DiffAfterBaselineEvictionIsHonest|PrimeTrimsToShrunkenLimits|MatcherBoundsARetainedLine|BufferBoundsBytesNotLines|BufferGlobalBoundStopsOneContainerCostingAnother|InstanceCapEvictsTheLeastRecentlyObserved|RawRingStaysBounded|ImportClampsToTheConfiguredBounds)$$' -count=1
	go test -tags=scale ./internal/evidence/inventory -run '^TestScannerMillionObjectSnapshotIsBounded$$' -count=1
