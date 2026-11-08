# Security-contract targets (per-namespace evidence token audience, the
# console's status-writer check), included by the Makefile's
# `-include hack/mk/*.mk`. test-fuzz and test-envtest depend on them, so the
# existing CI jobs run them.

##@ Security contract

.PHONY: test-fuzz-audience
test-fuzz-audience: ## Fuzz the per-namespace evidence audience parsers (audience string, unverified aud claim).
	cd $(EVIDENCE_MODULE) && $(call fuzz,./v1alpha1,FuzzParseTokenAudience)
	cd $(EVIDENCE_MODULE) && $(call fuzz,./v1alpha1,FuzzClassifyAudienceClaim)

test-fuzz: test-fuzz-audience

.PHONY: test-envtest-security
test-envtest-security: ## The agent's TokenReview/SAR with real bound tokens, and the console's status-writer check through its cache, on envtest.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_API) -p path)" \
		go test -tags envtest -count=1 -run 'OnEnvtest$$' ./internal/evidence/agent/ ./internal/console/evidence/

test-envtest: test-envtest-security
