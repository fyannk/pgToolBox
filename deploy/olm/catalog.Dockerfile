# File-based OLM catalog index for pgToolBox.
#
# Build with the catalog directory as context:
#   docker build -f deploy/olm/catalog.Dockerfile -t pgtoolbox-catalog:v0.1.0 deploy/olm/catalog
#
# The upstream opm image (quay.io/operator-framework/opm v1.74.0, the newest
# stable release) ships opm and grpc_health_probe built with an older Go and
# dependencies that carry fixable high vulnerabilities. So both are built here
# from source, by the module in catalog/opm (go.mod pins opm v1.74.0,
# grpc-health-probe and the fixed dependency versions; .indexignore keeps it
# out of the catalog), with the repository's Go toolchain. opm is built
# without cgo: its SQLite index backend is unused by a file-based catalog.
#
# Every stage runs on the build platform and cross-compiles, so a multi-arch
# build needs no emulation. The cache is built, and the catalog thereby
# validated, by the build platform's opm; opm re-checks the cache's integrity
# when it starts, so a cache that does not suit the target platform degrades
# to a rebuild at startup rather than to wrong answers.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS tools
ARG BUILDARCH
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /src
COPY opm/go.mod opm/go.sum ./
RUN for i in 1 2 3 4 5; do go mod download && exit 0; echo "go mod download failed (attempt $i/5)" >&2; sleep $((i * 10)); done; exit 1
RUN GOOS=linux GOARCH=${BUILDARCH} go build -tags=json1,netgo,osusergo,containers_image_openpgp -ldflags="-s -w" -o /out/build/opm github.com/operator-framework/operator-registry/cmd/opm \
 && GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -tags=json1,netgo,osusergo,containers_image_openpgp -ldflags="-s -w" -o /out/target/opm github.com/operator-framework/operator-registry/cmd/opm \
 && GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /out/target/grpc_health_probe github.com/grpc-ecosystem/grpc-health-probe

FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS cache
COPY --from=tools /out/build/opm /bin/opm
COPY pgtoolbox /configs/pgtoolbox
RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

LABEL operators.operatorframework.io.index.configs.v1=/configs

COPY --from=tools /out/target/opm /bin/opm
COPY --from=tools /out/target/grpc_health_probe /bin/grpc_health_probe
COPY pgtoolbox /configs/pgtoolbox
COPY --from=cache --chown=65532:65532 /tmp/cache /tmp/cache
ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]
