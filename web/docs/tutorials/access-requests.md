# Access requests

The self-service flow for identities the console does not know yet.

## 1. An unknown user signs in

The proxy authenticates them at the IdP, finds no `PgToolBoxUser` for their
subject, and issues a `none`-level session. Every route shows the styled
403 page with a request-access form; the form's CSRF token is bound to that
session.

## 2. They file a request

Submitting the form makes the proxy create a `PgToolBoxAccessRequest`:

```bash
kubectl get pgreq -n app-db
```

The proxy only ever calls `create pgtoolboxaccessrequests` — it has no
code path to read or decide the requests it files.

## 3. A dba reviews

A user with a `dba` role opens the console's access-request panel, sees the
pending request (subject, message, age), and approves with a chosen
`PgToolBoxRole` or denies. The panel writes only the request's **status**:

```yaml
status:
  state: approved
  requestedRoleRef: { name: app-readonly }
  decidedBy: jane@corp.example
  decidedAt: "2026-07-29T12:00:00Z"
```

## 4. The operator grants access

The `PgToolBoxAccessRequest` controller materializes the `PgToolBoxUser`
(deterministic name, idempotent), the PgConsole controller renders them
into the proxy configuration, and pgAdmin sync provisions their account
once the backing `DatabaseRole` is applied.

```bash
kubectl get pgreq pgreq-abcde -n app-db -o yaml | yq '.status.conditions'
# Decided=True, UserReady=True
```

The requester reloads the console and is in.
