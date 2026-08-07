# Configuration

## Operator flags

| Flag | Default | Meaning |
|---|---|---|
| `--operator-image` | baked at build time | operator image used by the pgAdmin admin-sync init container |
| `--default-pgtoolbox-proxy-image` | `""` | proxy image when a `PgConsole` spec names none |
| `--default-pgconsole-image` | `""` | pgconsole image when a spec names none |
| `--default-pgadmin-image` | `""` | pgAdmin image when a spec names none |
| `--default-objectstoreviewer-image` | `""` | evidence sidecar image when a spec names none |
| `--leader-elect` | `true` | leader election |
| `--metrics-bind-address` | `:8080` | metrics listener |
| `--health-probe-bind-address` | `:8081` | health/readiness listener |

The Helm chart renders all of these flags (the `--default-*-image` ones
only when non-empty). The OLM CSV renders `--leader-elect`, the bind
addresses and `--operator-image`. In both, `--operator-image` always
reflects the deployed operator image.

## PgConsole spec highlights

```yaml
spec:
  cnpgClusterRef: { name: pg-main }      # immutable, same namespace
  proxy:
    authentication:
      mode: openshift | oidc | local
      oidc: { issuerURL, clientID, clientSecretRef }   # mode=oidc only
  pgAdmin:
    enabled: true
    accessMinLevel: dba                  # dba | poweruser
    storage: { size: 1Gi, storageClass: "" }
  evidence:
    enabled: false
    image: { ... }
  exposure:
    type: clusterIP | route | ingress | gateway
    hostname: pgconsole.apps.example.com
  networkPolicy:
    enabled: true
    policyTypes: full                    # full | ingress
```

### Authentication modes

- **oidc** — authorization-code flow with PKCE against any OIDC provider;
  the client Secret is mounted from `clientSecretRef`.
- **openshift** — the console ServiceAccount is the OAuth client; the
  operator adds the `oauth-redirecturi` annotation from `exposure.hostname`.
- **local** — bcrypt accounts rendered from `PgToolBoxUser`
  `localPasswordSecretRef` Secrets (key `password`).

### Levels

`X-PgToolBox-Level` is set from the user's `PgToolBoxRole.level`:
`view` < `poweruser` < `dba`. Unknown authenticated users get `none` and
can only file an access request.

## PgToolBoxRole profiles

| Profile | DatabaseRole shape |
|---|---|
| `monitor` | `inRoles: [pg_monitor]` |
| `database-readonly` | `inRoles: [pg_read_all_data]` |
| `database-owner` | `createdb` + `createrole` |

`databaseRoleRef` selects an existing `DatabaseRole` instead.
