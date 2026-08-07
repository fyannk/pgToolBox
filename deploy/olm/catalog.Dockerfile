# File-based OLM catalog index for pgToolBox.
#
# Build with the repository root as context:
#   docker build -f deploy/olm/catalog.Dockerfile -t pgtoolbox-catalog:v0.1.0 deploy/olm/catalog
FROM quay.io/operator-framework/opm:latest

LABEL operators.operatorframework.io.index.configs.v1=/configs
LABEL operators.operatorframework.io.index.database.v1=/database/index.db

COPY pgtoolbox /configs/pgtoolbox
RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]
ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs"]
