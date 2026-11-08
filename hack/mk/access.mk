# Access-request targets (PgAccessRequest: the CRD, the operator's
# expiry/revocation controller, the console's endpoints), included by the
# Makefile's `-include hack/mk/*.mk`. test-envtest and test-fuzz depend on the
# targets below, so the CI jobs that run those already run these.

##@ Access requests

.PHONY: test-envtest-access
test-envtest-access: manifests ## PgAccessRequest on envtest with real RBAC: the operator's controller (expiry, revocation, escalation refused) and the console's write path as viewer/operator tokens.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_API) -p path)" \
		go test -count=1 -run 'Envtest' ./internal/operator/accessrequest/
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_API) -p path)" \
		go test -tags envtest -count=1 -run '^TestAccessWriterOnEnvtest$$' ./internal/console/ops/

test-envtest: test-envtest-access

.PHONY: test-fuzz-access
test-fuzz-access: ## Fuzz the access-request parsers (create body, username to subject).
	$(call fuzz,./internal/console/api,FuzzNewAccessRequest)
	$(call fuzz,./api/v1alpha1,FuzzSubjectForUsername)

test-fuzz: test-fuzz-access

.PHONY: test-e2e-access
test-e2e-access: ## The access-request scenario on kind: the operator and console from the working tree, viewer/operator tokens, approval, four eyes, expiry, revocation, garbage collection. Docker + kind.
	./test/e2e/access/run.sh
