# OLM bundle image for pgToolBox.
#
# Build with the repository root as context:
#   docker build -f deploy/olm/bundle.Dockerfile -t pgtoolbox-bundle:v0.1.0 deploy/olm/bundle
FROM scratch

LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1=pgtoolbox
LABEL operators.operatorframework.io.bundle.channels.v1=alpha
LABEL operators.operatorframework.io.bundle.channel.default.v1=alpha
LABEL operators.operatorframework.io.metrics.project_layout=go.kubebuilder.io/v4

COPY manifests /manifests
COPY metadata /metadata
