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
