# OpenShift

The OpenShift flavor: Route exposure and the integrated OAuth server.

## Console with Route + OpenShift auth

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
      openshift: {}
  exposure:
    type: route
    hostname: pgconsole-apps.example.com
```

In `openshift` mode:

- The console ServiceAccount is the OAuth client
  (`system:serviceaccount:app-db:main-pgconsole`); whenever
  `exposure.hostname` is set, the operator annotates it with
  `serviceaccounts.openshift.io/oauth-redirecturi.pgconsole` set to
  `https://<exposure.hostname>`.
- The proxy discovers endpoints from
  `https://openshift.default.svc/.well-known/oauth-authorization-server`
  and resolves the user through `/apis/user.openshift.io/v1/users/~`.
- The ServiceAccount's projected token doubles as the OAuth client secret
  and is re-read at every redemption, so token rotation just works.

## Subjects

OpenShift usernames are the subjects: create `PgToolBoxUser` objects with
`subject: <openshift-username>` — exact `metadata.name`, not an identity
string from the IdP.

## Verify

```bash
kubectl get sa main-pgconsole -n app-db -o yaml | yq '.metadata.annotations'
kubectl get route -n app-db
```

Sign in at the Route URL with a cluster user that has a matching
`PgToolBoxUser`.
