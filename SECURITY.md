# Security Policy

## Supported versions

Security fixes are provided for the latest published release. pgToolBox is
pre-1.0: only the most recent 0.x minor receives fixes, and there are no
backports to earlier ones.

## Reporting a vulnerability

Please do not open a public issue for a suspected vulnerability. Use
[GitHub private vulnerability reporting](https://github.com/fyannk/pgToolBox/security/advisories/new)
so the report and any supporting material remain confidential.

Include the affected version or commit, deployment configuration, impact,
reproduction steps, and any suggested mitigation when available. Do not
include live credentials, ServiceAccount tokens, customer data, or other
secrets in the report.

Public disclosure should wait until a fix or mitigation is available and a
coordinated disclosure date has been agreed.

## Scope

pgToolBox composes an access stack from components built in their own
repositories. A vulnerability in the console UI belongs to
[pgConsole](https://github.com/fyannk/pgConsole), and one in the repository
evidence scanner to
[pgObjectStoreViewer](https://github.com/fyannk/pgObjectStoreViewer); report
those there. This repository owns the operator, the `pgtoolbox-proxy`
authentication proxy, and everything the operator generates — the Roles, the
NetworkPolicy, the proxy configuration, and the pod composition.

Reports about the boundaries between them belong here, because the operator
is what draws them. In particular:

- anything that reaches a console screen or pgAdmin without passing the
  proxy, or with a level the proxy did not assert;
- any generated Role granting more than the component it serves needs,
  especially any grant on `secrets`;
- any path by which secret material reaches a status field, an event, or a
  log line.
