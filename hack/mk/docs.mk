# Documentation targets: the one Docusaurus site under docs/site,
# and the CRD reference generated from api/v1alpha1. Included by the Makefile
# (`-include hack/mk/*.mk`); uses tool/mise_v from hack/common.mk.
#
# `make docs` is the CI gate: it fails when the committed CRD reference differs
# from what the code generates, when a registered condition reason is missing
# from the conditions page, when the site does not build (Docusaurus is
# configured to fail on a broken internal link, anchor or Markdown link, and the
# build copies the tour's screenshots from the UI baselines, failing on a missing
# one), and when the console's documentation bundle cannot be made from the build
# (manifest, 1 MiB budget, load and search of what was written).
#
# The site is single-version: it documents main, and the generated-content
# gates (crd-ref-docs-check, docs-reasons-check, docs-env-check, docs-bundle)
# look at docs/.

CRD_REF_DOCS ?= $(call tool,crd-ref-docs,go run github.com/elastic/crd-ref-docs@v$(call mise_v,go:github.com/elastic/crd-ref-docs))
CRD_REF_DIR = docs/site/crd-ref-docs
CRD_REF_PAGE = docs/site/docs/reference/api.md
CONDITIONS_PAGE = docs/site/docs/operator/conditions.md

##@ Documentation

.PHONY: crd-ref-docs
crd-ref-docs: ## Regenerate the api/v1alpha1 reference page (docs/site/docs/reference/api.md).
	@set -eu; body="$$(mktemp)"; trap 'rm -f "$$body" "$$body.raw"' EXIT; \
	$(CRD_REF_DOCS) --source-path=./api/v1alpha1 --config=$(CRD_REF_DIR)/config.yaml \
	  --templates-dir=$(CRD_REF_DIR)/templates --renderer=markdown --log-level=ERROR \
	  --output-path="$$body.raw"; \
	sed -e 's|<br />|@BR@|g' -e 's|<\([A-Za-z]\)|\&lt;\1|g' -e 's|@BR@|<br />|g' "$$body.raw" > "$$body"; rm -f "$$body.raw"; \
	{ cat $(CRD_REF_DIR)/front-matter.md "$$body" $(CRD_REF_DIR)/rules-heading.md; \
	  awk -f $(CRD_REF_DIR)/cel-rules.awk api/v1alpha1/*_types.go; } > "$(CRD_REF_PAGE)"

.PHONY: crd-ref-docs-check
# The generator is invoked through $(MAKE) so the page has one recipe, which
# makes make run this line even under `make -n`; the inner make then only
# prints, so a dry run (MAKEFLAGS carries n) stops after printing instead of
# diffing an empty file.
crd-ref-docs-check: ## Fail when the committed CRD reference differs from what api/v1alpha1 generates.
	@set -eu; out="$$(mktemp)"; trap 'rm -f "$$out"' EXIT; \
	$(MAKE) --no-print-directory crd-ref-docs CRD_REF_PAGE="$$out"; \
	case "$(firstword -$(MAKEFLAGS))" in *n*) exit 0 ;; esac; \
	diff -u $(CRD_REF_PAGE) "$$out" >&2 || { \
		echo "$(CRD_REF_PAGE) is stale; run 'make crd-ref-docs' and commit the result" >&2; exit 1; }

.PHONY: docs-reasons-check
docs-reasons-check: ## Fail when a registered condition reason is missing from the conditions page.
	@status=0; for f in api/v1alpha1/testdata/reasons/*.golden; do \
		for reason in $$(cat "$$f"); do \
			grep -q "\`$$reason\`" $(CONDITIONS_PAGE) || { echo "$(CONDITIONS_PAGE) does not document the reason $$reason ($$f)" >&2; status=1; }; \
		done; \
	done; exit $$status

.PHONY: docs-env-check
docs-env-check: ## Fail when a console environment variable has no line on the site.
	./hack/check-docs-env.sh

# The console embeds the documentation of its own version:
# the Markdown the site's llms plugin writes for `current` and a BM25 index
# over its sections, in internal/console/docsbundle/data (under 1 MiB). The
# bundle version is the product's: the chart version.
#
# The bundle is generated and NOT committed (it used to be, and every pull
# request that touched a page conflicted with every other on index.bin). Source
# builds, `go test` and lint work without it: the console then serves no docs
# tools. Every image build produces it: the Dockerfile's `docs` and `docs-bundle`
# stages run the same steps as this target, so `make images`, hack/release.sh and
# the CI images job need no step of their own. To run a locally built console
# with the docs, run `make docs-bundle` before `go build ./cmd/console`.
DOCS_BUNDLE_DIR = internal/console/docsbundle/data
DOCS_BUNDLE_VERSION = $(shell sed -n 's/^version: *//p' charts/pgtoolbox/Chart.yaml | head -n 1)

.PHONY: docs-build
docs-build: ## Build the documentation site (typecheck, the llms plugin and script tests, Mermaid parse, screenshots copy, build; fails on a broken link). Writes docs/site/build.
	cd docs/site && npm ci --ignore-scripts && npm run typecheck && npm test && npm run check:mermaid && npm run build

.PHONY: docs-bundle
docs-bundle: docs-build ## Write the console's embedded documentation bundle (Markdown pages and BM25 index; git-ignored) from the built site.
	go run ./hack/docsbundle -site docs/site/build -out $(DOCS_BUNDLE_DIR) -version $(DOCS_BUNDLE_VERSION)

.PHONY: test-docs-bundle
test-docs-bundle: ## Run the tests that need the real bundle (make docs-bundle first); they skip without it and fail here.
	DOCSBUNDLE_REQUIRED=1 go test -count=1 -run 'Embedded|RealDocs' ./internal/console/docsbundle/ ./internal/console/server/

.PHONY: docs
docs: crd-ref-docs-check docs-reasons-check docs-env-check docs-bundle test-docs-bundle ## Build the documentation site and check it: stale CRD reference, an undocumented setting or condition reason, a Mermaid syntax error, a broken link or missing screenshot, a bundle the console cannot embed.
	@echo "documentation: all gates passed"

.PHONY: docs-serve
docs-serve: ## Serve the documentation site locally with live reload (http://localhost:3000/).
	cd docs/site && npm ci --ignore-scripts && npm run start
