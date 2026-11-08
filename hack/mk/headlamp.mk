# The Headlamp plugin (headlamp/): "open in the console" and the CloudNativePG
# phase badge. Its own toolchain (@kinvolk/headlamp-plugin, npm and
# package-lock.json), separate from the SPA's pnpm workspace. Included by the
# Makefile's `-include hack/mk/*.mk`.

##@ Headlamp plugin

.PHONY: headlamp-check
headlamp-check: ## Headlamp plugin: npm ci, lint, tsc, unit tests, build (headlamp/).
	cd headlamp && npm run check

.PHONY: headlamp-build
headlamp-build: ## Build the Headlamp plugin into headlamp/dist (copy it into Headlamp's plugins directory).
	cd headlamp && npm ci --no-audit --no-fund && npm run build
