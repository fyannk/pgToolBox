#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
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
  for copy in "charts/pgtoolbox/operator-crds/${base}" "deploy/olm/bundle/manifests/${base}"; do
    if [ ! -f "${copy}" ]; then
      bad "${copy} is missing — run make manifests"
    elif ! cmp -s "${crd}" "${copy}"; then
      bad "${copy} differs from ${crd} — run make manifests"
    fi
  done
done
# And no copy outlives the CRD it came from.
for copy in charts/pgtoolbox/operator-crds/*.yaml deploy/olm/bundle/manifests/pgtoolbox.fyannk.dev_*.yaml; do
  base="$(basename "${copy}")"
  [ -f "config/crd/bases/${base}" ] || bad "${copy} has no source in config/crd/bases"
done

# The kustomize CRD set (make install, docs "kustomize" install) lists every
# generated CRD and nothing else: a CRD missing from it crash-loops the
# operator, which registers a controller for each kind.
for crd in config/crd/bases/*.yaml; do
  grep -qxE -- "- bases/$(basename "${crd}")" config/crd/kustomization.yaml \
    || bad "config/crd/kustomization.yaml does not list bases/$(basename "${crd}")"
done
for entry in $(sed -n 's/^- //p' config/crd/kustomization.yaml); do
  [ -f "config/crd/${entry}" ] || bad "config/crd/kustomization.yaml lists ${entry}, which does not exist"
done

python3 - <<'PY' || fail=1
import json, re, sys, pathlib, yaml

problems = []


def load(path):
    return yaml.safe_load(pathlib.Path(path).read_text())


csv = load("deploy/olm/bundle/manifests/pgtoolbox.clusterserviceversion.yaml")
chart = load("charts/pgtoolbox/Chart.yaml")
tools_chart = load("charts/pgtools/Chart.yaml")
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
    # Older bundles in the upgrade graph advertise the kinds they owned then.
    if bundle["name"] == csv["metadata"]["name"] and set(filter(None, gvks)) != owned:
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

# 6. The chart's icon URL names `main`, not a release tag. A tag-pinned URL
#    404s for the whole life of a release candidate (the final tag does not
#    exist yet) and would have to be bumped by hand at every release; the logo
#    changes rarely and a moved logo is harmless. helm lint only checks that an
#    icon exists, so this checks that it is one that resolves.
icon = chart.get("icon", "")
if not icon:
    problems.append("chart has no icon: registries that render one show a placeholder")
else:
    match = re.search(r"raw\.githubusercontent\.com/fyannk/pgToolBox/([^/]+)/", icon)
    if not match:
        problems.append(f"chart icon URL is not a raw.githubusercontent.com/fyannk/pgToolBox URL: {icon}")
    elif match.group(1) != "main":
        problems.append(
            f"chart icon URL points at {match.group(1)}, which does not exist during a release "
            f"candidate (the final tag is cut after it); point it at main: {icon}")

# 7. The changelog is the one release artifact no tool generates, so it is
#    the one most easily left describing the previous version.
changelog = pathlib.Path("CHANGELOG.md").read_text()
for field in ("version", "appVersion"):
    if str(tools_chart[field]) != str(chart[field]):
        problems.append(f"charts/pgtools {field} {tools_chart[field]} != charts/pgtoolbox {field} {chart[field]}: the two charts release as one version")
if tools_chart.get("icon") != chart.get("icon"):
    problems.append("charts/pgtools icon differs from charts/pgtoolbox's: both carry the same URL")
if f"## [{csv_version}]" not in changelog:
    problems.append(f"CHANGELOG.md has no '## [{csv_version}]' section for the version being shipped")

# 8. The installation page prints a copy-pasteable `helm install ... --version`
#    for the published chart. A command that names the previous release is
#    worse than no command: it installs, and it installs the wrong thing.
install_doc = pathlib.Path("docs/site/docs/install/chart.md").read_text()
published = re.findall(r"charts/pgtool(?:box|s)\b.*?--version ([0-9][0-9A-Za-z.+-]*)", install_doc, re.S)
if not published:
    problems.append(
        "docs/site/docs/install/chart.md names no --version for the OCI chart, "
        "so the documented install cannot be pinned")
#    Helm's --version selects the chart version, which is the field this
#    compares against; appVersion is the operator it carries.
for version in set(published):
    if version != str(chart["version"]):
        problems.append(
            f"docs/site/docs/install/chart.md installs chart version {version} "
            f"but Chart.yaml's version is {chart['version']}")

#    And the two Chart.yaml versions move together here, which the checks
#    above assume: the CSV is compared against appVersion
#    while the documented install is compared against version, so a chart
#    whose fields diverged would be checked against two different releases.
if str(chart["version"]) != str(chart["appVersion"]):
    problems.append(
        f"chart version {chart['version']} != appVersion {chart['appVersion']} — "
        "this repository releases them as one version")

# 9. A disconnected mirror carries relatedImages. If the operator's own
#    image is not among them, the mirror is incomplete by construction.
related = {r["image"] for r in csv["spec"].get("relatedImages", [])}
manager = None
for deployment in csv["spec"]["install"]["spec"]["deployments"]:
    for container in deployment["spec"]["template"]["spec"]["containers"]:
        if container["name"] == "manager":
            manager = container["image"]
if "evidence-agent" not in {r["name"] for r in csv["spec"].get("relatedImages", [])}:
    problems.append("CSV relatedImages lacks the evidence-agent image: a disconnected mirror would miss the default agent")
if not related:
    problems.append("CSV has no relatedImages")
elif manager not in related:
    problems.append(f"manager image {manager} is not in relatedImages")

# 10. The images this repository publishes are named by Helm values, the
#     kustomize overlay, the CSV (annotation, deployment, args, relatedImages)
#     and the catalog, field by field. They must all be the registry the
#     release workflow pushes to, at the version being shipped. The sibling
#     images (pgadmin) are other projects on
#     their own release cadence and are not compared here.
REGISTRY_PREFIX = "ghcr.io/fyannk"
values = load("charts/pgtoolbox/values.yaml")
kustomization = load("config/default/kustomization.yaml")
version = str(chart["appVersion"])
own = {
    "manager": "pgtoolbox-operator",
    "bundle": "pgtoolbox-bundle", "evidence": "pgtoolbox-evidence-agent",
}
want = {k: f"{REGISTRY_PREFIX}/{v}" for k, v in own.items()}


def repo_of(ref):
    ref = ref.split("@", 1)[0]
    return ref.rsplit(":", 1) if ":" in ref.rsplit("/", 1)[-1] else [ref, ""]


def expect(where, ref, key, tag=version):
    repo, got_tag = repo_of(ref)
    if repo != want[key]:
        problems.append(f"{where} is {ref}; expected repository {want[key]}")
    elif tag is not None and got_tag != tag:
        problems.append(f"{where} is {ref}; expected tag {tag}")


expect("values.yaml image.repository", values["image"]["repository"], "manager", tag=None)
expect("pgtools values.yaml evidence.image.repository", load("charts/pgtools/values.yaml")["evidence"]["image"]["repository"], "evidence", tag=None)
expect("values.yaml operator.evidenceAgent.imageRepository", values["operator"]["evidenceAgent"]["imageRepository"], "evidence", tag=None)
if values["operator"]["evidenceAgent"]["image"] != "":
    problems.append("values.yaml operator.evidenceAgent.image must be empty: the default follows appVersion")
if values["image"].get("tag") not in ("", None, version):
    problems.append(f"values.yaml image.tag {values['image']['tag']} is neither empty nor {version}")
for image in kustomization.get("images", []):
    if image.get("name") == "controller":
        expect("kustomization controller image", f"{image['newName']}:{image.get('newTag', '')}", "manager")
expect("CSV containerImage", csv["metadata"]["annotations"]["containerImage"], "manager")
expect("CSV manager image", manager or "", "manager")
for deployment in csv["spec"]["install"]["spec"]["deployments"]:
    for container in deployment["spec"]["template"]["spec"]["containers"]:
        for arg in container.get("args", []):
            if arg.startswith("--operator-image="):
                expect("CSV --operator-image", arg.split("=", 1)[1], "manager")
            elif arg.startswith("--default-evidence-agent-image="):
                expect("CSV --default-evidence-agent-image", arg.split("=", 1)[1], "evidence")
for r in csv["spec"].get("relatedImages", []):
    name = r["image"].rsplit("/", 1)[-1].split(":")[0]
    if name == "pgtoolbox-operator":
        expect("CSV relatedImages manager", r["image"], "manager")
    elif r["name"] == "evidence-agent":
        expect("CSV relatedImages evidence-agent", r["image"], "evidence")
for bundle in bundles:
    # Older bundles in the upgrade graph keep their own tags.
    expect(f"catalog bundle {bundle['name']} image", bundle["image"], "bundle",
           tag=version if bundle["name"] == csv["metadata"]["name"] else None)
if manager and "." not in manager.split("/")[0]:
    problems.append(f"manager image {manager} is not a pullable reference")

# 11. (operator half) Helm, kustomize and OLM are three renderings of one
#     Deployment and one ClusterRole. Compare them field by field on what must not differ: the
#     manager's RBAC rules, its container security context, resources, ports,
#     probes, the pod security context and the non-image flags. The image
#     flags (--operator-image, --default-*-image) are explicit in Helm and
#     OLM and left to the compiled-in defaults by the kustomize overlay, so
#     they are compared Helm against OLM only.
import subprocess

def render(cmd):
    out = subprocess.run(cmd, capture_output=True, text=True)
    if out.returncode != 0:
        problems.append(f"{' '.join(cmd)} failed: {out.stderr.strip()[:200]}")
        return []
    return [d for d in yaml.safe_load_all(out.stdout) if d]

helm_docs = render(["helm", "template", "pgtoolbox", "charts/pgtoolbox", "--namespace", "pgtoolbox",
                    "--set", "operator.enabled=true"])
kust_docs = render(["kubectl", "kustomize", "config/default"])


def manager_pod(docs, label):
    deployments = [d for d in docs if d["kind"] == "Deployment"]
    if len(deployments) != 1:
        problems.append(f"{label} renders {len(deployments)} Deployments, expected 1")
        return None
    return deployments[0]["spec"]["template"]["spec"]


def manager_rules(docs, label):
    roles = [d for d in docs if d["kind"] == "ClusterRole" and d["metadata"]["name"].endswith("manager-role")]
    if len(roles) != 1:
        problems.append(f"{label} renders {len(roles)} manager ClusterRoles, expected 1")
        return None
    return roles[0]["rules"]


def norm_rules(rules):
    return sorted(json.dumps(r, sort_keys=True) for r in rules)


def image_flag(arg):
    return arg.startswith(("--operator-image=", "--default-"))


pods = {
    "Helm": manager_pod(helm_docs, "Helm"),
    "kustomize": manager_pod(kust_docs, "kustomize"),
    "OLM": csv["spec"]["install"]["spec"]["deployments"][0]["spec"]["template"]["spec"],
}
# The admission webhook (--admission-webhook and its port) is OLM's alone: the
# bundle cannot ship the ValidatingAdmissionPolicies Helm and kustomize do, and
# one install never runs both. Checked here, then left out of the comparison.
WEBHOOK_ARG, WEBHOOK_PORT = "--admission-webhook", {"containerPort": 9443, "name": "webhook", "protocol": "TCP"}
if all(pods.values()):
    for label, pod in pods.items():
        c = pod["containers"][0]
        has = (WEBHOOK_ARG in c.get("args", []), WEBHOOK_PORT in c.get("ports", []))
        if has != ((True, True) if label == "OLM" else (False, False)):
            problems.append(f"{label}: the admission webhook flag and port (OLM only), found {has}")
        c["args"] = [a for a in c.get("args", []) if a != WEBHOOK_ARG]
        c["ports"] = [p for p in c.get("ports", []) if p != WEBHOOK_PORT]
    ref = pods["Helm"]
    refc = ref["containers"][0]
    for label in ("kustomize", "OLM"):
        pod, c = pods[label], pods[label]["containers"][0]
        for field in ("securityContext", "resources", "ports", "livenessProbe", "readinessProbe"):
            if c.get(field) != refc.get(field):
                problems.append(f"manager container {field}: {label} {c.get(field)} != Helm {refc.get(field)}")
        if pod.get("securityContext") != ref.get("securityContext"):
            problems.append(f"manager pod securityContext: {label} differs from Helm")
        if [a for a in c.get("args", []) if not image_flag(a)] != [a for a in refc.get("args", []) if not image_flag(a)]:
            problems.append(f"manager non-image args: {label} {c.get('args')} != Helm {refc.get('args')}")
    # The image flags: kustomize carries the same three as Helm (tags in the tree,
    # digests after pin-refs), so a CR without spec.image does not get ImageRequired.
    helm_img = sorted(a for a in refc.get("args", []) if image_flag(a))
    kust_img = sorted(a for a in pods["kustomize"]["containers"][0].get("args", []) if image_flag(a))
    if kust_img != helm_img:
        problems.append(f"manager image flags: kustomize {kust_img} != Helm {helm_img} (config/manager/manager.yaml)")
    if pods["OLM"]["containers"][0].get("args") != refc.get("args"):
        problems.append("manager args: OLM CSV and Helm differ (image flags included)")

# The CSV's webhooks match what the two policies match.
for path, path_name in (("config/components/admission/admission_policy.yaml", "/validate-pgtoolbox-resources"),
                        ("config/admission/restore_drill_guard.yaml", "/validate-cnpg-cluster-delete")):
    policy = next(d for d in yaml.safe_load_all(open(path)) if d and d["kind"] == "ValidatingAdmissionPolicy")
    hooks = [w for w in csv["spec"].get("webhookdefinitions", []) if w.get("webhookPath") == path_name]
    if len(hooks) != 1 or hooks[0]["rules"] != policy["spec"]["matchConstraints"]["resourceRules"]:
        problems.append(f"OLM webhook {path_name}: rules differ from {path}'s matchConstraints")
    # The delete guard fails open on OLM: it cannot be scoped to the drills role.
    elif hooks[0].get("failurePolicy") != ("Ignore" if path_name == "/validate-cnpg-cluster-delete" else policy["spec"]["failurePolicy"]):
        problems.append(f"OLM webhook {path_name}: unexpected failurePolicy (resources: the policy's; delete guard: Ignore)")

rules = {
    "kustomize (config/rbac/role.yaml)": load("config/rbac/role.yaml")["rules"],
    "Helm": manager_rules(helm_docs, "Helm"),
    "OLM": csv["spec"]["install"]["spec"]["clusterPermissions"][0]["rules"],
}
if all(v is not None for v in rules.values()):
    base = norm_rules(rules["kustomize (config/rbac/role.yaml)"])
    for label in ("Helm", "OLM"):
        if norm_rules(rules[label]) != base:
            problems.append(f"manager RBAC rules: {label} differs from config/rbac/role.yaml — run make manifests")


# 11b. The rest of the operator half, three renderings against one source (config/rbac,
#      config/manager): the leader-election Role, the two user ClusterRoles, the
#      console's own two and the metrics reader, and the operator's own flag and environment contract.
helm_full = render(["helm", "template", "pgtoolbox", "charts/pgtoolbox", "--namespace", "pgtoolbox",
                    "-f", "charts/pgtoolbox/ci/operator-values.yaml"])

# With operator.config.watchNamespaces the Helm chart binds the manager ClusterRole by
# RoleBinding per namespace and a second ClusterRole, bound cluster-wide, carries the
# cluster-scoped rules alone. That second role is hand-written, so it is derived here
# from the generated manager role: every rule on a cluster-scoped resource (reduced to
# those resources) must be in it, and nothing else; otherwise the operator would lose a
# permission (a RoleBinding cannot grant a cluster-scoped resource) or gain one.
# Except `bind` (and `escalate`) on clusterroles: their check runs where the binding is
# made, so the per-namespace RoleBinding of the manager role grants them for the
# RoleBindings the operator creates; held cluster-wide next to `create
# clusterrolebindings`, `bind` would let the operator's token ClusterRoleBind
# pgtoolbox-console-restore-drills to itself. The cluster-wide role holds no
# bind, escalate, impersonate or wildcard verb and no namespaced resource.
CLUSTER_SCOPED = {"clusterroles", "clusterrolebindings", "tokenreviews", "subjectaccessreviews",
                  "selfsubjectaccessreviews", "selfsubjectrulesreviews",
                  "securitycontextconstraints", "nodes", "namespaces", "persistentvolumes",
                  "customresourcedefinitions", "validatingadmissionpolicies"}
scoped_manager = [d for d in helm_full if d["kind"] == "ClusterRole" and d["metadata"]["name"].endswith("-manager-role")]
scoped_cluster = [d for d in helm_full if d["kind"] == "ClusterRole" and d["metadata"]["name"].endswith("-manager-cluster-role")]
if len(scoped_manager) != 1 or len(scoped_cluster) != 1:
    problems.append("Helm with watchNamespaces must render one manager ClusterRole and one manager-cluster-role")
else:
    NAMESPACED_GRANT_VERBS = {"bind", "escalate"}
    expected = []
    for r in scoped_manager[0]["rules"]:
        res = [x for x in r.get("resources", []) if x in CLUSTER_SCOPED]
        verbs = [v for v in r.get("verbs", []) if not (v in NAMESPACED_GRANT_VERBS and set(res) <= {"clusterroles", "roles"})]
        if res and verbs:
            expected.append({**r, "resources": res, "verbs": verbs})
        elif r.get("apiGroups") == ["discovery.k8s.io"] and r.get("resources") == ["endpointslices"] and r.get("resourceNames") == ["kubernetes"]:
            expected.append(r)  # one named object in the default namespace: needs the cluster-wide grant
            # (the unnamed rule is the MCP leader's slices, in the watched namespaces only)
    if norm_rules(expected) != norm_rules(scoped_cluster[0]["rules"]):
        problems.append("charts/pgtoolbox clusterrole-cluster-scoped.yaml differs from the cluster-scoped rules of the manager role")
    for r in scoped_cluster[0]["rules"]:
        bad_verbs = set(r.get("verbs", [])) & {"bind", "escalate", "impersonate", "*"}
        bad_res = [x for x in r.get("resources", []) if x not in CLUSTER_SCOPED and x != "endpointslices"]
        if bad_verbs or bad_res or "*" in r.get("resources", []) or "*" in r.get("apiGroups", []):
            problems.append(f"charts/pgtoolbox clusterrole-cluster-scoped.yaml grants {sorted(bad_verbs) or ''}{bad_res or ''} cluster-wide: {r}")
    # One binding name per mode: a roleRef is immutable, so switching the mode with
    # helm upgrade must replace the binding, not patch its roleRef.
    cluster_bindings = [d for d in helm_full if d["kind"] == "ClusterRoleBinding" and d["roleRef"]["name"].endswith(("-manager-cluster-role", "-manager-role"))]
    if len(cluster_bindings) != 1 or not cluster_bindings[0]["roleRef"]["name"].endswith("-manager-cluster-role") \
            or not cluster_bindings[0]["metadata"]["name"].endswith("-manager-cluster"):
        problems.append("Helm with watchNamespaces must bind only the cluster-scoped role cluster-wide, as <fullname>-manager-cluster")
    default_bindings = [d for d in helm_docs if d["kind"] == "ClusterRoleBinding" and d["roleRef"]["name"].endswith(("-manager-cluster-role", "-manager-role"))]
    if len(default_bindings) != 1 or not default_bindings[0]["roleRef"]["name"].endswith("-manager-role") \
            or default_bindings[0]["metadata"]["name"] == (cluster_bindings[0]["metadata"]["name"] if cluster_bindings else None):
        problems.append("Helm by default must bind the manager role cluster-wide under a name of its own (not the watchNamespaces binding's)")
    # The operator's own namespace: with watchNamespaces it holds nothing there but
    # leader election and get on its own ConfigMap and Secret, by the names
    # cmd/operator reads (--config-map, --config-secret defaults: the chart passes neither).
    main_go = open("cmd/operator/main.go").read()
    config_names = sorted(set(re.findall(r'flag\.StringVar\(&(?:configMapName|secretName), "config-(?:map|secret)", "([^"]+)"', main_go)))
    config_roles = [d for d in helm_full if d["kind"] == "Role" and d["metadata"]["name"].endswith("-config")]
    config_bindings = [d for d in helm_full if d["kind"] == "RoleBinding" and d["metadata"]["name"].endswith("-config")]
    if not config_names:
        problems.append("cmd/operator/main.go: --config-map/--config-secret defaults not found")
    elif len(config_roles) != 1 or config_roles[0]["metadata"].get("namespace") != "pgtoolbox" or norm_rules(config_roles[0]["rules"]) != norm_rules(
            [{"apiGroups": [""], "resources": ["configmaps", "secrets"], "resourceNames": config_names, "verbs": ["get"]}]):
        problems.append(f"Helm with watchNamespaces must grant the operator get on its ConfigMap and Secret {config_names} in its namespace, and nothing else")
    elif len(config_bindings) != 1 or config_bindings[0]["roleRef"] != {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": config_roles[0]["metadata"]["name"]}:
        problems.append("Helm with watchNamespaces must bind the operator config Role to the operator")
    if any(d["kind"] == "Role" and d["metadata"]["name"].endswith("-config") for d in helm_docs):
        problems.append("Helm by default renders the operator config Role: the manager ClusterRole already grants it")
    bound = sorted(d["metadata"]["namespace"] for d in helm_full if d["kind"] == "RoleBinding" and d["metadata"]["name"].endswith("-manager"))
    if bound != ["team-a", "team-b"]:
        problems.append(f"Helm with watchNamespaces binds the manager role in {bound}, expected [team-a, team-b]")


# The leader-election Role.
le_kust = load("config/rbac/leader_election_role.yaml")["rules"]
le_helm = [d for d in helm_docs if d["kind"] == "Role" and d["metadata"]["name"].endswith("leader-election")]
le_olm = csv["spec"]["install"]["spec"].get("permissions", [{}])[0].get("rules", [])
if len(le_helm) != 1 or norm_rules(le_helm[0]["rules"]) != norm_rules(le_kust):
    problems.append("leader-election Role rules: Helm differs from config/rbac/leader_election_role.yaml")
if norm_rules(le_olm) != norm_rules(le_kust):
    problems.append("leader-election Role rules: the CSV permissions differ from config/rbac/leader_election_role.yaml")

# The user ClusterRoles and the metrics reader.
bundle_dir = pathlib.Path("deploy/olm/bundle/manifests")
for name, src, olm_file, helm_name in (
        ("pgtoolbox-console-viewer", "config/rbac/pgtoolbox_console_viewer_clusterrole.yaml", "pgtoolbox-console-viewer_rbac.authorization.k8s.io_v1_clusterrole.yaml", "pgtoolbox-console-viewer"),
        ("pgtoolbox-console-operator", "config/rbac/pgtoolbox_console_operator_clusterrole.yaml", "pgtoolbox-console-operator_rbac.authorization.k8s.io_v1_clusterrole.yaml", "pgtoolbox-console-operator"),
        ("pgtoolbox-console-approver", "config/rbac/pgtoolbox_console_approver_clusterrole.yaml", "pgtoolbox-console-approver_rbac.authorization.k8s.io_v1_clusterrole.yaml", "pgtoolbox-console-approver"),
        ("pgtoolbox-aggregate-to-admin", "config/rbac/pgtoolbox_aggregate_to_admin_clusterrole.yaml", "pgtoolbox-aggregate-to-admin_rbac.authorization.k8s.io_v1_clusterrole.yaml", "pgtoolbox-aggregate-to-admin"),
        ("pgtoolbox-console", "config/rbac/pgtoolbox_console_clusterrole.yaml", "pgtoolbox-console_rbac.authorization.k8s.io_v1_clusterrole.yaml", "pgtoolbox-console"),
        ("pgtoolbox-console-pod-logs", "config/rbac/pgtoolbox_console_pod_logs_clusterrole.yaml", "pgtoolbox-console-pod-logs_rbac.authorization.k8s.io_v1_clusterrole.yaml", "pgtoolbox-console-pod-logs"),
        ("pgtoolbox-console-restore-drills", "config/rbac/pgtoolbox_console_restore_drills_clusterrole.yaml", "pgtoolbox-console-restore-drills_rbac.authorization.k8s.io_v1_clusterrole.yaml", "pgtoolbox-console-restore-drills"),
        ("pgtoolbox-metrics-reader", "config/rbac/metrics_reader_clusterrole.yaml", "pgtoolbox-metrics-reader_rbac.authorization.k8s.io_v1_clusterrole.yaml", "pgtoolbox-metrics-reader")):
    kust = load(src)
    if kust["metadata"]["name"] != name:
        problems.append(f"{src} is named {kust['metadata']['name']}, expected {name}")
    bundled = load(bundle_dir / olm_file) if (bundle_dir / olm_file).exists() else None
    if bundled is None or bundled["rules"] != kust["rules"] or bundled["metadata"]["name"] != name:
        problems.append(f"the bundle's {olm_file} differs from {src} — run make manifests")
    # The metrics reader carries the release's name in Helm; the user roles are fixed.
    helm_cr = next((d for d in helm_full if d["kind"] == "ClusterRole" and (
        d["metadata"]["name"] == name or (name.endswith("metrics-reader") and d["metadata"]["name"].endswith("metrics-reader")))), None)
    if helm_cr is None or helm_cr["rules"] != kust["rules"]:
        problems.append(f"Helm ClusterRole {helm_name}: missing or different from {src}")

# The restore-drill delete guard (config/admission, Kubernetes 1.30+): the chart
# renders exactly it when asked; the OLM bundle cannot carry it; the marker it
# checks is the drills ClusterRole's.
guard_src = [d for d in yaml.safe_load_all(open("config/admission/restore_drill_guard.yaml")) if d]
guard_helm = render(["helm", "template", "pgtoolbox", "charts/pgtoolbox", "--namespace", "pgtoolbox",
                     "--set", "operator.enabled=true", "--set", "operator.restoreDrillGuard.enabled=true"])
for src_doc in guard_src:
    helm_doc = next((d for d in guard_helm if d["kind"] == src_doc["kind"] and d["metadata"]["name"] == src_doc["metadata"]["name"]), None)
    if helm_doc is None or helm_doc["spec"] != src_doc["spec"]:
        problems.append(f"Helm {src_doc['kind']} {src_doc['metadata']['name']}: missing or different from config/admission/restore_drill_guard.yaml — run make manifests")
for src_doc in guard_src:
    kust_doc = next((d for d in kust_docs if d["kind"] == src_doc["kind"] and d["metadata"]["name"] == src_doc["metadata"]["name"]), None)
    if kust_doc is None or kust_doc["spec"] != src_doc["spec"] or "namespace" in kust_doc["metadata"]:
        problems.append(f"kustomize config/default: {src_doc['kind']} {src_doc['metadata']['name']} missing, different from config/admission or namespaced")
if any("admissionregistration" in f.read_text() for f in bundle_dir.glob("*.yaml")):
    problems.append("the OLM bundle carries an admission policy, which OLM does not install")
# The operator chart's admission policy (image and workload-identity allow-lists,
# Secret references, PgAdmin credentials). Helm renders it by default; kustomize
# ships it as the opt-in component config/components/admission, a render of the
# chart's defaults that must say exactly what the chart says; config/default
# leaves it out on purpose (opt-in) and the OLM bundle cannot carry it (checked
# above), which docs/site/docs/install/olm.md must say.
comp_dir = pathlib.Path("config/components/admission")
comp_kust = load(comp_dir / "kustomization.yaml") if (comp_dir / "kustomization.yaml").exists() else {}
if comp_kust.get("kind") != "Component" or comp_kust.get("resources") != ["admission_policy.yaml"]:
    problems.append("config/components/admission/kustomization.yaml must be a kustomize Component listing admission_policy.yaml")
comp_docs = [d for d in yaml.safe_load_all(open(comp_dir / "admission_policy.yaml")) if d] if (comp_dir / "admission_policy.yaml").exists() else []
helm_vap = [d for d in helm_docs if d["kind"] in ("ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding") and d["metadata"]["name"].endswith("image-override")]
if sorted((d["kind"], d["metadata"]["name"]) for d in comp_docs) != sorted((d["kind"], d["metadata"]["name"]) for d in helm_vap):
    problems.append(f"config/components/admission renders {[(d['kind'], d['metadata']['name']) for d in comp_docs]}, Helm {[(d['kind'], d['metadata']['name']) for d in helm_vap]} — run make manifests")
for doc in helm_vap:
    comp_doc = next((d for d in comp_docs if d["kind"] == doc["kind"] and d["metadata"]["name"] == doc["metadata"]["name"]), None)
    if comp_doc is not None and (comp_doc["spec"] != doc["spec"] or "namespace" in comp_doc["metadata"]):
        problems.append(f"config/components/admission: {doc['kind']} {doc['metadata']['name']} differs from the Helm render or is namespaced — run make manifests")
if any(d["kind"] in ("ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding") and d["metadata"]["name"].endswith("image-override") for d in kust_docs):
    problems.append("kustomize config/default applies the operator chart's admission policy: it is the opt-in component config/components/admission")
olm_doc = pathlib.Path("docs/site/docs/install/olm.md").read_text()
if "--admission-webhook" not in olm_doc or "never both" not in olm_doc:
    problems.append("docs/site/docs/install/olm.md must say the bundle enforces the admission rules through the operator's webhook (--admission-webhook), and never with the policies as well")
guard_policy = next((d for d in guard_src if d["kind"] == "ValidatingAdmissionPolicy"), None)
drills_rules = load("config/rbac/pgtoolbox_console_restore_drills_clusterrole.yaml")["rules"]
marker = {"apiGroups": ["pgtoolbox.fyannk.dev"], "resources": ["restoredrills"], "verbs": ["teardown"]}
if marker not in drills_rules:
    problems.append("config/rbac/pgtoolbox_console_restore_drills_clusterrole.yaml lacks the delete guard's marker (teardown on restoredrills)")
if guard_policy is None or "resource('restoredrills')" not in str(guard_policy["spec"]) or "check('teardown')" not in str(guard_policy["spec"]):
    problems.append("config/admission/restore_drill_guard.yaml does not check the drills ClusterRole's marker")
for key in ("pgtoolbox.fyannk.dev/drill-owner", "pgtoolbox.fyannk.dev/restore-drill-of", "pgtoolbox.fyannk.dev/restore-drill-seal"):
    if guard_policy is not None and key not in guard_policy["spec"]["validations"][0]["expression"]:
        problems.append(f"config/admission/restore_drill_guard.yaml does not check {key}")

viewer, operator_role = load("config/rbac/pgtoolbox_console_viewer_clusterrole.yaml")["rules"], load("config/rbac/pgtoolbox_console_operator_clusterrole.yaml")["rules"]
WRITES = {"create", "update", "patch", "delete", "deletecollection", "impersonate", "escalate", "bind", "approve", "sign", "*"}
# The viewer reads, and may ask for more: its one write is creating a
# PgAccessRequest, which changes no access by itself.
VIEWER_REQUEST = {"apiGroups": ["pgtoolbox.fyannk.dev"], "resources": ["pgaccessrequests"], "verbs": ["create", "get", "list", "watch"]}
for rule in viewer:
    if WRITES & set(rule["verbs"]) and rule != VIEWER_REQUEST:
        problems.append(f"pgtoolbox-console-viewer grants {sorted(WRITES & set(rule['verbs']))} on {rule['resources']}: it is read-only apart from creating PgAccessRequests")
if VIEWER_REQUEST not in viewer:
    problems.append("pgtoolbox-console-viewer must grant create/get/list/watch on pgaccessrequests (a viewer asks for more through the console)")
EXPECTED_WRITES = [
    {"apiGroups": ["postgresql.cnpg.io"], "resources": ["backups"], "verbs": ["create"]},
    {"apiGroups": ["postgresql.cnpg.io"], "resources": ["databaseroles"], "verbs": ["create"]},
    {"apiGroups": ["postgresql.cnpg.io"], "resources": ["clusters", "clusters/status"], "verbs": ["patch"]},
]
extra = [r for r in operator_role if r not in viewer]
if sorted(json.dumps(r, sort_keys=True) for r in extra) != sorted(json.dumps(r, sort_keys=True) for r in EXPECTED_WRITES + [
        {"apiGroups": ["pgtoolbox.fyannk.dev"], "resources": ["pgadmins"], "verbs": ["connect", "get"]}]):
    problems.append(f"pgtoolbox-console-operator must be the viewer plus exactly create backups, create databaseroles, patch clusters and clusters/status, connect and get pgadmins; extra rules are {extra}")
# Opening pgAdmin (connect pgadmins, the OpenShift oauth-proxy's SubjectAccessReview)
# is reading the credentials it offers: only the operator role and the share of
# admin carry it, never a role aggregated to view or edit, nor the console's own.
OPENS_PGADMIN = {"pgtoolbox-console-operator", "pgtoolbox-aggregate-to-admin"}
for f in sorted(pathlib.Path("config/rbac").glob("*.yaml")):
    for doc in yaml.safe_load_all(f.read_text()):
        if not doc or doc.get("kind") not in ("ClusterRole", "Role"):
            continue
        name, labels = doc["metadata"]["name"], doc["metadata"].get("labels") or {}
        opens = any(("pgtoolbox.fyannk.dev" in r.get("apiGroups", []) or "*" in r.get("apiGroups", [])) and
                    ("pgadmins" in r.get("resources", []) or "*" in r.get("resources", [])) and
                    ("connect" in r.get("verbs", []) or "*" in r.get("verbs", []))
                    for r in doc.get("rules") or [])
        if opens and name not in OPENS_PGADMIN:
            problems.append(f"{f}: {name} may open pgAdmin (connect pgadmins); only {sorted(OPENS_PGADMIN)} may")
        if opens and (labels.get("rbac.authorization.k8s.io/aggregate-to-view") or labels.get("rbac.authorization.k8s.io/aggregate-to-edit")):
            problems.append(f"{f}: {name} is aggregated to view or edit and may open pgAdmin")
        if name in OPENS_PGADMIN and not opens:
            problems.append(f"{f}: {name} must grant connect on pgadmins (who opens pgAdmin)")
# Deciding a PgAccessRequest is the approver's alone. pgtoolbox-console-operator
# is what an approved request grants, time-boxed: holding create rolebindings or
# update pgaccessrequests/status, a grantee could bind themselves a second,
# permanent RoleBinding or approve other people's requests.
for rule in viewer + operator_role:
    if "rolebindings" in rule.get("resources", []) or "pgaccessrequests/status" in rule.get("resources", []) or "clusterroles" in rule.get("resources", []):
        problems.append(f"a grantable console role holds {rule}: deciding access is pgtoolbox-console-approver's")
approver_role = load("config/rbac/pgtoolbox_console_approver_clusterrole.yaml")["rules"]
EXPECTED_APPROVER = [
    {"apiGroups": ["pgtoolbox.fyannk.dev"], "resources": ["pgaccessrequests"], "verbs": ["get", "list", "watch"]},
    {"apiGroups": ["pgtoolbox.fyannk.dev"], "resources": ["pgaccessrequests/status"], "verbs": ["update"]},
    {"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["rolebindings"], "verbs": ["create"]},
    # bind on exactly the two grantable roles: the escalation check passes
    # without the approver holding them, and nothing else can be bound this way.
    {"apiGroups": ["rbac.authorization.k8s.io"], "resources": ["clusterroles"],
     "resourceNames": ["pgtoolbox-console-operator", "pgtoolbox-console-viewer"], "verbs": ["bind"]},
]
if approver_role != EXPECTED_APPROVER:
    problems.append(f"pgtoolbox-console-approver must be exactly {EXPECTED_APPROVER}; it is {approver_role}")
if "pgtoolbox-console-approver" in pathlib.Path("api/v1alpha1/pgaccessrequest_funcs.go").read_text():
    problems.append("api/v1alpha1/pgaccessrequest_funcs.go names pgtoolbox-console-approver: no PgAccessRequest may grant it")
if [r for r in viewer if r not in operator_role]:
    problems.append("pgtoolbox-console-operator must contain every rule of pgtoolbox-console-viewer")
# Promote lists the Cluster's pods as the caller, so both roles must be able to.
for label, rules in (("pgtoolbox-console-viewer", viewer), ("pgtoolbox-console-operator", operator_role)):
    if not any("pods" in r.get("resources", []) and r.get("apiGroups") == [""] and {"get", "list", "watch"} <= set(r["verbs"]) for r in rules):
        problems.append(f"{label} must get, list and watch pods (promote lists them with the caller's token)")
for rule in viewer + operator_role:
    if "secrets" in rule.get("resources", []) or "*" in rule.get("resources", []) or "*" in rule.get("apiGroups", []):
        problems.append(f"a user ClusterRole grants {rule}: no Secret and no wildcard")
if any(d["kind"] == "ClusterRoleBinding" and d["roleRef"]["name"] in ("pgtoolbox-console-viewer", "pgtoolbox-console-operator", "pgtoolbox-console-approver")
       for d in helm_full + kust_docs):
    problems.append("a ClusterRoleBinding to a user ClusterRole is rendered: people are bound per namespace by whoever may create RoleBindings")

# The operator's flag and environment contract with the binary: whatever the chart
# renders is read by cmd/operator or internal/operator/configuration.
src = "".join(f.read_text() for f in list(pathlib.Path("cmd/operator").glob("*.go")) + list(pathlib.Path("internal/operator/configuration").glob("*.go"))
              if not f.name.endswith("_test.go"))
operator_flags = set(re.findall(r'flag\.\w+Var\(&[\w.]+, "([a-z][a-z-]+)"', src)) | {"leader-elect"}
operator_env = set(re.findall(r'"([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)"', src)) | {"POD_NAMESPACE"}
for label, docs in (("default", helm_docs), ("ci/operator-values.yaml", helm_full)):
    pod = manager_pod(docs, f"Helm ({label})")
    if not pod:
        continue
    c = pod["containers"][0]
    for arg in c.get("args", []):
        if arg.lstrip("-").split("=")[0] not in operator_flags:
            problems.append(f"Helm ({label}) passes {arg}: cmd/operator defines no such flag")
    for env in c.get("env", []):
        if env["name"] not in operator_env:
            problems.append(f"Helm ({label}) sets {env['name']}: the operator reads no such variable")
for label, pod in (("kustomize", pods["kustomize"]), ("OLM", pods["OLM"])):
    if pod:
        names = {e["name"] for e in pod["containers"][0].get("env", [])}
        helm_names = {e["name"] for e in pods["Helm"]["containers"][0].get("env", [])} if pods["Helm"] else set()
        if names != helm_names:
            problems.append(f"manager env names: {label} {sorted(names)} != Helm default {sorted(helm_names)}")
if pods["Helm"] and not any(a.startswith("--default-evidence-agent-image=") for a in pods["Helm"]["containers"][0]["args"]):
    problems.append("Helm does not pass --default-evidence-agent-image: an EvidenceAgent without spec.image would have no image")


# 11c. Availability and hardening of the operator pods:
#      grace period, PodDisruptionBudget and Pod Security labels agree in the three
#      renderings; the NetworkPolicy is never on by default; the admission policy is
#      on by default (it is the only check that an editor may read the Secrets a
#      PgToolBoxConsole or PgAdmin references); the monitors speak HTTPS because the
#      endpoint does.
grace = {label: pod.get("terminationGracePeriodSeconds") for label, pod in pods.items() if pod}
if len(set(grace.values())) != 1 or (set(grace.values()) and min(v or 0 for v in grace.values()) < 40):
    problems.append(f"manager terminationGracePeriodSeconds must be the same and >= 40 in Helm, kustomize and OLM, got {grace}")


def pdb_of(docs):
    found = [d for d in docs if d["kind"] == "PodDisruptionBudget"]
    return (found[0]["spec"]["minAvailable"], found[0]["spec"].get("unhealthyPodEvictionPolicy")) if len(found) == 1 else None


olm_pdb_file = bundle_dir / "pgtoolbox_policy_v1_poddisruptionbudget.yaml"
pdbs = {"Helm": pdb_of(helm_docs), "kustomize": pdb_of(kust_docs),
        "OLM": (load(olm_pdb_file)["spec"]["minAvailable"], load(olm_pdb_file)["spec"].get("unhealthyPodEvictionPolicy")) if olm_pdb_file.exists() else None}
if len(set(pdbs.values())) != 1 or None in pdbs.values():
    problems.append(f"operator PodDisruptionBudget differs or is missing: {pdbs}")

PSA = {f"pod-security.kubernetes.io/{m}": "restricted" for m in ("enforce", "audit", "warn")}
helm_ns = render(["helm", "template", "pgtoolbox", "charts/pgtoolbox", "--namespace", "pgtoolbox",
                  "--set", "operator.enabled=true", "--set", "namespace.create=true"])
helm_ns_labels = next((d["metadata"]["labels"] for d in helm_ns if d["kind"] == "Namespace"), {})
kust_ns_labels = next((d["metadata"]["labels"] for d in kust_docs if d["kind"] == "Namespace"), {})
olm_ns = json.loads(csv["metadata"]["annotations"].get("operatorframework.io/suggested-namespace-template", "{}")).get("metadata", {}).get("labels", {})
for label, labels in (("Helm", helm_ns_labels), ("kustomize", kust_ns_labels), ("OLM suggested-namespace-template", olm_ns)):
    if {k: v for k, v in labels.items() if k.startswith("pod-security.")} != PSA:
        problems.append(f"{label} namespace does not carry restricted Pod Security labels enforce/audit/warn")

for doc in helm_docs:
    if doc["kind"] == "NetworkPolicy":
        problems.append(f"operator.enabled renders a {doc['kind']} by default: it is the installing admin's choice, never on by default")
default_vap = [d["metadata"]["name"] for d in helm_docs if d["kind"] in ("ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding") and d["metadata"]["name"].endswith("image-override")]
if sorted(default_vap) != ["pgtoolbox-image-override", "pgtoolbox-image-override"]:
    problems.append(f"operator.enabled must render the admission policy and its binding by default (the Secret-reference rule has no other check); renders {default_vap}")
for doc in helm_full:
    if doc["kind"] in ("ServiceMonitor", "PodMonitor"):
        endpoints = doc["spec"].get("endpoints") or doc["spec"].get("podMetricsEndpoints")
        for ep in endpoints:
            if ep.get("scheme") != "https" or "tlsConfig" not in ep:
                problems.append(f"operator {doc['kind']} must scrape https with a tlsConfig, got {ep}")
            if not (ep.get("bearerTokenFile") or ep.get("authorization")):
                problems.append(f"operator {doc['kind']} sends no token: /metrics needs one")
        if doc["kind"] == "ServiceMonitor" or doc["kind"] == "PodMonitor":
            tls_cfg = endpoints[0]["tlsConfig"]
            if not (tls_cfg.get("insecureSkipVerify") or tls_cfg.get("ca")):
                problems.append(f"operator {doc['kind']} tlsConfig verifies against nothing and does not skip verification")
helm_full_pod = manager_pod(helm_full, "Helm (ci/operator-values.yaml)")
if helm_full_pod:
    if not any(a.startswith("--metrics-cert-dir=") for a in helm_full_pod["containers"][0]["args"]):
        problems.append("operator.metrics.tls.certSecret does not pass --metrics-cert-dir")
    mounted = [m["mountPath"] for m in helm_full_pod["containers"][0].get("volumeMounts", [])]
    if "/var/run/pgtoolbox/metrics-cert" not in mounted:
        problems.append("operator.metrics.tls.certSecret is not mounted where --metrics-cert-dir points")
    if not helm_full_pod.get("priorityClassName") or not helm_full_pod.get("topologySpreadConstraints"):
        problems.append("operator priorityClassName/topologySpreadConstraints values are not rendered")
# The restore-drill guard is rendered by default, without API discovery (a plain helm template).
if not [d for d in helm_docs if d["kind"] == "ValidatingAdmissionPolicy" and d["metadata"]["name"] == "pgtoolbox-console-restore-drills"]:
    problems.append("operator.enabled must render the restore-drill delete guard by default, also without API discovery (operator.restoreDrillGuard.enabled)")
vap = [d for d in helm_full if d["kind"] == "ValidatingAdmissionPolicy" and d["metadata"]["name"].endswith("image-override")]
if len(vap) != 1 or not [d for d in helm_full if d["kind"] == "ValidatingAdmissionPolicyBinding"]:
    problems.append("operator.admissionPolicy.enabled must render one ValidatingAdmissionPolicy and its binding")
elif not any(r["resources"] == ["pgadmins", "evidenceagents", "pgtoolboxconsoles"] for r in vap[0]["spec"]["matchConstraints"]["resourceRules"]):
    problems.append("the admission policy does not match pgadmins, evidenceagents and pgtoolboxconsoles")
if not [d for d in helm_full if d["kind"] == "NetworkPolicy"]:
    problems.append("operator.networkPolicy.enabled must render a NetworkPolicy")


# 12. The console: the pgtools chart's namespace console
#     against its kustomize rendering (config/console) and against the OLM
#     bundle. config/console is the
#     console beside the operator, so the chart renders as on a cluster that
#     serves the operator's kinds (console.rbac.operatorKinds: auto).
console_helm = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                       "--api-versions", "pgtoolbox.fyannk.dev/v1alpha1"])
console_kust = render(["kubectl", "kustomize", "config/console"])
console_cat = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                      "--set", "console.clusterImageCatalogs.enabled=true"])
console_hist = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                       "--set", "console.history.persistent=true"])
console_digest = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                         "--set", "console.history.persistent=true", "--set", "console.publicURL=https://c.example.org",
                         "--set", "console.digest.delivery.chat.existingSecret=chat"])
console_metrics = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                          "--set", "console.metrics.readerClusterRole=true",
                          "--set", "console.metrics.readerSubjects[0].name=prom",
                          "--set", "console.metrics.readerSubjects[0].namespace=monitoring"])
console_bg = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                     "--set", "console.breakGlass.enabled=true", "--set", "console.watchNamespaces={team-a,team-b}"])
# roleKind=Role: with and without the operator's kinds being served.
console_bg_role = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                          "--set", "console.breakGlass.enabled=true", "--set", "console.breakGlass.roleKind=Role",
                          "--set", "console.watchNamespaces={team-a,team-b}",
                          "--api-versions", "pgtoolbox.fyannk.dev/v1alpha1"])
console_bg_role_bare = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                               "--set", "console.breakGlass.enabled=true", "--set", "console.breakGlass.roleKind=Role",
                               "--set", "console.watchNamespaces={team-a,team-b}"])
console_multi = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                        "--set", "console.watchNamespaces={team-a,team-b}"])
console_drills = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                         "--set", "console.watchNamespaces={team-a,team-b}", "--set", "console.restoreDrills.enabled=true"])


def of(docs, kind, suffix=None):
    return [d for d in docs if d["kind"] == kind and (suffix is None or d["metadata"]["name"].endswith(suffix))]


def sole(docs, kind, label, suffix=None):
    found = of(docs, kind, suffix)
    if len(found) != 1:
        problems.append(f"{label} renders {len(found)} {kind} objects{' ending ' + suffix if suffix else ''}, expected 1")
        return None
    return found[0]


FORBIDDEN_VERBS = {"create", "update", "patch", "delete", "deletecollection", "impersonate", "escalate", "bind", "approve", "sign", "*"}

# The one enumerated exception to the read-only console: the
# opt-in restore-drill teardown Role, exactly this rule, nothing else.
DRILL_TEARDOWN_RULES = [{"apiGroups": ["postgresql.cnpg.io"], "resources": ["clusters"], "verbs": ["get", "list", "watch", "delete"]}]


def check_console_rules(label, docs):
    """What every rendering of the console must hold, whatever the values."""
    for role in of(docs, "Role"):
        if role["metadata"]["name"].endswith("-restore-drills"):
            if role["rules"] != DRILL_TEARDOWN_RULES:
                problems.append(f"{label} Role {role['metadata']['name']}: the drill teardown Role must be exactly {DRILL_TEARDOWN_RULES}, got {role['rules']}")
            continue
        if role["metadata"]["name"].endswith("-state"):
            # The console's own state ConfigMap: that one name.
            want = [{"apiGroups": [""], "resources": ["configmaps"], "resourceNames": [role["metadata"]["name"]],
                     "verbs": ["get", "watch", "update"]}]
            if role["rules"] != want:
                problems.append(f"{label} Role {role['metadata']['name']}: the state Role must be exactly {want}, got {role['rules']}")
            continue
        if role["metadata"]["name"].endswith("-digest"):
            # The digest delivery Lease: get and update on that one
            # name; create cannot be scoped by name (hack/readonlycheck pins the
            # one Create call site).
            want = [{"apiGroups": ["coordination.k8s.io"], "resources": ["leases"], "resourceNames": [role["metadata"]["name"]],
                     "verbs": ["get", "update"]},
                    {"apiGroups": ["coordination.k8s.io"], "resources": ["leases"], "verbs": ["create"]}]
            if role["rules"] != want:
                problems.append(f"{label} Role {role['metadata']['name']}: the digest Lease Role must be exactly {want}, got {role['rules']}")
            continue
        for rule in role["rules"]:
            where = f"{label} Role {role['metadata']['name']}"
            if "secrets" in rule.get("resources", []) or "*" in rule.get("resources", []) or "*" in rule.get("apiGroups", []):
                problems.append(f"{where} grants {rule['resources']}: the console has no Secret verb and no wildcard")
            bad = FORBIDDEN_VERBS & set(rule["verbs"])
            if bad:
                problems.append(f"{where} grants write verbs {sorted(bad)} on {rule['resources']}: the console is read-only")
    for role in of(docs, "ClusterRole"):
        for rule in role["rules"]:
            where = f"{label} ClusterRole {role['metadata']['name']}"
            resources = set(rule.get("resources", []))
            if rule.get("nonResourceURLs"):
                if rule["nonResourceURLs"] != ["/metrics"] or rule["verbs"] != ["get"]:
                    problems.append(f"{where}: the only non-resource rule allowed is get /metrics, got {rule}")
                if not role["metadata"]["name"].endswith(":metrics-reader"):
                    problems.append(f"{where}: only the opt-in metrics-reader may grant a non-resource URL")
            elif resources == {"clusterimagecatalogs"}:
                if set(rule["verbs"]) - {"get", "list", "watch"}:
                    problems.append(f"{where}: clusterimagecatalogs must be read-only, got {rule['verbs']}")
            else:
                problems.append(f"{where} grants {sorted(resources)}: the only cluster-scoped authority is the opt-in clusterimagecatalogs read")
    for binding in of(docs, "ClusterRoleBinding"):
        for subject in binding["subjects"]:
            if subject["kind"] != "ServiceAccount":
                problems.append(f"{label} ClusterRoleBinding {binding['metadata']['name']} binds a {subject['kind']}")
    if of(docs, "CustomResourceDefinition"):
        problems.append(f"{label} renders a CRD: the console half ships none")


for label, docs in (("Helm console", console_helm), ("Helm console + clusterImageCatalogs", console_cat),
                    ("Helm console + persistent history", console_hist), ("Helm console + two namespaces", console_multi),
                    ("Helm console + metrics reader", console_metrics), ("Helm console + break-glass", console_bg),
                    ("Helm console + restore drills", console_drills), ("Helm console + digest delivery", console_digest),
                    ("kustomize console", console_kust)):
    if docs:
        check_console_rules(label, docs)

# The restore drills' guards: a drill NetworkPolicy in
# every drill namespace, selecting the labels the console puts on a drill's
# inheritedMetadata; and the opt-in delete guard, pinned to the console's seal.
drill_go = pathlib.Path("internal/console/ops/restoredrill.go").read_text()
def drill_const(name):
    m = re.search(rf'^\s*{name}\s*=\s*"([^"]+)"', drill_go, re.M)
    return m.group(1) if m else None
owner_label, of_label, seal = drill_const("LabelDrillOwner"), drill_const("LabelRestoreDrillOf"), drill_const("AnnotationRestoreDrillSeal")
if not (owner_label and of_label and seal):
    problems.append("internal/console/ops/restoredrill.go: LabelDrillOwner, LabelRestoreDrillOf or AnnotationRestoreDrillSeal not found")
elif console_drills:
    nps = of(console_drills, "NetworkPolicy", "-restore-drills")
    if sorted(n["metadata"]["namespace"] for n in nps) != ["team-a", "team-b"]:
        problems.append(f"Helm console + restore drills: a drill NetworkPolicy per drill namespace expected, got {[n['metadata'].get('namespace') for n in nps]}")
    for n in nps:
        sel = n["spec"]["podSelector"]
        if sel.get("matchLabels") != {owner_label: "team-a"} or {"key": of_label, "operator": "Exists"} not in sel.get("matchExpressions", []):
            problems.append(f"drill NetworkPolicy {n['metadata']['namespace']}: selects {sel}, not the console's drill labels")
        if sorted(n["spec"]["policyTypes"]) != ["Egress", "Ingress"]:
            problems.append(f"drill NetworkPolicy {n['metadata']['namespace']}: must restrict both Ingress and Egress")
    if of(console_drills, "ValidatingAdmissionPolicy"):
        problems.append("Helm console + restore drills renders a ValidatingAdmissionPolicy without admissionPolicy.create (cluster-scoped, opt-in)")
    guarded = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
                      "--set", "console.watchNamespaces={team-a,drills}", "--set", "console.restoreDrills.enabled=true",
                      "--set", "console.restoreDrills.namespace=drills", "--set", "console.restoreDrills.admissionPolicy.create=true"])
    vap = sole(guarded, "ValidatingAdmissionPolicy", "Helm console + drill guard") if guarded else None
    vapb = sole(guarded, "ValidatingAdmissionPolicyBinding", "Helm console + drill guard") if guarded else None
    if vap:
        rules = vap["spec"]["matchConstraints"]["resourceRules"]
        if rules != [{"apiGroups": ["postgresql.cnpg.io"], "apiVersions": ["*"], "operations": ["DELETE"], "resources": ["clusters"]}]:
            problems.append(f"drill guard policy matches {rules}, expected DELETE on clusters.postgresql.cnpg.io")
        expr = vap["spec"]["validations"][0]["expression"]
        for needle in (owner_label, of_label, seal, "startsWith(variables.prefix)"):
            if needle not in expr:
                problems.append(f"drill guard policy does not check {needle}")
        if vap["spec"].get("failurePolicy") != "Fail":
            problems.append("drill guard policy must fail closed")
    if vapb:
        if "Deny" not in vapb["spec"]["validationActions"]:
            problems.append("drill guard binding does not Deny by default")
        if vap and vapb["spec"]["policyName"] != vap["metadata"]["name"]:
            problems.append("drill guard binding names another policy")

if console_helm and console_kust:
    h_role = sole(console_helm, "Role", "Helm console", "pgc-console")
    k_role = sole(console_kust, "Role", "kustomize console", "pgtoolbox-console")
    if h_role and k_role and norm_rules(h_role["rules"]) != norm_rules(k_role["rules"]):
        problems.append("console Role rules: Helm differs from config/console/role.yaml")

    def rev_rules(docs, label):
        role = sole(docs, "Role", label, "revocations")
        return norm_rules(role["rules"]) if role else None

    h_rev, k_rev = rev_rules(console_helm, "Helm console"), rev_rules(console_kust, "kustomize console")
    if h_rev and k_rev:
        # The ConfigMap name is release-specific; shape and verbs are not.
        strip = lambda rules: [json.loads(r) | {"resourceNames": ["<revocations>"]} for r in rules]
        if strip(h_rev) != strip(k_rev):
            problems.append("console revocation Role: Helm differs from config/console/role.yaml")
        for rule in (json.loads(r) for r in h_rev):
            if rule.get("resources") != ["configmaps"] or len(rule.get("resourceNames", [])) != 1:
                problems.append(f"console revocation Role must grant configmaps by exactly one name, got {rule}")

    for label, docs in (("Helm console", console_helm), ("kustomize console", console_kust)):
        cms = of(docs, "ConfigMap", "-state")
        roles = of(docs, "Role", "-state")
        if len(cms) != 1 or len(roles) != 1 or roles[0]["rules"][0].get("resourceNames") != [cms[0]["metadata"]["name"]]:
            problems.append(f"{label}: the state ConfigMap and its Role must be rendered together, the Role naming that ConfigMap")
        if of(docs, "Role", "-digest"):
            problems.append(f"{label}: the digest Lease Role is rendered without a delivery channel")

    # No cluster-scoped grant by default: access questions are self reviews
    # sent with the asker's token.
    for label, docs in (("Helm console", console_helm), ("kustomize console", console_kust)):
        for kind in ("ClusterRole", "ClusterRoleBinding"):
            if of(docs, kind):
                problems.append(f"{label} renders a {kind} by default; the console needs none")

    h_dep = sole(console_helm, "Deployment", "Helm console")
    k_dep = sole(console_kust, "Deployment", "kustomize console")
    if h_dep and k_dep:
        hs, ks = h_dep["spec"], k_dep["spec"]
        hp, kp = hs["template"]["spec"], ks["template"]["spec"]
        hc, kc = hp["containers"][0], kp["containers"][0]
        for field, h, k in (
            ("replicas", hs["replicas"], ks["replicas"]),
            ("strategy", hs["strategy"], ks["strategy"]),
            ("pod securityContext", hp["securityContext"], kp["securityContext"]),
            ("automountServiceAccountToken", hp["automountServiceAccountToken"], kp["automountServiceAccountToken"]),
            ("container securityContext", hc["securityContext"], kc["securityContext"]),
            ("resources", hc["resources"], kc["resources"]),
            ("ports", hc["ports"], kc["ports"]),
            ("livenessProbe", hc["livenessProbe"], kc["livenessProbe"]),
            ("readinessProbe", hc["readinessProbe"], kc["readinessProbe"]),
            ("volumeMounts", sorted(hc["volumeMounts"], key=lambda m: m["name"]), sorted(kc["volumeMounts"], key=lambda m: m["name"])),
            ("volume names", sorted(v["name"] for v in hp["volumes"]), sorted(v["name"] for v in kp["volumes"])),
        ):
            if h != k:
                problems.append(f"console Deployment {field}: Helm {h} != kustomize {k}")
        henv = {e["name"]: e for e in hc["env"]}
        kenv = {e["name"]: e for e in kc["env"]}
        if set(henv) != set(kenv):
            problems.append(f"console env contract: Helm {sorted(henv)} != kustomize {sorted(kenv)}")
        else:
            # Values that are not release-specific must agree too.
            for name in sorted(henv):
                if name in ("WATCH_NAMESPACES", "REVOCATION_CONFIGMAP", "STATE_CONFIGMAP", "INSIGHT_PACK_CONFIGMAP", "SESSION_KEY_FILE"):
                    continue
                if henv[name] != kenv[name]:
                    problems.append(f"console env {name}: Helm {henv[name]} != kustomize {kenv[name]}")
    h_svc = sole(console_helm, "Service", "Helm console")
    k_svc = sole(console_kust, "Service", "kustomize console")
    if h_svc and k_svc and h_svc["spec"]["ports"] != k_svc["spec"]["ports"]:
        problems.append("console Service ports: Helm differs from config/console/service.yaml")

# 12b. The console's environment contract with the binary: every
#      variable the chart renders is one the console reads. A name the binary does
#      not read is a setting that silently does nothing (OIDC_GROUPS_CLAIM was one).
#      The names are taken from the Go sources, not from a list kept here.
def go_env_names():
    names = set()
    sources = list(pathlib.Path("internal/console").rglob("*.go")) + list(pathlib.Path("cmd/console").glob("*.go"))
    for src in sources:
        if src.name.endswith("_test.go"):
            continue
        names |= set(re.findall(r'"([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)"', src.read_text()))
    return names


binary_env = go_env_names()
contract_renderings = {
    "default": [],
    "oidc": ["--set", "console.auth.provider=oidc", "--set", "console.auth.oidc.issuerURL=https://idp.example.org",
             "--set", "console.auth.oidc.clientID=c", "--set", "console.auth.oidc.clientSecret.existingSecret=s",
             "--set", "console.auth.oidc.caConfigMap=ca", "--set", "console.auth.oidc.requireEmailVerified=false"],
    "openshift": ["--set", "console.auth.provider=openshift", "--set", "console.auth.openshift.redirectURI=https://c.example.org/auth/callback",
                  "--set", "console.auth.openshift.clientID=system:serviceaccount:team-a:c",
                  "--set", "console.auth.openshift.clientSecret.existingSecret=s",
                  "--set", "console.auth.openshift.discoveryURL=https://o.example.org/x",
                  "--set", "console.auth.openshift.caConfigMap=ca"],
    "everything else": ["--set", "console.tls.secretName=t", "--set", "console.history.persistent=true",
                        "--set", "console.clusterSelector.team=a", "--set", "console.publicURL=https://c.example.org",
                        "--set-string", "console.cookieSecure=true", "--set", "console.trustedProxyHops=1",
                        "--set", "console.auth.sessionMaxAge=12h", "--set", "console.auth.tokenLogin=false",
                        "--set", "console.auth.idpTimeout=10s", "--set", "console.clusterImageCatalogs.enabled=true",
                        "--set", "console.rbac.podLogs=false", "--set", "console.evidence.interval=2m",
                        "--set", "console.operations.enabled=false", "--set", "console.monitoringURL=https://m.example.org"],
    "restore drills": ["--set", "console.watchNamespaces={team-a,drills}", "--set", "console.restoreDrills.enabled=true",
                       "--set", "console.restoreDrills.namespace=drills",
                       "--set", "console.restoreDrills.resources.requests.cpu=1", "--set", "console.restoreDrills.resources.requests.memory=1Gi",
                       "--set", "console.restoreDrills.resources.limits.cpu=2", "--set", "console.restoreDrills.resources.limits.memory=2Gi",
                       "--set", "console.restoreDrills.storage.size=10Gi", "--set", "console.restoreDrills.storage.storageClass=fast",
                       "--set", "console.restoreDrills.maxConcurrent=5", "--set", "console.restoreDrills.serverCA.secret=drill-ca"],
    "evidence off": ["--set", "console.evidence.enabled=false"],
    "digest delivery": ["--set", "console.history.persistent=true", "--set", "console.publicURL=https://c.example.org",
                        "--set", "console.digest.delivery.chat.existingSecret=chat"],
    "evidence agents": ["--set", "evidence.agents[0].cluster=orders", "--set", "evidence.agents[0].secrets[0]=s"],
    "prometheus": ["--set", "console.prometheus.url=https://prom.example.org", "--set", "console.prometheus.token.projected.enabled=true",
                   "--set", "console.prometheus.caConfigMap=ca", "--set", "console.prometheus.namespaced=true",
                   "--set", "console.prometheus.range=2h"],
}
rendered_env = set()
for label, extra in contract_renderings.items():
    docs = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a"] + extra)
    # The console's workload only: "evidence agents" renders an agent's too;
    # persistent history makes it a StatefulSet.
    kind = "StatefulSet" if docs and of(docs, "StatefulSet", "console") else "Deployment"
    dep = sole(docs, kind, f"Helm console ({label})", "console") if docs else None
    if dep:
        rendered_env |= {e["name"] for e in dep["spec"]["template"]["spec"]["containers"][0]["env"]}
unread = sorted(rendered_env - binary_env)
if unread:
    problems.append(f"the console chart renders {unread}, which the console binary does not read (internal/console, cmd/console)")
if not {"OPENSHIFT_CLIENT_ID", "OPENSHIFT_CLIENT_SECRET_FILE", "OPENSHIFT_DISCOVERY_URL", "OPENSHIFT_SCOPES", "OPENSHIFT_CA_FILE"} <= rendered_env:
    problems.append("the chart does not render all five OPENSHIFT_* variables the console reads")
if "EVIDENCE_AGENTS_FILE" not in rendered_env:
    problems.append("the chart does not render EVIDENCE_AGENTS_FILE with evidence.agents")
if "DIGEST_LEASE" not in rendered_env:
    problems.append("the chart does not render DIGEST_LEASE with a digest delivery channel")


# 12c. The EvidenceAgent channel, against the Go constants it must agree with: the audience
#      and the token path (pkg/evidence/v1alpha1), the pod label the NetworkPolicy
#      selects and the port (internal/operator/evidenceagent, pkg/evidence/v1alpha1).
channel = pathlib.Path("pkg/evidence/v1alpha1/channel.go").read_text()
# Anchored: a prefix constant must never satisfy a search for the audience. The
# agent of namespace N accepts the audience ClusterWideTokenAudience + "/" + N,
# projected under DefaultTokenDirectory/N/TokenFileName.
def go_const(name):
    m = re.search(rf'^\s*{name}\s*=\s*"([^"]+)"', channel, re.M)
    return m.group(1) if m else None
audience = go_const("ClusterWideTokenAudience")
token_dir = go_const("DefaultTokenDirectory")
token_file = go_const("TokenFileName") or "token"
if not audience or not token_dir:
    problems.append("pkg/evidence/v1alpha1/channel.go defines no ClusterWideTokenAudience or DefaultTokenDirectory")
    audience, token_dir = audience or "", token_dir or ""
agent_port = int(re.search(r'AgentPort\s+=\s*(\d+)', channel).group(1))
component = re.search(r'componentName\s*=\s*"([^"]+)"', pathlib.Path("internal/operator/evidenceagent/specs.go").read_text()).group(1)
console_vals = load("charts/pgtools/values.yaml")["console"]
agent_np = console_vals["networkPolicy"]["evidenceAgent"]
if agent_np["podLabels"] != {"app.kubernetes.io/name": component}:
    problems.append(f"console.networkPolicy.evidenceAgent.podLabels {agent_np['podLabels']} is not the label the operator gives agent pods (app.kubernetes.io/name={component})")
if agent_np["port"] != agent_port:
    problems.append(f"console.networkPolicy.evidenceAgent.port {agent_np['port']} is not the agent's port {agent_port}")
for label, docs in (("Helm console", console_helm), ("kustomize console", console_kust)):
    dep = sole(docs, "Deployment", label) if docs else None
    if not dep:
        continue
    pod = dep["spec"]["template"]["spec"]
    # One projection per watched namespace: the audience is
    # <TokenAudience>/<namespace> and the file <namespace>/token under the
    # directory the console reads (EVIDENCE_TOKEN_DIR).
    watched = [e for e in pod["containers"][0]["env"] if e["name"] == "WATCH_NAMESPACES"]
    if "value" in watched[0]:
        namespaces = watched[0]["value"].split(",")
    else:  # the kustomize base watches the namespace it is applied to
        namespaces = [dep["metadata"]["namespace"]]
    projected = [s["serviceAccountToken"] for v in pod["volumes"] if v["name"] == "evidence-agent-token"
                 for s in v.get("projected", {}).get("sources", []) if "serviceAccountToken" in s]
    want_sources = sorted((f"{audience}/{ns}", f"{ns}/{token_file}") for ns in namespaces)
    if sorted((p_.get("audience"), p_.get("path")) for p_ in projected) != want_sources:
        problems.append(f"{label}: the evidence-agent-token projections {[(p_.get('audience'), p_.get('path')) for p_ in projected]} are not {want_sources} (one per watched namespace)")
    envs = {e["name"]: e.get("value") for e in pod["containers"][0]["env"]}
    if "EVIDENCE_TOKEN_PATH" in envs:
        problems.append(f"{label}: EVIDENCE_TOKEN_PATH is gone, the console refuses to start with it (use EVIDENCE_TOKEN_DIR)")
    if envs.get("EVIDENCE_TOKEN_DIR") != token_dir:
        problems.append(f"{label}: EVIDENCE_TOKEN_DIR {envs.get('EVIDENCE_TOKEN_DIR')} is not {token_dir}")
    mounts = [m for m in pod["containers"][0]["volumeMounts"] if m["name"] == "evidence-agent-token"]
    if [m["mountPath"] for m in mounts] != [token_dir] or not all(m.get("readOnly") for m in mounts):
        problems.append(f"{label}: evidence-agent-token must be mounted read-only at {token_dir}")
console_role_rules = [r for role in of(console_helm, "Role") for r in role["rules"]] if console_helm else []
for kind in ("evidenceagents", "pgadmins", "pgaccessrequests"):
    if not any(kind in r.get("resources", []) and {"get", "list", "watch"} <= set(r["verbs"]) for r in console_role_rules):
        problems.append(f"the console Role must hold get, list and watch on {kind} (informers; the agent's SubjectAccessReview checks get; pgaccessrequests: the console is a UI over them)")

# The break-glass identity is a person's access, not the console's: a token-less
# ServiceAccount bound to pgtoolbox-console-operator by RoleBindings in the watched namespaces
# only, and nothing else (no Role of its own, no ClusterRoleBinding, nothing off by default).
if console_helm and (of(console_helm, "RoleBinding", "pgtoolbox-console-breakglass") or
                     any(d["kind"] == "ServiceAccount" and d["metadata"]["name"].endswith("breakglass") for d in console_helm)):
    problems.append("the break-glass identity is rendered by default: it is opt-in")
if console_bg:
    bg_sa = [d for d in of(console_bg, "ServiceAccount") if d["metadata"]["name"] == "pgtoolbox-console-breakglass"]
    bg_rb = [d for d in of(console_bg, "RoleBinding") if d["metadata"]["name"] == "pgtoolbox-console-breakglass"]
    if len(bg_sa) != 1 or bg_sa[0].get("automountServiceAccountToken") is not False:
        problems.append("the break-glass ServiceAccount must be rendered once, with automountServiceAccountToken: false")
    if sorted(d["metadata"]["namespace"] for d in bg_rb) != ["team-a", "team-b"] or any(
            d["roleRef"] != {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "pgtoolbox-console-operator"}
            or [s["name"] for s in d["subjects"]] != ["pgtoolbox-console-breakglass"] for d in bg_rb):
        problems.append("break-glass must be bound to ClusterRole pgtoolbox-console-operator by one RoleBinding per watched namespace")
    if len(of(console_bg, "ClusterRoleBinding")) != len(of(console_helm, "ClusterRoleBinding")):
        problems.append("break-glass must add no ClusterRoleBinding")

# console.breakGlass.roleKind=Role binds a namespaced Role instead of the ClusterRole, which only
# the operator chart creates. Its rules are the pgtoolbox chart's pgtoolbox-console-operator ClusterRole,
# rule for rule, so the two cannot drift; the pgtoolbox.fyannk.dev rules are left out when the
# operator's kinds are not served (Kubernetes' escalation check refuses a creator who lacks
# those verbs, and a namespace admin lacks them on kinds that are not installed).
bg_cluster_role = next((d for d in helm_full if d["kind"] == "ClusterRole" and d["metadata"]["name"] == "pgtoolbox-console-operator"), None)
if bg_cluster_role is None:
    problems.append("the pgtoolbox chart renders no pgtoolbox-console-operator ClusterRole to compare the break-glass Role with")
else:
    for label, docs, want in (
            ("with the operator's kinds", console_bg_role, bg_cluster_role["rules"]),
            ("without the operator's kinds", console_bg_role_bare,
             [r for r in bg_cluster_role["rules"] if "pgtoolbox.fyannk.dev" not in r.get("apiGroups", [])])):
        if not docs:
            continue
        roles = [d for d in of(docs, "Role") if d["metadata"]["name"] == "pgtoolbox-console-breakglass"]
        bindings = [d for d in of(docs, "RoleBinding") if d["metadata"]["name"] == "pgtoolbox-console-breakglass"]
        if sorted(d["metadata"]["namespace"] for d in roles) != ["team-a", "team-b"]:
            problems.append(f"break-glass roleKind=Role ({label}) must render one Role per watched namespace")
        for r in roles:
            if r["rules"] != want:
                problems.append(f"break-glass Role in {r['metadata']['namespace']} ({label}) differs from the pgtoolbox-console-operator ClusterRole's rules: "
                                f"{json.dumps(r['rules'], sort_keys=True)} != {json.dumps(want, sort_keys=True)}")
        if sorted(d["metadata"]["namespace"] for d in bindings) != ["team-a", "team-b"] or any(
                d["roleRef"] != {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "pgtoolbox-console-breakglass"}
                for d in bindings):
            problems.append(f"break-glass roleKind=Role ({label}) must bind that Role by one RoleBinding per watched namespace")
        if of(docs, "ClusterRole") != of(console_helm, "ClusterRole") or of(docs, "ClusterRoleBinding"):
            problems.append(f"break-glass roleKind=Role ({label}) must add no ClusterRole or ClusterRoleBinding")
# The availability rule, on the rendered objects: two
# replicas rolling, with or without persistent history; persistent history is
# a StatefulSet with one history claim per replica (the file takes an
# exclusive lock, so two pods never share one), and the same disruption budget.
if console_helm and console_hist:
    default = sole(console_helm, "Deployment", "Helm console")
    persistent = sole(console_hist, "StatefulSet", "Helm console + persistent history")
    if of(console_hist, "Deployment"):
        problems.append("console with history.persistent renders a Deployment: it is a StatefulSet")
    if default and (default["spec"]["replicas"] != 2 or default["spec"]["strategy"]["type"] != "RollingUpdate"):
        problems.append("console default must be 2 replicas with RollingUpdate")
    if persistent:
        ps = persistent["spec"]
        claims = ps.get("volumeClaimTemplates", [])
        if (ps["replicas"] != 2 or ps.get("podManagementPolicy") != "Parallel"
                or ps.get("updateStrategy", {}).get("type") != "RollingUpdate"
                or [c["metadata"]["name"] for c in claims] != ["history"]
                or any(v["name"] == "history" for v in ps["template"]["spec"].get("volumes", []))):
            problems.append("console with history.persistent must be a StatefulSet of 2 replicas, Parallel, RollingUpdate, "
                            "with the history claim template and no shared history volume")
        if default and ps["template"]["spec"]["securityContext"] != default["spec"]["template"]["spec"]["securityContext"]:
            problems.append("console StatefulSet pod securityContext differs from the Deployment's")
    if len(of(console_hist, "PodDisruptionBudget")) != 1:
        problems.append("console with history.persistent must keep its PodDisruptionBudget")
if console_multi:
    roles = [d for d in of(console_multi, "Role") if not d["metadata"]["name"].endswith(("revocations", "-state"))]
    if sorted(r["metadata"]["namespace"] for r in roles) != ["team-a", "team-b"]:
        problems.append("console with two watchNamespaces must render one Role in each")
if console_metrics:
    roles = [r["metadata"]["name"] for r in of(console_metrics, "ClusterRole")]
    if len(roles) != 1 or not roles[0].endswith(":metrics-reader"):
        problems.append(f"console with metrics.readerClusterRole renders ClusterRoles {roles}, expected the metrics-reader alone")
    for b in of(console_metrics, "ClusterRoleBinding"):
        if b["metadata"]["name"].endswith(":metrics-reader") and [s["name"] for s in b["subjects"]] != ["prom"]:
            problems.append("the metrics-reader binding must bind exactly the named scraper ServiceAccounts, never the console")
if console_cat:
    if len(of(console_cat, "ClusterRole")) != 1:
        problems.append("console with clusterImageCatalogs renders other than 1 ClusterRole (the catalog read)")
# NetworkPolicy is the installing admin's choice and never a default.
for label, docs in (("Helm console", console_helm), ("Helm console + persistent history", console_hist)):
    if docs and of(docs, "NetworkPolicy"):
        problems.append(f"{label} renders a NetworkPolicy by default: it is the admin's choice, never on by default")

# OLM: the bundle ships the operator and no console. The manager legitimately holds
# subjectaccessreviews/tokenreviews `create` (it grants them to the EvidenceAgent
# ServiceAccount, which may not hold more than its creator), so the check is on
# what runs: one Deployment, the manager, and no console container.
olm_deployments = csv["spec"]["install"]["spec"]["deployments"]
if [d["name"] for d in olm_deployments] != ["pgtoolbox"] or any(
        c["name"] != "manager" for d in olm_deployments for c in d["spec"]["template"]["spec"]["containers"]):
    problems.append("the OLM bundle must run the manager only: a team console is installed from the chart or config/console")
# Each chart ships its own piece on: the operator in pgtoolbox, the
# console in pgtools; the console half left pgtoolbox's values.
chart_defaults = load("charts/pgtoolbox/values.yaml")
tools_defaults = load("charts/pgtools/values.yaml")
if chart_defaults["operator"]["enabled"] is not True or "console" in chart_defaults:
    problems.append("charts/pgtoolbox/values.yaml must ship operator.enabled: true and no console section")
if tools_defaults["console"]["enabled"] is not True or "operator" in tools_defaults:
    problems.append("charts/pgtools/values.yaml must ship console.enabled: true and no operator section")
if tools_defaults["console"]["networkPolicy"]["enabled"] is not False:
    problems.append("charts/pgtools/values.yaml console.networkPolicy.enabled must be false: never on by default")
if console_helm and any(d["kind"] in ("ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition") for d in console_helm):
    problems.append("the pgtools chart renders a cluster-scoped object by default: a namespace admin must be able to install it")
if expect_image := tools_defaults["console"]["image"]["repository"]:
    if expect_image != chart_defaults["operator"]["console"]["image"]["repository"]:
        problems.append("operator.console.image.repository (pgtoolbox) and console.image.repository (pgtools) differ: the two install paths would drift")
# The CRDs of the operator half, rendered from operator-crds/, equal the generated ones.
if helm_docs:
    rendered = {d["spec"]["names"]["kind"]: d["spec"] for d in helm_docs if d["kind"] == "CustomResourceDefinition"}
    for crd in pathlib.Path("config/crd/bases").glob("*.yaml"):
        want = load(crd)
        if rendered.get(want["spec"]["names"]["kind"]) != want["spec"]:
            problems.append(f"Helm operator.enabled renders a different {want['spec']['names']['kind']} CRD than {crd}")

# 12e. The pgtools evidence agents: every variable the chart gives an
#      agent is one the agent binary reads (internal/evidence, cmd/evidence-agent),
#      the Role is namespaced and names what it grants (the Cluster, `get` on the
#      listed Secrets only), and the pod carries the label and port the console's
#      NetworkPolicy selects (the operator's, compared above with the Go constants).
def agent_env_names():
    names = set()
    sources = list(pathlib.Path("internal/evidence").rglob("*.go")) + list(pathlib.Path("cmd/evidence-agent").glob("*.go"))
    for src in sources:
        if src.name.endswith("_test.go"):
            continue
        names |= set(re.findall(r'"([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)"', src.read_text()))
    return names


agent_binary_env = agent_env_names()
agent_base = ["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a",
              "--set", "evidence.agents[0].cluster=orders", "--set", "evidence.agents[0].secrets={orders-s3,minio-ca}",
              "--set", "evidence.agents[0].objectStore=orders-store", "--set", "evidence.logLevel=debug"]
agent_renderings = {
    "plain": [],
    "certManager": ["--set", "evidence.tls.mode=certManager", "--set", "evidence.tls.certManager.issuerRef.name=ca"],
    "openshiftServiceCA": ["--set", "evidence.tls.mode=openshiftServiceCA"],
}
agent_rendered_env = set()
for label, extra in agent_renderings.items():
    docs = render(agent_base + extra)
    if not docs:
        continue
    where = f"pgtools evidence agent ({label})"
    dep = sole(docs, "Deployment", where, "evidence-orders")
    if dep:
        pod = dep["spec"]["template"]["spec"]
        container = pod["containers"][0]
        envs = {e["name"]: e.get("value") for e in container["env"]}
        agent_rendered_env |= set(envs)
        if any("valueFrom" in e for e in container["env"]):
            problems.append(f"{where}: an agent variable is read from a Secret; the agent reads its credentials at run time, not from its environment")
        if envs.get("AGENT_ALLOWED_SECRETS") != "orders-s3,minio-ca" or envs.get("CNPG_CLUSTER_NAME") != "orders":
            problems.append(f"{where}: AGENT_ALLOWED_SECRETS / CNPG_CLUSTER_NAME are not the entry's secrets and cluster")
        if dep["spec"]["replicas"] != 1 or pod.get("automountServiceAccountToken") is not False:
            problems.append(f"{where}: one replica and automountServiceAccountToken: false")
        if "runAsUser" in pod.get("securityContext", {}) or "runAsUser" in container.get("securityContext", {}):
            problems.append(f"{where}: no fixed UID")
        if pod["securityContext"].get("runAsNonRoot") is not True or container["securityContext"].get("allowPrivilegeEscalation") is not False \
                or container["securityContext"].get("readOnlyRootFilesystem") is not True or container["securityContext"].get("capabilities", {}).get("drop") != ["ALL"]:
            problems.append(f"{where}: the pod is not Pod Security restricted")
        if dep["spec"]["template"]["metadata"]["labels"].get("app.kubernetes.io/name") != component:
            problems.append(f"{where}: the pod label app.kubernetes.io/name is not {component}, which the console's NetworkPolicy selects")
        if [p_["containerPort"] for p_ in container["ports"]] != [agent_port]:
            problems.append(f"{where}: the container port is not the agent port {agent_port}")
    for kind in ("ClusterRole", "ClusterRoleBinding"):
        if of(docs, kind):
            problems.append(f"{where} renders a {kind}: the team agents need nothing cluster-scoped")
    role = sole(docs, "Role", where, "evidence-orders")
    if role:
        by_resource = {r_["resources"][0]: r_ for r_ in role["rules"]}
        secrets = by_resource.get("secrets", {})
        if set(by_resource) != {"clusters", "objectstores", "secrets"} or any(len(r_["resources"]) != 1 or "*" in r_["apiGroups"] for r_ in role["rules"]):
            problems.append(f"{where} Role grants {sorted(by_resource)}, expected clusters, objectstores and secrets, one resource per rule")
        if secrets.get("verbs") != ["get"] or secrets.get("resourceNames") != ["orders-s3", "minio-ca"]:
            problems.append(f"{where} Role must grant get, and only get, on the listed Secrets by name; got {secrets}")
        if by_resource.get("clusters", {}).get("resourceNames") != ["orders"] or by_resource.get("objectstores", {}).get("resourceNames") != ["orders-store"]:
            problems.append(f"{where} Role must name its Cluster and its ObjectStore")
        for r_ in role["rules"]:
            if set(r_["verbs"]) - {"get", "list", "watch"}:
                problems.append(f"{where} Role grants {r_['verbs']} on {r_['resources']}: the agent only reads")
agent_unread = sorted(agent_rendered_env - agent_binary_env)
if agent_unread:
    problems.append(f"the pgtools chart gives the evidence agent {agent_unread}, which the agent binary does not read (internal/evidence, cmd/evidence-agent)")
if not {"AGENT_PLAIN_HTTP", "TLS_CERT_FILE", "TLS_KEY_FILE", "AGENT_RESOLVE", "AGENT_AUTH", "AGENT_TOKEN_FILE", "AGENT_ALLOWED_SECRETS", "LOG_LEVEL"} <= agent_rendered_env:
    problems.append(f"the pgtools agent renderings miss variables of the team profile: {sorted(agent_rendered_env)}")

# 12d. The probe listener follows the TLS setting: with
#      console.tls.secretName the whole probe port serves HTTPS, so the probes must say
#      scheme HTTPS and the monitors must scrape https; without it, plain HTTP everywhere.
for tls_on in (False, True):
    extra = ["--set", "console.metrics.serviceMonitor.enabled=true", "--set", "console.metrics.podMonitor.enabled=true"]
    if tls_on:
        extra += ["--set", "console.tls.secretName=t"]
    docs = render(["helm", "template", "pgc", "charts/pgtools", "--namespace", "team-a"] + extra)
    dep = sole(docs, "Deployment", "Helm console (TLS probe check)") if docs else None
    if not dep:
        continue
    want_scheme = "HTTPS" if tls_on else None
    c = dep["spec"]["template"]["spec"]["containers"][0]
    for probe in ("livenessProbe", "readinessProbe"):
        got = c[probe]["httpGet"].get("scheme")
        if got != want_scheme:
            problems.append(f"console.tls.secretName {'set' if tls_on else 'unset'}: {probe} scheme is {got}, expected {want_scheme}")
    for d in docs:
        eps = d["spec"].get("endpoints", []) + d["spec"].get("podMetricsEndpoints", []) if d["kind"] in ("ServiceMonitor", "PodMonitor") else []
        for ep in eps:
            if (ep.get("scheme") == "https") != tls_on or (tls_on and not ep.get("tlsConfig", {}).get("serverName")):
                problems.append(f"console.tls.secretName {'set' if tls_on else 'unset'}: {d['kind']} endpoint is {ep.get('scheme')}/{ep.get('tlsConfig')}, expected {'https with a serverName' if tls_on else 'http'}")

for problem in problems:
    print(f"FAIL {problem}")
sys.exit(1 if problems else 0)
PY

# 13. Digest pins. The committed tree names tags (the digests do not exist until the
#     images are pushed); the release rewrites a copy through `release.sh pin-chart` /
#     `pin-refs`. Run that rewrite here with made-up digests and compare the three
#     renderings: Helm, the CSV and the catalog must all name the SAME digest for each
#     image, the tag must survive in a comment, and nothing may stay on a tag.
pin_tmp="$(mktemp -d)"
trap 'rm -rf "${pin_tmp}"' EXIT
pin_prefix=ghcr.io/fyannk
{
  for n in pgtoolbox-operator pgtoolbox-console pgtoolbox-evidence-agent pgtoolbox-bundle; do
    printf '%s/%s@sha256:%s\n' "${pin_prefix}" "${n}" "$(printf '%s' "${n}" | sha256sum | cut -d' ' -f1)"
  done
} > "${pin_tmp}/digests.txt"
pin_version="$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' charts/pgtoolbox/Chart.yaml)"
cp -r charts/pgtoolbox "${pin_tmp}/chart"
cp -r charts/pgtools "${pin_tmp}/tools"
cp -r deploy/olm/bundle "${pin_tmp}/bundle"
cp -r deploy/olm/catalog "${pin_tmp}/catalog"
./hack/release.sh pin-chart "${pin_tmp}/chart" "${pin_tmp}/digests.txt" "${pin_version}" || { bad "release.sh pin-chart failed on the committed chart"; }
./hack/release.sh verify-pins "${pin_tmp}/chart" "${pin_tmp}/digests.txt" || { bad "the pinned chart does not render digests"; }
./hack/release.sh pin-chart "${pin_tmp}/tools" "${pin_tmp}/digests.txt" "${pin_version}" || { bad "release.sh pin-chart failed on the committed pgtools chart"; }
./hack/release.sh verify-pins "${pin_tmp}/tools" "${pin_tmp}/digests.txt" || { bad "the pinned pgtools chart does not render digests"; }
./hack/release.sh pin-refs "${pin_tmp}/bundle/manifests/pgtoolbox.clusterserviceversion.yaml" "${pin_tmp}/digests.txt" "${pin_version}"
./hack/release.sh pin-refs "${pin_tmp}/catalog/pgtoolbox/catalog.yaml" "${pin_tmp}/digests.txt" "${pin_version}"
PIN_TMP="${pin_tmp}" PIN_VERSION="${pin_version}" python3 - <<'PY' || fail=1
import os, re, pathlib, subprocess, yaml
tmp = pathlib.Path(os.environ["PIN_TMP"]); version = os.environ["PIN_VERSION"]
problems = []
digests = {l.split("@")[0].rsplit("/", 1)[1]: l.strip() for l in (tmp / "digests.txt").read_text().split("\n") if l}
docs = []
for chart_dir in ("chart", "tools"):
    # pgtools renders its evidence agents only when one is listed.
    extra = ["--set", "evidence.agents[0].cluster=pins", "--set", "evidence.agents[0].secrets[0]=pins"] if chart_dir == "tools" else []
    out = subprocess.run(["helm", "template", "pins", str(tmp / chart_dir), "--namespace", "pins"] + extra, capture_output=True, text=True)
    rendered = [d for d in yaml.safe_load_all(out.stdout) if d] if out.returncode == 0 else []
    if not rendered:
        problems.append(f"pinned {chart_dir} does not render: {out.stderr.strip()[:200]}")
    docs += rendered
helm_images = {}
for d in docs:
    if d["kind"] != "Deployment":
        continue
    for c in d["spec"]["template"]["spec"]["containers"]:
        helm_images[c["name"]] = c["image"]
        for a in c.get("args", []):
            if a.startswith(("--operator-image=", "--default-evidence-agent-image=", "--default-console-image=")):
                helm_images[a.split("=")[0]] = a.split("=", 1)[1]
csv = yaml.safe_load((tmp / "bundle/manifests/pgtoolbox.clusterserviceversion.yaml").read_text())
manager = csv["spec"]["install"]["spec"]["deployments"][0]["spec"]["template"]["spec"]["containers"][0]
olm_args = {a.split("=")[0]: a.split("=", 1)[1] for a in manager["args"] if a.startswith(("--operator-image=", "--default-evidence-agent-image=", "--default-console-image="))}
pairs = [("manager image", helm_images.get("manager"), manager["image"], digests["pgtoolbox-operator"]),
         ("--operator-image", helm_images.get("--operator-image"), olm_args.get("--operator-image"), digests["pgtoolbox-operator"]),
         ("--default-evidence-agent-image", helm_images.get("--default-evidence-agent-image"), olm_args.get("--default-evidence-agent-image"), digests["pgtoolbox-evidence-agent"]),
         ("--default-console-image", helm_images.get("--default-console-image"), olm_args.get("--default-console-image"), digests["pgtoolbox-console"]),
         ("console image", helm_images.get("console"), None, digests["pgtoolbox-console"]),
         ("pgtools evidence agent image", helm_images.get("agent"), None, digests["pgtoolbox-evidence-agent"])]
for what, helm, olm, want in pairs:
    if helm != want:
        problems.append(f"pinned Helm {what} is {helm}, expected {want}")
    if olm is not None and olm != want:
        problems.append(f"pinned CSV {what} is {olm}, expected {want}")
if csv["metadata"]["annotations"]["containerImage"] != digests["pgtoolbox-operator"]:
    problems.append("pinned CSV containerImage is not the operator digest")
related = {r["name"]: r["image"] for r in csv["spec"]["relatedImages"]}
if related.get("manager") != digests["pgtoolbox-operator"] or related.get("evidence-agent") != digests["pgtoolbox-evidence-agent"] or related.get("console") != digests["pgtoolbox-console"]:
    problems.append(f"pinned CSV relatedImages {related} do not carry the digests")
if related.get("pgadmin", "").count("@") not in (0, 1):
    problems.append("pgadmin relatedImage is malformed")
catalog = [d for d in yaml.safe_load_all((tmp / "catalog/pgtoolbox/catalog.yaml").read_text()) if d]
for b in (d for d in catalog if d.get("schema") == "olm.bundle" and d["name"] == csv["metadata"]["name"]):
    if b["image"] != digests["pgtoolbox-bundle"]:
        problems.append(f"pinned catalog bundle image {b['image']} is not {digests['pgtoolbox-bundle']}")
for chart_dir, tagrefs in (("chart", ("pgtoolbox-operator", "pgtoolbox-console", "pgtoolbox-evidence-agent")), ("tools", ("pgtoolbox-console", "pgtoolbox-evidence-agent"))):
    values = (tmp / chart_dir / "values.yaml").read_text()
    for tagref in (f"{n}:{version}" for n in tagrefs):
        if not re.search(rf"^\s*(digest|image): .*# \S*{re.escape(tagref)}$", values, re.M):
            problems.append(f"the pinned {chart_dir} values.yaml does not keep {tagref} in a comment next to its digest")
# The committed tree is the unpinned one: digests appear only in a release.
src = yaml.safe_load(pathlib.Path("charts/pgtoolbox/values.yaml").read_text())
tools = yaml.safe_load(pathlib.Path("charts/pgtools/values.yaml").read_text())
if src["image"].get("digest") or src["operator"]["console"]["image"].get("digest") or tools["console"]["image"].get("digest") or tools["evidence"]["image"].get("digest") or src["operator"]["evidenceAgent"]["image"]:
    problems.append("values.yaml carries an image digest in the source tree: only hack/release.sh writes those, into the released copy")
for problem in problems:
    print(f"FAIL {problem}")
raise SystemExit(1 if problems else 0)
PY

# 14. pgAdmin without the operator, in the pgtools chart: off by
#     default; its pgAdmin image is the operator's compiled-in default and its
#     tools image the operator's own repository (the two install paths run the
#     same Pod); every ci rendering with it on stays in the namespace, and its
#     Role names objects (get, watch) and never lists; the release pins the
#     tools image like the operator's.
PIN_TMP="${pin_tmp}" python3 - <<'PY' || fail=1
import os, pathlib, re, subprocess, yaml
problems = []
tools = yaml.safe_load(pathlib.Path("charts/pgtools/values.yaml").read_text())
operator_values = yaml.safe_load(pathlib.Path("charts/pgtoolbox/values.yaml").read_text())
configuration = pathlib.Path("internal/operator/configuration/configuration.go").read_text()
pga = tools["pgadmin"]
if pga["enabled"] is not False or pga["networkPolicy"]["enabled"] is not False:
    problems.append("charts/pgtools/values.yaml must ship pgadmin.enabled: false and pgadmin.networkPolicy.enabled: false")
go_default = re.search(r'DefaultPgAdminImage = "([^"]+)"', configuration)
chart_image = f'{pga["image"]["repository"]}:{pga["image"]["tag"]}@{pga["image"]["digest"]}'
if not go_default or chart_image != go_default.group(1):
    problems.append(f"pgadmin.image {chart_image} is not the operator's DefaultPgAdminImage {go_default and go_default.group(1)}: the two install paths would drift")
go_proxy = re.search(r'DefaultOAuthProxyImage = "([^"]+)"', configuration)
proxy = pga["auth"]["openshift"]["image"]
if not go_proxy or "@sha256:" not in go_proxy.group(1) or f'{proxy["repository"]}:{proxy["tag"]}@{proxy["digest"]}' != go_proxy.group(1):
    problems.append(f"pgadmin.auth.openshift.image is not the operator's DefaultOAuthProxyImage {go_proxy and go_proxy.group(1)}, or either is not pinned by digest")
if pga["tools"]["image"]["repository"] != operator_values["image"]["repository"] or pga["tools"]["image"]["tag"] or pga["tools"]["image"]["digest"]:
    problems.append("pgadmin.tools.image must be the operator's repository, with tag and digest empty in the source tree (appVersion; the release pins it)")
for values in sorted(pathlib.Path("charts/pgtools/ci").glob("pgadmin*-values.yaml")):
    out = subprocess.run(["helm", "template", "pga", "charts/pgtools", "--namespace", "team-a", "-f", str(values)], capture_output=True, text=True)
    docs = [d for d in yaml.safe_load_all(out.stdout) if d] if out.returncode == 0 else []
    if not docs:
        problems.append(f"{values} does not render: {out.stderr.strip()[:200]}")
        continue
    for d in docs:
        if d["kind"] in ("ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition") or d["metadata"].get("namespace") != "team-a":
            problems.append(f"{values}: {d['kind']} {d['metadata']['name']} is cluster-scoped or outside the release namespace")
        if d["kind"] == "Role" and d["metadata"]["name"].endswith("-credentials"):
            for rule in d.get("rules") or []:
                if rule["verbs"] != ["get", "watch"] or not rule.get("resourceNames"):
                    problems.append(f"{values}: the pgAdmin Role rule {rule} is not get/watch by name")
pinned = (pathlib.Path(os.environ["PIN_TMP"]) / "tools" / "values.yaml").read_text()
if not re.search(r"^ {6}digest: sha256:[0-9a-f]{64} # \S*/pgtoolbox-operator:\S+$", pinned, re.M):
    problems.append("release.sh pin-chart does not pin pgadmin.tools.image.digest in the pgtools chart")
for problem in problems:
    print(f"FAIL {problem}")
raise SystemExit(1 if problems else 0)
PY

if [ "${fail}" -ne 0 ]; then
  echo "packaging is inconsistent"
  exit 1
fi
echo "packaging is consistent: Helm, kustomize and OLM agree on CRDs, RBAC, deployment, versions and images (the operator, charts/pgtoolbox); Helm and kustomize agree on the admission policies (config/admission, the opt-in config/components/admission), which the OLM bundle carries as webhooks the operator serves; Helm and kustomize agree on the console (charts/pgtools)"
