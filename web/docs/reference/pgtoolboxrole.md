# PgToolBoxRole

One console authorization level backed by one postgres role.

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxRole
metadata:
  name: dba
spec:
  pgConsoleRef: { name: main }
  level: view | poweruser | dba
  postgresRole:
    profile: monitor | database-readonly | database-owner
    # or: databaseRoleRef: { name: existing-databaserole }
```

Exactly one of `profile` or `databaseRoleRef` must be set (CEL).

## Profile shapes

| Profile | DatabaseRole |
|---|---|
| `monitor` | `inRoles: [pg_monitor]` |
| `database-readonly` | `inRoles: [pg_read_all_data]` |
| `database-owner` | `createdb` + `createrole` |

## Operator behavior

- A profile role materializes a CNPG `DatabaseRole` named
  `<role>-pgrole` and a basic-auth Secret `<role>-pgrole-credentials` with a
  generated password (long role names are truncated with a hash suffix to
  stay within the name length limit).
- `status.databaseRoleName` reports the backing DatabaseRole.
- For profile roles, `CredentialReady` only turns true once CNPG reports
  the DatabaseRole applied with the current Secret resourceVersion. For
  `databaseRoleRef` roles it turns true once the referenced DatabaseRole
  names a password Secret.
- Deleting the role deletes the managed DatabaseRole and Secret.

## Status

`databaseRoleName`, `observedGeneration`, and conditions: `Ready`,
`PgConsoleReady`, `DatabaseRoleReady`, `CredentialReady`.
