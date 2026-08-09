# Uninstall

Remove the consoles first, then the operator, then the CRDs. Consoles must
go first: each carries a finalizer that only the running operator removes.

```bash
kubectl delete pgc --all --all-namespaces    # every console stack (first)
helm uninstall pgtoolbox -n pgtoolbox        # operator
kubectl delete -k config/crd                 # CRDs (last)
```

With OLM, delete the `Subscription` and the installed CSV instead of the
Helm release.

## What deleting a PgConsole removes

Everything the operator generated carries an owner reference to the
`PgConsole`, so garbage collection removes: the Deployment, Service,
ServiceAccount, generated Roles and RoleBindings, NetworkPolicy, exposure
(Route/Ingress/HTTPRoute), the proxy configuration Secret, the admin-sync
Secret, the evidence token Secrets, and the pgAdmin settings PVC.

:::warning
The pgAdmin settings PVC is deleted with the console. Back it up first if
you need pgAdmin's settings database; there is no operator-managed backup.
:::

One object is not collected that way. With `console.allowClusterCatalogs`
enabled the console also has a `ClusterRole` and `ClusterRoleBinding`,
and a cluster-scoped object cannot carry an owner reference to a
namespaced one. The console's finalizer deletes them instead, so removing
the operator before deleting its consoles would strand them — delete the
`PgConsole` resources first, which the steps above already do.

`PgToolBoxUser` deletion removes the user from the proxy
configuration and pgAdmin on the next reconcile — there is nothing else to
tear down.
