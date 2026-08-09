# Getting started

End-to-end on stock Kubernetes with CNPG and OIDC.

## 1. Install the operator

```bash
helm install pgtoolbox deploy/helm/pgtoolbox \
  --namespace pgtoolbox --create-namespace \
  --set image.repository=<registry>/pgtoolbox \
  --set image.tag=<tag> \
  --set proxyImage=<registry>/pgtoolbox-proxy:<tag>
```

## 2. Declare the console

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgConsole
metadata:
  name: main
  namespace: app-db
spec:
  cnpgClusterRef: { name: pg-main }
  proxy:
    authentication:
      bootstrapAdmin:
        subject: jane@corp.example
      local: {}
      oidc:
        issuerURL: https://idp.example.com
        clientID: pgconsole
        clientSecretRef: { name: pgconsole-oidc }
  exposure:
    type: ingress
    hostname: pgconsole.apps.example.com
```

Wait for `Ready=True`:

```bash
kubectl get pgc -n app-db main -w
```

## 3. Sign in

Jane needs nothing declared: she is the console's `bootstrapAdmin`, so the
operator already materialized her as a `dba`. She opens
`https://pgconsole.apps.example.com`, picks the SSO button, signs in at the
IdP, and lands on the console. `/pgadmin` opens with the cluster's own
credentials already configured.

## 4. Grant access to everyone else

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxUser
metadata:
  name: sam
  namespace: app-db
spec:
  pgConsoleRef: { name: main }
  subject: sam@corp.example
  level: view
```

Sam authenticates at the IdP because this object carries no
`localPasswordSecretRef`. Add one and he can also use the form on the same
page — which is how a break-glass account gets in when the IdP is down.

Sam could instead sign in first, land on the 403 page, and file a
`PgToolBoxAccessRequest`; Jane approves it in the review panel and the
operator materializes the same object.

## 5. Verify

```bash
kubectl get pguser -n app-db
kubectl get pguser main-bootstrap-admin -n app-db -o yaml | yq '.status'
```

`ProxySynced` and `PgAdminSynced` true on the user. The bootstrap admin is
owned by the console: deleting it just gets it back.
