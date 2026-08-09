# pgToolBox

**One declarative access stack per CloudNativePG cluster** — an
authentication proxy, an observation console, and embedded pgAdmin — with
declarative user and role provisioning.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

pgToolBox is a Kubernetes/OpenShift operator. For each `PgConsole` you
declare, it composes a dedicated pod next to one
[CloudNativePG](https://cloudnative-pg.io) cluster:

```
┌─ PgConsole pod ────────────────────────────────────────────┐
│  pgtoolbox-proxy  (auth: OIDC / OpenShift / local)         │
│  pgconsole        (observation UI, K8s API only)           │
│  pgadmin          (embedded, dedicated to this cluster)    │
│  objectstoreviewer (evidence sidecar, optional)            │
└────────────────────────────────────────────────────────────┘
```

Users are plain Kubernetes objects too: a `PgToolBoxUser` binds an
identity to one of the three console authorization levels (`view` /
`poweruser` / `dba`), and a
`PgToolBoxAccessRequest` lets an unknown authenticated user ask a `dba`
for access from the proxy's 403 page.

## Features

- **One console per cluster**, never mutualized: dedicated proxy, dedicated
  pgAdmin, dedicated pgpass — security isolation beats resource sharing.
- **pgtoolbox-proxy** as the single authentication and coarse authorization
  boundary: OIDC (PKCE S256), OpenShift service-account OAuth, and local
  bcrypt accounts rendered from `PgToolBoxUser` — any mix at once, so a
  local account is still the way in when the identity provider is down.
- **A declared first administrator**: `bootstrapAdmin` is required on every
  console and materialized as a `dba` the operator puts back if deleted, so
  a console can never be deployed with nobody able to approve access.
- **pgAdmin is embedded** behind the same proxy, reachable at `/pgadmin`
  by sessions at or above `pgAdmin.accessMinLevel`.
- **Embedded pgAdmin user/server sync** through an in-pod mTLS admin-sync
  API; the operator never execs into the pod.
- **Every exposure type**: OpenShift `Route`, `Ingress`, Gateway API
  `HTTPRoute`, or plain `ClusterIP`, behind a generated default-deny
  `NetworkPolicy`.
- **Deterministic and safe**: byte-stable rendering, rollouts keyed on a
  config checksum, hardened pods, Secret contents never in the operator's
  cache and never logged.
- **Packaging for every install path**: Helm chart (`deploy/helm/pgtoolbox`),
  OLM bundle and file-based catalog (`deploy/olm`), and a kustomize
  development overlay (`config/default`).

## Quick start

Prerequisites: Kubernetes ≥ 1.28 or OpenShift ≥ 4.14, and
[CloudNativePG](https://cloudnative-pg.io) ≥ 1.30.

```bash
helm install pgtoolbox deploy/helm/pgtoolbox \
  --namespace pgtoolbox --create-namespace \
  --set image.repository=pgtoolbox \
  --set image.tag=<version> \
  --set proxyImage=pgtoolbox-proxy:<version>
```

Then declare a console, a role, and a user:

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgConsole
metadata:
  name: main
spec:
  cnpgClusterRef: { name: pg-main }
  proxy:
    authentication:
      bootstrapAdmin:
        subject: jane@corp.example
      local: {}                        # keeps a way in when the IdP is down
      oidc:
        issuerURL: https://idp.example.com
        clientID: pgconsole
        clientSecretRef: { name: pgconsole-oidc }
  exposure:
    type: ingress
    hostname: pgconsole.apps.example.com
---
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxUser
metadata:
  name: jane
spec:
  pgConsoleRef: { name: main }
  subject: jane@corp.example
  level: dba
```

Jane signs in through the proxy and sees the console sized to her `dba`
level, including `/pgadmin`. Roles and users configure the proxy only —
they are not postgres roles, and nothing about them reaches the database.

## Documentation

- [Full documentation site](web/) (Docusaurus) — install, operations,
  architecture, CRD reference, tutorials

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache 2.0](LICENSE).
