# pgToolBox operator and proxy image build.
#
# The build context is this repository. It used to have to be the parent
# directory, because the pgObjectStoreViewer API module was reached through
# a go.mod replace to a sibling checkout; that module is published now, so
# the build resolves it like any other dependency.
#
#   docker build --target manager -t pgtoolbox:latest .
#   docker build --target proxy   -t pgtoolbox-proxy:latest .

FROM golang:1.27.0@sha256:0ecdc2a9f6156af6451080bfe3d8382a662fcc4e209608c6f919e643453514c1 AS builder
WORKDIR /workspace

# Module files first for dependency caching.
COPY go.mod go.sum ./
RUN go mod download

COPY api api
COPY cmd cmd
COPY internal internal
COPY hack hack

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
FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7 AS manager
COPY --from=builder /workspace/manager /
USER 65532:65532
ENTRYPOINT ["/manager"]

# The pgtoolbox-proxy image used as the console's authentication proxy.
FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7 AS proxy
COPY --from=builder /workspace/proxy /
USER 65532:65532
ENTRYPOINT ["/proxy"]
