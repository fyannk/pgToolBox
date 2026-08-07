# Labels and annotations

Label and annotation keys under `pgtoolbox.fyannk.dev/` are frozen API
surface: extended, never renamed.

## Labels

| Key | Meaning |
|---|---|
| `app.kubernetes.io/managed-by=pgtoolbox` | every generated resource |
| `pgtoolbox.fyannk.dev/pgconsole` | owning PgConsole |
| `pgtoolbox.fyannk.dev/pgtoolboxrole` | owning PgToolBoxRole |
| `pgtoolbox.fyannk.dev/pgtoolboxuser` | owning PgToolBoxUser |
| `pgtoolbox.fyannk.dev/evidence-token` | evidence token Secrets (for GC) |

## Annotations

| Key | Meaning |
|---|---|
| `pgtoolbox.fyannk.dev/config-checksum` | rendered proxy config checksum on the pod template; the rollout trigger |
| `pgtoolbox.fyannk.dev/admin-sync-secret-version` | admin-sync Secret resourceVersion on the pod template |
| `pgtoolbox.fyannk.dev/pgadmin-sync-revision` | sha256 of the last applied pgAdmin sync payload (on the Deployment) |
| `pgtoolbox.fyannk.dev/rotate-evidence-token` | set to `"now"` on a PgConsole to rotate the pod-local evidence token |
| `pgtoolbox.fyannk.dev/reconcile` | set to `"skip"` to pause reconciliation of a resource |

## Finalizers

| Key | Controller |
|---|---|
| `pgtoolbox.fyannk.dev/pgconsole` | PgConsole |
| `pgtoolbox.fyannk.dev/pgtoolboxrole` | PgToolBoxRole |

## OpenShift

| Key | Meaning |
|---|---|
| `serviceaccounts.openshift.io/oauth-redirecturi.pgconsole` | the exposure URL, set on the console ServiceAccount in openshift auth mode |
