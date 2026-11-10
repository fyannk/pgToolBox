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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
	"github.com/fyannk/pgToolBox/pkg/lifecycletable"
)

// The operator identity the suite reconciles as: a user bound to exactly the
// generated manager ClusterRole (config/rbac/role.yaml). The operator binds
// the shipped console ClusterRoles without holding their rules, so a missing
// `bind` marker, or one the API server's escalation check refuses, fails here.
const operatorUser = "system:serviceaccount:pgtoolbox-system:pgtoolbox-operator"

const suiteNamespace = "team-a"

type envtestSuite struct {
	admin client.Client
	// operatorConfig is the operator's identity, for a suite that starts
	// a manager.
	operatorConfig *rest.Config
	reconciler     *Reconciler
}

func newEnvtestSuite(t *testing.T) *envtestSuite {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-console)")
	}
	// CloudNativePG's CRDs, vendored per minor: the instance CA mirror
	// lists Clusters.
	crds := os.Getenv("CNPG_CRDS")
	if crds == "" {
		crds = filepath.Join("..", "..", "..", "test", "crds", "cnpg-1.30")
	}
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases"), crds},
		ErrorIfCRDPathMissing: true,
	}
	// OpenShift enables OwnerReferencesPermissionEnforcement: an owner
	// reference with blockOwnerDeletion is admitted only from whoever may
	// update the owner's finalizers. kind does not enable it, so it is
	// enabled here, where the operator identity creates every child.
	environment.ControlPlane.GetAPIServer().Configure().Append("enable-admission-plugins", "OwnerReferencesPermissionEnforcement")
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = environment.Stop() })

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, pgtoolboxv1alpha1.AddToScheme, cnpgv1.AddToScheme} {
		must(t, add(scheme))
	}
	admin, err := client.New(config, client.Options{Scheme: scheme})
	must(t, err)
	ctx := context.Background()

	role := &rbacv1.ClusterRole{}
	readYAML(t, filepath.Join("..", "..", "..", "config", "rbac", "role.yaml"), role)
	must(t, admin.Create(ctx, role))
	must(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-operator"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: operatorUser}},
	}))
	must(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: suiteNamespace}}))

	impersonated := rest.CopyConfig(config)
	impersonated.Impersonate = rest.ImpersonationConfig{UserName: operatorUser, Groups: []string{"system:authenticated"}}
	operator, err := client.New(impersonated, client.Options{Scheme: scheme})
	must(t, err)
	return &envtestSuite{
		admin: admin, operatorConfig: impersonated,
		reconciler: &Reconciler{
			Client: operator, APIReader: operator, Scheme: scheme, Recorder: events.NewFakeRecorder(256),
			DefaultImage: "registry.example/pgtoolbox-console:1.2.0",
		},
	}
}

// completeRollout writes the status a StatefulSet controller would once
// every replica runs the current template (envtest runs no controller).
func completeRollout(ctx context.Context, t *testing.T, s *envtestSuite, name string) {
	t.Helper()
	set := &appsv1.StatefulSet{}
	s.get(ctx, t, name, set)
	replicas := ptrValue(set.Spec.Replicas, 1)
	set.Status = appsv1.StatefulSetStatus{
		ObservedGeneration: set.Generation, Replicas: replicas, ReadyReplicas: replicas, CurrentReplicas: replicas,
		UpdatedReplicas: replicas, AvailableReplicas: replicas, CurrentRevision: "r", UpdateRevision: "r",
	}
	must(t, s.admin.Status().Update(ctx, set))
}

// installShippedRoles creates the console ClusterRoles the way an
// operator installation does, from config/rbac.
func (s *envtestSuite) installShippedRoles(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, file := range []string{"pgtoolbox_console_clusterrole.yaml", "pgtoolbox_console_pod_logs_clusterrole.yaml", "pgtoolbox_console_restore_drills_clusterrole.yaml"} {
		role := &rbacv1.ClusterRole{}
		readYAML(t, filepath.Join("..", "..", "..", "config", "rbac", file), role)
		must(t, s.admin.Create(ctx, role))
	}
}

func readYAML(t *testing.T, path string, into client.Object) {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- repository manifest
	must(t, err)
	must(t, yaml.Unmarshal(raw, into))
	into.SetResourceVersion("")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (s *envtestSuite) reconcile(ctx context.Context, t *testing.T, name string) ctrl.Result {
	t.Helper()
	result, err := s.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: suiteNamespace, Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return result
}

func (s *envtestSuite) console(ctx context.Context, t *testing.T, name string) *pgtoolboxv1alpha1.PgToolBoxConsole {
	t.Helper()
	console := &pgtoolboxv1alpha1.PgToolBoxConsole{}
	must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: name}, console))
	return console
}

// asConsole is a client with the console's own ServiceAccount, as its Pods
// authenticate.
func (s *envtestSuite) asConsole(t *testing.T, console *pgtoolboxv1alpha1.PgToolBoxConsole) client.Client {
	t.Helper()
	config := rest.CopyConfig(s.operatorConfig)
	config.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + suiteNamespace + ":" + console.WorkloadName(),
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + suiteNamespace, "system:authenticated"},
	}
	c, err := client.New(config, client.Options{Scheme: s.admin.Scheme()})
	must(t, err)
	return c
}

func (s *envtestSuite) get(ctx context.Context, t *testing.T, name string, object client.Object) {
	t.Helper()
	must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: name}, object))
}

func (s *envtestSuite) absent(ctx context.Context, t *testing.T, name string, object client.Object) {
	t.Helper()
	err := s.admin.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: name}, object)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("%T %s: err = %v, want NotFound", object, name, err)
	}
}

func assertCondition(t *testing.T, console *pgtoolboxv1alpha1.PgToolBoxConsole, conditionType string, status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason) {
	t.Helper()
	for _, c := range console.Status.Conditions {
		if c.Type == conditionType {
			if c.Status != status || c.Reason != reason.String() {
				t.Fatalf("%s = %s/%s (%s), want %s/%s", conditionType, c.Status, c.Reason, c.Message, status, reason)
			}
			return
		}
	}
	t.Fatalf("no %s condition in %+v", conditionType, console.Status.Conditions)
}

// clusterScopedForConsoles lists the ClusterRoles and ClusterRoleBindings
// the operator labels as a console's: there must be none, because a
// console asks its access questions with the user's own token.
func clusterScopedForConsoles(ctx context.Context, t *testing.T, s *envtestSuite) []string {
	t.Helper()
	selector := client.MatchingLabels{
		pgtoolboxv1alpha1.LabelManagedBy: pgtoolboxv1alpha1.ManagedByValue,
		"app.kubernetes.io/component":    componentConsole,
	}
	var roles rbacv1.ClusterRoleList
	must(t, s.admin.List(ctx, &roles, selector))
	var bindings rbacv1.ClusterRoleBindingList
	must(t, s.admin.List(ctx, &bindings, selector))
	names := make([]string, 0, len(roles.Items)+len(bindings.Items))
	for i := range roles.Items {
		names = append(names, "ClusterRole/"+roles.Items[i].Name)
	}
	for i := range bindings.Items {
		names = append(names, "ClusterRoleBinding/"+bindings.Items[i].Name)
	}
	return names
}

func TestPgToolBoxConsoleReconcilerOnEnvtest(t *testing.T) {
	s := newEnvtestSuite(t)
	ctx := context.Background()
	must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: suiteNamespace}}))

	t.Run("an operator installed without the console roles stalls and looks again", func(t *testing.T) {
		result := s.reconcile(ctx, t, "console")
		console := s.console(ctx, t, "console")
		assertCondition(t, console, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonConsoleRoleMissing)
		assertCondition(t, console, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonConsoleRoleMissing)
		if result.RequeueAfter != roleMissingRetry {
			t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, roleMissingRetry)
		}
		s.absent(ctx, t, "console", &appsv1.Deployment{})
	})

	s.installShippedRoles(ctx, t)
	var sessionKey []byte
	t.Run("first reconcile renders the console as the operator", func(t *testing.T) {
		s.reconcile(ctx, t, "console")
		console := s.console(ctx, t, "console")
		assertCondition(t, console, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		assertCondition(t, console, pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress)
		if console.Status.Image != "registry.example/pgtoolbox-console:1.2.0" || console.Status.URL != "http://console.team-a.svc/" {
			t.Fatalf("status = %+v", console.Status)
		}
		if l := console.Status.Lifecycle; l == nil || l.KeptBy != pgtoolboxv1alpha1.LifecycleKeptByOperator || l.Reviewed != lifecycletable.Reviewed() {
			t.Fatalf("status.lifecycle = %+v", console.Status.Lifecycle)
		}

		// Persistent history by default: a StatefulSet of two,
		// one history claim each, and no Deployment.
		if !console.HistoryPersistent() || console.Spec.History.Persistent == nil {
			t.Fatalf("history.persistent = %v, want defaulted to true", console.Spec.History.Persistent)
		}
		set := &appsv1.StatefulSet{}
		s.get(ctx, t, "console", set)
		if *set.Spec.Replicas != 2 || set.Spec.Template.Spec.ServiceAccountName != "console" || set.Spec.PodManagementPolicy != appsv1.ParallelPodManagement ||
			len(set.Spec.VolumeClaimTemplates) != 1 || set.Spec.VolumeClaimTemplates[0].Name != pgtoolboxv1alpha1.HistoryClaimTemplateName {
			t.Fatalf("StatefulSet replicas %d, account %s, policy %s, claims %v", *set.Spec.Replicas, set.Spec.Template.Spec.ServiceAccountName,
				set.Spec.PodManagementPolicy, set.Spec.VolumeClaimTemplates)
		}
		s.absent(ctx, t, "console", &appsv1.Deployment{})
		s.get(ctx, t, "console", &corev1.ServiceAccount{})
		s.get(ctx, t, "console", &corev1.Service{})
		s.get(ctx, t, "console", &policyv1.PodDisruptionBudget{})
		binding := &rbacv1.RoleBinding{}
		s.get(ctx, t, "console", binding)
		if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != ConsoleClusterRole {
			t.Fatalf("roleRef = %+v", binding.RoleRef)
		}
		s.get(ctx, t, podLogsBindingName(console), &rbacv1.RoleBinding{})
		secret := &corev1.Secret{}
		s.get(ctx, t, console.SessionSecretName(), secret)
		for _, value := range secret.Data {
			sessionKey = value
		}
		if len(sessionKey) != 48 {
			t.Fatalf("session key is %d bytes, want 48", len(sessionKey))
		}
		lifecycle := &corev1.ConfigMap{}
		s.get(ctx, t, console.LifecycleConfigMapName(), lifecycle)
		if lifecycle.Data[lifecycletable.Key] != string(lifecycletable.JSON()) {
			t.Fatal("the lifecycle ConfigMap does not carry the operator's table")
		}
		if got := clusterScopedForConsoles(ctx, t, s); len(got) != 0 {
			t.Fatalf("the operator wrote cluster-scoped objects for a console: %v", got)
		}
	})

	t.Run("the console may write its state ConfigMap, that one only, and a reconcile keeps what it wrote", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		state := &corev1.ConfigMap{}
		s.get(ctx, t, console.StateConfigMapName(), state)
		if len(state.Data) != 0 || !metav1.IsControlledBy(state, console) {
			t.Fatalf("state ConfigMap data %v, owners %v: want empty, controlled by the PgToolBoxConsole", state.Data, state.OwnerReferences)
		}
		consoleClient := s.asConsole(t, console)
		state.Data = map[string]string{"sessions.json": `{"version":1,"signedOut":{}}`}
		must(t, consoleClient.Update(ctx, state))
		revocations := &corev1.ConfigMap{}
		s.get(ctx, t, console.RevocationsConfigMapName(), revocations)
		revocations.Data = map[string]string{"x": "y"}
		if err := consoleClient.Update(ctx, revocations); !apierrors.IsForbidden(err) {
			t.Fatalf("the console updated its revocation list: %v", err)
		}
		s.reconcile(ctx, t, "console")
		s.get(ctx, t, console.StateConfigMapName(), state)
		if state.Data["sessions.json"] == "" {
			t.Fatal("a reconcile erased what the console wrote to its state")
		}
	})

	t.Run("a second reconcile writes nothing and keeps the session key", func(t *testing.T) {
		before := &appsv1.StatefulSet{}
		s.get(ctx, t, "console", before)
		s.reconcile(ctx, t, "console")
		after := &appsv1.StatefulSet{}
		s.get(ctx, t, "console", after)
		if after.ResourceVersion != before.ResourceVersion {
			t.Fatal("an unchanged PgToolBoxConsole rewrote its StatefulSet")
		}
		secret := &corev1.Secret{}
		s.get(ctx, t, s.console(ctx, t, "console").SessionSecretName(), secret)
		for _, value := range secret.Data {
			if string(value) != string(sessionKey) {
				t.Fatal("the session key changed")
			}
		}
	})

	t.Run("the operator brings its newer table to a ConfigMap nobody edited", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		lifecycle := &corev1.ConfigMap{}
		s.get(ctx, t, console.LifecycleConfigMapName(), lifecycle)
		// What an older operator wrote: other data, its hash recorded.
		lifecycle.Data = map[string]string{lifecycletable.Key: `{"reviewed":"2020-01-01"}`}
		specs.SetHash(lifecycle, specs.Hash(lifecycle.Data))
		must(t, s.admin.Update(ctx, lifecycle))
		s.reconcile(ctx, t, "console")
		s.get(ctx, t, console.LifecycleConfigMapName(), lifecycle)
		if lifecycle.Data[lifecycletable.Key] != string(lifecycletable.JSON()) {
			t.Fatal("the operator did not bring its table")
		}
		if s.console(ctx, t, "console").Status.Lifecycle.KeptBy != pgtoolboxv1alpha1.LifecycleKeptByOperator {
			t.Fatal("keptBy is not Operator")
		}
	})

	t.Run("an edited ConfigMap is a person's and is never written again", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		lifecycle := &corev1.ConfigMap{}
		s.get(ctx, t, console.LifecycleConfigMapName(), lifecycle)
		edited := `{"reviewed":"2030-01-01","products":[]}`
		lifecycle.Data[lifecycletable.Key] = edited
		must(t, s.admin.Update(ctx, lifecycle))
		s.reconcile(ctx, t, "console")
		s.get(ctx, t, console.LifecycleConfigMapName(), lifecycle)
		if lifecycle.Data[lifecycletable.Key] != edited {
			t.Fatal("the operator overwrote an edited lifecycle table")
		}
		if l := s.console(ctx, t, "console").Status.Lifecycle; l.KeptBy != pgtoolboxv1alpha1.LifecycleKeptByUser || l.Reviewed != "" {
			t.Fatalf("status.lifecycle = %+v, want User", l)
		}
	})

	t.Run("the user-managed annotation keeps the operator's hands off an unedited table", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		lifecycle := &corev1.ConfigMap{}
		s.get(ctx, t, console.LifecycleConfigMapName(), lifecycle)
		lifecycle.Data = map[string]string{lifecycletable.Key: `{"reviewed":"2020-01-01"}`}
		specs.SetHash(lifecycle, specs.Hash(lifecycle.Data))
		lifecycle.Annotations[pgtoolboxv1alpha1.AnnotationUserManaged] = pgtoolboxv1alpha1.UserManagedValue
		must(t, s.admin.Update(ctx, lifecycle))
		s.reconcile(ctx, t, "console")
		s.get(ctx, t, console.LifecycleConfigMapName(), lifecycle)
		if lifecycle.Data[lifecycletable.Key] != `{"reviewed":"2020-01-01"}` {
			t.Fatal("the operator wrote a ConfigMap annotated user-managed")
		}
	})

	t.Run("restore drills bind the teardown role and fence the drill Pods; turning them off removes both", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		console.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true}
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		binding := &rbacv1.RoleBinding{}
		s.get(ctx, t, restoreDrillsBindingName(console), binding)
		if binding.RoleRef.Name != ConsoleRestoreDrillsClusterRole || len(binding.Subjects) != 1 ||
			binding.Subjects[0].Name != console.WorkloadName() || binding.Subjects[0].Namespace != console.Namespace {
			t.Fatalf("restore-drills binding = %+v", binding)
		}
		policy := &networkingv1.NetworkPolicy{}
		s.get(ctx, t, restoreDrillsNetworkPolicyName(console), policy)
		if policy.Spec.PodSelector.MatchLabels[drillOwnerLabel] != console.Namespace || len(policy.Spec.Egress) != 3 {
			t.Fatalf("restore-drills NetworkPolicy = %+v", policy.Spec)
		}
		console = s.console(ctx, t, "console")
		console.Spec.RestoreDrills.NetworkPolicy.Enabled = new(false)
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		s.absent(ctx, t, restoreDrillsNetworkPolicyName(console), &networkingv1.NetworkPolicy{})
		console = s.console(ctx, t, "console")
		console.Spec.RestoreDrills.NetworkPolicy.Enabled = nil
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		s.get(ctx, t, restoreDrillsNetworkPolicyName(console), policy)
		console = s.console(ctx, t, "console")
		console.Spec.RestoreDrills.Enabled = false
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		s.absent(ctx, t, restoreDrillsBindingName(console), &rbacv1.RoleBinding{})
		s.absent(ctx, t, restoreDrillsNetworkPolicyName(console), &networkingv1.NetworkPolicy{})
	})

	t.Run("pod logs off removes the binding; one replica removes the PDB", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		console.Spec.PodLogs = new(false)
		console.Spec.Replicas = new(int32(1))
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		s.absent(ctx, t, podLogsBindingName(console), &rbacv1.RoleBinding{})
		s.absent(ctx, t, "console", &policyv1.PodDisruptionBudget{})
	})

	t.Run("digest delivery grants the console its Lease by name, which the operator creates", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		console.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{
			WebhookSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "digest-webhook", Key: "url"},
		}
		must(t, s.admin.Update(ctx, console))

		// The webhook Secret does not exist yet: a stall naming the field,
		// and nothing granted.
		s.reconcile(ctx, t, "console")
		stalled := s.console(ctx, t, "console")
		assertCondition(t, stalled, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonSecretNotFound)
		assertCondition(t, stalled, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonSecretNotFound)
		if message := meta.FindStatusCondition(stalled.Status.Conditions, pgtoolboxv1alpha1.ConditionConfigurationValid).Message; !strings.Contains(message, "spec.digest.delivery.webhookSecretRef: Secret digest-webhook") {
			t.Fatalf("ConfigurationValid message = %q, want the field and the Secret named", message)
		}
		s.absent(ctx, t, console.DigestLeaseName(), &rbacv1.Role{})
		s.absent(ctx, t, console.DigestLeaseName(), &coordinationv1.Lease{})

		must(t, s.admin.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "digest-webhook", Namespace: suiteNamespace},
			Data:       map[string][]byte{"url": []byte("test-only")},
		}))
		s.reconcile(ctx, t, "console")
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		lease := &coordinationv1.Lease{}
		s.get(ctx, t, console.DigestLeaseName(), lease)
		if !metav1.IsControlledBy(lease, console) || lease.Spec.HolderIdentity != nil {
			t.Fatalf("Lease owners %v, holder %v: want empty, controlled by the PgToolBoxConsole", lease.OwnerReferences, lease.Spec.HolderIdentity)
		}
		set := &appsv1.StatefulSet{}
		s.get(ctx, t, "console", set)
		found := false
		for _, e := range set.Spec.Template.Spec.Containers[0].Env {
			found = found || (e.Name == "DIGEST_LEASE" && e.Value == console.DigestLeaseName())
		}
		if !found {
			t.Fatal("the console is not told DIGEST_LEASE")
		}

		// What a replica does: take the Lease. It writes that one Lease and
		// no other (it reads every Lease of the namespace anyway: the
		// console Role's read rules).
		consoleClient := s.asConsole(t, console)
		holder := "console-0_abc"
		lease.Spec.HolderIdentity = &holder
		must(t, consoleClient.Update(ctx, lease))
		other := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses", Namespace: suiteNamespace}}
		must(t, s.admin.Create(ctx, other))
		other.Spec.HolderIdentity = &holder
		if err := consoleClient.Update(ctx, other); !apierrors.IsForbidden(err) {
			t.Fatalf("the console wrote another Lease: %v", err)
		}
		// A deleted Lease: the console may make it again (create cannot be
		// scoped by name).
		must(t, s.admin.Delete(ctx, lease))
		must(t, consoleClient.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: console.DigestLeaseName(), Namespace: suiteNamespace}}))
		// The operator leaves the replicas' Lease alone.
		s.reconcile(ctx, t, "console")
		s.get(ctx, t, console.DigestLeaseName(), lease)
		if metav1.GetControllerOf(lease) != nil {
			t.Fatal("the operator rewrote the replicas' Lease")
		}

		console = s.console(ctx, t, "console")
		console.Spec.Digest.Delivery = nil
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		s.absent(ctx, t, console.DigestLeaseName(), &rbacv1.Role{})
		s.absent(ctx, t, console.DigestLeaseName(), &rbacv1.RoleBinding{})
	})

	t.Run("switching to in-memory history keeps the StatefulSet until the Deployment has a replica, and back", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		console.Spec.History.Persistent = new(false)
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		deployment := &appsv1.Deployment{}
		s.get(ctx, t, "console", deployment)
		// Nothing is available yet: the StatefulSet keeps serving.
		s.get(ctx, t, "console", &appsv1.StatefulSet{})
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress)

		// What the Deployment controller reports once a Pod is ready.
		deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: deployment.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
		must(t, s.admin.Status().Update(ctx, deployment))
		s.reconcile(ctx, t, "console")
		s.absent(ctx, t, "console", &appsv1.StatefulSet{})
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)

		// Back to persistent history: the operator's way from a console
		// that has a Deployment (an operator up to 1.2 made one, with one
		// claim, for persistent history).
		console = s.console(ctx, t, "console")
		console.Spec.History.Persistent = new(true)
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		set := &appsv1.StatefulSet{}
		s.get(ctx, t, "console", set)
		s.get(ctx, t, "console", &appsv1.Deployment{})
		set.Status = appsv1.StatefulSetStatus{ObservedGeneration: set.Generation, Replicas: 1, UpdatedReplicas: 1, CurrentReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
		must(t, s.admin.Status().Update(ctx, set))
		s.reconcile(ctx, t, "console")
		s.absent(ctx, t, "console", &appsv1.Deployment{})
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	})

	t.Run("a new history size replaces the StatefulSet without its Pods", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		console.Spec.History.Size = new(resource.MustParse("2Gi"))
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		set := &appsv1.StatefulSet{}
		s.get(ctx, t, "console", set)
		// envtest runs no garbage collector: the orphaning deletion waits on
		// its finalizer, and the console waits with it.
		if set.DeletionTimestamp.IsZero() || len(set.Finalizers) != 1 || set.Finalizers[0] != metav1.FinalizerOrphanDependents {
			t.Fatalf("StatefulSet deletion %v, finalizers %v: want an orphaning deletion", set.DeletionTimestamp, set.Finalizers)
		}
		s.reconcile(ctx, t, "console")
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress)
		// What the garbage collector does once the Pods are orphaned.
		set.Finalizers = nil
		must(t, s.admin.Update(ctx, set))
		s.reconcile(ctx, t, "console")
		s.get(ctx, t, "console", set)
		if !set.DeletionTimestamp.IsZero() || set.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String() != "2Gi" {
			t.Fatalf("StatefulSet not recreated with the new size: %v", set.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests)
		}
	})

	t.Run("an image outside the allow-list is refused", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		console.Spec.Image = &pgtoolboxv1alpha1.ImageSpec{Repository: "evil.example/pgtoolbox-console", Tag: "1"}
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonImageNotAllowed)
		console = s.console(ctx, t, "console")
		console.Spec.Image = nil
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	})

	t.Run("a hostname outside the allowed suffixes is refused and its Ingress withdrawn", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		console.Spec.Exposure.ExposureSpec = pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: "login.corp.example.com"}
		must(t, s.admin.Update(ctx, console))
		// Published before the operator had an allow-list.
		s.reconcile(ctx, t, "console")
		must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: console.WorkloadName()}, &networkingv1.Ingress{}))

		s.reconciler.AllowedHostSuffixes = []string{"apps.example.com"}
		defer func() { s.reconciler.AllowedHostSuffixes = nil }()
		s.reconcile(ctx, t, "console")
		refused := s.console(ctx, t, "console")
		assertCondition(t, refused, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
		// Nothing is published any more: ExposureConfined goes with it.
		if got := meta.FindStatusCondition(refused.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureConfined); got != nil {
			t.Fatalf("ExposureConfined = %+v on a withdrawn exposure, want none", got)
		}
		assertCondition(t, refused, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
		s.absent(ctx, t, "console", &networkingv1.Ingress{})

		refused.Spec.Exposure.Hostname = "console.team-a.apps.example.com"
		must(t, s.admin.Update(ctx, refused))
		s.reconcile(ctx, t, "console")
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: console.WorkloadName()}, &networkingv1.Ingress{}))

		back := s.console(ctx, t, "console")
		back.Spec.Exposure = pgtoolboxv1alpha1.PgToolBoxConsoleExposureSpec{}
		must(t, s.admin.Update(ctx, back))
		s.reconcile(ctx, t, "console")
	})

	t.Run("an Ingress behind its NetworkPolicy trusts only the peers the policy admits", func(t *testing.T) {
		proxyEnv := func() map[string]string {
			set := &appsv1.StatefulSet{}
			s.get(ctx, t, "console", set)
			got := map[string]string{}
			for _, e := range set.Spec.Template.Spec.Containers[0].Env {
				if strings.HasPrefix(e.Name, "TRUSTED_PROXY_") {
					got[e.Name] = e.Value
				}
			}
			return got
		}
		console := s.console(ctx, t, "console")
		console.Spec.Exposure.ExposureSpec = pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: "console.example"}
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
		// No peer named: no policy, and the console ignores the header.
		open := s.console(ctx, t, "console")
		assertCondition(t, open, pgtoolboxv1alpha1.ConditionExposureConfined, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonExposureOpen)
		s.absent(ctx, t, console.WorkloadName()+"-ingress", &networkingv1.NetworkPolicy{})
		if got := proxyEnv(); got["TRUSTED_PROXY_HOPS"] != "1" || got["TRUSTED_PROXY_CIDRS"] != "" {
			t.Fatalf("without a policy: %v, want one hop and no trusted peer", got)
		}

		open.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{From: []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{{
			NamespaceSelector: &pgtoolboxv1alpha1.PgToolBoxConsoleLabelSelector{MatchLabels: map[string]pgtoolboxv1alpha1.PgToolBoxConsoleLabelValue{"kubernetes.io/metadata.name": "ingress-nginx"}},
		}}}
		must(t, s.admin.Update(ctx, open))
		s.reconcile(ctx, t, "console")
		confined := s.console(ctx, t, "console")
		assertCondition(t, confined, pgtoolboxv1alpha1.ConditionExposureConfined, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		policy := &networkingv1.NetworkPolicy{}
		s.get(ctx, t, console.WorkloadName()+"-ingress", policy)
		if !metav1.IsControlledBy(policy, confined) || len(policy.Spec.Ingress) != 2 || len(policy.Spec.Ingress[0].From) != 1 {
			t.Fatalf("NetworkPolicy owners %v, ingress %v", policy.OwnerReferences, policy.Spec.Ingress)
		}
		if got := proxyEnv(); got["TRUSTED_PROXY_CIDRS"] != "0.0.0.0/0,::/0" {
			t.Fatalf("behind the policy: %v, want every admitted peer trusted", got)
		}
		// Byte-stable: a second reconcile rewrites neither. Another
		// NetworkPolicy may appear unseen (only the operator's own are
		// cached): the console looks again.
		before := policy.ResourceVersion
		if result := s.reconcile(ctx, t, "console"); result.RequeueAfter != policyRecheck {
			t.Fatalf("RequeueAfter = %s while every peer is trusted, want %s", result.RequeueAfter, policyRecheck)
		}
		s.get(ctx, t, console.WorkloadName()+"-ingress", policy)
		if policy.ResourceVersion != before {
			t.Fatal("an unchanged PgToolBoxConsole rewrote its NetworkPolicy")
		}

		// Another NetworkPolicy that admits the namespace to every port
		// reopens the console port (NetworkPolicies add up): no peer is
		// trusted any more.
		sameNamespace := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "allow-same-namespace", Namespace: suiteNamespace},
			Spec: networkingv1.NetworkPolicySpec{
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}}},
			},
		}
		must(t, s.admin.Create(ctx, sameNamespace))
		s.reconcile(ctx, t, "console")
		// Its removal is seen by the same look again (no watch reports it).
		if result := s.reconcile(ctx, t, "console"); result.RequeueAfter != policyRecheck {
			t.Fatalf("RequeueAfter = %s while another policy bypasses the exposure's, want %s", result.RequeueAfter, policyRecheck)
		}
		bypassed := s.console(ctx, t, "console")
		assertCondition(t, bypassed, pgtoolboxv1alpha1.ConditionExposureConfined, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonExposurePolicyBypassed)
		if got := proxyEnv(); got["TRUSTED_PROXY_CIDRS"] != "" {
			t.Fatalf("with allow-same-namespace: %v, want no trusted peer", got)
		}
		must(t, s.admin.Delete(ctx, sameNamespace))
		s.reconcile(ctx, t, "console")
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionExposureConfined, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		if got := proxyEnv(); got["TRUSTED_PROXY_CIDRS"] != "0.0.0.0/0,::/0" {
			t.Fatalf("allow-same-namespace removed: %v, want every admitted peer trusted again", got)
		}

		// A selector the CRD lets through but the API server refuses in
		// the NetworkPolicy (a label-key prefix longer than a DNS subdomain):
		// a named stall, looked at again in five minutes, not a hot loop.
		refused := s.console(ctx, t, "console")
		refused.Spec.Exposure.NetworkPolicy.From = append(refused.Spec.Exposure.NetworkPolicy.From, pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{
			PodSelector: &pgtoolboxv1alpha1.PgToolBoxConsoleLabelSelector{MatchLabels: map[string]pgtoolboxv1alpha1.PgToolBoxConsoleLabelValue{strings.Repeat("a", 254) + "/x": "y"}},
		})
		must(t, s.admin.Update(ctx, refused))
		if result := s.reconcile(ctx, t, "console"); result.RequeueAfter != policyRecheck {
			t.Fatalf("RequeueAfter = %s on a refused NetworkPolicy, want %s", result.RequeueAfter, policyRecheck)
		}
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonExposurePolicyInvalid)
		fixed := s.console(ctx, t, "console")
		fixed.Spec.Exposure.NetworkPolicy.From = fixed.Spec.Exposure.NetworkPolicy.From[:1]
		must(t, s.admin.Update(ctx, fixed))
		s.reconcile(ctx, t, "console")
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)

		// A Route without the Route API (this API server serves none) is
		// not published: nothing is in front, so nothing is trusted and no
		// policy is rendered, whatever the spec asks for.
		route := s.console(ctx, t, "console")
		route.Spec.Exposure = pgtoolboxv1alpha1.PgToolBoxConsoleExposureSpec{
			ExposureSpec: pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeRoute, Hostname: "console.apps.example"},
		}
		must(t, s.admin.Update(ctx, route))
		s.reconcile(ctx, t, "console")
		unpublished := s.console(ctx, t, "console")
		assertCondition(t, unpublished, pgtoolboxv1alpha1.ConditionExposureReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRouteAPIUnavailable)
		if meta.FindStatusCondition(unpublished.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureConfined) != nil {
			t.Fatal("ExposureConfined on a Route that is not published")
		}
		if got := proxyEnv(); len(got) != 0 {
			t.Fatalf("a Route that is not published: %v, want no trusted proxy", got)
		}
		// The rollout has not happened (envtest runs no controller): the
		// old Pods still trust every peer, so the policy stays. The
		// workload's status and its Pods are watched: no polling.
		if result := s.reconcile(ctx, t, "console"); result.RequeueAfter != 0 {
			t.Fatalf("RequeueAfter = %s during the rollout, want none (watched)", result.RequeueAfter)
		}
		s.get(ctx, t, console.WorkloadName()+"-ingress", &networkingv1.NetworkPolicy{})
		// Rolled out, but a Pod that still trusts every peer (terminating,
		// say) keeps the policy.
		completeRollout(ctx, t, s, "console")
		oldPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "console-old", Namespace: suiteNamespace, Labels: selectorLabels(console),
				Annotations: map[string]string{annotationTrustsEveryPeer: "true"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "console", Image: "registry.example/pgtoolbox-console:1.2.0",
				Env: []corev1.EnvVar{{Name: "TRUSTED_PROXY_CIDRS", Value: "0.0.0.0/0,::/0"}},
			}}},
		}
		must(t, s.admin.Create(ctx, oldPod))
		s.reconcile(ctx, t, "console")
		s.get(ctx, t, console.WorkloadName()+"-ingress", &networkingv1.NetworkPolicy{})
		must(t, s.admin.Delete(ctx, oldPod, client.GracePeriodSeconds(0)))
		// A Pod whose any-peer trust is the user's own trustedProxy.cidrs
		// (the same TRUSTED_PROXY_CIDRS, no grant) does not hold it back.
		userPod := oldPod.DeepCopy()
		userPod.Name, userPod.Annotations, userPod.ResourceVersion, userPod.UID = "console-user", nil, "", ""
		must(t, s.admin.Create(ctx, userPod))
		s.reconcile(ctx, t, "console")
		s.absent(ctx, t, console.WorkloadName()+"-ingress", &networkingv1.NetworkPolicy{})
		must(t, s.admin.Delete(ctx, userPod, client.GracePeriodSeconds(0)))

		back := s.console(ctx, t, "console")
		back.Spec.Exposure = pgtoolboxv1alpha1.PgToolBoxConsoleExposureSpec{}
		must(t, s.admin.Update(ctx, back))
		s.reconcile(ctx, t, "console")
		s.absent(ctx, t, console.WorkloadName()+"-ingress", &networkingv1.NetworkPolicy{})
		if meta.FindStatusCondition(s.console(ctx, t, "console").Status.Conditions, pgtoolboxv1alpha1.ConditionExposureConfined) != nil {
			t.Fatal("ExposureConfined stays on an unexposed console")
		}
		if got := proxyEnv(); len(got) != 0 {
			t.Fatalf("unexposed: %v, want no trusted proxy", got)
		}
	})

	t.Run("a referenced ConfigMap that does not exist stalls and is looked for again", func(t *testing.T) {
		console := s.console(ctx, t, "console")
		console.Spec.LifecycleConfigMapRef = &pgtoolboxv1alpha1.LocalObjectReference{Name: "eol-table"}
		must(t, s.admin.Update(ctx, console))
		result := s.reconcile(ctx, t, "console")
		stalled := s.console(ctx, t, "console")
		assertCondition(t, stalled, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonConfigMapNotFound)
		assertCondition(t, stalled, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonConfigMapNotFound)
		if message := meta.FindStatusCondition(stalled.Status.Conditions, pgtoolboxv1alpha1.ConditionConfigurationValid).Message; !strings.Contains(message, "spec.lifecycleConfigMapRef: ConfigMap eol-table") {
			t.Fatalf("ConfigurationValid message = %q, want the field and the ConfigMap named", message)
		}
		// The label-filtered ConfigMap cache never sees a user's ConfigMap
		// appear: the stall looks again.
		if result.RequeueAfter != configMapMissingRetry {
			t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, configMapMissingRetry)
		}

		must(t, s.admin.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "eol-table", Namespace: suiteNamespace}}))
		if result := s.reconcile(ctx, t, "console"); result.RequeueAfter != 0 {
			t.Fatalf("RequeueAfter = %s once the ConfigMap exists, want none", result.RequeueAfter)
		}
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)

		console = s.console(ctx, t, "console")
		console.Spec.LifecycleConfigMapRef = nil
		must(t, s.admin.Update(ctx, console))
		s.reconcile(ctx, t, "console")
	})

	t.Run("a name taken by someone else's StatefulSet stalls, and grants it nothing", func(t *testing.T) {
		must(t, s.admin.Create(ctx, &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "other-console", Namespace: suiteNamespace},
			Spec: appsv1.StatefulSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "x"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "x"}}},
				},
			},
		}))
		must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "other-console", Namespace: suiteNamespace}}))
		s.reconcile(ctx, t, "other-console")
		assertCondition(t, s.console(ctx, t, "other-console"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonResourceNotOwned)
		set := &appsv1.StatefulSet{}
		s.get(ctx, t, "other-console", set)
		if set.Spec.Template.Spec.Containers[0].Image != "x" || !set.DeletionTimestamp.IsZero() {
			t.Fatal("the operator wrote or deleted someone else's StatefulSet")
		}
	})
}
