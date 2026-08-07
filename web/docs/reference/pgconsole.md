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

## Notes

- `cnpgClusterRef` is immutable; a console is dedicated to one cluster for
  its whole life.
- `proxy.authentication.oidc` is required exactly when `mode: oidc`.
- `evidence.image` is a pointer: omitting it genuinely means "no image"
  rather than an invalid empty object.
