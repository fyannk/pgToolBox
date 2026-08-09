# PgToolBoxUser

One identity, one console, one authorization level.

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxUser
metadata:
  name: jane
spec:
  pgConsoleRef: { name: main }
  subject: jane@corp.example
  level: view | poweruser | dba
  localPasswordSecretRef: { name: jane-password, key: password }
```

## The level

The set is closed — `view`, `poweruser`, `dba` — and hardcoded on both
sides: the proxy asserts it in `X-PgToolBox-Level`, the console maps it
onto its own ladder. There is nothing an operator could add, so it is a
field here rather than a reference to an object that would only ever
carry the same word back.

## Identity, in every authentication mode

`subject` is matched case-insensitively against whatever identity the
proxy established, and the matching is the same in all three modes:

| Mode | Where `subject` comes from |
|---|---|
| `local` | the username typed into the proxy's own form; `localPasswordSecretRef` holds a **bcrypt hash** |
| `oidc` | the subject the identity provider returns |
| `openshift` | the OpenShift username |

So an OIDC deployment declares one `PgToolBoxUser` per person, with no
`localPasswordSecretRef` — the identity provider holds the credential,
and this object only says what level that identity gets.

:::note
There is no mapping from provider groups or claims to levels yet. Access
is granted per identity, either by declaring the object or by a `dba`
approving a [PgToolBoxAccessRequest](pgtoolboxaccessrequest.md).
:::

## Status

| Field | Meaning |
|---|---|
| `proxySynced` | rendered into the proxy configuration |
| `observedGeneration` | last reconciled generation |
| `conditions` | `Ready`, `ProxySynced` — see [Conditions](conditions.md) |
