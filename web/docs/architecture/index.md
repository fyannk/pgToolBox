# Architecture

pgToolBox is a Kubernetes operator that manages, per CloudNativePG cluster,
a complete access stack: authentication, an observation console, and
pgAdmin — with declarative user/role provisioning.

```mermaid
flowchart TB
    subgraph cluster["Namespace"]
        cnpg["CNPG Cluster"]
        subgraph pod["PgConsole pod"]
            proxy["pgtoolbox-proxy"]
            console["pgconsole"]
            pgadmin["pgAdmin"]
            sync["admin-sync sidecar"]
            viewer["objectstoreviewer (optional)"]
        end
        proxy --> console
        proxy --> pgadmin
        console --> viewer
        sync --> pgadmin
    end
    op["pgToolBox operator"] --> pod
    op -. reads .-> cnpg
    idp["OIDC / OpenShift / local"] --> proxy
```

## Components

- **pgtoolbox-proxy** — purpose-built slim OAuth2/OIDC proxy. Not an
  oauth2-proxy fork: the fork would require resurrecting htpasswd, adding
  an OpenShift provider, and carrying behavior upstream would never accept
  (operator-rendered user lists, access-request flow, level header
  injection).
- **pgconsole** — the observation UI. Kubernetes API only, never SQL, zero
  Secret access, one cluster per namespace.
- **pgAdmin** — embedded in the PgConsole pod, dedicated to the one
  cluster. Its settings DB is written only through the admin-sync
  mechanism, never rendered-and-replaced wholesale.
- **objectstoreviewer** — optional evidence sidecar, consumed over a
  pod-private Unix socket plus bearer token.

## Invariants

- CNPG 1.30+ only (`DatabaseRole` CRD always exists).
- The CNPG `Cluster` object is read-only.
- No secret material in status, logs, or events.
- Deterministic byte-stable rendering: a no-op reconcile updates nothing.
- Apps stay auth-free; the proxy is the only auth boundary and the
  generated NetworkPolicy ensures nothing bypasses it.

## The console's authority

The Roles the operator generates are the console pod's entire authority —
its ServiceAccount holds no other credential — and their shape is the
console application's own deploy manifest, rule for rule. That
correspondence is load-bearing in an unobvious direction: a missing grant
does not fail the console, it makes the affected screen report "not
granted" forever, which reads as a broken console rather than as a
missing rule.

Three Roles, in decreasing scope:

| Object | Contents | Present when |
|---|---|---|
| read `Role` | the namespaced reads behind every screen: the Cluster and its backups, poolers, declared database objects, pods, services, claims, events, and the further objects the Cluster owns | always |
| operate `Role` | the four day-2 mutations, and the access-request read plus status patch | either capability is on |
| catalog `ClusterRole` | `get` on `clusterimagecatalogs`, never `list`, never `watch` | `console.allowClusterCatalogs` |

Two rules hold across all three. Nothing is granted on `secrets` — RBAC
cannot express "metadata only", so the console's children drawing states
Secrets as not granted rather than reading them. And the reads pinned to
one object (`clusters`, `failoverquorums`) carry `get` by
`resourceNames` plus an unpinnable `watch`, and never a `list`, so
nothing namespace-wide is enumerated through them.

Deep dives: [Authentication](authentication.md),
[pgAdmin sync](pgadmin-sync.md).
