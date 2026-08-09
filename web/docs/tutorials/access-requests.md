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
level or denies. The panel is served when
`spec.console.allowAccessReview` is true, which is the default; with it
false the console serves no panel and holds no authority over requests at
all, so a request filed by the proxy waits until someone decides it with
`kubectl`. The panel writes only the request's **status**:

```yaml
status:
  state: approved
  requestedLevel: view
  decidedBy: jane@corp.example
  decidedAt: "2026-07-29T12:00:00Z"
```

## 4. The operator grants access

The `PgToolBoxAccessRequest` controller materializes the `PgToolBoxUser`
(deterministic name, idempotent), and the PgConsole controller renders it
into the proxy configuration. If the granted level reaches
`spec.pgAdmin.accessMinLevel`, pgAdmin admits that identity too — there is
no postgres role to create first, because the connections pgAdmin offers
are the cluster's own credentials rather than anything derived from who
signed in.

```bash
kubectl get pgreq pgreq-abcde -n app-db -o yaml | yq '.status.conditions'
# Decided=True, UserReady=True
```

The requester reloads the console and is in.
