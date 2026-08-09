# Troubleshooting

Read conditions first; the vocabulary below is the operator's contract.

```bash
kubectl get pgc -n <ns> <name> -o yaml | yq '.status.conditions'
kubectl get pguser,pgreq -n <ns>
```

## PgConsole

| Condition | Reason | Meaning and fix |
|---|---|---|
| `ClusterReady=False` | `ClusterNotFound` | The referenced CNPG `Cluster` does not exist. Create it or recreate the PgConsole (the reference is immutable). |
| `ProxyConfigReady=False` | `SecretNotFound` / `SecretKeyMissing` | The OIDC client Secret or key is missing. |
| `ProxyConfigReady=False` | `RenderFailed` | The rendered proxy config failed the proxy parser; check the authentication block and `accessMinLevel`. |
| `Ready=False` | `RolloutInProgress` | Rollout not complete; describe the pod. |
| `PgAdminSynced=False` | `PendingRollout` | Sync resumes when the pod reaches the current config revision. |
| `PgAdminSynced=False` | `SomeDegraded` | Some `PgToolBoxUser` could not be provisioned; read the user conditions. |
| `PgAdminSynced=False` | `SyncFailed` | The admin-sync sidecar call failed; check the `admin-sync` container logs and the admin-sync Secret. |
| `RepositoryEvidenceReady=False` | `ObjectStoreNotFound` | Evidence is enabled but no Barman `ObjectStore` is served/resolved. Disable evidence or install the Barman Cloud Plugin. |

## PgToolBoxUser

| Condition | Reason | Fix |
|---|---|---|
| `ProxySynced=False` | `SomeDegraded` | A `localPasswordSecretRef` that is missing or is not a bcrypt hash, or a subject already claimed by another user. The console's `bootstrapAdmin` holds its subject even when its own Secret is missing, so a second user naming it is the one dropped. |

## PgToolBoxAccessRequest

| Condition | Reason | Fix |
|---|---|---|
| `Decided=False` | `Pending` | Awaiting a dba reviewer. |
| `UserReady=False` | `PgConsoleNotFound` | The referenced PgConsole does not exist. |
| `UserReady=False` | `ConfigurationInvalid` | Approved without a `requestedLevel`. |
| `UserReady=False` | `RoleNotFound` | Re-approve with a valid role. |

## Runtime issues

**Login redirect loop.** The proxy derives its redirect URL from
`spec.exposure.hostname`; it must exactly match the external URL users
open. On OpenShift the `oauth-redirecturi` annotation on the console
ServiceAccount must name the same `https://` URL (set automatically from
`exposure.hostname`).

**`/pgadmin` returns 403.** The user's level is below
`spec.pgAdmin.accessMinLevel` (default `dba`).

**New user cannot log in.** Check `ProxySynced` — in local mode only users
with a valid bcrypt Secret are rendered.

**pgAdmin asks for a password.** The password Secret rotated and CNPG has
not applied it yet, or the Secret is incomplete.

**Traffic blocked.** The generated `NetworkPolicy` only admits the proxy
port by design.
