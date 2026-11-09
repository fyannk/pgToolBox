---
sidebar_position: 6
title: Images and allow-lists
---

# Images and allow-lists

## What runs

| Image | Repository | Chosen by |
|---|---|---|
| Operator | `ghcr.io/fyannk/pgtoolbox-operator` | `image.repository` and `image.tag` (default: the chart's `appVersion`) |
| Console | `ghcr.io/fyannk/pgtoolbox-console` | `console.image.*` |
| Evidence agent | `ghcr.io/fyannk/pgtoolbox-evidence-agent` | `operator.evidenceAgent.image`, else `imageRepository` at `appVersion` |
| pgAdmin | `ghcr.io/fyannk/pgadmin`, hardened, **pinned by digest** in the operator | `operator.pgAdmin.image` (`PGADMIN_IMAGE`) |
| OpenShift oauth-proxy | `quay.io/openshift/origin-oauth-proxy:4.18`, pinned by digest | `operator.oauthProxyImage` (`OAUTH_PROXY_IMAGE`) |

The operator image is also the one copied into every pgAdmin Pod by an init
container, so the credential sidecar and the registration Jobs run the operator's
own binary under pgAdmin's UID.

All images are published to `ghcr.io/fyannk` by the release workflow, signed,
and listed in the release's `digests.txt` ([verify](../release.md)). Prefer digests everywhere in production.

## `spec.image`

`PgAdmin.spec.image`, `PgAdmin.spec.auth.openshiftOAuthProxy.image` and
`EvidenceAgent.spec.image` take the same shape (`ImageSpec`): `repository`
(required), `tag` and/or `digest` (at least one), `pullPolicy`, `pullSecrets`.

- A digest is what the Pod runs; the tag is then informative.
- Pull policy defaults to `IfNotPresent` with a digest and `Always` with a tag alone.
- A tag alone is accepted, but the resolved reference in `status.image` shows
  that it is not pinned.
- Omitted, the operator's default applies, echoed in `status.image`.

## Why images are gated

The agent Pod mounts the object-store credentials; the pgAdmin Pod holds database
credentials. **Whoever can choose the image of either runs code with those
credentials.** The installer bounds the choice, and you can add an admission gate.

### The allow-lists (always on)

- `EvidenceAgent.spec.image`: `--evidence-agent-image-repositories` (chart:
  `operator.evidenceAgent.imageRepositories`).
- `PgAdmin.spec.image` and the oauth-proxy image: `--pgadmin-image-repositories` or
  `PGADMIN_IMAGE_REPOSITORIES` (chart: `operator.pgAdmin.imageRepositories`).

Each is a comma list of repositories: an exact repository, or a prefix ending in
`/`. **Empty means each image's default repository only**; set it to your mirror
when `PGADMIN_IMAGE` or `OAUTH_PROXY_IMAGE` point at one. A custom resource outside
the list is `ConfigurationValid=False` with reason `ImageNotAllowed` and nothing is
rendered. With no `spec.image` the default is used; for an `EvidenceAgent` with no
default either, the reason is `ImageRequired`.

### The admission policy

`operator.admissionPolicy.enabled` (on by default in the chart; kustomize and OLM do not
apply it, see [security](../security.md)) renders a `ValidatingAdmissionPolicy` (CEL, no
webhook) under which only the listed users and groups
may set `spec.image` on a `PgAdmin` or an `EvidenceAgent`. Everyone else uses the
operator's default. It adds the requester's identity, which the allow-list cannot
see.

The same policy carries the rule that keeps a `PgToolBoxConsole` or `PgAdmin` from
naming a Secret its editor cannot read; see
[Security](../security.md#secret-references-need-get).

## Air-gapped and mirrored registries

Set the image values to your mirror (`global.imageRegistry` prefixes the console;
the operator values are full references). The OLM CSV's `relatedImages` lists the
images for `oc mirror`. On OpenShift, set `operator.oauthProxyImage` to the image
from the cluster's release payload.
