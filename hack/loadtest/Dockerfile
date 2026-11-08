# The loadgen image: only `loadgen instance-sim` runs in the cluster (the other
# subcommands run from the host). cluster.sh builds the static binary on the
# host and this image just carries it, on the same distroless base as the
# product images.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY loadgen /loadgen
USER 65532:65532
ENTRYPOINT ["/loadgen"]
