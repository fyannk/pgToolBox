# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# Every image this repository builds, one stage per image, one build context.
#
#   docker buildx build --target operator       -t pgtoolbox-operator:dev .
#   docker buildx build --target console        -t pgtoolbox-console:dev .
#   docker buildx build --target evidence-agent -t pgtoolbox-evidence-agent:dev .
#
# The builder runs on the build host's own architecture and cross-compiles
# (CGO is off), so a linux/arm64 image is built natively fast, with no QEMU:
#   docker buildx build --platform linux/amd64,linux/arm64 --target console .
#
# Published as ghcr.io/fyannk/pgtoolbox-operator, pgtoolbox-console and
# pgtoolbox-evidence-agent.
#
# Nothing in the module graph is private: the evidence module is in this tree
# and reached through the replace in go.mod (GOWORK=off), so the build needs no
# git and no credentials.

# pnpm, the version mise.toml pins (hack/check-toolchain.sh holds them
# together), from its GitHub release and checked against the release's sha256.
# The npm package is only a shim that fetches this same native binary at install
# time, so installing it with npm would pin nothing but the shim. Both
# architectures are here, and the ui stage takes its build platform's: a FROM
# that names a stage through a variable is not a pinned base to a scanner.
FROM scratch AS pnpm
ADD --checksum=sha256:565ff81174a14f6f7bd5d84c1b17e51031a80230d370a1fe24be5d16bd02a825 https://github.com/pnpm/pnpm/releases/download/v12.16.1/pnpm-linux-x64.tar.gz /amd64.tar.gz
ADD --checksum=sha256:f5847699e5d528f8f8e9bcfb385e58cb9fba7482af1b1aa7b70737be42277f81 https://github.com/pnpm/pnpm/releases/download/v12.16.1/pnpm-linux-arm64.tar.gz /arm64.tar.gz

# The console SPA. Built once on the build host's
# platform: the output is architecture-independent. Node and pnpm are the
# versions mise.toml pins; the lockfile is frozen and the workspace's
# allowBuilds/minimumReleaseAge rules apply, so no dependency script runs.
# ui/dist: index.html, content-hashed assets/ and their .br/.gz siblings.
FROM --platform=$BUILDPLATFORM node:24.22.1-bookworm-slim@sha256:be8204d4bd327363357f532187b752ff62759c31bedf98e9e18fc08c9cbd605c AS ui
WORKDIR /ui
ENV CI=1
ARG BUILDARCH
RUN --mount=from=pnpm,target=/pnpm \
    mkdir /opt/pnpm && tar -xzf "/pnpm/${BUILDARCH}.tar.gz" -C /opt/pnpm \
    && ln -s /opt/pnpm/pnpm /usr/local/bin/pnpm && pnpm --version
COPY ui/package.json ui/pnpm-lock.yaml ui/pnpm-workspace.yaml ./
COPY ui/tokens/package.json tokens/package.json
RUN for i in 1 2 3 4 5; do pnpm install --frozen-lockfile && exit 0; echo "pnpm install failed (attempt $i/5)" >&2; sleep $((i * 10)); done; exit 1
COPY ui/ ./
RUN pnpm i18n && pnpm build

# The documentation of the console's own version, embedded in the console
# binary for the MCP docs tools. It is generated, not
# committed: the site is built here, with the same steps as `make docs-bundle`
# (docs/site's `npm run build` copies the tour's screenshots from the UI
# baselines first), then hack/docsbundle writes the Markdown pages and the BM25
# index. Build host platform: the output is architecture-independent. A failure
# fails the image build, so a release image never ships without it.
FROM --platform=$BUILDPLATFORM node:24.22.1-bookworm-slim@sha256:be8204d4bd327363357f532187b752ff62759c31bedf98e9e18fc08c9cbd605c AS docs-site
WORKDIR /repo/docs/site
ENV CI=1 DO_NOT_TRACK=1
COPY docs/site/package.json docs/site/package-lock.json docs/site/.npmrc ./
RUN for i in 1 2 3 4 5; do npm ci --ignore-scripts --no-audit --no-fund && exit 0; echo "npm ci failed (attempt $i/5)" >&2; sleep $((i * 10)); done; exit 1
COPY docs/site/ ./
# The site reads the chart version and the tour's baselines from outside docs/site.
COPY charts/pgtoolbox/Chart.yaml /repo/charts/pgtoolbox/Chart.yaml
COPY ui/e2e/__screenshots__/screens.spec.ts /repo/ui/e2e/__screenshots__/screens.spec.ts
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS docs-bundle
WORKDIR /workspace
ENV GOWORK=off
COPY go.mod go.sum ./
COPY pkg/evidence/go.mod pkg/evidence/go.mod
# A proxy error (stream reset, 5xx) is retried with backoff rather than failing
# the image; go.sum still verifies every module.
RUN for i in 1 2 3 4 5; do go mod download && exit 0; echo "go mod download failed (attempt $i/5)" >&2; sleep $((i * 10)); done; exit 1
COPY internal/console/docsbundle internal/console/docsbundle
COPY hack/docsbundle hack/docsbundle
COPY charts/pgtoolbox/Chart.yaml charts/pgtoolbox/Chart.yaml
COPY --from=docs-site /repo/docs/site/build docs/site/build
RUN go run ./hack/docsbundle -site docs/site/build -out internal/console/docsbundle/data \
    -version "$(awk '/^version:/ {print $2; exit}' charts/pgtoolbox/Chart.yaml)"

FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
ENV GOWORK=off

# Module files first for dependency caching.
COPY go.mod go.sum ./
COPY pkg/evidence/go.mod pkg/evidence/go.mod
# A proxy error (stream reset, 5xx) is retried with backoff rather than failing
# the image; go.sum still verifies every module.
RUN for i in 1 2 3 4 5; do go mod download && exit 0; echo "go mod download failed (attempt $i/5)" >&2; sleep $((i * 10)); done; exit 1

COPY api api
COPY cmd cmd
COPY internal internal
COPY pkg pkg

# -trimpath drops the build host's paths from the binary; -s -w strip the
# symbol table and DWARF, which a distroless runtime has no debugger for.
ARG VERSION=development
ARG IMG=pgtoolbox-operator:latest
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.operatorVersion=${VERSION} -X main.defaultOperatorImage=${IMG}" \
    -o /workspace/operator ./cmd/operator
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /workspace/evidence-agent ./cmd/evidence-agent
# The SPA last, so a UI change rebuilds the console binary only. go:embed
# cannot reach outside its package: internal/console/uiassets embeds dist/.
COPY --from=ui /ui/dist internal/console/uiassets/dist
# The documentation bundle, for the same reason (internal/console/docsbundle
# embeds data/); .dockerignore keeps a locally generated copy out of the context.
COPY --from=docs-bundle /workspace/internal/console/docsbundle/data internal/console/docsbundle/data
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /workspace/console ./cmd/console

# The operator image. The install init container of every pgAdmin Pod copies
# this same binary out of this image (its credential sidecar and Jobs run it),
# so the image reference is baked in at build time and can be overridden with
# --operator-image.
#
# The labels are the ones a plain `docker build` can know. The release
# workflow (docker/metadata-action) sets title and description per image, the
# same words as below, and adds revision and created; a local build has no
# commit to claim.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS operator
ARG VERSION=development
LABEL org.opencontainers.image.title="pgToolBox operator" \
      org.opencontainers.image.description="Kubernetes operator that deploys the pgToolBox access stack for CloudNativePG clusters" \
      org.opencontainers.image.source="https://github.com/fyannk/pgToolBox" \
      org.opencontainers.image.documentation="https://fyannk.github.io/pgToolBox/" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=builder /workspace/operator /manager
COPY LICENSE /licenses/pgtoolbox/LICENSE
USER 65532:65532
ENTRYPOINT ["/manager"]

# The console: Go API and the embedded SPA, one per team namespace.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS console
ARG VERSION=development
LABEL org.opencontainers.image.title="pgToolBox console" \
      org.opencontainers.image.description="Operational console for one CloudNativePG cluster" \
      org.opencontainers.image.source="https://github.com/fyannk/pgToolBox" \
      org.opencontainers.image.documentation="https://fyannk.github.io/pgToolBox/" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=builder /workspace/console /console
COPY LICENSE /licenses/console/LICENSE
# The front-end assets are embedded in the binary above, so the licence of the
# library they bundle (MIT, which asks for its text to travel with copies)
# comes from the exact npm package the build used.
COPY --from=ui /ui/node_modules/uplot/LICENSE /licenses/console/uplot/LICENSE
USER 65532:65532
EXPOSE 3000
ENTRYPOINT ["/console"]

# The evidence agent: standalone, or as the agent an
# EvidenceAgent (or the pgtools chart) runs.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS evidence-agent
ARG VERSION=development
LABEL org.opencontainers.image.title="pgToolBox evidence agent" \
      org.opencontainers.image.description="Read-only viewer and evidence producer for a PostgreSQL backup repository" \
      org.opencontainers.image.source="https://github.com/fyannk/pgToolBox" \
      org.opencontainers.image.documentation="https://fyannk.github.io/pgToolBox/" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=builder /workspace/evidence-agent /evidence-agent
COPY LICENSE /licenses/evidence-agent/LICENSE
USER 65532:65532
EXPOSE 3000
ENTRYPOINT ["/evidence-agent"]
