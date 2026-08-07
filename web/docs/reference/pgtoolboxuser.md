# PgToolBoxUser

One identity on one console.

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxUser
metadata:
  name: jane
spec:
  pgConsoleRef: { name: main }
  subject: jane@corp.example          # IdP identity (email/sub/username)
  roleRef: { name: dba }
  localPasswordSecretRef:             # local mode only
    name: jane-local-password
    key: password                     # default
```

## Behavior

- The PgConsole controller resolves the user once per reconcile: role,
  level, local bcrypt hash (local mode), and postgres credential.
- A user with a missing role, a duplicate subject, or an unreadable local
  password is **degraded** — left out of the proxy config and pgAdmin sync
  without failing the rest of the console.
- Deleting the user de-provisions them from the proxy configuration and
  pgAdmin on the next reconcile.

## Status

`observedGeneration`, `proxySynced`, `pgAdminSynced`, and conditions:
`RoleReady`, `ProxySynced`, `PgAdminSynced`.
