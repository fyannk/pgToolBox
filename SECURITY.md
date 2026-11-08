# Security policy

## Scope

This repository holds the whole product, and vulnerabilities in any of it are
reported here: the operator, the console, the evidence agent, the two Helm
charts, the kustomize and OLM packaging, and the nested `pkg/evidence` module.
The pgAdmin image (`ghcr.io/fyannk/pgadmin`) is built in its own repository,
[fyannk/pgAdmin](https://github.com/fyannk/pgAdmin); report a vulnerability in the
image itself there, and one in how pgToolBox deploys or configures it here.

Reports about the boundaries between components are the most valuable, because
those boundaries are the product's security model. In particular:

- anything that reaches a console screen, an API response, the MCP endpoint or
  pgAdmin without a signed-in identity, or that the API server's
  SubjectAccessReview did not allow for that identity;
- any write the console makes with its own identity beyond the documented
  restore-drill cleanup, or any impersonation;
- any generated Role granting more than the component it serves needs,
  especially any grant on `secrets`;
- any path by which secret material reaches a status field, an event or a log
  line;
- any path by which object-store credentials reach the console, or by which the
  evidence agent claims more than it read.

## Supported versions

| Version | Supported |
|---|---|
| 0.1.x | yes |
| older | no |

pgToolBox is pre-1.0: security fixes go to the latest 0.x minor, as a patch
release, and are not backported to earlier minors.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Report it privately
through GitHub's private vulnerability reporting:
<https://github.com/fyannk/pgToolBox/security/advisories/new>.

Include:

- the affected version, and the **image digest** of each component involved
  (`kubectl get pod -o jsonpath='{..imageID}'` shows it);
- the deployment configuration (chart values or custom resources, with secrets
  removed), the Kubernetes or OpenShift version and the CloudNativePG version;
- the impact, reproduction steps and any suggested mitigation.

Do not include live credentials, ServiceAccount tokens, customer data or other
secrets. Public disclosure waits until a fix or a mitigation is available and a
disclosure date has been agreed.

## How scan findings are triaged

Every image is scanned for known vulnerabilities, and the results for each
release are published on the documentation site's
[Security page](https://fyannk.github.io/pgToolBox/security/). Findings are
handled with three rules:

- **A fixable critical blocks the release.** A critical finding with a fixed
  version available is fixed (by updating the dependency or the base image)
  before the release is published.
- **A finding is dismissed only when it is demonstrably unreachable or not a
  vulnerability where it stands**: the vulnerable code is not compiled into the
  binary or the vulnerable function is never called, or (for code analysis) the
  flagged value is not secret or an external format mandates the construct. The
  dismissal names the exact package or file and records how that was shown, in
  [hack/security-accepted.txt](hack/security-accepted.txt), and stays visible on
  the Security page.
- **"No fix available" is never a reason to dismiss.** A finding without a fix
  stays open and visible until a fix exists or it is shown to be unreachable.

Which scanners run, on what, and which of them block a merge is in
[docs/site/docs/development/ci.md](docs/site/docs/development/ci.md#security);
private vulnerability reporting, code scanning and the other settings that make this
policy work are listed in
[docs/site/docs/development/repository-settings.md](docs/site/docs/development/repository-settings.md).

## Verifying a release

Images and charts are signed by digest with cosign, and each image carries an
SPDX SBOM and a SLSA provenance attestation. The documentation's
[release page](https://fyannk.github.io/pgToolBox/docs/release/) gives the exact
commands to verify a signature and an attestation. Pin by digest in anything that
deploys.
