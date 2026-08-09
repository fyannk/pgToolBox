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

## Identity, whichever provider authenticated

`subject` is matched case-insensitively against whatever identity the
proxy established, and the matching is the same for every provider:

| Provider | Where `subject` comes from |
|---|---|
| `local` | the username typed into the proxy's own form; `localPasswordSecretRef` holds a **bcrypt hash** |
| `oidc` | the subject the identity provider returns |
| `openshift` | the OpenShift username |

`localPasswordSecretRef` is optional, and its presence is what decides
whether this user can use the local form. A federated user carries none:
the identity provider holds the credential, and this object only says what
level that identity gets. With `local` enabled alongside an identity
provider, giving one to a break-glass account is how somebody still gets
in when the provider is down.

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
