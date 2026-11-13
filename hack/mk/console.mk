# Console targets: the fuzz, envtest and unit suites of the console's parsers
# and write path. Included by the Makefile (`-include hack/mk/*.mk`);
# `make test-fuzz` and `make test-console-core` pick up the targets below.

##@ Console

.PHONY: test-fuzz-console-metrics
test-fuzz-console-metrics: ## Fuzz the Prometheus range-answer parser.
	$(call fuzz,./pkg/cnpgstatus/metrics,FuzzParseRange)

test-fuzz: test-fuzz-console-metrics

# The parsers of the console's operations and of the evidence channel it reads.
.PHONY: test-fuzz-console-operations
test-fuzz-console-operations: ## Fuzz the console parsers (operation handles, evidence channel).
	$(call fuzz,./internal/console/operations,FuzzParseID)
	$(call fuzz,./internal/console/evidence,FuzzParseEndpoint)
	$(call fuzz,./internal/console/evidence,FuzzDecodeSnapshot)
	$(call fuzz,./internal/console/evidence,FuzzDecodeBackupPage)
	$(call fuzz,./internal/console/evidence,FuzzDecodeAPIError)
	$(call fuzz,./internal/console/evidence,FuzzDecodePGBackRestBackupPage)
	$(call fuzz,./internal/console/evidence,FuzzDecodePGBackRestRecoveryPage)

test-fuzz: test-fuzz-console-operations

.PHONY: test-console-backend
test-console-backend: ## The console backend's unit tests (view models, read model, API, operations, evidence) with the race detector.
	go test -race ./internal/console/viewmodel/... ./internal/console/status/... ./internal/console/api/... \
		./internal/console/operations/... ./internal/console/evidence/... ./internal/console/history/... ./pkg/cnpgstatus/

.PHONY: test-envtest-console-ops
test-envtest-console-ops: ## The console's write path (token passthrough, RBAC per verb, promote lock, CNPG schema) on envtest, once per vendored CNPG minor.
	set -e; assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_CONSOLE) -p path)"; \
	for crds in test/crds/cnpg-*; do \
		echo "== $$crds"; \
		KUBEBUILDER_ASSETS="$$assets" CNPG_CRDS="$$PWD/$$crds" go test -tags envtest -count=1 -run '^TestUserWriterOnEnvtest$$' ./internal/console/ops/; \
	done

.PHONY: test-envtest-console-pgadmin
test-envtest-console-pgadmin: ## The console's per-person DatabaseRole writer on envtest (strict schema on CloudNativePG 1.30, the user's own RBAC).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_CONSOLE) -p path)" \
		go test -tags envtest -count=1 -run '^TestPersonalDatabaseRoleOnEnvtest$$' ./internal/console/ops/

.PHONY: test-envtest-console-mcpleader
test-envtest-console-mcpleader: ## The MCP leader's hand-over on envtest (two replicas: Lease, EndpointSlice, grant index).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_CONSOLE) -p path)" \
		go test -tags envtest -count=1 -run '^TestHandOverOnEnvtest$$' ./internal/console/mcpleader/

# `make test-console-core` (Makefile) runs the write-path envtests with the
# informer-cache ones.
test-console-core: test-envtest-console-ops test-envtest-console-pgadmin test-envtest-console-mcpleader

# RBAC and SubjectAccessReview semantics on a real API server, no kind. The
# kind scenarios (SSO against dex, the cluster list a DBA may see, the honest
# overview, revocation, sign-out, the NetworkPolicy job) are the two chart e2e
# targets, test-e2e-chart and test-e2e-chart-netpol. See test/e2e/README.md for
# the coverage table and the blind spots.
.PHONY: test-e2e-console-authz
test-e2e-console-authz: ## RBAC / SubjectAccessReview / SelfSubjectReview semantics of the console on envtest (no kind).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_CONSOLE) -p path)" \
		go test -tags e2econsole -count=1 -v ./test/e2e/console/
