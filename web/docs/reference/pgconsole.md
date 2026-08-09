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
    authentication:                   # one block per provider, any mix
      local: {}
      oidc: { issuerURL, clientID, clientSecretRef: { name, key? } }
      openshift: {}
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
  podSecurityContext:
    fsGroup: 65532                      # leave unset on OpenShift
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

`monitoringURL` must use `https` unless `allowInsecureLinks: true`.

The pgAdmin link-out is derived, not configured: it is the root-relative
`/pgadmin`, so it traverses the proxy like every other request and obeys
`pgAdmin.accessMinLevel`. Being same-origin, it resolves correctly
however the console was reached — an ingress hostname, a Route, or a
`kubectl port-forward` — which an absolute URL cannot do, since a
`clusterIP` console has no external address the operator could name.

:::note
This needs pgConsole **0.2.0 or newer**, which accepts root-relative
link-outs for same-origin siblings. Against 0.1.x the console validates
link-outs as absolute URLs only and refuses to start.
:::

Deliberately absent: the application's history and metrics journals.
Both imply a PersistentVolumeClaim and pin the console to a single
replica, which is a storage decision this API does not yet make. History
and metrics are retained in memory and live with the process.

## `podSecurityContext.fsGroup`

The console Pod's hardening — non-root, dropped capabilities, read-only
root filesystem, the runtime-default seccomp profile — is the operator's
to set and is not configurable. `fsGroup` is the one exception, because
only the platform knows what value is admissible.

It matters for the **evidence sidecar**. pgObjectStoreViewer refuses to
serve unless the shared socket directory is setgid with group rwx, and on
a Pod's `emptyDir` that mode comes from the kubelet applying `fsGroup`.
With none applied the directory is `0777` with no setgid, and the viewer
reports an invalid socket path instead of serving evidence.

| Platform | What to do |
|---|---|
| OpenShift | Leave it unset. The `restricted-v2` SCC allocates an fsGroup from the namespace's range, and a value outside that range is rejected outright. |
| Kubernetes that defaults an fsGroup | Leave it unset. |
| Kubernetes that does not | Set it. Without it the evidence sidecar cannot come up; nothing else in the console is affected. |

:::note
This only bites with `evidence.enabled: true`, which is not the default.
:::

## Notes

- `cnpgClusterRef` is immutable; a console is dedicated to one cluster for
  its whole life.
- `proxy.authentication` must enable at least one provider, and may
  enable several: the login page then shows the local form with the others
  as buttons beside it. A local account is how the first administrator
  gets in, and how anyone gets in when the identity provider is down.
- `evidence.image` is a pointer: omitting it genuinely means "no image"
  rather than an invalid empty object.
- Omitting the whole `console` block deploys the console the application
  ships: operations, logs and the review panel on, catalogs off, and
  every tunable at the application's default.
