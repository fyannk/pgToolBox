# PgToolBoxAccessRequest

One request for access to a console, created by the proxy's 403 form and
decided by a `dba`.

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxAccessRequest
metadata:
  generateName: pgreq-
spec:
  pgConsoleRef: { name: main }
  subject: new.user@corp.example      # set by the proxy, immutable
  message: "need dba for incident"
```

The proxy only ever calls `create` on this resource; the decision is
written **only to the status subresource**, so the filing path can never
self-approve.

## Status (written by the reviewer)

```yaml
status:
  state: pending | approved | denied
  requestedRoleRef: { name: dba }     # required when approved
  decidedBy: reviewer@corp.example
  decidedAt: "2026-07-29T12:00:00Z"
```

## Operator behavior

On `state: approved` with a valid `requestedRoleRef`, the
`PgToolBoxAccessRequest` controller materializes the `PgToolBoxUser` with a
deterministic name (`<console>-pguser-<hash>`), so repeated approvals of
the same identity converge. Denial keeps the object as an audit record.

Conditions: `Decided`, `UserReady`.
