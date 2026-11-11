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

package pgadmin

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/configuration"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, pgtoolboxv1alpha1.AddToScheme, cnpgv1.AddToScheme, routev1.AddToScheme, gatewayv1.Install,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

// harness is a Reconciler on a fake client, with its Events drained.
type harness struct {
	t        *testing.T
	client   client.Client
	r        *Reconciler
	recorder *events.FakeRecorder
}

func newHarness(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) *harness {
	t.Helper()
	scheme := testScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&pgtoolboxv1alpha1.PgAdmin{})
	for field, extract := range fieldIndexes() {
		builder = builder.WithIndex(&pgtoolboxv1alpha1.PgAdmin{}, field, extract)
	}
	if funcs != nil {
		builder = builder.WithInterceptorFuncs(*funcs)
	}
	c := builder.Build()
	recorder := events.NewFakeRecorder(100)
	cfg := configuration.Defaults()
	cfg.OperatorImage, cfg.PgAdminImage = testOperatorImage, testPgAdminImage
	return &harness{t: t, client: c, recorder: recorder, r: &Reconciler{
		Client: c, APIReader: c, Scheme: scheme, Recorder: recorder, Config: cfg,
		Features: Features{
			DatabaseRoles: discovery.Result{State: discovery.Available}, Poolers: discovery.Result{State: discovery.Available},
			Routes:   discovery.Result{State: discovery.Available},
			Gateways: discovery.Result{State: discovery.NotInstalled, Reason: "HTTPRoute.gateway.networking.k8s.io is not served by this cluster"},
		},
		Random: func(b []byte) (int, error) {
			for i := range b {
				b[i] = byte(i)
			}
			return len(b), nil
		},
	}}
}

func (h *harness) reconcile(name string) error {
	h.t.Helper()
	_, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name}})
	return err
}

func (h *harness) pgadmin(name string) *pgtoolboxv1alpha1.PgAdmin {
	h.t.Helper()
	var p pgtoolboxv1alpha1.PgAdmin
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &p); err != nil {
		h.t.Fatal(err)
	}
	return &p
}

func (h *harness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func condition(p *pgtoolboxv1alpha1.PgAdmin, conditionType string) metav1.Condition {
	c := meta.FindStatusCondition(p.Status.Conditions, conditionType)
	if c == nil {
		return metav1.Condition{Type: conditionType, Status: "absent"}
	}
	return *c
}

func expectCondition(t *testing.T, p *pgtoolboxv1alpha1.PgAdmin, conditionType string, status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason) {
	t.Helper()
	c := condition(p, conditionType)
	if c.Status != status || c.Reason != string(reason) {
		t.Fatalf("%s = %s/%s (%s), want %s/%s", conditionType, c.Status, c.Reason, c.Message, status, reason)
	}
	if c.ObservedGeneration != p.Generation {
		t.Fatalf("%s observedGeneration %d, want %d", conditionType, c.ObservedGeneration, p.Generation)
	}
}

func clientSecret(reload bool) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pgadmin-oidc", Namespace: testNamespace, ResourceVersion: "42"},
		Data:       map[string][]byte{"client-secret": []byte("s3cr3t-client")},
	}
	if reload {
		s.Labels = map[string]string{pgtoolboxv1alpha1.LabelReload: "true"}
	}
	return s
}

func baseObjects(p *pgtoolboxv1alpha1.PgAdmin) []client.Object {
	clusters, roles := testClusters(), testRoles()
	objs := make([]client.Object, 0, 2+len(clusters)+len(roles))
	objs = append(objs, p, clientSecret(true))
	for _, c := range clusters {
		objs = append(objs, &c)
	}
	for _, r := range roles {
		objs = append(objs, &r)
	}
	return objs
}

// markPodReady creates a Ready pgAdmin Pod at the Deployment's template
// (of PgAdmin "team").
func (h *harness) markPodReady() {
	const name = "team"
	h.t.Helper()
	var d appsv1.Deployment
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name + "-pgadmin"}, &d); err != nil {
		h.t.Fatal(err)
	}
	_ = h.client.DeleteAllOf(context.Background(), &corev1.Pod{}, client.InNamespace(testNamespace),
		client.MatchingLabels(d.Spec.Selector.MatchLabels))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-pgadmin-" + specs.HashOf(&d)[:5], Namespace: testNamespace,
			Labels: d.Spec.Template.Labels, Annotations: d.Spec.Template.Annotations,
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	if err := h.client.Create(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
}

// finishJob marks the only Job of component (of PgAdmin "team") finished, with a Pod whose
// container left message.
func (h *harness) finishJob(component string, failed bool, message string) *batchv1.Job {
	h.t.Helper()
	var jobs batchv1.JobList
	if err := h.client.List(context.Background(), &jobs, client.InNamespace(testNamespace),
		client.MatchingLabels{pgtoolboxv1alpha1.PgAdminLabelKey: "team", "app.kubernetes.io/component": component}); err != nil {
		h.t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		h.t.Fatalf("%d %s Jobs, want 1", len(jobs.Items), component)
	}
	job := &jobs.Items[0]
	conditionType := batchv1.JobComplete
	if failed {
		conditionType = batchv1.JobFailed
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: conditionType, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "pod failed"}}
	if err := h.client.Status().Update(context.Background(), job); err != nil {
		h.t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job.Name + "-x", Namespace: testNamespace,
			Labels: map[string]string{"job-name": job.Name, batchv1.ControllerUidLabel: string(job.UID)},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  component,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}},
		}}},
	}
	if err := h.client.Create(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
	return job
}

func TestReconcileToReady(t *testing.T) {
	p := oauth2PgAdmin()
	h := newHarness(t, nil, baseObjects(p)...)

	// Loop 1: children applied, pgAdmin not running yet.
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	expectCondition(t, got, "WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress)
	expectCondition(t, got, "ServersSynced", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonPgAdminNotRunning)
	expectCondition(t, got, "PgAdminEgressConfined", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonPgAdminNotRunning)
	expectCondition(t, got, "Ready", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress)
	expectCondition(t, got, "Progressing", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonReconciling)
	expectCondition(t, got, "Stalled", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonAsExpected)
	if got.Status.ObservedGeneration != 3 || got.Status.URL != "http://team-pgadmin.team-a.svc/" {
		t.Fatalf("status %+v", got.Status)
	}
	var role rbacv1.Role
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin-credentials"}, &role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 || role.Rules[0].Resources[0] != "secrets" || !slices.Equal(role.Rules[0].Verbs, []string{"get", "watch"}) {
		t.Fatalf("the Role must be one get/watch rule on Secrets: %+v", role.Rules)
	}
	if names := role.Rules[0].ResourceNames; !slices.Equal(names, []string{"billing-app", "billing-ca", "orders-app", "orders-ca", "reporting-pw", "team-pgadmin-offer"}) {
		t.Fatalf("the Role grants %v: exactly the offer, the offered Secrets and CAs, no privileged one", names)
	}
	var bootstrap corev1.Secret
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin-bootstrap"}, &bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap.Immutable == nil || !*bootstrap.Immutable {
		t.Fatal("the generated Secret must be immutable")
	}
	var deployment appsv1.Deployment
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Template.Annotations[AnnotationClientSecretVersion] != "42" {
		t.Fatal("a reload-labelled client Secret's version must be on the Pod template")
	}
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("a normal first rollout emits no Warning: %v", evs)
	}

	// Loop 2: pgAdmin Ready, registration starts.
	h.markPodReady()
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got = h.pgadmin("team")
	expectCondition(t, got, "WorkloadReady", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	expectCondition(t, got, "ServersSynced", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonRegistrationRunning)
	expectCondition(t, got, "PgAdminEgressConfined", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonPgAdminEgressOpen)
	if got.Status.Image != testPgAdminImage {
		t.Fatalf("status.image %q", got.Status.Image)
	}
	evs := strings.Join(h.events(), "\n")
	for _, want := range []string{"Normal RegistrationStarted", "Warning PgAdminEgressOpen"} {
		if !strings.Contains(evs, want) {
			t.Fatalf("events lack %q:\n%s", want, evs)
		}
	}

	// Loop 3: registration completes.
	h.finishJob(componentRegistration, false, `{"pgAdminVersion":"9.18","servers":3}`)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got = h.pgadmin("team")
	expectCondition(t, got, "ServersSynced", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	expectCondition(t, got, "ExposureReady", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonNotRequested)
	expectCondition(t, got, "Ready", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	expectCondition(t, got, "Progressing", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonAsExpected)
	if got.Status.PgAdminVersion != "9.18" || got.Status.RegisteredHash == "" {
		t.Fatalf("status %+v", got.Status)
	}
	registered := 0
	for _, row := range got.Status.Servers {
		if row.State == pgtoolboxv1alpha1.ServerStateRegistered {
			registered++
		}
	}
	if registered != 3 {
		t.Fatalf("%d servers Registered, want 3: %+v", registered, got.Status.Servers)
	}
	evs = strings.Join(h.events(), "\n")
	if !strings.Contains(evs, "Normal ServersRegistered") || !strings.Contains(evs, "Normal AsExpected") {
		t.Fatalf("events:\n%s", evs)
	}

	// Loop 4: nothing changed, so nothing is written and nothing emitted.
	version := got.ResourceVersion
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	if again := h.pgadmin("team"); again.ResourceVersion != version {
		t.Fatalf("a no-op reconcile wrote the status (%s -> %s)", version, again.ResourceVersion)
	}
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("a no-op reconcile emitted %v", evs)
	}
}

func TestReconcileNeverReadsASecretItDoesNotOwn(t *testing.T) {
	p := oauth2PgAdmin()
	objs := baseObjects(p)
	for _, name := range []string{"orders-app", "billing-app", "reporting-pw", "orders-ca", "billing-ca"} {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Data:       map[string][]byte{"password": []byte("pw-" + name)},
		})
	}
	var typedSecretReads []string
	h := newHarness(t, &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Secret); ok {
			typedSecretReads = append(typedSecretReads, key.Name)
		}
		return c.Get(ctx, key, obj, opts...)
	}}, objs...)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	if len(typedSecretReads) != 0 {
		t.Fatalf("the reconciler read Secret content: %v (only metadata reads are allowed)", typedSecretReads)
	}
}

func TestReconcileStallsWithoutTheClientSecret(t *testing.T) {
	p := oauth2PgAdmin()
	objs := baseObjects(p)[:1]
	h := newHarness(t, nil, objs...)
	if err := h.reconcile("team"); err != nil {
		t.Fatalf("a stall is not an error to retry: %v", err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonSecretNotFound)
	expectCondition(t, got, "Stalled", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonSecretNotFound)
	expectCondition(t, got, "Ready", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonSecretNotFound)
	var deployment appsv1.Deployment
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &deployment); err == nil {
		t.Fatal("no Deployment may be rendered without its client Secret")
	}
	if evs := strings.Join(h.events(), "\n"); !strings.Contains(evs, "Warning SecretNotFound") {
		t.Fatalf("events: %s", evs)
	}
}

func TestReconcileRefusesAForeignObject(t *testing.T) {
	p := oauth2PgAdmin()
	foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "team-pgadmin", Namespace: testNamespace}}
	h := newHarness(t, nil, append(baseObjects(p), foreign)...)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonResourceNotOwned)
	expectCondition(t, got, "Stalled", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonResourceNotOwned)
	var sa corev1.ServiceAccount
	_ = h.client.Get(context.Background(), client.ObjectKeyFromObject(foreign), &sa)
	if len(sa.OwnerReferences) != 0 || len(sa.Labels) != 0 {
		t.Fatal("the foreign ServiceAccount was taken over")
	}
}

func TestReconcilePaused(t *testing.T) {
	p := oauth2PgAdmin()
	p.Annotations = map[string]string{pgtoolboxv1alpha1.AnnotationReconcile: pgtoolboxv1alpha1.ReconcileSkip}
	h := newHarness(t, nil, baseObjects(p)...)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "Progressing", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonReconciliationPaused)
	expectCondition(t, got, "Ready", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciliationPaused)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	if again := h.pgadmin("team"); again.ResourceVersion != got.ResourceVersion {
		t.Fatal("a paused PgAdmin's status is rewritten on every loop")
	}
	var sas corev1.ServiceAccountList
	_ = h.client.List(context.Background(), &sas, client.InNamespace(testNamespace))
	if len(sas.Items) != 0 {
		t.Fatal("a paused PgAdmin must not be applied")
	}
}

func TestReconcileMissingClusterStillDeploys(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.ClusterRefs = append(p.Spec.ClusterRefs, pgtoolboxv1alpha1.LocalObjectReference{Name: "gone"})
	h := newHarness(t, nil, baseObjects(p)...)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonClusterNotFound)
	expectCondition(t, got, "Stalled", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonAsExpected)
	var deployment appsv1.Deployment
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &deployment); err != nil {
		t.Fatalf("the resolvable Clusters are still served: %v", err)
	}
}

func TestReconcileRegistrationFailure(t *testing.T) {
	p := oauth2PgAdmin()
	h := newHarness(t, nil, baseObjects(p)...)
	_ = h.reconcile("team")
	h.markPodReady()
	_ = h.reconcile("team")
	h.finishJob(componentRegistration, true, "load-servers: exit status 1: Error: role does not exist")
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "ServersSynced", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRegistrationFailed)
	if msg := condition(got, "ServersSynced").Message; !strings.Contains(msg, "role does not exist") || !strings.Contains(msg, "delete the Job to retry") {
		t.Fatalf("message %q must copy the Job's fact", msg)
	}
	expectCondition(t, got, "Ready", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRegistrationFailed)
}

func TestReconcileUpgradeBacksUpFirst(t *testing.T) {
	p := oauth2PgAdmin()
	h := newHarness(t, nil, baseObjects(p)...)
	_ = h.reconcile("team")
	h.markPodReady()
	_ = h.reconcile("team")
	h.finishJob(componentRegistration, false, `{"pgAdminVersion":"9.18"}`)
	_ = h.reconcile("team")
	h.events()

	// A new image: the backup Job runs first, the old image stays.
	const next = "ghcr.io/fyannk/pgadmin:9.19-hardened@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	current := h.pgadmin("team")
	current.Spec.Image = &pgtoolboxv1alpha1.ImageSpec{
		Repository: "ghcr.io/fyannk/pgadmin", Tag: "9.19-hardened",
		Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
	}
	current.Generation++
	if err := h.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonUpgradeBackupRunning)
	var deployment appsv1.Deployment
	_ = h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &deployment)
	if image := existingImage(&deployment); image != testPgAdminImage {
		t.Fatalf("the old image must keep running until the backup completes, got %s", image)
	}
	backup := h.finishJob(componentBackup, false, "/var/lib/pgadmin/.pgtoolbox/backups/pgadmin4-x.db")
	if backup.Spec.Template.Spec.Containers[0].Image != testPgAdminImage {
		t.Fatal("the backup must run the image that wrote the database")
	}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	_ = h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &deployment)
	if image := existingImage(&deployment); image != next {
		t.Fatalf("after the backup the new image rolls out, got %s", image)
	}
	if evs := strings.Join(h.events(), "\n"); !strings.Contains(evs, "UpgradeBackupCompleted") || !strings.Contains(evs, "RolloutStarted") {
		t.Fatalf("events: %s", evs)
	}
}

// TestReconcileUpgradeBackupSurvivesTheJobTTL: the finished backup Job is
// deleted by its TTL while the new image is still not Ready. The backup's
// completion is recorded in status, so the operator neither runs the backup
// again (its --keep pruning would drop the real pre-upgrade copy) nor rolls
// the Deployment back to the old image on a database the new one may have
// migrated.
func TestReconcileUpgradeBackupSurvivesTheJobTTL(t *testing.T) {
	p := oauth2PgAdmin()
	h := newHarness(t, nil, baseObjects(p)...)
	_ = h.reconcile("team")
	h.markPodReady()
	_ = h.reconcile("team")
	h.finishJob(componentRegistration, false, `{"pgAdminVersion":"9.18"}`)
	_ = h.reconcile("team")

	const next = "ghcr.io/fyannk/pgadmin:9.19-hardened@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	current := h.pgadmin("team")
	current.Spec.Image = &pgtoolboxv1alpha1.ImageSpec{
		Repository: "ghcr.io/fyannk/pgadmin", Tag: "9.19-hardened",
		Digest: "sha256:2222222222222222222222222222222222222222222222222222222222222222",
	}
	current.Generation++
	if err := h.client.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	backup := h.finishJob(componentBackup, false, "/var/lib/pgadmin/.pgtoolbox/backups/pgadmin4-x.db")
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	if got := h.pgadmin("team").Status.UpgradeBackup; got == nil || got.From != testPgAdminImage || got.To != next {
		t.Fatalf("status.upgradeBackup = %+v, want %s -> %s", got, testPgAdminImage, next)
	}
	// The new image's Pod is not Ready for longer than the Job's TTL: the
	// TTL controller deletes the finished Job.
	if err := h.client.Delete(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}

	var deployment appsv1.Deployment
	_ = h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &deployment)
	if image := existingImage(&deployment); image != next {
		t.Fatalf("the Deployment rolled back to %s after the backup Job's TTL", image)
	}
	var jobs batchv1.JobList
	_ = h.client.List(context.Background(), &jobs, client.InNamespace(testNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": componentBackup})
	if len(jobs.Items) != 0 {
		t.Fatalf("the backup ran again: %d backup Job(s)", len(jobs.Items))
	}
}

// TestReconcileUpgradeBackupSkippedByAnnotation: when the old image can no
// longer run its backup (it cannot be pulled), the skip annotation naming
// the target releases that one upgrade, recorded in status and as a
// Warning; an annotation naming another target releases nothing, and a
// left-over annotation does not skip the next upgrade.
func TestReconcileUpgradeBackupSkippedByAnnotation(t *testing.T) {
	p := oauth2PgAdmin()
	h := newHarness(t, nil, baseObjects(p)...)
	_ = h.reconcile("team")
	h.markPodReady()
	_ = h.reconcile("team")
	h.finishJob(componentRegistration, false, `{"pgAdminVersion":"9.18"}`)
	_ = h.reconcile("team")

	const (
		repository = "ghcr.io/fyannk/pgadmin"
		digestB    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		digestC    = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
		imageB     = repository + ":9.19-hardened@" + digestB
	)
	upgrade := func(tag, digest string, annotation string) {
		t.Helper()
		current := h.pgadmin("team")
		if tag != "" {
			current.Spec.Image = &pgtoolboxv1alpha1.ImageSpec{Repository: repository, Tag: tag, Digest: digest}
			current.Generation++
		}
		if annotation != "" {
			current.Annotations = map[string]string{pgtoolboxv1alpha1.AnnotationSkipUpgradeBackup: annotation}
		}
		if err := h.client.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}
	image := func() string {
		var d appsv1.Deployment
		_ = h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &d)
		return existingImage(&d)
	}

	upgrade("9.19-hardened", digestB, "")
	_ = h.reconcile("team")
	h.finishJob(componentBackup, true, "ErrImagePull")
	_ = h.reconcile("team")
	expectCondition(t, h.pgadmin("team"), "WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonUpgradeBackupFailed)
	h.events()

	upgrade("", "", digestC)
	_ = h.reconcile("team")
	if got := image(); got != testPgAdminImage {
		t.Fatalf("an annotation naming another target released the upgrade: %s", got)
	}

	upgrade("", "", digestB)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	if got := image(); got != imageB {
		t.Fatalf("the skip annotation naming the target must release it, got %s", got)
	}
	got := h.pgadmin("team")
	if b := got.Status.UpgradeBackup; b == nil || !b.Skipped || b.From != testPgAdminImage || b.To != imageB {
		t.Fatalf("status.upgradeBackup = %+v, want a recorded skip %s -> %s", b, testPgAdminImage, imageB)
	}
	if evs := strings.Join(h.events(), "\n"); !strings.Contains(evs, "Warning UpgradeBackupSkipped") || strings.Contains(evs, "UpgradeBackupCompleted") {
		t.Fatalf("the skip must be a Warning Event, and no backup claimed: %s", evs)
	}
	_ = h.reconcile("team")
	if evs := h.events(); slices.ContainsFunc(evs, func(e string) bool { return strings.Contains(e, "UpgradeBackupSkipped") }) {
		t.Fatalf("the skip Event repeats: %v", evs)
	}

	// B runs; the annotation still names B. The next upgrade backs up.
	h.markPodReady()
	_ = h.reconcile("team")
	if got := h.pgadmin("team").Status.Image; got != imageB {
		t.Fatalf("status.image %s, want %s", got, imageB)
	}
	upgrade("9.20-hardened", digestC, "")
	_ = h.reconcile("team")
	expectCondition(t, h.pgadmin("team"), "WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonUpgradeBackupRunning)
	if got := image(); got != imageB {
		t.Fatalf("a left-over skip annotation released the next upgrade: %s", got)
	}
}

// TestReconcileUpgradeChainBacksUpWithTheLastWriter: A -> B is released
// (backed up, or skipped by the annotation) but B never becomes Ready, then
// the spec moves to C. B may already have migrated the settings database:
// the backup for C runs with B, the Deployment is held on B (never rolled
// back to A), and C rolls out once that backup completes.
func TestReconcileUpgradeChainBacksUpWithTheLastWriter(t *testing.T) {
	const (
		repository = "ghcr.io/fyannk/pgadmin"
		digestB    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		digestC    = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
		imageB     = repository + ":9.19-hardened@" + digestB
		imageC     = repository + ":9.20-hardened@" + digestC
	)
	for _, skipB := range []bool{false, true} {
		name := "A to B backed up"
		if skipB {
			name = "A to B skipped"
		}
		t.Run(name, func(t *testing.T) {
			p := oauth2PgAdmin()
			h := newHarness(t, nil, baseObjects(p)...)
			_ = h.reconcile("team")
			h.markPodReady()
			_ = h.reconcile("team")
			h.finishJob(componentRegistration, false, `{"pgAdminVersion":"9.18"}`)
			_ = h.reconcile("team")
			setImage := func(tag, digest string, annotations map[string]string) {
				t.Helper()
				current := h.pgadmin("team")
				current.Spec.Image = &pgtoolboxv1alpha1.ImageSpec{Repository: repository, Tag: tag, Digest: digest}
				current.Annotations = annotations
				current.Generation++
				if err := h.client.Update(context.Background(), current); err != nil {
					t.Fatal(err)
				}
				if err := h.reconcile("team"); err != nil {
					t.Fatal(err)
				}
			}
			image := func() string {
				var d appsv1.Deployment
				_ = h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "team-pgadmin"}, &d)
				return existingImage(&d)
			}
			backupJobs := func() []batchv1.Job {
				var jobs batchv1.JobList
				_ = h.client.List(context.Background(), &jobs, client.InNamespace(testNamespace),
					client.MatchingLabels{"app.kubernetes.io/component": componentBackup})
				return jobs.Items
			}

			if skipB {
				setImage("9.19-hardened", digestB, map[string]string{pgtoolboxv1alpha1.AnnotationSkipUpgradeBackup: digestB})
			} else {
				setImage("9.19-hardened", digestB, nil)
				backup := h.finishJob(componentBackup, false, "x")
				_ = h.reconcile("team")
				// Out of the way, as its TTL would.
				_ = h.client.Delete(context.Background(), backup)
			}
			if got := image(); got != imageB {
				t.Fatalf("B must be released, the Deployment runs %s", got)
			}
			if got := h.pgadmin("team").Status.Image; got != testPgAdminImage {
				t.Fatalf("B never became Ready, yet status.image is %s", got)
			}

			setImage("9.20-hardened", digestC, nil)
			jobs := backupJobs()
			if len(jobs) != 1 || jobs[0].Spec.Template.Spec.Containers[0].Image != imageB {
				t.Fatalf("the backup before C must run with B, the last writer; Jobs: %d", len(jobs))
			}
			expectCondition(t, h.pgadmin("team"), "WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonUpgradeBackupRunning)
			if got := image(); got != imageB {
				t.Fatalf("the Deployment must be held on B, got %s", got)
			}

			h.finishJob(componentBackup, false, "x")
			if err := h.reconcile("team"); err != nil {
				t.Fatal(err)
			}
			if got := image(); got != imageC {
				t.Fatalf("after B's backup C rolls out, got %s", got)
			}
			if b := h.pgadmin("team").Status.UpgradeBackup; b == nil || b.From != imageB || b.To != imageC || b.Skipped {
				t.Fatalf("status.upgradeBackup = %+v, want %s -> %s", b, imageB, imageC)
			}
		})
	}
}

func TestReconcileAPIErrorIsPublished(t *testing.T) {
	p := oauth2PgAdmin()
	h := newHarness(t, &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*cnpgv1.ClusterList); ok {
			return errors.New("etcdserver: request timed out")
		}
		return c.List(ctx, list, opts...)
	}}, baseObjects(p)...)
	if err := h.reconcile("team"); err == nil {
		t.Fatal("an API failure must be returned for backoff")
	}
	got := h.pgadmin("team")
	expectCondition(t, got, "Ready", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonReconcileError)
	if msg := condition(got, "Ready").Message; !strings.HasPrefix(msg, "ReconciliationFailed: ") {
		t.Fatalf("Ready message %q", msg)
	}
	if evs := h.events(); len(evs) != 1 || !strings.Contains(evs[0], "Warning ReconciliationFailed") {
		t.Fatalf("events %v", evs)
	}
	_ = h.reconcile("team")
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("the same class of error is one Event, not one per retry: %v", evs)
	}
}

func TestMapFunctions(t *testing.T) {
	byRef := oauth2PgAdmin()
	bySelector := openshiftPgAdmin()
	h := newHarness(t, nil, byRef, bySelector)
	names := func(requests []ctrl.Request) []string {
		out := make([]string, 0, len(requests))
		for _, r := range requests {
			out = append(out, r.Name)
		}
		slices.Sort(out)
		return out
	}
	orders := testClusters()[0]
	if got := names(h.r.forCluster(context.Background(), &orders)); !slices.Equal(got, []string{"ops", "team"}) {
		t.Fatalf("a Cluster named by one and selected by the other: %v", got)
	}
	unlabelled := testCluster2("orders", func(c *cnpgv1.Cluster) { c.Labels = nil })
	if got := names(h.r.forCluster(context.Background(), &unlabelled)); !slices.Equal(got, []string{"team"}) {
		t.Fatalf("an unselected Cluster: %v", got)
	}
	roles := testRoles()
	if got := names(h.r.forDatabaseRole(context.Background(), &roles[0])); !slices.Equal(got, []string{"ops", "team"}) {
		t.Fatalf("a DatabaseRole of orders: %v", got)
	}
	pooler := testPoolers()[0]
	if got := names(h.r.forPooler(context.Background(), &pooler)); !slices.Equal(got, []string{"ops"}) {
		t.Fatalf("a Pooler: %v", got)
	}
	if got := names(h.r.forClientSecret(context.Background(), clientSecret(true))); !slices.Equal(got, []string{"team"}) {
		t.Fatalf("the client Secret: %v", got)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Labels: map[string]string{pgtoolboxv1alpha1.PgAdminLabelKey: "team"}}}
	if got := names(byInstanceLabel(context.Background(), pod)); !slices.Equal(got, []string{"team"}) {
		t.Fatalf("a Pod: %v", got)
	}
}

// A Secret planted under a generated name with our predictable labels (a
// cookie secret whose value the planter knows) is refused, never used.
func TestReconcileRefusesAPlantedGeneratedSecret(t *testing.T) {
	p := openshiftPgAdmin()
	p.Spec.NetworkPolicy = pgtoolboxv1alpha1.PgAdminNetworkPolicySpec{}
	planted := generatedSecret(p, "ops-pgadmin-proxy-cookie", "cookie-secret", []byte("known-to-the-attacker"))
	planted.OwnerReferences = nil
	h := newHarness(t, nil, append(baseObjects(p), planted)...)
	if err := h.reconcile("ops"); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin("ops")
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonResourceNotOwned)
	var deployment appsv1.Deployment
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "ops-pgadmin"}, &deployment); err == nil {
		t.Fatal("no Deployment may mount a Secret this PgAdmin did not create")
	}
}

// A Deployment planted under the generated name with our labels (and an
// extra container) is refused, not adopted with its container.
func TestReconcileRefusesAPlantedDeployment(t *testing.T) {
	p := oauth2PgAdmin()
	planted := buildDeployment(p, buildInputs{Image: specs.ResolvedImage{Reference: "evil"}, OperatorImage: "evil"})
	planted.OwnerReferences = nil
	planted.TypeMeta = metav1.TypeMeta{}
	h := newHarness(t, nil, append(baseObjects(p), planted)...)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	expectCondition(t, h.pgadmin("team"), "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonResourceNotOwned)
	var deployment appsv1.Deployment
	_ = h.client.Get(context.Background(), client.ObjectKeyFromObject(planted), &deployment)
	if len(deployment.OwnerReferences) != 0 {
		t.Fatal("the planted Deployment was adopted")
	}
}

// A Pod carrying only the job-name label (anyone who may create Pods can)
// does not feed the registration's report.
func TestJobReportIgnoresAnImpostorPod(t *testing.T) {
	p := oauth2PgAdmin()
	h := newHarness(t, nil, baseObjects(p)...)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "team-pgadmin-sync-x", Namespace: testNamespace, UID: "job-uid"}}
	impostor := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "impostor", Namespace: testNamespace, Labels: map[string]string{"job-name": job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: `{"pgAdminVersion":"evil"}`}},
		}}},
	}
	if err := h.client.Create(context.Background(), impostor); err != nil {
		t.Fatal(err)
	}
	if got := h.r.jobTerminationMessage(context.Background(), job); got != "" {
		t.Fatalf("an impostor Pod fed the report: %q", got)
	}
}

// TestReconcileUnavailableOptionalAPI: an optional API the spec needs and
// the start-time probe found unusable is named in ConfigurationValid with
// its own reason and one Warning, and pgAdmin still serves the rest.
func TestReconcileUnavailableOptionalAPI(t *testing.T) {
	forbidden := discovery.Result{State: discovery.Forbidden, Reason: "not allowed to list X in " + testNamespace}
	notServed := discovery.Result{State: discovery.NotInstalled, Reason: "X is not served by this cluster"}
	tests := []struct {
		name     string
		pgadmin  func() *pgtoolboxv1alpha1.PgAdmin
		features func(*Features)
		want     pgtoolboxv1alpha1.Reason
	}{
		{
			name: "forbidden DatabaseRole API", pgadmin: oauth2PgAdmin,
			features: func(f *Features) { f.DatabaseRoles = forbidden },
			want:     pgtoolboxv1alpha1.ReasonDatabaseRoleAPIUnavailable,
		},
		{
			name: "pooler endpoint without the Pooler API",
			pgadmin: func() *pgtoolboxv1alpha1.PgAdmin {
				p := oauth2PgAdmin()
				p.Spec.Endpoints = []pgtoolboxv1alpha1.Endpoint{"rw", "pooler:orders-pooler"}
				return p
			},
			features: func(f *Features) { f.Poolers = notServed },
			want:     pgtoolboxv1alpha1.ReasonPoolerAPIUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.pgadmin()
			h := newHarness(t, nil, baseObjects(p)...)
			tt.features(&h.r.Features)
			if err := h.reconcile(p.Name); err != nil {
				t.Fatal(err)
			}
			got := h.pgadmin(p.Name)
			if tt.want == pgtoolboxv1alpha1.ReasonAsExpected {
				expectCondition(t, got, "ConfigurationValid", metav1.ConditionTrue, tt.want)
				return
			}
			expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, tt.want)
			expectCondition(t, got, "Stalled", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonAsExpected)
			if msg := condition(got, "ConfigurationValid").Message; !strings.Contains(msg, "unavailable: X ") && !strings.Contains(msg, "unavailable: not allowed to list X") {
				t.Errorf("message does not carry the probe's reason: %q", condition(got, "ConfigurationValid").Message)
			}
			warnings := 0
			for _, e := range h.events() {
				if strings.HasPrefix(e, "Warning "+string(tt.want)) {
					warnings++
				}
			}
			if warnings != 1 {
				t.Errorf("Warning %s events = %d, want 1 (first transition)", tt.want, warnings)
			}
			if err := h.reconcile(p.Name); err != nil {
				t.Fatal(err)
			}
			for _, e := range h.events() {
				if strings.HasPrefix(e, "Warning "+string(tt.want)) {
					t.Errorf("repeated Warning on an unchanged condition: %s", e)
				}
			}
			var deployment appsv1.Deployment
			if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: p.Name + "-pgadmin"}, &deployment); err != nil {
				t.Errorf("pgAdmin is no longer deployed: %v", err)
			}
		})
	}
}

// TestReconcileDatabaseRolesNotServed: on a CloudNativePG install that
// does not serve DatabaseRole (CRDs without it), a PgAdmin offering DatabaseRoles (the
// default, which the API server stores as databaseRoles: true) offers
// none, says so in ConfigurationValid's message, warns nothing, and
// becomes Ready. Regression: the stored default was read as an explicit
// request and the PgAdmin never became Ready.
func TestReconcileDatabaseRolesNotServed(t *testing.T) {
	notServed := discovery.Result{State: discovery.NotInstalled, Reason: "DatabaseRole.postgresql.cnpg.io is not served by this cluster"}
	for name, databaseRoles := range map[string]*bool{
		"as stored by the API server (defaulted true)": new(true),
		"unset (Go zero value)":                        nil,
	} {
		t.Run(name, func(t *testing.T) {
			p := oauth2PgAdmin()
			p.Spec.Credentials.DatabaseRoles = databaseRoles
			h := newHarness(t, nil, baseObjects(p)...)
			h.r.Features.DatabaseRoles = notServed

			if err := h.reconcile("team"); err != nil {
				t.Fatal(err)
			}
			got := h.pgadmin("team")
			expectCondition(t, got, "ConfigurationValid", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
			if msg := condition(got, "ConfigurationValid").Message; !strings.Contains(msg, "no DatabaseRole offered: "+notServed.Reason) {
				t.Errorf("ConfigurationValid message does not say why no DatabaseRole is offered: %q", msg)
			}
			for _, server := range h.offerDocument().Servers {
				if strings.HasSuffix(server.SecretName, "-pw") { // the fixture DatabaseRoles' password Secrets
					t.Errorf("server %s offers the DatabaseRole %s, a kind this CloudNativePG does not serve", server.Name, server.User)
				}
			}

			h.markPodReady()
			if err := h.reconcile("team"); err != nil {
				t.Fatal(err)
			}
			h.finishJob(componentRegistration, false, `{"pgAdminVersion":"9.18","servers":2}`)
			if err := h.reconcile("team"); err != nil {
				t.Fatal(err)
			}
			got = h.pgadmin("team")
			expectCondition(t, got, "Ready", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
			for _, e := range h.events() {
				if strings.Contains(e, string(pgtoolboxv1alpha1.ReasonDatabaseRoleAPIUnavailable)) {
					t.Errorf("an absent DatabaseRole kind raised an event: %s", e)
				}
			}
		})
	}
}

// TestReconcileImageAllowList: a PgAdmin image outside the operator's
// allow-list (PGADMIN_IMAGE_REPOSITORIES / --pgadmin-image-repositories)
// stalls with ImageNotAllowed and nothing is applied; inside it, pgAdmin
// deploys. With no list, each image's default repository is the list.
func TestReconcileImageAllowList(t *testing.T) {
	foreign := &pgtoolboxv1alpha1.ImageSpec{Repository: "registry.example.com/pgadmin", Tag: "9.18"}
	tests := []struct {
		name    string
		pgadmin func() *pgtoolboxv1alpha1.PgAdmin
		allowed []string
		want    pgtoolboxv1alpha1.Reason
	}{
		{name: "default image", pgadmin: oauth2PgAdmin, want: pgtoolboxv1alpha1.ReasonAsExpected},
		{
			name: "foreign repository without a list",
			pgadmin: func() *pgtoolboxv1alpha1.PgAdmin {
				p := oauth2PgAdmin()
				p.Spec.Image = foreign
				return p
			},
			want: pgtoolboxv1alpha1.ReasonImageNotAllowed,
		},
		{
			name: "repository under an allowed prefix",
			pgadmin: func() *pgtoolboxv1alpha1.PgAdmin {
				p := oauth2PgAdmin()
				p.Spec.Image = foreign
				return p
			},
			allowed: []string{"registry.example.com/"},
			want:    pgtoolboxv1alpha1.ReasonAsExpected,
		},
		{
			name: "look-alike of an allowed prefix",
			pgadmin: func() *pgtoolboxv1alpha1.PgAdmin {
				p := oauth2PgAdmin()
				p.Spec.Image = &pgtoolboxv1alpha1.ImageSpec{Repository: "registry.example.com.evil/pgadmin", Tag: "9.18"}
				return p
			},
			allowed: []string{"registry.example.com/"},
			want:    pgtoolboxv1alpha1.ReasonImageNotAllowed,
		},
		{
			name: "foreign oauth-proxy image",
			pgadmin: func() *pgtoolboxv1alpha1.PgAdmin {
				p := oauth2PgAdmin()
				p.Spec.Auth = pgtoolboxv1alpha1.PgAdminAuthSpec{OpenShiftOAuthProxy: &pgtoolboxv1alpha1.PgAdminOpenShiftOAuthProxySpec{
					Image: &pgtoolboxv1alpha1.ImageSpec{Repository: "attacker.example/oauth-proxy", Tag: "1"},
				}}
				return p
			},
			want: pgtoolboxv1alpha1.ReasonImageNotAllowed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.pgadmin()
			h := newHarness(t, nil, baseObjects(p)...)
			h.r.Config.PgAdminImageRepositories = tt.allowed
			if err := h.reconcile(p.Name); err != nil {
				t.Fatal(err)
			}
			got := h.pgadmin(p.Name)
			var deployment appsv1.Deployment
			deployErr := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: p.Name + "-pgadmin"}, &deployment)
			if tt.want == pgtoolboxv1alpha1.ReasonAsExpected {
				expectCondition(t, got, "ConfigurationValid", metav1.ConditionTrue, tt.want)
				if deployErr != nil {
					t.Fatalf("an allowed image is not deployed: %v", deployErr)
				}
				return
			}
			expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, tt.want)
			expectCondition(t, got, "Stalled", metav1.ConditionTrue, tt.want)
			if deployErr == nil {
				t.Fatal("a Deployment was rendered with an image outside the allow-list")
			}
			warnings := 0
			for _, e := range h.events() {
				if strings.HasPrefix(e, "Warning ImageNotAllowed") {
					warnings++
				}
			}
			if warnings != 1 {
				t.Errorf("ImageNotAllowed Warnings = %d, want 1", warnings)
			}
		})
	}
}

// A Cluster of one instance gets no ro server, a NotOffered row instead;
// scaled to two, the Cluster's own event re-resolves the PgAdmin and the
// server is offered; scaled back, it is withdrawn.
func TestReconcileROFollowsTheClusterScale(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.Endpoints = []pgtoolboxv1alpha1.Endpoint{"rw", "ro"}
	p.Spec.Credentials.DatabaseRoles = new(false)
	objs := baseObjects(p)
	for _, obj := range objs {
		if c, ok := obj.(*cnpgv1.Cluster); ok && c.Name == "billing" {
			c.Spec.Instances = 1
		}
	}
	h := newHarness(t, nil, objs...)
	ctx := context.Background()
	billingRO := func() pgtoolboxv1alpha1.PgAdminServerStatus {
		t.Helper()
		for _, row := range h.pgadmin("team").Status.Servers {
			if row.Cluster == "billing" && row.Endpoint == "ro" {
				return row
			}
		}
		t.Fatal("no status row for billing ro")
		return pgtoolboxv1alpha1.PgAdminServerStatus{}
	}
	scale := func(instances int) {
		t.Helper()
		var billing cnpgv1.Cluster
		must(t, h.client.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "billing"}, &billing))
		billing.Spec.Instances = instances
		must(t, h.client.Update(ctx, &billing))
		requests := h.r.forCluster(ctx, &billing)
		if !slices.ContainsFunc(requests, func(r ctrl.Request) bool { return r.Name == "team" }) {
			t.Fatalf("a change to Cluster billing does not enqueue PgAdmin team: %v", requests)
		}
		if _, err := h.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "team"}}); err != nil {
			t.Fatal(err)
		}
	}

	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	if row := billingRO(); row.State != pgtoolboxv1alpha1.ServerStateNotOffered || row.Message != "no replica: the Cluster has one instance" {
		t.Fatalf("one instance: billing ro %+v, want NotOffered with the reason", row)
	}
	expectCondition(t, h.pgadmin("team"), "ConfigurationValid", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)

	scale(2)
	if row := billingRO(); row.State != pgtoolboxv1alpha1.ServerStatePending || row.Message != "" {
		t.Fatalf("two instances: billing ro %+v, want offered (Pending until registered)", row)
	}

	scale(1)
	if row := billingRO(); row.State != pgtoolboxv1alpha1.ServerStateNotOffered {
		t.Fatalf("back to one instance: billing ro %+v, want NotOffered", row)
	}
}

// contextAware makes every fake-client call fail once its context is done,
// as a real client does; cancelOnClusterList cancels the loop's context
// mid-loop, the way ReconciliationTimeout does.
func contextAware(cancelOnClusterList *func()) *interceptor.Funcs {
	done := func(ctx context.Context) error { return ctx.Err() }
	return &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := done(ctx); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*cnpgv1.ClusterList); ok && *cancelOnClusterList != nil {
				(*cancelOnClusterList)()
			}
			if err := done(ctx); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := done(ctx); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := done(ctx); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if err := done(ctx); err != nil {
				return err
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}
}

// TestReconcileTimedOutStillPublishes: a loop cancelled by
// ReconciliationTimeout still writes its status, Ready=False with why,
// instead of leaving the previous Ready=True in place.
func TestReconcileTimedOutStillPublishes(t *testing.T) {
	p := oauth2PgAdmin()
	var cancelLoop func()
	h := newHarness(t, contextAware(&cancelLoop), baseObjects(p)...)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	h.markPodReady()
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	h.finishJob(componentRegistration, false, `{"pgAdminVersion":"9.18","servers":3}`)
	if err := h.reconcile("team"); err != nil {
		t.Fatal(err)
	}
	expectCondition(t, h.pgadmin("team"), "Ready", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	_ = h.events()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelLoop = cancel
	_, err := h.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "team"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled loop returned %v", err)
	}
	cancelLoop = nil
	got := h.pgadmin("team")
	expectCondition(t, got, "Ready", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonReconcileError)
	expectCondition(t, got, "Progressing", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonReconciling)
}

// TestPublishConflictIsReturnedNotOverwritten: a status computed from a
// stale read is refused, returned for backoff, and the newer object stays
// as it is (the shared conflict policy of every controller).
func TestPublishConflictIsReturnedNotOverwritten(t *testing.T) {
	p := oauth2PgAdmin()
	h := newHarness(t, nil, baseObjects(p)...)
	stale := h.pgadmin("team")
	// Someone else writes the object after the loop read it.
	fresh := stale.DeepCopy()
	fresh.Annotations = map[string]string{"touched": "yes"}
	if err := h.client.Update(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	before := stale.Status.DeepCopy()
	stale.Status.URL = "http://stale/"
	err := h.r.publish(context.Background(), stale, before, false, nil)
	if !apierrors.IsConflict(err) {
		t.Fatalf("publish of a stale read returned %v, want a Conflict", err)
	}
	if got := h.pgadmin("team"); got.Status.URL == "http://stale/" {
		t.Fatal("a status computed from a stale read was written")
	}
	if evs := h.events(); len(evs) != 0 {
		t.Fatalf("a refused write emitted %v", evs)
	}
}

// TestReconcileHostnameAllowList: with EXPOSURE_ALLOWED_HOST_SUFFIXES set, a
// spec.exposure.hostname outside them stalls with HostnameNotAllowed, and
// the Ingress already published under it is withdrawn; inside them, or with
// no list, the Ingress is published.
func TestReconcileHostnameAllowList(t *testing.T) {
	ingress := func(hostname string) func() *pgtoolboxv1alpha1.PgAdmin {
		return func() *pgtoolboxv1alpha1.PgAdmin {
			p := oauth2PgAdmin()
			p.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: hostname}
			return p
		}
	}
	// The round-10 probe: a check that stalls before the hostname's (the
	// image allow-list, the settings database) must not keep the Ingress
	// published under a refused name.
	refusedImage := func() *pgtoolboxv1alpha1.PgAdmin {
		p := ingress("login.corp.example.com")()
		p.Spec.Image = &pgtoolboxv1alpha1.ImageSpec{Repository: "evil.example/pgadmin", Tag: "1"}
		return p
	}
	withConfigDatabase := func() *pgtoolboxv1alpha1.PgAdmin {
		p := ingress("login.corp.example.com")()
		p.Spec.ConfigDatabase = &pgtoolboxv1alpha1.PgAdminConfigDatabaseSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "billing"}}
		return p
	}
	for _, tt := range []struct {
		name     string
		pgadmin  func() *pgtoolboxv1alpha1.PgAdmin
		suffixes []string
		// before runs ahead of the first loop, after ahead of the second,
		// with the suffixes.
		before, after func(h *harness)
		message       string
		want          pgtoolboxv1alpha1.Reason
	}{
		{name: "no list keeps any hostname", pgadmin: ingress("login.corp.example.com"), want: pgtoolboxv1alpha1.ReasonAsExpected},
		{name: "under a suffix", pgadmin: ingress("pgadmin.team-a.apps.example.com"), suffixes: []string{"apps.example.com"}, want: pgtoolboxv1alpha1.ReasonAsExpected},
		{name: "clusterIP publishes no name", pgadmin: oauth2PgAdmin, suffixes: []string{"apps.example.com"}, want: pgtoolboxv1alpha1.ReasonAsExpected},
		{name: "another team's name", pgadmin: ingress("login.corp.example.com"), suffixes: []string{"apps.example.com"}, want: pgtoolboxv1alpha1.ReasonHostnameNotAllowed},
		{name: "a look-alike of a suffix", pgadmin: ingress("evilapps.example.com"), suffixes: []string{"apps.example.com"}, want: pgtoolboxv1alpha1.ReasonHostnameNotAllowed},
		{
			name: "the image refused too", pgadmin: refusedImage, suffixes: []string{"apps.example.com"},
			before:  func(h *harness) { h.r.Config.PgAdminImageRepositories = []string{"evil.example/pgadmin"} },
			after:   func(h *harness) { h.r.Config.PgAdminImageRepositories = []string{"registry.example/pgadmin"} },
			message: "; also spec.image repository evil.example/pgadmin is not in the operator's allow-list",
			want:    pgtoolboxv1alpha1.ReasonHostnameNotAllowed,
		},
		{
			name: "the settings database stalls too", pgadmin: withConfigDatabase, suffixes: []string{"apps.example.com"},
			before: func(h *harness) { h.r.Features.Databases = discovery.Result{State: discovery.Available} },
			after: func(h *harness) {
				h.r.Features.Databases = discovery.Result{State: discovery.NotInstalled, Reason: "Database.postgresql.cnpg.io is not served"}
			},
			want: pgtoolboxv1alpha1.ReasonHostnameNotAllowed,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.pgadmin()
			h := newHarness(t, nil, baseObjects(p)...)
			if tt.before != nil {
				tt.before(h)
			}
			// Published before the list existed.
			if err := h.reconcile(p.Name); err != nil {
				t.Fatal(err)
			}
			if p.Spec.Exposure.Type == pgtoolboxv1alpha1.ExposureTypeIngress {
				if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: p.Name + "-pgadmin"}, &networkingv1.Ingress{}); err != nil {
					t.Fatalf("no Ingress before the allow-list: %v", err)
				}
			}
			h.r.Config.ExposureAllowedHostSuffixes = tt.suffixes
			if tt.after != nil {
				tt.after(h)
			}
			if err := h.reconcile(p.Name); err != nil {
				t.Fatal(err)
			}
			got := h.pgadmin(p.Name)
			ingressErr := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: p.Name + "-pgadmin"}, &networkingv1.Ingress{})
			if tt.want == pgtoolboxv1alpha1.ReasonAsExpected {
				expectCondition(t, got, "ConfigurationValid", metav1.ConditionTrue, tt.want)
				if p.Spec.Exposure.Type == pgtoolboxv1alpha1.ExposureTypeIngress && ingressErr != nil {
					t.Fatalf("an allowed hostname is not published: %v", ingressErr)
				}
				return
			}
			expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, tt.want)
			expectCondition(t, got, "Stalled", metav1.ConditionTrue, tt.want)
			if !apierrors.IsNotFound(ingressErr) {
				t.Fatalf("the Ingress under a refused hostname was not withdrawn: %v", ingressErr)
			}
			// Nothing stale says pgAdmin is still reachable under the name.
			expectCondition(t, got, "ExposureReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
			if got.Status.URL != "" {
				t.Errorf("status.url = %q while nothing is published", got.Status.URL)
			}
			if c := condition(got, "ConfigurationValid"); !strings.Contains(c.Message, tt.message) {
				t.Errorf("ConfigurationValid message %q does not contain %q", c.Message, tt.message)
			}
			// The stalled loop is a no-op the next time: no status write.
			if err := h.reconcile(p.Name); err != nil {
				t.Fatal(err)
			}
			if again := h.pgadmin(p.Name); again.ResourceVersion != got.ResourceVersion {
				t.Errorf("a repeated stalled loop wrote status (resourceVersion %s -> %s)", got.ResourceVersion, again.ResourceVersion)
			}
			warnings := 0
			for _, e := range h.events() {
				if strings.HasPrefix(e, "Warning HostnameNotAllowed") {
					warnings++
				}
			}
			if warnings != 1 {
				t.Errorf("HostnameNotAllowed Warnings = %d, want 1", warnings)
			}
		})
	}
}
