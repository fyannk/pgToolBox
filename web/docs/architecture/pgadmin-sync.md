# Embedded pgAdmin user/server sync

## The pattern

The operator never execs into the console pod. Instead, the manager binary
itself is injected into the pod:

1. An `admin-sync-init` init container copies the operator binary out of
   the operator image into a shared emptyDir.
2. The `admin-sync` sidecar runs that binary as an HTTPS+mTLS API inside
   the pod, next to pgAdmin.
3. The operator posts the complete desired state — one entry per
   `PgToolBoxUser`, including the postgres password — over the in-pod mTLS
   API. The sidecar writes the combined pgpass file itself and calls
   pgAdmin's supported `setup.py` CLI (`update-external-user`,
   `load-servers --replace`).

The connection pins a per-console self-signed CA and a shared bearer token,
both generated once into the `<console>-pgconsole-pgadmin-sync` Secret. The
Secret's resourceVersion rides on the pod template, so regeneration rolls
the pod exactly once.

## What gets synced

Per `PgToolBoxUser` whose backing `DatabaseRole` is applied:

- the pgAdmin account (`dba` level → Administrator, anything else → User);
- one shared server definition for the console's cluster, pointing at the
  read-write Service, authenticated with the saved password of the user's
  postgres role.

pgAdmin sync waits for the console rollout to reach the current config
revision and for CloudNativePG to report the `DatabaseRole` applied with
the current password Secret resourceVersion — a password is never stored
before it works.

## Idempotency

The desired-state payload is hashed (`pgadmin-sync-revision` annotation on
the Deployment); the operator skips re-applying an unchanged state, and
`load-servers --replace` plus the rewritten pgpass make replays converge
instead of compound. Removing a user removes them from the payload, the
pgpass, and the rendered proxy configuration on the next reconcile.
