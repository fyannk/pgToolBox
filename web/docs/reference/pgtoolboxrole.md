# PgToolBoxRole

One console authorization level, for one PgConsole.

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxRole
metadata:
  name: dba
spec:
  pgConsoleRef: { name: main }
  level: view | poweruser | dba
```

## What a role is, and is not

A role is **pgtoolbox-proxy configuration**. Its level is what the proxy
asserts in `X-PgToolBox-Level` for the sessions of the users bound to it,
and that is the whole of its effect.

It is **not** a postgres role, and it has no relationship with the
CloudNativePG cluster. Nothing about a role reaches the database: pgAdmin
connects with the cluster's own credentials, never with anything derived
from who signed into the console.

## Status

| Field | Meaning |
|---|---|
| `observedGeneration` | last reconciled generation |
| `conditions` | `Ready`, `PgConsoleReady` — see [Conditions](conditions.md) |

The controller creates nothing. It resolves the referenced `PgConsole` and
reports whether it exists; the `PgConsole` controller watches roles and
renders them into the proxy configuration.
