# pgToolBox operator and proxy image build.
#
# The build context must be the parent directory containing this repository
# and the object-store-viewer repository (as object-store-viewer/). The
# objectstoreviewer API module is vendored in via the go.mod replace to
# ../objectstoreviewer/api. REPO_DIR names the checked-out repository
# directory inside the context (pgToolBox locally, the repository name in CI).
#
#   docker build -f pgToolBox/Dockerfile --target manager -t pgtoolbox:latest ..
#   docker build -f pgToolBox/Dockerfile --target proxy   -t pgtoolbox-proxy:latest ..

FROM golang:1.26 AS builder
ARG REPO_DIR=pgToolBox
WORKDIR /workspace

# Module files first for dependency caching.
COPY ${REPO_DIR}/go.mod ${REPO_DIR}/go.sum ${REPO_DIR}/
COPY object-store-viewer/api objectstoreviewer/api

WORKDIR /workspace/${REPO_DIR}
RUN go mod download

COPY ${REPO_DIR}/api api
COPY ${REPO_DIR}/cmd cmd
COPY ${REPO_DIR}/internal internal
COPY ${REPO_DIR}/hack hack

ARG VERSION=development
ARG IMG=pgtoolbox:latest
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-X main.operatorVersion=${VERSION} -X main.defaultOperatorImage=${IMG}" \
    -o /workspace/manager ./cmd/manager
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -o /workspace/proxy ./cmd/proxy

# The operator image. The admin-sync init container copies this same binary
# out of this image into console pods, so the image reference is baked in at
# build time and can be overridden with --operator-image.
FROM gcr.io/distroless/static:nonroot AS manager
COPY --from=builder /workspace/manager /
USER 65532:65532
ENTRYPOINT ["/manager"]

# The pgtoolbox-proxy image used as the console's authentication proxy.
FROM gcr.io/distroless/static:nonroot AS proxy
COPY --from=builder /workspace/proxy /
USER 65532:65532
ENTRYPOINT ["/proxy"]
