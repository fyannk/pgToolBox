# pgToolBox evidence wire contract

This separate Go module holds the dependency-light evidence wire vocabulary
shared by the evidence agent, the console, and the pgToolBox operator. It
imports no scanner, cloud provider SDK, compression library, repository
parser, HTTP server, or credential implementation, so any of the three can
import it without gaining object-store access.

`v1alpha1` (`evidence.pgtoolbox.fyannk.dev/v1alpha1`) is what the evidence
agent serves over in-cluster HTTPS (`RUNTIME_MODE=agent`), with the media type
`application/vnd.pgtoolbox.evidence.v1alpha1+json` and the routes
`/api/v1alpha1/{snapshot,backups,wal-ranges,wal-gaps,recovery-paths}` plus the
unauthenticated `/healthz` and `/readyz`. It provides:

- exact health, readiness, snapshot, error, and collection wire types for the
  Barman Cloud and pgBackRest profiles; every snapshot carries a
  `producer_instance` (one per process) and a `producer_epoch` (the deployed
  configuration);
- conservative validators for states, nullable evidence, deterministic order,
  and bounds; the Barman profile reads S3, Azure Blob Storage or Google Cloud
  Storage (`provider` is `s3`, `azure` or `gcs`);
- unknown-details-tag handling that discards unrecognized payloads;
- the transport constants in `v1alpha1/channel.go`: media type, routes,
  response and page bounds, the request deadline, the SubjectAccessReview
  attributes, `AgentPort`, `Endpoint`, `ServerName`, the per-namespace token
  audience (`TokenAudience(namespace)` = `pgtoolbox-evidence-agent/<namespace>`,
  `ParseTokenAudience`, `ClassifyAudienceClaim`), the console's token path
  convention (`TokenPath(DefaultTokenDirectory, namespace)` =
  `/var/run/secrets/pgtoolbox.fyannk.dev/evidence-agent/<namespace>/token`, one
  projected token per watched namespace), and the bound-token and shared-token
  helpers (`ParseAuthorization`, `ParseSharedAuthorization`,
  `ReadBearerTokenFile`, `ReadSharedTokenFile`);
- the destination parsers (`ParseS3Destination`, `ParseAzureDestination`,
  `ParseGCSDestination`), canonicalization, and the provider-neutral
  `FingerprintRepository` (equal to `FingerprintS3` for S3; domain
  `pgtoolbox-evidence-repository-v1`, region-free) with golden vectors in
  [`testdata/s3-fingerprints.json`](v1alpha1/testdata/s3-fingerprints.json)
  and
  [`testdata/repository-fingerprints.json`](v1alpha1/testdata/repository-fingerprints.json);
- the generated Draft 2020-12 [`schema.json`](v1alpha1/schema.json); and
- deterministic JSON wire goldens under
  [`testdata/wire`](v1alpha1/testdata/wire).

The main module's provider-free `internal/evidence/evidenceapi` package
projects the agent's validated immutable inventory into these types and owns
the process-local publication and pagination engine and the authenticated
HTTPS handler. It is kept outside this module so the contract cannot import
scanners, format parsers, HTTP, or runtime implementations.

This module and its committed goldens are the authority on the wire contract;
the reader-facing description is the evidence agent page of the documentation
site.

Regenerate the schema and wire goldens from the repository root with:

```sh
make generate-evidence-artifacts
```

The unit suite compiles the schema, validates every golden, fails when a
committed artifact differs from what the generator produces, and checks Go
JSON round trips and additive-field behavior. Run it with:

```sh
go test ./...
```
