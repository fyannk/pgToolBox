# Operator targets: the fuzz and envtest suites that no other target runs.
# Included by the Makefile (`-include hack/mk/*.mk`); `make test-fuzz`
# aggregates them with the console's and the evidence agent's.

##@ Operator

# The trust boundary of the operator's parsers: a registration report read
# from a Pod's termination message never puts more than a short version token
# into status; a region guessed from an endpoint stays a region; an S3
# destination of the evidence contract round-trips or is refused.
.PHONY: test-fuzz-operator
test-fuzz-operator: ## Fuzz the operator's untrusted parsers (registration report, endpoint region, S3 destination).
	$(call fuzz,./internal/operator/pgadmin,FuzzParseReport)
	$(call fuzz,./internal/backupresolve,FuzzRegionFromEndpoint)
	cd $(EVIDENCE_MODULE) && $(call fuzz,./v1alpha1,FuzzParseS3Destination)

# The operator binary's own wiring on envtest: /metrics over HTTPS answers 401
# without a token, 403 to a ServiceAccount that may not get /metrics, and 200
# to a metrics reader, with the manager holding only the two review verbs; and
# /readyz stays unready while an informer cannot list, ready once it syncs.
.PHONY: test-envtest-manager
test-envtest-manager: ## The operator binary on envtest: /metrics (TLS, TokenReview, SubjectAccessReview) and /readyz following the informer cache.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION_CONSOLE) -p path)" \
		go test ./cmd/operator/ -run 'Envtest' -count=1 -v

# The operator confined by operator.config.watchNamespaces on kind: Ready with
# the release namespace not watched, a PgAdmin reconciled only where it
# watches, a watched namespace deleted under it, and helm upgrade back to the
# default mode.
.PHONY: test-e2e-operator-scope
test-e2e-operator-scope: ## The operator chart with watchNamespaces on kind (own config read, deleted namespace, mode switch). Docker + kind.
	./test/e2e/scope/run.sh
