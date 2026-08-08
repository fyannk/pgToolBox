# PgConsole

One console per CNPG cluster, in the same namespace.

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgConsole
metadata:
  name: main
spec:
  cnpgClusterRef: { name: pg-main }     # immutable
  image: { repository, tag, digest?, pullPolicy?, pullSecrets? }
  proxy:
    image: { ... }
    authentication:
      mode: openshift | oidc | local
      oidc: { issuerURL, clientID, clientSecretRef: { name, key? } }
  pgAdmin:
    enabled: true
    image: { ... }
    accessMinLevel: dba                 # dba | poweruser
    storage: { size, storageClass? }
    resources: { ... }
  console:
    allowOperations: true               # day-2 operations
    allowLogs: true                     # bounded instance log tail
    allowAccessReview: true             # dba access-request review panel
    allowClusterCatalogs: false         # opt-in cluster-scoped catalog read
    allowInsecureLinks: false           # accept http link-out URLs
    monitoringURL: https://grafana.example.com/d/pg
    apiRequestTimeout: 10s              # 1s … 1m
    eventsMaxAge: 1h                    # 1m … 24h
    logTail: { lines: 200, maxBytes: 1048576 }
    metrics:
      enabled: true
      interval: 10s                     # 5s … 5m
      retention: 168h                   # 1h … 720h
    history:
      enabled: true
      maxRevisions: 2000                # 100 … 20000
      maxBytes: 16777216                # 1Mi … 64Mi
      perObjectRevisions: 20            # 2 … 200
      coalesceWindow: 1m                # 1s … 1h
  evidence:
    enabled: false
    image: { ... }
    resources: { ... }
  exposure:
    type: clusterIP | route | ingress | gateway
    hostname: pgconsole.apps.example.com
    tls: { termination: edge|reencrypt, certificateSecretRef? }
    ingressClassName: ""
    gateway: { parentRef: { name, namespace?, sectionName? } }
    annotations: { ... }
  networkPolicy:
    enabled: true
    policyTypes: full                   # full | ingress
    extraEgress: [ ... ]
  resources: { ... }
```

## Status

| Field | Meaning |
|---|---|
| `url` | external URL when exposed |
| `configRevision` | rendered proxy config checksum (`cfg-…`) |
| `userSync` | desired / synced / degraded user counters |
| `evidence` | sidecar enabled flag + active token Secret name |
| `observedGeneration` | last reconciled generation |
| `conditions` | see [Conditions](conditions.md) |

## The `console` block

Every field configures the pgconsole application itself. Two properties
are worth knowing before tuning them.

**A capability is a grant, not a flag.** Switching one off removes the
matching rules from the generated Roles as well as the routes from the
application, so RBAC denies the operation independently of the setting.
`allowLogs: false` drops `pods/log` from the read Role;
`allowOperations` and `allowAccessReview` each contribute their own rules
to the operate Role, and with both off the operate Role and its binding
are deleted rather than left empty.

**The bounds are the application's own.** The console validates its
configuration at startup and refuses to serve on a value it rejects, so
every range above is the range the application enforces. A `PgConsole`
that admission accepts cannot produce a Pod that crash-loops on its own
configuration; the controller re-checks the durations too and reports
`ConfigurationValid=False` naming the field.

`allowClusterCatalogs` is the one setting that reaches outside the
namespace. It generates a separate `ClusterRole` granting exactly `get`
on `clusterimagecatalogs` — never `list`, never `watch` — plus its
binding. A cluster-scoped object cannot be owned by a namespaced one, so
the operator removes the pair itself: when you switch the capability off,
and from the console's finalizer when you delete it. Declining the grant
costs a panel that reports the catalog content as unread, never as
absent.

`monitoringURL` must use `https` unless `allowInsecureLinks: true`. The
pgAdmin link-out follows the same rule and is derived, not configured:
it is `https://<exposure.hostname>/pgadmin`, so it traverses the proxy
like every other request and obeys `pgAdmin.accessMinLevel`. A console
with no exposure hostname has no `https` base URL, so it renders no
pgAdmin link rather than one the application would refuse to start on.

Deliberately absent: the application's history and metrics journals.
Both imply a PersistentVolumeClaim and pin the console to a single
replica, which is a storage decision this API does not yet make. History
and metrics are retained in memory and live with the process.

## Notes

- `cnpgClusterRef` is immutable; a console is dedicated to one cluster for
  its whole life.
- `proxy.authentication.oidc` is required exactly when `mode: oidc`.
- `evidence.image` is a pointer: omitting it genuinely means "no image"
  rather than an invalid empty object.
- Omitting the whole `console` block deploys the console the application
  ships: operations, logs and the review panel on, catalogs off, and
  every tunable at the application's default.
