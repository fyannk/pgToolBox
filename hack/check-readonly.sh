#!/bin/sh
# Read-only boundary scan of the console (its read-model packages and the
# diagnostics engine). Mutation-shaped call sites may exist only in
# internal/console/ops, and the example Role must grant no mutating verb and
# nothing on secrets. The operator legitimately writes to the API server and
# is out of scope; so is the evidence agent, which has no
# client-go client: its only API-server calls are the two POSTs of
# internal/evidence/agent/reviewer.go (a TokenReview and a
# SubjectAccessReview per request), which create review objects and change
# no cluster state (hack/check-layering.sh keeps it away from the console
# and operator packages).
set -eu

status=0

# The console's mutation surface, on the typed syntax tree (hack/readonlycheck): every call of a mutating method on a Kubernetes
# client, or on any method that takes a Kubernetes object, in cmd/console,
# internal/console and pkg, is pinned by enclosing function and count. The
# writes are the person's own (internal/console/ops) and the reviews of
# internal/console/auth; the one write with the console's own identity is
# DrillReaper.Teardown's UID-preconditioned Delete, whose client
# (ops.NewDrillReaperClient) is built once, in internal/console/wiring, and
# reaches nothing else. A key-value store's Delete (pkg/cnpgstatus/history,
# internal/console/timeline/boltfile: bbolt buckets in a local file) takes
# no Kubernetes object and is not one, so neither package needs an exclusion.
if ! go run ./hack/readonlycheck; then
  status=1
fi

# The opt-in teardown Role of the chart (charts/pgtools, restoreDrills): delete
# on clusters only. hack/validate-packaging.sh checks the rendered rule exactly.
if ! grep -Eq '^ *verbs: \[get, list, watch, delete\]$' charts/pgtools/templates/console/rbac.yaml \
  || [ "$(grep -Ec '^ *verbs:.*\bdelete\b' charts/pgtools/templates/console/rbac.yaml)" -ne 1 ]; then
  echo "charts/pgtools: the restore-drill teardown Role must be the chart's only delete grant" >&2
  status=1
fi

# The same teardown grant, shipped for consoles the operator runs
# (config/rbac, bound only with PgToolBoxConsole spec.restoreDrills.enabled): its one
# delete is on clusters.postgresql.cnpg.io, its marker for the delete guard
# (config/admission) names no served API, and it holds nothing else.
drill_role=config/rbac/pgtoolbox_console_restore_drills_clusterrole.yaml
if [ "$(sed -n '/^rules:/,$p' "$drill_role" | grep -Ev '^ *#' | tr -d ' \n')" != \
  'rules:-apiGroups:-postgresql.cnpg.ioresources:-clustersverbs:-delete-get-list-watch-apiGroups:-pgtoolbox.fyannk.devresources:-restoredrillsverbs:-teardown' ]; then
  echo "$drill_role must grant get, list, watch and delete on clusters.postgresql.cnpg.io, the delete guard's marker (teardown on restoredrills) and nothing else" >&2
  status=1
fi

# pkg/cnpgstatus/history is pure domain plus a local storage engine, and must
# never import a Kubernetes client or API type.
if grep -rEn --include='*.go' 'k8s\.io/(client-go|api|apimachinery)|cloudnative-pg/api' pkg/cnpgstatus/history; then
  echo "pkg/cnpgstatus/history imports a Kubernetes client or API type" >&2
  status=1
fi

# The same for the timeline file's engine (internal/console/timeline/boltfile):
# raw bytes in bbolt, and no way to reach a Kubernetes client or any other
# console package.
if grep -rEn --include='*.go' 'k8s\.io/|cloudnative-pg/|sigs\.k8s\.io/|pgToolBox/internal/' internal/console/timeline/boltfile; then
  echo "internal/console/timeline/boltfile imports a Kubernetes client or a console package" >&2
  status=1
fi

# The console's own RBAC (config/console, which the chart and
# hack/validate-packaging.sh mirror): its ServiceAccount reads. Every write
# the console offers is made with the signed-in person's own token, so no
# Role it is bound to may carry a mutating or privilege verb, and none may
# name the secret resource. It holds no ClusterRole: its access questions are
# SelfSubjectAccessReviews asked with the user's own token.
# The exceptions are its own state ConfigMap, in a file of its
# own and pinned to exactly that rule, and the digest Lease below.
if grep -En '^ *verbs:.*\b(create|update|patch|delete|deletecollection|impersonate|escalate|bind)\b' config/console/role.yaml; then
  echo "mutating or privilege verb in the console's Role" >&2
  status=1
fi
if grep -En '\bsecrets\b' config/console/role.yaml; then
  echo "secret resource referenced in the console's RBAC" >&2
  status=1
fi
state_role=config/console/state.yaml
if [ "$(sed -n '/^kind: Role$/,/^---$/p' "$state_role" | sed -n '/^rules:/,/^---$/p' | grep -Ev '^ *#|^---$' | tr -d ' \n')" != \
  'rules:-apiGroups:[""]resources:[configmaps]resourceNames:[pgtoolbox-console-state]verbs:[get,watch,update]' ]; then
  echo "$state_role must grant get, watch and update on the pgtoolbox-console-state ConfigMap by name and nothing else" >&2
  status=1
fi
if grep -En '\bsecrets\b' "$state_role"; then
  echo "secret resource referenced in the console's state RBAC" >&2
  status=1
fi
# The digest Lease, with a delivery channel only: get and update
# on that one Lease by name, and create on leases, which RBAC cannot scope by
# name; hack/readonlycheck pins the one Create call site
# (internal/console/leader). hack/validate-packaging.sh checks the rendered
# Role exactly.
lease_role=charts/pgtools/templates/console/digest-lease.yaml
if [ "$(sed -n '/^rules:/,/^---$/p' "$lease_role" | grep -Ev '^ *#|^---$' | tr -d ' \n')" != \
  'rules:-apiGroups:[coordination.k8s.io]resources:[leases]resourceNames:[{{$lease|quote}}]verbs:[get,update]-apiGroups:[coordination.k8s.io]resources:[leases]verbs:[create]' ]; then
  echo "$lease_role must grant get and update on the digest Lease by name, create on leases, and nothing else" >&2
  status=1
fi
if grep -lE '^kind: *ClusterRole(Binding)?$' config/console/*.yaml; then
  echo "the console's kustomize base ships a cluster-scoped grant; it needs none" >&2
  status=1
fi

exit "$status"
