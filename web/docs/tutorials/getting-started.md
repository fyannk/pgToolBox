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

## 3. Grant access

```yaml
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgToolBoxUser
metadata:
  name: jane
  namespace: app-db
spec:
  pgConsoleRef: { name: main }
  subject: jane@corp.example
  level: dba
```

Jane opens `https://pgconsole.apps.example.com`, picks the SSO button,
signs in at the IdP, and lands on the console at `dba` level. `/pgadmin`
opens with the cluster's own credentials already configured.

Jane authenticates at the IdP because her `PgToolBoxUser` carries no
`localPasswordSecretRef`. Give one to a break-glass account and it can
sign in at the form on the same page, IdP or no IdP.

## 4. Verify

```bash
kubectl get pguser jane -n app-db -o yaml | yq '.status'
```

`ProxySynced` and `PgAdminSynced` true on the user.
