// Copyright 2026 The pgToolBox Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package console

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/exposure"
	"github.com/fyannk/pgToolBox/internal/operator/metadata"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
	"github.com/fyannk/pgToolBox/pkg/lifecycletable"
)

// The builders render, for one PgToolBoxConsole, the objects the pgtools chart's
// console renders for one release (charts/pgtools/templates/console), with
// the same environment, mounts and paths: the console binary cannot tell
// which of the two installed it. TestDeploymentMatchesTheChart holds them
// to that. What differs is ownership (a controller reference instead of a
// Helm release) and what a namespaced CR may ask for: the console watches
// its own namespace only.

// Names of the cluster-scoped RBAC the console relies on.
const (
	// ConsoleClusterRole holds the console's namespaced read rules (the
	// chart's console Role, rule for rule). Shipped with the operator
	// (config/rbac), never written by it: the operator may only bind it
	// (the bind verb on this name), so it cannot grant a console more
	// than this.
	ConsoleClusterRole = "pgtoolbox-console"
	// ConsolePodLogsClusterRole adds `get pods/log`.
	ConsolePodLogsClusterRole = "pgtoolbox-console-pod-logs"
	// ConsoleRestoreDrillsClusterRole adds get, list, watch and delete on
	// clusters.postgresql.cnpg.io: the teardown of the console's own drills.
	ConsoleRestoreDrillsClusterRole = "pgtoolbox-console-restore-drills"
)

// The console's ports, paths and keys: those of the chart.
const (
	httpPort    = 3000
	probePort   = 3001
	servicePort = 80

	sessionKey = "session-key"
	tokenKey   = "token"

	runDir           = "/var/run/console"
	lifecycleDir     = runDir + "/lifecycle"
	instanceCADir    = runDir + "/instance-ca"
	evidenceTokenDir = "/var/run/secrets/pgtoolbox.fyannk.dev/evidence-agent" // #nosec G101 -- a mount path, not a credential
	historyDir       = "/var/lib/console"
	digestDir        = runDir + "/digest"
	tracingDir       = runDir + "/tracing"
	drillSealDir     = runDir + "/restore-drill-seal"

	// streamTimeout keeps the console's event stream (SSE) open at the
	// edge: the chart's exposure.sse.timeout default.
	streamTimeout = time.Hour

	// stickyCookie is the edge's affinity cookie (the chart's
	// exposure.sticky.cookieName default): each replica keeps its own
	// history and charts, so a person stays on one. It names a replica,
	// nothing of the session.
	stickyCookie = "pgtoolbox-console-replica"

	// claimTemplateHashAnnotation records the hash of the StatefulSet's
	// claim templates, which the API server does not let change: a
	// different hash means the StatefulSet is replaced (its Pods kept).
	claimTemplateHashAnnotation = "pgtoolbox.fyannk.dev/claim-template-hash"

	componentConsole = "console"
)

// selectorLabels select the console's Pods. The PgToolBoxConsole label is in it, so
// a chart release of the same name in the namespace never selects them.
func selectorLabels(c *pgtoolboxv1alpha1.PgToolBoxConsole) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":                "pgtoolbox-console",
		"app.kubernetes.io/instance":            c.Name,
		"app.kubernetes.io/component":           componentConsole,
		pgtoolboxv1alpha1.LabelPgToolBoxConsole: c.Name,
	}
}

// identityLabels are the operator's labels on every generated object; they
// always win over propagated ones.
func identityLabels(c *pgtoolboxv1alpha1.PgToolBoxConsole) map[string]string {
	labels := selectorLabels(c)
	labels[pgtoolboxv1alpha1.LabelManagedBy] = pgtoolboxv1alpha1.ManagedByValue
	labels[pgtoolboxv1alpha1.LabelReconcile] = pgtoolboxv1alpha1.ReconcileLabelValue
	labels["app.kubernetes.io/part-of"] = "pgtoolbox"
	return labels
}

// objectMeta is a namespaced child's metadata: propagated metadata for
// target, then the identity labels, and the controller reference.
func objectMeta(c *pgtoolboxv1alpha1.PgToolBoxConsole, name string, target pgtoolboxv1alpha1.PropagationTarget) metav1.ObjectMeta {
	labels, annotations := metadata.Propagate(target, c.Spec.MetadataPropagation, identityLabels(c), nil)
	return metav1.ObjectMeta{
		Name:            name,
		Namespace:       c.Namespace,
		Labels:          labels,
		Annotations:     annotations,
		OwnerReferences: []metav1.OwnerReference{ownerReference(c)},
	}
}

func ownerReference(c *pgtoolboxv1alpha1.PgToolBoxConsole) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         pgtoolboxv1alpha1.GroupVersion.String(),
		Kind:               "PgToolBoxConsole",
		Name:               c.Name,
		UID:                c.UID,
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	}
}

func buildServiceAccount(c *pgtoolboxv1alpha1.PgToolBoxConsole) *corev1.ServiceAccount {
	sa := &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: objectMeta(c, c.WorkloadName(), pgtoolboxv1alpha1.PropagationTargetAll),
		// The API token is a projected volume of the console container
		// only, never a pod-wide automount.
		AutomountServiceAccountToken: new(false),
	}
	if c.AuthProvider() == pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift {
		// The ServiceAccount is the OAuth client; OpenShift redirects only
		// to the URIs listed here.
		if sa.Annotations == nil {
			sa.Annotations = map[string]string{}
		}
		sa.Annotations["serviceaccounts.openshift.io/oauth-redirecturi.console"] = c.EffectivePublicURL() + "/auth/openshift/callback"
	}
	return sa
}

func buildService(c *pgtoolboxv1alpha1.PgToolBoxConsole) *corev1.Service {
	meta := objectMeta(c, c.WorkloadName(), pgtoolboxv1alpha1.PropagationTargetService)
	if c.Spec.Exposure.TypeOrDefault() == pgtoolboxv1alpha1.ExposureTypeIngress {
		// Traefik's Ingress provider reads its cookie affinity from the
		// Service (ingress-nginx's is on the Ingress); each ignores the
		// other's annotations.
		if meta.Annotations == nil {
			meta.Annotations = map[string]string{}
		}
		maps.Copy(meta.Annotations, map[string]string{
			"traefik.ingress.kubernetes.io/service.sticky.cookie":          "true",
			"traefik.ingress.kubernetes.io/service.sticky.cookie.name":     stickyCookie,
			"traefik.ingress.kubernetes.io/service.sticky.cookie.httponly": "true",
			"traefik.ingress.kubernetes.io/service.sticky.cookie.samesite": "lax",
		})
		if c.Spec.Exposure.TLS != nil {
			meta.Annotations["traefik.ingress.kubernetes.io/service.sticky.cookie.secure"] = "true"
		}
	}
	svc := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: meta,
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(c),
			Ports: []corev1.ServicePort{{
				Name: "http", Port: servicePort, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	specs.SetHash(svc, specs.Hash(svc.Spec))
	return svc
}

// buildRoleBindings binds the console's ServiceAccount, in its namespace,
// to the shipped read ClusterRole, with pod logs to the pod-logs one and
// with restore drills to the drill-teardown one.
func buildRoleBindings(c *pgtoolboxv1alpha1.PgToolBoxConsole) []*rbacv1.RoleBinding {
	bind := func(name, role string) *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
			ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetAll),
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: c.WorkloadName(), Namespace: c.Namespace}},
		}
	}
	bindings := []*rbacv1.RoleBinding{bind(c.WorkloadName(), ConsoleClusterRole)}
	if c.PodLogsAllowed() {
		bindings = append(bindings, bind(c.WorkloadName()+"-pod-logs", ConsolePodLogsClusterRole))
	}
	if c.RestoreDrillsEnabled() {
		bindings = append(bindings, bind(restoreDrillsBindingName(c), ConsoleRestoreDrillsClusterRole))
	}
	return bindings
}

// restoreDrillsBindingName is the restore-drills RoleBinding, deleted when
// drills are turned off.
func restoreDrillsBindingName(c *pgtoolboxv1alpha1.PgToolBoxConsole) string {
	return c.WorkloadName() + "-restore-drills"
}

// podLogsBindingName is the pod-logs RoleBinding, deleted when podLogs is
// turned off.
func podLogsBindingName(c *pgtoolboxv1alpha1.PgToolBoxConsole) string {
	return c.WorkloadName() + "-pod-logs"
}

// buildRevocationsRBAC lets the console get and watch its revocation list,
// by name: the only ConfigMap it reads through the API.
func buildRevocationsRBAC(c *pgtoolboxv1alpha1.PgToolBoxConsole) (*rbacv1.Role, *rbacv1.RoleBinding) {
	name := c.WorkloadName() + "-revocations"
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetAll),
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"configmaps"},
			ResourceNames: []string{c.RevocationsConfigMapName()}, Verbs: []string{"get", "watch"},
		}},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetAll),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: c.WorkloadName(), Namespace: c.Namespace}},
	}
	return role, binding
}

// buildStateRBAC lets the console keep its shared state: get,
// watch and update on that one ConfigMap, by name. The operator holds
// these verbs on ConfigMaps, so it may grant them (RBAC escalation
// prevention).
func buildStateRBAC(c *pgtoolboxv1alpha1.PgToolBoxConsole) (*rbacv1.Role, *rbacv1.RoleBinding) {
	name := c.StateConfigMapName()
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetAll),
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"configmaps"},
			ResourceNames: []string{name}, Verbs: []string{"get", "watch", "update"},
		}},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetAll),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: c.WorkloadName(), Namespace: c.Namespace}},
	}
	return role, binding
}

// buildDigestLeaseRBAC lets the console take the digest Lease:
// get and update on that one Lease, by name, and create on leases, which
// RBAC cannot scope by name (a create is authorized before the object
// exists). The operator creates the Lease itself, owned by the PgToolBoxConsole so
// it goes with it; create lets the console make it again should it be
// deleted. The operator holds these verbs, so it may grant them.
func buildDigestLeaseRBAC(c *pgtoolboxv1alpha1.PgToolBoxConsole) (*rbacv1.Role, *rbacv1.RoleBinding) {
	name := c.DigestLeaseName()
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetAll),
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{coordinationv1.GroupName}, Resources: []string{"leases"}, ResourceNames: []string{name}, Verbs: []string{"get", "update"}},
			{APIGroups: []string{coordinationv1.GroupName}, Resources: []string{"leases"}, Verbs: []string{"create"}},
		},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetAll),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: c.WorkloadName(), Namespace: c.Namespace}},
	}
	return role, binding
}

// buildDigestLease is the digest Lease, created empty: the console's
// replicas compete for it and the operator never writes it again.
func buildDigestLease(c *pgtoolboxv1alpha1.PgToolBoxConsole) *coordinationv1.Lease {
	return &coordinationv1.Lease{
		TypeMeta:   metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"},
		ObjectMeta: objectMeta(c, c.DigestLeaseName(), pgtoolboxv1alpha1.PropagationTargetAll),
	}
}

// buildState is the shared state, created empty: the console fills it and
// the operator never writes it again.
func buildState(c *pgtoolboxv1alpha1.PgToolBoxConsole) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: objectMeta(c, c.StateConfigMapName(), pgtoolboxv1alpha1.PropagationTargetAll),
		Data:       map[string]string{},
	}
}

// buildSessionSecret is the cookie-sealing key, generated once and never
// rewritten: every open session survives a reconcile and a roll.
func buildSessionSecret(c *pgtoolboxv1alpha1.PgToolBoxConsole, key string) *corev1.Secret {
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: objectMeta(c, c.SessionSecretName(), pgtoolboxv1alpha1.PropagationTargetAll),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{sessionKey: []byte(key)},
	}
}

// buildRevocations is the revocation list, created empty: an admin edits
// its keys and the operator never writes it again.
func buildRevocations(c *pgtoolboxv1alpha1.PgToolBoxConsole) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: objectMeta(c, c.RevocationsConfigMapName(), pgtoolboxv1alpha1.PropagationTargetAll),
		Data:       map[string]string{},
	}
}

// buildLifecycle is the lifecycle ConfigMap holding the built-in table; its
// spec-hash annotation is the hash of that data, which is how the operator
// later tells its own table from an edited one.
func buildLifecycle(c *pgtoolboxv1alpha1.PgToolBoxConsole) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: objectMeta(c, c.LifecycleConfigMapName(), pgtoolboxv1alpha1.PropagationTargetAll),
		Data:       map[string]string{lifecycletable.Key: string(lifecycletable.JSON())},
	}
	specs.SetHash(cm, specs.Hash(cm.Data))
	return cm
}

// historyClaimTemplate is the StatefulSet's claim template: one history
// volume per replica, history-<workload>-<ordinal>. Its metadata is
// stable (no version): a template cannot change after creation.
func historyClaimTemplate(c *pgtoolboxv1alpha1.PgToolBoxConsole) corev1.PersistentVolumeClaim {
	size := resource.MustParse("1Gi")
	if c.Spec.History.Size != nil {
		size = *c.Spec.History.Size
	}
	meta := objectMeta(c, pgtoolboxv1alpha1.HistoryClaimTemplateName, pgtoolboxv1alpha1.PropagationTargetStorage)
	return corev1.PersistentVolumeClaim{
		// The StatefulSet controller owns the claims it makes from this.
		ObjectMeta: metav1.ObjectMeta{Name: meta.Name, Labels: meta.Labels, Annotations: meta.Annotations},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: c.Spec.History.StorageClassName,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
}

func buildPodDisruptionBudget(c *pgtoolboxv1alpha1.PgToolBoxConsole) *policyv1.PodDisruptionBudget {
	minAvailable := intstr.FromInt32(1)
	policy := policyv1.AlwaysAllow
	pdb := &policyv1.PodDisruptionBudget{
		TypeMeta:   metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"},
		ObjectMeta: objectMeta(c, c.WorkloadName(), pgtoolboxv1alpha1.PropagationTargetAll),
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			// A console that is not Ready must not block a node drain.
			UnhealthyPodEvictionPolicy: &policy,
			Selector:                   &metav1.LabelSelector{MatchLabels: selectorLabels(c)},
		},
	}
	specs.SetHash(pdb, specs.Hash(pdb.Spec))
	return pdb
}

// exposureTarget is the console's Service as an exposure object sees it.
func exposureTarget(c *pgtoolboxv1alpha1.PgToolBoxConsole) exposure.Target {
	meta := objectMeta(c, c.WorkloadName(), pgtoolboxv1alpha1.PropagationTargetExposure)
	labels := metadata.FilteredOverlay(c.Spec.Exposure.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, meta.Labels)
	meta.Labels = labels
	if extra := metadata.FilteredOverlay(c.Spec.Exposure.Annotations); len(extra) > 0 {
		if meta.Annotations == nil {
			meta.Annotations = map[string]string{}
		}
		maps.Copy(meta.Annotations, extra)
	}
	t := exposure.Target{
		Meta: meta, Service: c.WorkloadName(), PortName: "http", Port: servicePort, StreamTimeout: streamTimeout,
		StickyCookie: stickyCookie,
	}
	if c.MCPEnabled() {
		t.Extra = mcpPaths(c)
	}
	return t
}

// mcpPaths are the paths sent to the MCP leader's Service: the console's
// mcpleader.Paths, which TestMCPPathsMatchTheConsole holds this to (the
// operator does not import the console).
func mcpPaths(c *pgtoolboxv1alpha1.PgToolBoxConsole) []exposure.PathBackend {
	service := c.MCPLeaseName()
	return []exposure.PathBackend{
		{Name: "mcp", Path: "/mcp", Service: service},
		{Name: "oauth", Path: "/oauth", Service: service},
		{Name: "grants", Path: "/api/v1/session/grants", Service: service},
		{Name: "logout", Path: "/logout", Exact: true, Service: service},
	}
}

// buildMCPLeader is what sends the MCP paths to the one replica that
// serves them: the Service selecting the console Pod the operator labels
// as the holder of the Lease (mcpleader.go), and that Lease, created
// empty (the replicas write it, the operator never does).
func buildMCPLeader(c *pgtoolboxv1alpha1.PgToolBoxConsole) (*corev1.Service, *coordinationv1.Lease) {
	name := c.MCPLeaseName()
	selector := selectorLabels(c)
	selector[pgtoolboxv1alpha1.LabelMCPLeader] = pgtoolboxv1alpha1.MCPLeaderLabelValue
	svc := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetService),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports: []corev1.ServicePort{{
				Name: "http", Port: servicePort, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	specs.SetHash(svc, specs.Hash(svc.Spec))
	return svc, &coordinationv1.Lease{
		TypeMeta:   metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"},
		ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetAll),
	}
}

// buildMCPLeaderRBAC lets the console's replicas elect the MCP leader:
// get and update on that one Lease, by name; no create (the operator
// creates it), nothing on Pods (the operator labels them).
func buildMCPLeaderRBAC(c *pgtoolboxv1alpha1.PgToolBoxConsole) (*rbacv1.Role, *rbacv1.RoleBinding) {
	name := c.MCPLeaseName()
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetAll),
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{coordinationv1.GroupName}, Resources: []string{"leases"}, ResourceNames: []string{name}, Verbs: []string{"get", "update"}},
		},
	}
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: objectMeta(c, name, pgtoolboxv1alpha1.PropagationTargetAll),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: c.WorkloadName(), Namespace: c.Namespace}},
	}
	return role, binding
}

// deploymentInputs is what the Deployment depends on beyond the PgToolBoxConsole.
type deploymentInputs struct {
	Image specs.ResolvedImage
	// OpenShift: the platform assigns UIDs and fsGroups.
	OpenShift bool
	// Exposed: a Route, Ingress or HTTPRoute is published in front of
	// the console in this reconcile (not merely requested: the Route and
	// Gateway APIs may be unavailable).
	Exposed bool
}

// env is one environment variable, in the chart's order.
func env(name, value string) corev1.EnvVar { return corev1.EnvVar{Name: name, Value: value} }

// durationEnv writes a duration the way the chart's values do: 1h, not
// 1h0m0s (both parse; the two renderings stay comparable).
func durationEnv(d *metav1.Duration) string {
	text := d.Duration.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}

// buildWorkload is the console's workload: a StatefulSet with persistent
// history, else a Deployment.
func buildWorkload(c *pgtoolboxv1alpha1.PgToolBoxConsole, in deploymentInputs) client.Object {
	if c.HistoryPersistent() {
		return buildStatefulSet(c, in)
	}
	return buildDeployment(c, in)
}

// buildDeployment renders the console Deployment, for in-memory history:
// two replicas rolling, never fewer than the current ones available.
func buildDeployment(c *pgtoolboxv1alpha1.PgToolBoxConsole, in deploymentInputs) *appsv1.Deployment {
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: objectMeta(c, c.WorkloadName(), pgtoolboxv1alpha1.PropagationTargetWorkload),
		Spec: appsv1.DeploymentSpec{
			Replicas: new(c.EffectiveReplicas()),
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(c)},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxSurge: new(intstr.FromInt32(1)), MaxUnavailable: new(intstr.FromInt32(0)),
			}},
			Template: podTemplate(c, in),
		},
	}
	specs.SetHash(deployment, specs.Hash(deployment.Spec))
	return deployment
}

// buildStatefulSet renders the console StatefulSet, for persistent history:
// the same Pods as the Deployment's, each with its own history volume (the
// journal takes an exclusive lock on its file, so replicas never share
// one). Parallel: the replicas share nothing on disk, so there is no reason
// for an ordered start; a roll still replaces one Pod at a time. The claims
// go with the StatefulSet, as the single claim went with the PgToolBoxConsole, and
// stay when scaling down (scaling up again finds them).
func buildStatefulSet(c *pgtoolboxv1alpha1.PgToolBoxConsole, in deploymentInputs) *appsv1.StatefulSet {
	claims := []corev1.PersistentVolumeClaim{historyClaimTemplate(c)}
	set := &appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: objectMeta(c, c.WorkloadName(), pgtoolboxv1alpha1.PropagationTargetWorkload),
		Spec: appsv1.StatefulSetSpec{
			Replicas:            new(c.EffectiveReplicas()),
			Selector:            &metav1.LabelSelector{MatchLabels: selectorLabels(c)},
			ServiceName:         c.WorkloadName(),
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			Template:             podTemplate(c, in),
			VolumeClaimTemplates: claims,
		},
	}
	if set.Annotations == nil {
		set.Annotations = map[string]string{}
	}
	set.Annotations[claimTemplateHashAnnotation] = specs.Hash(claims)
	specs.SetHash(set, specs.Hash(set.Spec))
	return set
}

// podTemplate is the console Pod. Every optional setting adds its env
// vars, mounts and volumes in the chart's order, so the two install paths
// render the same container (chart_parity_test.go).
func podTemplate(c *pgtoolboxv1alpha1.PgToolBoxConsole, in deploymentInputs) corev1.PodTemplateSpec {
	persistent := c.HistoryPersistent()
	mounts, volumes := consoleVolumes(c)
	container := consoleContainer(c, in, consoleEnv(c, in), mounts)
	replicas := c.EffectiveReplicas()
	podMeta := objectMeta(c, "", pgtoolboxv1alpha1.PropagationTargetPod)
	podSpec := corev1.PodSpec{
		ServiceAccountName: c.WorkloadName(),
		// Only the console container gets an API token, as a projected
		// volume.
		AutomountServiceAccountToken: new(false),
		SecurityContext:              specs.PodSecurityContext(true, 0),
		ImagePullSecrets:             in.Image.PullSecrets,
		Containers:                   []corev1.Container{container},
		Volumes:                      volumes,
	}
	if persistent && !in.OpenShift {
		// The history volume must be writable by the image's user where the
		// platform assigns no fsGroup.
		podSpec.SecurityContext = specs.PodSecurityContext(false, 65532)
	}
	if replicas > 1 {
		// Replicas prefer different nodes; a single-node cluster still
		// schedules.
		podSpec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey:   "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{MatchLabels: selectorLabels(c)},
				},
			}},
		}}
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: podMeta.Labels, Annotations: podMeta.Annotations},
		Spec:       podSpec,
	}
}

// consoleContainer is the console container: its env and mounts, the two
// ports, the probes on the probe port, and the resources (the spec's, or
// the chart's defaults).
func consoleContainer(c *pgtoolboxv1alpha1.PgToolBoxConsole, in deploymentInputs, envs []corev1.EnvVar, mounts []corev1.VolumeMount) corev1.Container {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	if c.Spec.Resources != nil {
		resources = *c.Spec.Resources.DeepCopy()
	}
	probe := func(path string, period int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromString("probes")}},
			PeriodSeconds: period,
		}
	}
	return corev1.Container{
		Name:            "console",
		Image:           in.Image.Reference,
		ImagePullPolicy: in.Image.PullPolicy,
		Env:             envs,
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: httpPort, Protocol: corev1.ProtocolTCP},
			{Name: "probes", ContainerPort: probePort, Protocol: corev1.ProtocolTCP},
		},
		SecurityContext: specs.ContainerSecurityContext(),
		LivenessProbe:   probe("/healthz", 20),
		ReadinessProbe:  probe("/readyz", 10),
		Resources:       resources,
		VolumeMounts:    mounts,
	}
}

// consoleEnv is the console container's environment, in the chart's
// order. Capabilities that default on are written only when turned off;
// the files the console reads are named by path, never by content.
func consoleEnv(c *pgtoolboxv1alpha1.PgToolBoxConsole, in deploymentInputs) []corev1.EnvVar {
	envs := []corev1.EnvVar{
		{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		// Never empty and never "all namespaces": the console reads its own
		// namespace, the only one its RoleBinding covers.
		env("WATCH_NAMESPACES", c.Namespace),
	}
	if c.Spec.ClusterSelector != nil {
		if selector, err := metav1.LabelSelectorAsSelector(c.Spec.ClusterSelector); err == nil && !selector.Empty() {
			envs = append(envs, env("CLUSTER_SELECTOR", selector.String()))
		}
	}
	envs = append(envs,
		env("LISTEN_ADDR", ":"+strconv.Itoa(httpPort)),
		env("PROBE_LISTEN_ADDR", ":"+strconv.Itoa(probePort)),
		env("AUTH_PROVIDER", string(c.AuthProvider())),
		env("SESSION_KEY_FILE", runDir+"/session/"+sessionKey),
		env("SESSION_IDLE_TIMEOUT", durationEnv(&metav1.Duration{Duration: c.SessionIdleTimeout()})),
	)
	envs = append(envs, authEnv(c, in.Exposed)...)
	envs = append(envs,
		// The end-of-life table, mounted from the lifecycle ConfigMap and
		// reloaded on change; absent, the built-in table applies.
		env("LIFECYCLE_FILE", lifecycleDir+"/"+lifecycletable.Key),
		env("REVOCATION_CONFIGMAP", c.RevocationsConfigMapName()),
		env("STATE_CONFIGMAP", c.StateConfigMapName()),
		// The operator renders no insight pack: a team applies
		// deploy/insight-pack/insight-pack.yaml, whose name this is.
		env("INSIGHT_PACK_CONFIGMAP", insight.ConfigMapName),
		// Read from the filesystem and reloaded on change; the console reads
		// no Secret and no ConfigMap through the API for this.
		env("INSTANCE_STATUS_CA_FILE", instanceCADir+"/ca.crt"),
	)
	if !c.EvidenceEnabled() {
		envs = append(envs, env("EVIDENCE_ENABLED", "false"))
	} else {
		// One token under this directory (<namespace>/token), for the
		// audience pgtoolbox-evidence-agent/<namespace>.
		envs = append(envs, env("EVIDENCE_TOKEN_DIR", evidenceTokenDir))
	}
	if c.Spec.Evidence.Interval != nil {
		envs = append(envs, env("EVIDENCE_INTERVAL", durationEnv(c.Spec.Evidence.Interval)))
	}
	if !c.OperationsEnabled() {
		envs = append(envs, env("OPERATIONS_ENABLED", "false"))
	}
	envs = append(envs, restoreDrillEnv(c)...)
	envs = append(envs, mcpEnv(c)...)
	envs = append(envs, tracingEnv(c)...)
	if c.Spec.MonitoringURL != "" {
		envs = append(envs, env("MONITORING_URL", c.Spec.MonitoringURL))
	}
	envs = append(envs, prometheusEnv(c.Spec.Prometheus)...)
	if !c.PodLogsAllowed() {
		envs = append(envs, env("ALLOW_LOGS", "false"))
	}
	// The continuous log matcher needs `get pods/log`.
	envs = append(envs,
		env("LOG_STREAM_ENABLED", strconv.FormatBool(c.PodLogsAllowed())),
		env("LOG_MATCH_MAX_AGE", "6h"),
	)
	if c.HistoryPersistent() {
		envs = append(envs, env("HISTORY_ENABLED", "true"), env("HISTORY_PATH", historyDir+"/history.db"))
	}
	return append(envs, digestEnv(c)...)
}

// authEnv is the sign-in provider's settings (OIDC or OpenShift), then
// the session and token-login settings, the public URL and the proxy
// trust.
func authEnv(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool) []corev1.EnvVar {
	provider := c.AuthProvider()
	oidc := c.Spec.Auth.OIDC
	openshift := c.Spec.Auth.OpenShift
	var envs []corev1.EnvVar
	if provider == pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC && oidc != nil {
		scopes := oidc.Scopes
		if len(scopes) == 0 {
			scopes = []string{"openid", "email", "profile", "offline_access"}
		}
		envs = append(envs,
			env("OIDC_ISSUER_URL", oidc.IssuerURL),
			env("OIDC_CLIENT_ID", oidc.ClientID),
			env("OIDC_CLIENT_SECRET_FILE", runDir+"/oidc/"+oidc.ClientSecretRef.KeyOrDefault()),
		)
		if oidc.RedirectURL != "" {
			envs = append(envs, env("OIDC_REDIRECT_URL", oidc.RedirectURL))
		}
		envs = append(envs, env("OIDC_SCOPES", strings.Join(scopes, ",")))
		if oidc.RequireEmailVerified != nil && !*oidc.RequireEmailVerified {
			envs = append(envs, env("OIDC_REQUIRE_EMAIL_VERIFIED", "false"))
		}
		if oidc.CAConfigMapRef != nil {
			envs = append(envs, env("OIDC_CA_FILE", runDir+"/oidc-ca/ca.crt"))
		}
		if g := oidc.GrantClient; g != nil {
			envs = append(envs,
				env("OIDC_GRANT_CLIENT_ID", g.ClientID),
				env("OIDC_GRANT_CLIENT_SECRET_FILE", runDir+"/oidc-grant/"+g.ClientSecretRef.KeyOrDefault()),
			)
		}
	}
	if provider == pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift && openshift != nil {
		if openshift.ClientSecretRef != nil {
			envs = append(envs, env("OPENSHIFT_CLIENT_SECRET_FILE", runDir+"/openshift/"+openshift.ClientSecretRef.Key))
		}
		if openshift.CAConfigMapRef != nil {
			envs = append(envs, env("OPENSHIFT_CA_FILE", runDir+"/openshift-ca/ca.crt"))
		}
	}
	if c.Spec.Auth.SessionMaxAge != nil {
		envs = append(envs, env("SESSION_MAX_AGE", durationEnv(c.Spec.Auth.SessionMaxAge)))
	}
	if c.Spec.Auth.TokenLogin != nil && !*c.Spec.Auth.TokenLogin {
		envs = append(envs, env("AUTH_TOKEN_LOGIN", "false"))
	}
	if publicURL := c.EffectivePublicURL(); publicURL != "" {
		envs = append(envs, env("PUBLIC_URL", publicURL))
	}
	// Behind the Route, Ingress or Gateway the console sees the proxy's
	// address: it reads X-Forwarded-For, from the proxies only, so that the
	// sign-in budgets are charged per client, not shared by every user.
	// Without a published exposure nothing is in front, and a forwarded
	// header is the caller's own claim (the default, 0, ignores it).
	return append(envs, proxyEnv(effectiveProxyTrust(c, exposed))...)
}

// prometheusEnv is the Prometheus settings, nil without Prometheus. The
// token is a Secret key or the projected ServiceAccount token, by file.
func prometheusEnv(prom *pgtoolboxv1alpha1.PgToolBoxConsolePrometheusSpec) []corev1.EnvVar {
	if prom == nil {
		return nil
	}
	envs := []corev1.EnvVar{env("PROMETHEUS_URL", prom.URL)}
	switch {
	case prom.TokenSecretRef != nil:
		envs = append(envs, env("PROMETHEUS_TOKEN_FILE", runDir+"/prometheus/"+prom.TokenSecretRef.Key))
	case prom.ProjectedToken:
		envs = append(envs, env("PROMETHEUS_TOKEN_FILE", runDir+"/prometheus/"+tokenKey))
	}
	if prom.CAConfigMapRef != nil {
		envs = append(envs, env("PROMETHEUS_CA_FILE", runDir+"/prometheus-ca/ca.crt"))
	}
	if prom.Namespaced {
		envs = append(envs, env("PROMETHEUS_NAMESPACED", "true"))
	}
	if prom.Range != nil {
		envs = append(envs, env("PROMETHEUS_RANGE", durationEnv(prom.Range)))
	}
	return envs
}

// consoleVolumes is the console container's mounts and the Pod's
// volumes, in the chart's order: the API token and the session key
// always, then each provider's Secret and CA, the evidence token, the
// instance CA and lifecycle ConfigMaps, the digest files and the history
// claim.
func consoleVolumes(c *pgtoolboxv1alpha1.PgToolBoxConsole) ([]corev1.VolumeMount, []corev1.Volume) {
	provider := c.AuthProvider()
	oidc := c.Spec.Auth.OIDC
	openshift := c.Spec.Auth.OpenShift
	prom := c.Spec.Prometheus
	v := volumeSet{
		mounts: []corev1.VolumeMount{
			{Name: "tmp", MountPath: "/tmp"},
			{Name: "kube-api-access", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true},
			{Name: "session", MountPath: runDir + "/session", ReadOnly: true},
		},
		volumes: []corev1.Volume{
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "kube-api-access", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: apiAccessSources(provider == pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift && (openshift == nil || openshift.CAConfigMapRef == nil)),
			}}},
			secretVolume("session", c.SessionSecretName(), sessionKey),
		},
	}
	if provider == pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC && oidc != nil {
		v.add("oidc", runDir+"/oidc", secretVolume("oidc", oidc.ClientSecretRef.Name, oidc.ClientSecretRef.KeyOrDefault()))
		if oidc.CAConfigMapRef != nil {
			v.add("oidc-ca", runDir+"/oidc-ca", configMapVolume("oidc-ca", oidc.CAConfigMapRef.Name, "ca.crt", false))
		}
		if g := oidc.GrantClient; g != nil {
			v.add("oidc-grant", runDir+"/oidc-grant", secretVolume("oidc-grant", g.ClientSecretRef.Name, g.ClientSecretRef.KeyOrDefault()))
		}
	}
	if prom != nil {
		switch {
		case prom.TokenSecretRef != nil:
			v.add("prometheus-token", runDir+"/prometheus", secretVolume("prometheus-token", prom.TokenSecretRef.Name, prom.TokenSecretRef.Key))
		case prom.ProjectedToken:
			v.add("prometheus-token", runDir+"/prometheus", corev1.Volume{Name: "prometheus-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: prom.TokenAudience, ExpirationSeconds: new(int64(3600)), Path: tokenKey}}},
			}}})
		}
		if prom.CAConfigMapRef != nil {
			v.add("prometheus-ca", runDir+"/prometheus-ca", configMapVolume("prometheus-ca", prom.CAConfigMapRef.Name, "ca.crt", false))
		}
	}
	if provider == pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift && openshift != nil {
		if openshift.ClientSecretRef != nil {
			v.add("openshift", runDir+"/openshift", secretVolume("openshift", openshift.ClientSecretRef.Name, openshift.ClientSecretRef.Key))
		}
		if openshift.CAConfigMapRef != nil {
			v.add("openshift-ca", runDir+"/openshift-ca", configMapVolume("openshift-ca", openshift.CAConfigMapRef.Name, "ca.crt", false))
		}
	}
	if c.EvidenceEnabled() {
		// The bound token for the EvidenceAgent channel; the console re-reads
		// it when the kubelet rotates it.
		v.add("evidence-agent-token", evidenceTokenDir, corev1.Volume{Name: "evidence-agent-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: "pgtoolbox-evidence-agent/" + c.Namespace, ExpirationSeconds: new(int64(3600)), Path: c.Namespace + "/token",
			}}},
		}}})
	}
	// Directory mounts, not subPaths: only a directory mount is refreshed
	// by the kubelet when the ConfigMap changes.
	v.mounts = append(v.mounts,
		corev1.VolumeMount{Name: "instance-ca", MountPath: instanceCADir, ReadOnly: true},
		corev1.VolumeMount{Name: "lifecycle", MountPath: lifecycleDir, ReadOnly: true},
	)
	v.volumes = append(v.volumes,
		// Absent at start is not fatal: every Cluster reads "not verifiable"
		// until the bundle exists, and the kubelet fills the mount in then.
		configMapVolume("instance-ca", c.InstanceCABundleName(), "ca.crt", true),
		// A ConfigMap of your own may not exist yet: the built-in table
		// applies until it does.
		configMapVolume("lifecycle", c.LifecycleConfigMapName(), lifecycletable.Key, true),
	)
	digestMounts, digestVolumes := digestFiles(c.Spec.Digest)
	v.mounts, v.volumes = append(v.mounts, digestMounts...), append(v.volumes, digestVolumes...)
	if c.TracingEnabled() && c.Spec.Tracing.HeadersSecretRef != nil {
		ref := c.Spec.Tracing.HeadersSecretRef
		v.add("tracing-headers", tracingDir, secretVolume("tracing-headers", ref.Name, ref.Key))
	}
	if ref := drillSealKeyRef(c); ref != nil {
		// The drill seal's own keys, read once at start.
		v.add("restore-drill-seal", drillSealDir, secretVolume("restore-drill-seal", ref.Name, ref.Key))
	}
	if c.HistoryPersistent() {
		// The volume comes from the StatefulSet's claim template.
		v.mounts = append(v.mounts, corev1.VolumeMount{Name: pgtoolboxv1alpha1.HistoryClaimTemplateName, MountPath: historyDir})
	}
	return v.mounts, v.volumes
}

// volumeSet pairs the container's mounts with the Pod's volumes, so a
// read-only mount and its volume are added together.
type volumeSet struct {
	mounts  []corev1.VolumeMount
	volumes []corev1.Volume
}

// add mounts volume read-only at path under the name name.
func (v *volumeSet) add(name, path string, volume corev1.Volume) {
	v.mounts = append(v.mounts, corev1.VolumeMount{Name: name, MountPath: path, ReadOnly: true})
	v.volumes = append(v.volumes, volume)
}

// apiAccessSources is the console container's API token, the cluster CA,
// its namespace and, for OpenShift sign-in without a CA of your own, the
// service CA that signs the OAuth server's certificate.
func apiAccessSources(serviceCA bool) []corev1.VolumeProjection {
	sources := []corev1.VolumeProjection{
		{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: new(int64(3600))}},
		{ConfigMap: &corev1.ConfigMapProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
			Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
		}},
	}
	if serviceCA {
		sources = append(sources, corev1.VolumeProjection{ConfigMap: &corev1.ConfigMapProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: "openshift-service-ca.crt"},
			Optional:             new(true),
			Items:                []corev1.KeyToPath{{Key: "service-ca.crt", Path: "service-ca.crt"}},
		}})
	}
	return append(sources, corev1.VolumeProjection{DownwardAPI: &corev1.DownwardAPIProjection{
		Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
	}})
}

// digestEnv renders spec.digest as DIGEST_* (internal/console/config):
// switches and paths only; every credential is a mounted file.
func digestEnv(c *pgtoolboxv1alpha1.PgToolBoxConsole) []corev1.EnvVar {
	d := c.Spec.Digest
	var envs []corev1.EnvVar
	if d.Enabled != nil {
		envs = append(envs, env("DIGEST_ENABLED", strconv.FormatBool(*d.Enabled)))
	}
	if d.Hour != nil {
		envs = append(envs, env("DIGEST_HOUR", strconv.Itoa(int(*d.Hour))))
	}
	if d.Retention != nil {
		envs = append(envs, env("DIGEST_RETENTION", d.Retention.Duration.String()))
	}
	del := d.Delivery
	if del == nil {
		return envs
	}
	if del.ChatWebhookSecretRef != nil {
		envs = append(envs, env("DIGEST_CHAT_WEBHOOK_URL_FILE", digestDir+"/chat/"+del.ChatWebhookSecretRef.Key))
	}
	if del.WebhookSecretRef != nil {
		envs = append(envs, env("DIGEST_WEBHOOK_URL_FILE", digestDir+"/webhook/"+del.WebhookSecretRef.Key))
	}
	if del.Timeout != nil {
		envs = append(envs, env("DIGEST_DELIVERY_TIMEOUT", del.Timeout.Duration.String()))
	}
	// Every replica makes the digest; only the one holding this Lease
	// delivers it (the delivery marks are shared in STATE_CONFIGMAP).
	envs = append(envs, env("DIGEST_LEASE", c.DigestLeaseName()))
	if m := del.SMTP; m != nil {
		tlsMode := string(m.TLS)
		if tlsMode == "" {
			tlsMode = "starttls"
		}
		envs = append(envs,
			env("DIGEST_SMTP_ADDRESS", m.Address), env("DIGEST_SMTP_TLS", tlsMode),
			env("DIGEST_SMTP_FROM", m.From), env("DIGEST_SMTP_TO", strings.Join(m.To, ",")),
		)
		if m.Username != "" && m.PasswordSecretRef != nil {
			envs = append(envs, env("DIGEST_SMTP_USERNAME", m.Username), env("DIGEST_SMTP_PASSWORD_FILE", digestDir+"/smtp/"+m.PasswordSecretRef.Key))
		}
	}
	return envs
}

// mcpEnv renders spec.mcp: nothing while the endpoint is off, and each
// tuning knob only when set (the console's default applies otherwise),
// exactly as the pgtools chart renders console.mcp.
func mcpEnv(c *pgtoolboxv1alpha1.PgToolBoxConsole) []corev1.EnvVar {
	if !c.MCPEnabled() {
		return nil
	}
	m := c.Spec.MCP
	envs := []corev1.EnvVar{env("MCP_ENABLED", "true")}
	if c.MCPWriteTools() {
		envs = append(envs, env("MCP_WRITE_TOOLS", "true"))
	}
	if m.MaxResultBytes != nil {
		envs = append(envs, env("MCP_MAX_RESULT_BYTES", strconv.Itoa(int(*m.MaxResultBytes))))
	}
	if m.CallTimeout != nil {
		envs = append(envs, env("MCP_CALL_TIMEOUT", durationEnv(m.CallTimeout)))
	}
	if m.OAuthAccessTTL != nil {
		envs = append(envs, env("MCP_OAUTH_ACCESS_TTL", durationEnv(m.OAuthAccessTTL)))
	}
	if c.MCPClientMetadataDocuments() {
		envs = append(envs, env("MCP_OAUTH_CLIENT_METADATA_DOCUMENTS", "true"))
	}
	if len(m.OAuthClientMetadataHosts) > 0 {
		envs = append(envs, env("MCP_OAUTH_CLIENT_METADATA_HOSTS", strings.Join(m.OAuthClientMetadataHosts, ",")))
	}
	// One replica serves MCP: the holder of this Lease (buildMCPLeader).
	return append(envs, env("MCP_LEASE", c.MCPLeaseName()))
}

// tracingEnv renders spec.tracing as the OpenTelemetry SDK's variables
// and TRACING_*: the headers (an API key) are a mounted file, never a
// value in the Deployment.
func tracingEnv(c *pgtoolboxv1alpha1.PgToolBoxConsole) []corev1.EnvVar {
	if !c.TracingEnabled() {
		return nil
	}
	t := c.Spec.Tracing
	envs := []corev1.EnvVar{env("OTEL_EXPORTER_OTLP_ENDPOINT", t.Endpoint)}
	if t.ServiceName != "" {
		envs = append(envs, env("OTEL_SERVICE_NAME", t.ServiceName))
	}
	if t.Sampler != "" {
		envs = append(envs, env("OTEL_TRACES_SAMPLER", t.Sampler))
	}
	if t.SamplerArg != "" {
		envs = append(envs, env("OTEL_TRACES_SAMPLER_ARG", t.SamplerArg))
	}
	if t.HeadersSecretRef != nil {
		envs = append(envs, env("TRACING_HEADERS_FILE", tracingDir+"/"+t.HeadersSecretRef.Key))
	}
	if t.IncludePayloads {
		envs = append(envs, env("TRACING_INCLUDE_PAYLOADS", "true"))
	}
	return envs
}

// digestFiles mounts the delivery credentials, each its own directory so
// the kubelet refreshes it on rotation (the console re-reads per attempt).
func digestFiles(d pgtoolboxv1alpha1.PgToolBoxConsoleDigestSpec) ([]corev1.VolumeMount, []corev1.Volume) {
	if d.Delivery == nil {
		return nil, nil
	}
	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume
	add := func(name, dir string, ref *pgtoolboxv1alpha1.SecretKeyReference) {
		if ref == nil {
			return
		}
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: digestDir + "/" + dir, ReadOnly: true})
		volumes = append(volumes, secretVolume(name, ref.Name, ref.Key))
	}
	add("digest-chat", "chat", d.Delivery.ChatWebhookSecretRef)
	add("digest-webhook", "webhook", d.Delivery.WebhookSecretRef)
	if m := d.Delivery.SMTP; m != nil && m.Username != "" {
		add("digest-smtp", "smtp", m.PasswordSecretRef)
	}
	return mounts, volumes
}

func secretVolume(name, secret, key string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: secret, Items: []corev1.KeyToPath{{Key: key, Path: key}},
	}}}
}

func configMapVolume(name, configMap, key string, optional bool) corev1.Volume {
	source := &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: configMap},
		Items:                []corev1.KeyToPath{{Key: key, Path: key}},
	}
	if optional {
		source.Optional = new(true)
	}
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{ConfigMap: source}}
}

// consoleURL is where the console is reached: the public URL, else the
// exposure's, else its in-cluster Service.
func consoleURL(c *pgtoolboxv1alpha1.PgToolBoxConsole) string {
	if url := c.EffectivePublicURL(); url != "" {
		return strings.TrimSuffix(url, "/") + "/"
	}
	if url := exposure.URL(&c.Spec.Exposure.ExposureSpec); url != "" {
		return url
	}
	return fmt.Sprintf("http://%s.%s.svc/", c.WorkloadName(), c.Namespace)
}

// restoreDrillEnv is RESTORE_DRILL_*: the drills run in the console's own
// namespace (the only one it watches), so RESTORE_DRILL_NAMESPACE stays unset.
func restoreDrillEnv(c *pgtoolboxv1alpha1.PgToolBoxConsole) []corev1.EnvVar {
	if !c.RestoreDrillsEnabled() {
		return nil
	}
	d := c.Spec.RestoreDrills
	// The prefix, the time limit and the cap are always set, with the chart's
	// defaults, so that both install paths render the same container.
	prefix, timeLimit := "drill-", "2h"
	if d.Prefix != "" {
		prefix = d.Prefix
	}
	if d.TimeLimit != nil {
		timeLimit = durationEnv(d.TimeLimit)
	}
	maxConcurrent := "3"
	if d.MaxConcurrent != nil {
		maxConcurrent = strconv.Itoa(int(*d.MaxConcurrent))
	}
	envs := []corev1.EnvVar{
		env("RESTORE_DRILL_ENABLED", "true"),
		env("RESTORE_DRILL_PREFIX", prefix),
		env("RESTORE_DRILL_TIME_LIMIT", timeLimit),
		env("RESTORE_DRILL_MAX_CONCURRENT", maxConcurrent),
	}
	quantity := func(name string, q *resource.Quantity) {
		if q != nil {
			envs = append(envs, env(name, q.String()))
		}
	}
	if r := d.Resources; r != nil {
		if r.Requests != nil {
			quantity("RESTORE_DRILL_CPU_REQUEST", r.Requests.CPU)
			quantity("RESTORE_DRILL_MEMORY_REQUEST", r.Requests.Memory)
		}
		if r.Limits != nil {
			quantity("RESTORE_DRILL_CPU_LIMIT", r.Limits.CPU)
			quantity("RESTORE_DRILL_MEMORY_LIMIT", r.Limits.Memory)
		}
	}
	quantity("RESTORE_DRILL_STORAGE_SIZE", d.Storage.Size)
	if d.Storage.StorageClass != "" {
		envs = append(envs, env("RESTORE_DRILL_STORAGE_CLASS", d.Storage.StorageClass))
	}
	envs = append(envs, env("RESTORE_DRILL_SERVER_CA", drillServerCA(c)))
	if ref := drillSealKeyRef(c); ref != nil {
		envs = append(envs, env("RESTORE_DRILL_SEAL_KEY_FILE", drillSealDir+"/"+ref.Key))
	}
	return envs
}

// drillSealKeyRef is the Secret key of the drill seal's own keys, nil
// when drills are off or the keys derive from the session secret.
func drillSealKeyRef(c *pgtoolboxv1alpha1.PgToolBoxConsole) *pgtoolboxv1alpha1.SecretKeyReference {
	if !c.RestoreDrillsEnabled() {
		return nil
	}
	return c.Spec.RestoreDrills.SealKeySecretRef
}

// drillServerCA is the CA a drill's server certificate is signed with, so
// that the console can verify the drill's /pg/status. The operator's
// instance CA mirror follows Clusters created later, so a drill keeps its
// own CA ("own"); a bundle the user keeps (spec.instanceCABundleRef) is
// filled before the drill exists, so the drill is signed by the source's
// server CA, which that bundle holds ("source"; the drills run in the
// console's namespace, the source's). The console reads neither Secret.
func drillServerCA(c *pgtoolboxv1alpha1.PgToolBoxConsole) string {
	if c.InstanceCAMirrored() {
		return "own"
	}
	return "source"
}
