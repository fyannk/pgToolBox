# Concepts

## The proxy is the only auth boundary

The console application and pgAdmin are **auth-free by design**. They trust
two headers set by `pgtoolbox-proxy`:

- `X-Forwarded-User` — the verified identity.
- `X-PgToolBox-Level` — the authorization level (`view` / `poweruser` /
  `dba`).

The headers are trustworthy because the generated `NetworkPolicy` confines
ingress to the proxy container: nothing can reach the console or pgAdmin
except through the proxy.

## Levels

| Level | Console | pgAdmin |
|---|---|---|
| `view` | read-only panels | not admitted by default |
| `poweruser` | day-2 operations (backup, reload, restart, promote) | admitted when `accessMinLevel: poweruser` |
| `dba` | operations + access-request review | admitted (default minimum) |

Levels come from the user's `PgToolBoxRole`. Unknown-but-authenticated
users get a `none`-level session that can only file an access request.

## Postgres roles

A `PgToolBoxRole` names a console authorization level. It is proxy
configuration and nothing more: it is not a postgres role, and nothing
about it reaches the CloudNativePG cluster.

The password reaches pgAdmin over the in-pod admin-sync mTLS API and lands
in a pod-private pgpass file; it is never logged and never put in status.

## Deterministic rendering

Every generated object is a pure function of the spec plus a config
checksum annotation, so a no-op reconcile issues zero API writes. Secret
content is never held in the informer cache.
