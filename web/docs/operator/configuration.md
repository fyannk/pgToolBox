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
  console:
    allowOperations: true                # day-2 operations
    allowLogs: true                      # bounded instance log tail
    allowAccessReview: true              # dba review panel
    allowClusterCatalogs: false          # opt-in ClusterRole
    monitoringURL: https://grafana.example.com/d/pg
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

The proxy strips any inbound copy of the identity and level headers before
setting its own, and the console reads them because the generated
NetworkPolicy confines its ingress to the proxy. There is no ungated
baseline in the console: `view` reaches the overviews and the metrics
screens, `poweruser` adds the remaining read screens, and `dba` adds the
day-2 operations, the access-request review panel and the pgAdmin
link-out.

### Console capabilities

The `console` block decides which of those screens exist at all. Each
capability moves the flag *and* the authority: switching one off removes
the matching rules from the generated Roles, so RBAC denies the operation
whatever the application is told. See
[the PgConsole reference](../reference/pgconsole.md#the-console-block)
for the full field set and its bounds.

## PgToolBoxRole profiles

| Profile | DatabaseRole shape |
|---|---|
| `monitor` | `inRoles: [pg_monitor]` |
| `database-readonly` | `inRoles: [pg_read_all_data]` |
| `database-owner` | `createdb` + `createrole` |

`databaseRoleRef` selects an existing `DatabaseRole` instead.
