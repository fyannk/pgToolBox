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

package accessrequest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, cnpgv1.AddToScheme, pgtoolboxv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

type fakeSuite struct {
	client     client.Client
	recorder   *events.FakeRecorder
	reconciler *Reconciler
	writes     *int
}

func newFakeSuite(t *testing.T, now time.Time, objects ...client.Object) *fakeSuite {
	t.Helper()
	writes := 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&pgtoolboxv1alpha1.PgAccessRequest{}).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				writes++
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				writes++
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	recorder := events.NewFakeRecorder(32)
	return &fakeSuite{client: c, recorder: recorder, writes: &writes, reconciler: &Reconciler{Client: c, Recorder: recorder, Now: func() time.Time { return now }}}
}

func (s *fakeSuite) reconcile(t *testing.T, r *pgtoolboxv1alpha1.PgAccessRequest) (ctrl.Result, *pgtoolboxv1alpha1.PgAccessRequest) {
	t.Helper()
	result, err := s.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(r)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &pgtoolboxv1alpha1.PgAccessRequest{}
	if err := s.client.Get(context.Background(), client.ObjectKeyFromObject(r), got); err != nil {
		t.Fatal(err)
	}
	return result, got
}

func (s *fakeSuite) events() []string {
	var out []string
	for {
		select {
		case e := <-s.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func condition(t *testing.T, r *pgtoolboxv1alpha1.PgAccessRequest, conditionType string) metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(r.Status.Conditions, conditionType)
	if c == nil {
		t.Fatalf("no %s condition in %+v", conditionType, r.Status.Conditions)
	}
	return *c
}

func TestReconcileNewRequestPublishesPendingOnce(t *testing.T) {
	r := request("")
	s := newFakeSuite(t, testNow, r)
	_, got := s.reconcile(t, r)
	if got.Status.Phase != pgtoolboxv1alpha1.AccessPhasePending {
		t.Fatalf("phase = %q", got.Status.Phase)
	}
	if c := condition(t, got, pgtoolboxv1alpha1.ConditionReady); c.Status != metav1.ConditionTrue || c.ObservedGeneration != 1 {
		t.Fatalf("Ready = %+v", c)
	}
	if c := condition(t, got, pgtoolboxv1alpha1.ConditionConfigurationValid); !strings.Contains(c.Message, "every Cluster of namespace team-a") {
		t.Fatalf("ConfigurationValid does not say the grant is namespace-wide: %q", c.Message)
	}
	writes := *s.writes
	s.reconcile(t, r)
	if *s.writes != writes {
		t.Fatalf("a no-op reconcile wrote %d times", *s.writes-writes)
	}
}

func TestReconcileExpiryDeletesTheGrantAndSaysSo(t *testing.T) {
	r := request(pgtoolboxv1alpha1.AccessPhaseApproved, expiresAt(testNow.Add(time.Hour)))
	binding := granted(r, testNow.Add(-time.Hour))
	s := newFakeSuite(t, testNow, r, &binding)
	result, got := s.reconcile(t, r)
	if result.RequeueAfter != time.Hour {
		t.Fatalf("requeue after %v, want the expiry in 1h", result.RequeueAfter)
	}
	if c := condition(t, got, pgtoolboxv1alpha1.ConditionRoleBindingSynced); c.Reason != pgtoolboxv1alpha1.ReasonAccessGranted.String() {
		t.Fatalf("RoleBindingSynced = %+v", c)
	}
	if got.Status.GrantedScope != pgtoolboxv1alpha1.AccessGrantScopeNamespace {
		t.Fatalf("grantedScope = %q", got.Status.GrantedScope)
	}
	s.events()

	s.reconciler.Now = func() time.Time { return testNow.Add(time.Hour) }
	_, got = s.reconcile(t, got)
	if got.Status.Phase != pgtoolboxv1alpha1.AccessPhaseExpired || got.Status.DecidedBy != "oidc:bob@corp.example" {
		t.Fatalf("status after expiry = %+v", got.Status)
	}
	err := s.client.Get(context.Background(), client.ObjectKeyFromObject(&binding), &rbacv1.RoleBinding{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("RoleBinding still there: %v", err)
	}
	evs := strings.Join(s.events(), "\n")
	for _, want := range []string{"RoleBindingDeleted", "AccessExpired"} {
		if !strings.Contains(evs, want) {
			t.Fatalf("events %q lack %s", evs, want)
		}
	}
	s.reconcile(t, got)
	if again := s.events(); len(again) != 0 {
		t.Fatalf("a settled request emitted %v", again)
	}
}

// A delete the API server applied but answered with an error (an etcd
// timeout): the live read settles it, and the deletion's Event is not lost
// to the next loop, which would find nothing left to delete.
func TestReconcileDeleteAppliedDespiteAnErrorStillSaysSo(t *testing.T) {
	r := request(pgtoolboxv1alpha1.AccessPhaseApproved, expiresAt(testNow))
	binding := granted(r, testNow.Add(-time.Hour))
	timeout := apierrors.NewTimeoutError("etcdserver: request timed out", 0)
	for _, live := range []bool{true, false} {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithStatusSubresource(&pgtoolboxv1alpha1.PgAccessRequest{}).
			WithObjects(r.DeepCopy(), binding.DeepCopy()).
			WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if err := c.Delete(ctx, obj, opts...); err != nil {
						return err
					}
					return timeout
				},
			}).Build()
		recorder := events.NewFakeRecorder(32)
		reconciler := &Reconciler{Client: c, Recorder: recorder, Now: func() time.Time { return testNow }}
		if live {
			reconciler.APIReader = c
		}
		_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(r)})
		s := &fakeSuite{recorder: recorder}
		evs := strings.Join(s.events(), "\n")
		if live && (err != nil || !strings.Contains(evs, "RoleBindingDeleted")) {
			t.Fatalf("with a live read: err = %v, events %q", err, evs)
		}
		if !live && err == nil {
			t.Fatal("without a live read the timeout must be retried")
		}
	}
}

func TestReconcileNeverTouchesABindingItDoesNotControl(t *testing.T) {
	r := request(pgtoolboxv1alpha1.AccessPhaseRevoked, func(r *pgtoolboxv1alpha1.PgAccessRequest) {
		r.Status.RevokedBy, r.Status.RevokedAt = "oidc:bob@corp.example", &metav1.Time{Time: testNow}
	})
	foreign := granted(r, testNow.Add(-time.Hour))
	foreign.Name = "someone-elses"
	foreign.OwnerReferences = nil
	s := newFakeSuite(t, testNow, r, &foreign)
	s.reconcile(t, r)
	if err := s.client.Get(context.Background(), client.ObjectKeyFromObject(&foreign), &rbacv1.RoleBinding{}); err != nil {
		t.Fatalf("a RoleBinding carrying the label but not controlled by the request was touched: %v", err)
	}
}

func TestReconcileNamedClusterMissingIsReportedNotRefused(t *testing.T) {
	r := request(pgtoolboxv1alpha1.AccessPhaseApproved, func(r *pgtoolboxv1alpha1.PgAccessRequest) { r.Spec.Cluster = "orders" })
	binding := granted(r, testNow.Add(-time.Hour))
	s := newFakeSuite(t, testNow, r, &binding)
	_, got := s.reconcile(t, r)
	if c := condition(t, got, pgtoolboxv1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != pgtoolboxv1alpha1.ReasonClusterNotFound.String() {
		t.Fatalf("Ready = %+v", c)
	}
	if c := condition(t, got, pgtoolboxv1alpha1.ConditionRoleBindingSynced); c.Status != metav1.ConditionTrue {
		t.Fatalf("the grant is still observed: %+v", c)
	}

	cluster := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders"}}
	if err := s.client.Create(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	_, got = s.reconcile(t, got)
	if c := condition(t, got, pgtoolboxv1alpha1.ConditionConfigurationValid); c.Status != metav1.ConditionTrue ||
		!strings.Contains(c.Message, "cannot be narrowed to one Cluster") {
		t.Fatalf("ConfigurationValid = %+v", c)
	}
}

func TestReconcilePausedObservesNothing(t *testing.T) {
	r := request(pgtoolboxv1alpha1.AccessPhaseApproved, expiresAt(testNow.Add(-time.Hour)), func(r *pgtoolboxv1alpha1.PgAccessRequest) {
		r.Annotations = map[string]string{pgtoolboxv1alpha1.AnnotationReconcile: pgtoolboxv1alpha1.ReconcileSkip}
	})
	binding := granted(r, testNow.Add(-2*time.Hour))
	s := newFakeSuite(t, testNow, r, &binding)
	_, got := s.reconcile(t, r)
	if got.Status.Phase != pgtoolboxv1alpha1.AccessPhaseApproved {
		t.Fatalf("a paused request moved to %s", got.Status.Phase)
	}
	if c := condition(t, got, pgtoolboxv1alpha1.ConditionReady); c.Reason != pgtoolboxv1alpha1.ReasonReconciliationPaused.String() {
		t.Fatalf("Ready = %+v", c)
	}
}

func TestReconcileGoneRequestIsNoError(t *testing.T) {
	s := newFakeSuite(t, testNow)
	if _, err := s.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "team-a", Name: "gone"}}); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileErrorStillPublishesStatus: a failing read ends the loop with
// an error and Ready=False (ReconcileError) in the same write.
func TestReconcileErrorStillPublishesStatus(t *testing.T) {
	r := request(pgtoolboxv1alpha1.AccessPhaseApproved)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&pgtoolboxv1alpha1.PgAccessRequest{}).WithObjects(r).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*rbacv1.RoleBindingList); ok {
				return apierrors.NewServiceUnavailable("etcd is away")
			}
			return c.List(ctx, list, opts...)
		}}).Build()
	reconciler := &Reconciler{Client: c, Recorder: events.NewFakeRecorder(8), Now: func() time.Time { return testNow }}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(r)}); err == nil {
		t.Fatal("a failed read returned no error")
	}
	got := &pgtoolboxv1alpha1.PgAccessRequest{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(r), got); err != nil {
		t.Fatal(err)
	}
	if ready := condition(t, got, pgtoolboxv1alpha1.ConditionReady); ready.Status != metav1.ConditionFalse || ready.Reason != pgtoolboxv1alpha1.ReasonReconcileError.String() {
		t.Fatalf("Ready = %+v", ready)
	}
}

// TestReconcileTimedOutStillPublishesStatus: ReconciliationTimeout cancels
// the loop's context mid-loop; the status write, past that context, still
// lands (a client honouring its context refuses calls on a done one).
func TestReconcileTimedOutStillPublishesStatus(t *testing.T) {
	r := request(pgtoolboxv1alpha1.AccessPhaseApproved)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&pgtoolboxv1alpha1.PgAccessRequest{}).WithObjects(r).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*rbacv1.RoleBindingList); ok {
					cancel()
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.List(ctx, list, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	reconciler := &Reconciler{Client: c, Recorder: events.NewFakeRecorder(8), Now: func() time.Time { return testNow }}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(r)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("the cancelled loop returned %v", err)
	}
	got := &pgtoolboxv1alpha1.PgAccessRequest{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(r), got); err != nil {
		t.Fatal(err)
	}
	if ready := condition(t, got, pgtoolboxv1alpha1.ConditionReady); ready.Status != metav1.ConditionFalse || ready.Reason != pgtoolboxv1alpha1.ReasonReconcileError.String() {
		t.Fatalf("Ready = %+v", ready)
	}
}
