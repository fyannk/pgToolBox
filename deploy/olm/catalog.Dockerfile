# File-based OLM catalog index for pgToolBox.
#
# Build with the repository root as context:
#   docker build -f deploy/olm/catalog.Dockerfile -t pgtoolbox-catalog:v0.1.0 deploy/olm/catalog
#
# opm is pinned by tag and digest (the digest is the multi-arch index).
#
# The cache is built, and the catalog thereby validated, in a stage that runs
# on the build platform. A RUN in the final stage would execute opm as the
# target architecture, which needs QEMU for linux/arm64 on an amd64 builder;
# this way a multi-arch build needs no emulation. opm re-checks the cache's
# integrity when it starts, so a cache that does not suit the target platform
# degrades to a rebuild at startup rather than to wrong answers.
FROM --platform=$BUILDPLATFORM quay.io/operator-framework/opm:v1.74.0@sha256:b32d3891616662620da08d7f0ec42c2e69fa2de43427dc975d35b12f7a969a0f AS cache
COPY pgtoolbox /configs/pgtoolbox
RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]

FROM quay.io/operator-framework/opm:v1.74.0@sha256:b32d3891616662620da08d7f0ec42c2e69fa2de43427dc975d35b12f7a969a0f

LABEL operators.operatorframework.io.index.configs.v1=/configs
LABEL operators.operatorframework.io.index.database.v1=/database/index.db

COPY pgtoolbox /configs/pgtoolbox
COPY --from=cache /tmp/cache /tmp/cache
ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]
