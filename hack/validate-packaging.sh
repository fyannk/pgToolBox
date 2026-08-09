#!/usr/bin/env bash
# Copyright © contributors to the pgtoolbox project.
# SPDX-License-Identifier: Apache-2.0
#
# Check that the two install paths agree with the generated manifests and
# with each other.
#
# Building the bundle and catalog images proves they are well-formed YAML,
# which is not the same as being right. A catalog kept advertising a GVK
# whose kind had been deleted — group and version, no kind at all — and
# every build passed. Everything here is a cross-check between files that
# are supposed to say the same thing, because that is the only failure
# these copies actually have.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

fail=0
note() { printf '  %s\n' "$1"; }
bad() { printf 'FAIL %s\n' "$1"; fail=1; }

# 1. The CRDs are copies. A drifted copy means an install path validating
#    against a shape the operator no longer implements.
for crd in config/crd/bases/*.yaml; do
  base="$(basename "${crd}")"
  for copy in "deploy/helm/pgtoolbox/crds/${base}" "deploy/olm/bundle/manifests/${base}"; do
    if [ ! -f "${copy}" ]; then
      bad "${copy} is missing — run make manifests"
    elif ! cmp -s "${crd}" "${copy}"; then
      bad "${copy} differs from ${crd} — run make manifests"
    fi
  done
done
# And no copy outlives the CRD it came from.
for copy in deploy/helm/pgtoolbox/crds/*.yaml deploy/olm/bundle/manifests/pgtoolbox.fyannk.dev_*.yaml; do
  base="$(basename "${copy}")"
  [ -f "config/crd/bases/${base}" ] || bad "${copy} has no source in config/crd/bases"
done

python3 - <<'PY' || fail=1
import json, sys, pathlib, yaml

problems = []


def load(path):
    return yaml.safe_load(pathlib.Path(path).read_text())


csv = load("deploy/olm/bundle/manifests/pgtoolbox.clusterserviceversion.yaml")
chart = load("deploy/helm/pgtoolbox/Chart.yaml")
catalog = list(yaml.safe_load_all(
    pathlib.Path("deploy/olm/catalog/pgtoolbox/catalog.yaml").read_text()))
catalog = [d for d in catalog if d]

# 2. One version, said in three places.
csv_version = str(csv["spec"]["version"])
if csv_version != str(chart["appVersion"]):
    problems.append(f"CSV version {csv_version} != chart appVersion {chart['appVersion']}")
if csv["metadata"]["name"] != f"pgtoolbox.v{csv_version}":
    problems.append(f"CSV name {csv['metadata']['name']} does not match version {csv_version}")

# 3. The kinds the CSV claims to own, the CRDs the bundle ships, and the
#    GVKs the catalog advertises are the same set — the drift that shipped
#    a kindless GVK for a deleted CRD.
owned = {c["kind"] for c in csv["spec"]["customresourcedefinitions"]["owned"]}
shipped = {
    load(p)["spec"]["names"]["kind"]
    for p in pathlib.Path("deploy/olm/bundle/manifests").glob("pgtoolbox.fyannk.dev_*.yaml")
}
if owned != shipped:
    problems.append(f"CSV owns {sorted(owned)} but the bundle ships CRDs for {sorted(shipped)}")

bundles = [d for d in catalog if d.get("schema") == "olm.bundle"]
for bundle in bundles:
    gvks = []
    for prop in bundle.get("properties", []):
        if prop["type"] != "olm.gvk":
            continue
        value = prop["value"]
        for field in ("group", "kind", "version"):
            if not value.get(field):
                problems.append(f"catalog {bundle['name']}: olm.gvk missing {field}: {value}")
        gvks.append(value.get("kind"))
    if set(filter(None, gvks)) != owned:
        problems.append(f"catalog {bundle['name']} advertises {sorted(filter(None, gvks))}, CSV owns {sorted(owned)}")

# 4. Every channel entry names a bundle the catalog actually defines.
names = {b["name"] for b in bundles}
for channel in [d for d in catalog if d.get("schema") == "olm.channel"]:
    for entry in channel["entries"]:
        if entry["name"] not in names:
            problems.append(f"channel {channel['name']} entry {entry['name']} has no olm.bundle")

# 5. alm-examples is what OperatorHub's "Create instance" renders. Broken
#    JSON or a kind this operator does not own gives the user a blank form.
raw = csv["metadata"]["annotations"].get("alm-examples")
if not raw:
    problems.append("CSV has no alm-examples annotation")
else:
    try:
        for example in json.loads(raw):
            if example["kind"] not in owned:
                problems.append(f"alm-examples has kind {example['kind']}, which the CSV does not own")
    except (ValueError, KeyError) as err:
        problems.append(f"alm-examples is not usable: {err}")

# 6. A disconnected mirror carries relatedImages. If the operator's own
#    image is not among them, the mirror is incomplete by construction.
related = {r["image"] for r in csv["spec"].get("relatedImages", [])}
manager = None
for deployment in csv["spec"]["install"]["spec"]["deployments"]:
    for container in deployment["spec"]["template"]["spec"]["containers"]:
        if container["name"] == "manager":
            manager = container["image"]
if not related:
    problems.append("CSV has no relatedImages")
elif manager not in related:
    problems.append(f"manager image {manager} is not in relatedImages")
if manager and not manager.startswith("ghcr.io/"):
    problems.append(f"manager image {manager} is not a pullable reference")

for problem in problems:
    print(f"FAIL {problem}")
sys.exit(1 if problems else 0)
PY

if [ "${fail}" -ne 0 ]; then
  echo "packaging is inconsistent"
  exit 1
fi
echo "packaging is consistent: CRDs, CSV, catalog and chart agree"
