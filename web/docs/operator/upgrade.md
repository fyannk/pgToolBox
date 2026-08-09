# Upgrade

## Helm

Bump the images and upgrade the release:

```bash
helm upgrade pgtoolbox deploy/helm/pgtoolbox \
  --namespace pgtoolbox \
  --set image.repository=<registry>/pgtoolbox \
  --set image.tag=<new-tag> \
  --set proxyImage=<registry>/pgtoolbox-proxy:<new-tag>
```

CRDs live in the chart's `crds/` directory, which Helm applies only at
install time — `helm upgrade` never touches them. Apply CRD updates
manually:

```bash
kubectl apply -k config/crd
```

## Breaking changes before 0.1.0

The API is `v1alpha1` and pre-release, so these are replacements rather
than deprecations.

### `PgToolBoxRole` is gone

Levels are a closed set (`view`, `poweruser`, `dba`) named directly on
`PgToolBoxUser.spec.level`, so the object only ever carried the same word
back. Delete the CRs before upgrading the operator:

```bash
kubectl delete pgtoolboxroles --all --all-namespaces
```

Doing it afterwards hangs. Each object carries the finalizer
`pgtoolbox.fyannk.dev/pgtoolboxrole`, and the controller that removes it
is no longer in the build — deletion then blocks until the finalizer is
cleared by hand:

```bash
kubectl patch pgtoolboxrole <name> -n <ns> --type=merge -p '{"metadata":{"finalizers":[]}}'
```

Then remove the CRD:

```bash
kubectl delete crd pgtoolboxroles.pgtoolbox.fyannk.dev
```

### `proxy.authentication.mode` is replaced by per-provider blocks

`mode: oidc` becomes `oidc: {...}`, `mode: local` becomes `local: {}`,
`mode: openshift` becomes `openshift: {}`. The field is pruned by the new
CRD, and a console left with no provider fails validation, so update the
`PgConsole` in the same change as the CRDs.

More than one may now be enabled at once — see
[Configuration](configuration.md#authentication-providers).

### `proxy.authentication.bootstrapAdmin` is new and required

Every `PgConsole` needs one before the new CRD will accept it. Name the
identity that is already your `dba`, and point `passwordSecretRef` at the
Secret that user already uses — the operator adopts the object rather than
creating a second account:

```yaml
bootstrapAdmin:
  subject: jane@corp.example
  passwordSecretRef: { name: jane-password }   # omit unless local-only
```

The existing hand-declared `PgToolBoxUser` for that subject becomes a
duplicate and is dropped from the proxy configuration, with the reason on
its status. Delete it once the console reports `Ready`.

## OLM

Publish a new bundle at a higher version and add it to the catalog's
channel entries above the old one. OLM upgrades the CSV in place.

## What rolls and when

The operator reconciles every owned object to the new desired state. A
console pod rolls exactly once per change, keyed on its pod template:

- Proxy configuration change → one rollout keyed on the config checksum
  annotation.
- admin-sync Secret regeneration → one rollout keyed on the Secret
  resourceVersion annotation.
- Evidence token rotation (`pgtoolbox.fyannk.dev/rotate-evidence-token: now`)
  → one rollout: the successor token Secret's name changes in the pod
  template.
- Operator image upgrade → one rollout for every console with pgAdmin
  enabled (the default), because the `admin-sync-init` init container runs
  the operator image. Consoles with `pgAdmin.enabled: false` do not roll.

A no-op reconcile issues zero API writes; upgrading the operator and
applying the same CRs again is the whole routine.
