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
	"errors"
	"strings"
	"testing"

	routev1 "github.com/openshift/api/route/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// TestRouteAPIDecidedPerNamespace: the Route API refused in one watched
// namespace while others allow it is unavailable to a console of that
// namespace only (ExposureReady names the namespace); a console of another
// namespace publishes its Route. Each namespace's RBAC decides there alone.
func TestRouteAPIDecidedPerNamespace(t *testing.T) {
	r := &Reconciler{Features: Features{Routes: discovery.Result{
		GVK: routev1.GroupVersion.WithKind("Route"), State: discovery.Available, RefusedIn: []string{"team-b"},
	}}}
	consoleIn := func(namespace string) *pgtoolboxv1alpha1.PgToolBoxConsole {
		c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: namespace, Generation: 1}}
		c.Spec.Exposure.Type = pgtoolboxv1alpha1.ExposureTypeRoute
		return c
	}
	if !r.exposureUsable(consoleIn("team-a")) {
		t.Error("a Route is not usable in team-a, which allows the Route API")
	}
	refused := consoleIn("team-b")
	if r.exposureUsable(refused) {
		t.Error("a Route is usable in team-b, which refuses the Route API")
	}
	r.observeExposure(context.Background(), refused, nil)
	var ready *metav1.Condition
	for i := range refused.Status.Conditions {
		if refused.Status.Conditions[i].Type == pgtoolboxv1alpha1.ConditionExposureReady {
			ready = &refused.Status.Conditions[i]
		}
	}
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != string(pgtoolboxv1alpha1.ReasonRouteAPIUnavailable) ||
		!strings.Contains(ready.Message, "in team-b") {
		t.Fatalf("ExposureReady = %+v, want False RouteAPIUnavailable naming team-b", ready)
	}
}

// TestStaleRouteAfterRefusal: round 14, operator item 4. team-b's Role
// started refusing Routes after the console published one, then the console
// switched to clusterIP. The cache holds no Route there, so the operator
// reads it past the cache: owned, it is deleted (UID precondition); if the
// read itself is refused and the console's history says it published a
// Route, ExposureReady says one may remain, exposurePublished counts it as
// unverified and ExposureConfined is Unknown (neither confined nor known to
// be open), with the same text at every loop. With nothing in its history
// (a console that was only ever clusterIP), a refused read reports nothing:
// round 15, k8s P3-2.
func TestStaleRouteAfterRefusal(t *testing.T) {
	scheme := cnpgScheme(t)
	if err := routev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	features := Features{Routes: discovery.Result{GVK: routev1.GroupVersion.WithKind("Route"), State: discovery.Available, RefusedIn: []string{"team-b"}}}
	newConsole := func() *pgtoolboxv1alpha1.PgToolBoxConsole {
		c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-b", UID: "console-uid", Generation: 2}}
		c.Spec.Exposure.Type = pgtoolboxv1alpha1.ExposureTypeClusterIP
		return c
	}
	staleRoute := func(c *pgtoolboxv1alpha1.PgToolBoxConsole) *routev1.Route {
		return owned(c, &routev1.Route{ObjectMeta: metav1.ObjectMeta{Name: c.WorkloadName(), Namespace: c.Namespace, UID: "route-uid"}})
	}

	t.Run("an owned stale Route is deleted", func(t *testing.T) {
		c := newConsole()
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c, staleRoute(c)).Build()
		r := &Reconciler{Client: cl, APIReader: cl, Features: features}
		exposed, leftovers, err := r.applyExposure(context.Background(), c)
		if err != nil || exposed || leftovers != nil {
			t.Fatalf("applyExposure = %t, %+v, %v; want nothing exposed, nothing left", exposed, leftovers, err)
		}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "team-b", Name: c.WorkloadName()}, &routev1.Route{}); !apierrors.IsNotFound(err) {
			t.Fatalf("the stale Route is still there: %v", err)
		}
		if published, _, err := r.exposurePublished(context.Background(), c); err != nil || published {
			t.Fatalf("exposurePublished = %t, %v once the Route is gone", published, err)
		}
	})

	refusing := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isRoute := obj.(*routev1.Route); isRoute {
				return apierrors.NewForbidden(schema.GroupResource{Group: routev1.GroupName, Resource: "routes"}, key.Name, errors.New("narrowed Role"))
			}
			return inner.Get(ctx, key, obj, opts...)
		},
	}).Build()

	t.Run("an unreadable Route never published is not reported", func(t *testing.T) {
		c := newConsole()
		setCondition(c, pgtoolboxv1alpha1.ConditionExposureReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonNotRequested, "in-cluster Service only (exposure type clusterIP)")
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c).Build()
		r := &Reconciler{Client: cl, APIReader: refusing, Features: features}
		exposed, leftovers, err := r.applyExposure(context.Background(), c)
		if err != nil || exposed || leftovers != nil {
			t.Fatalf("applyExposure = %t, %+v, %v; want nothing reported for a Route never published", exposed, leftovers, err)
		}
		setExposureConfined(c, exposed, confinement{})
		setLeftoverConfinement(c, exposed, leftovers)
		if confined := meta.FindStatusCondition(c.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureConfined); confined != nil {
			t.Fatalf("ExposureConfined = %+v on a clusterIP console that never published a Route", confined)
		}
		if published, _, err := r.exposurePublished(context.Background(), c); err != nil || published {
			t.Fatalf("exposurePublished = %t, %v; nothing was ever published", published, err)
		}
	})

	t.Run("an unreadable Route may remain, and the status says so", func(t *testing.T) {
		c := newConsole()
		// The loop before the refusal observed the Route this console
		// published then.
		setCondition(c, pgtoolboxv1alpha1.ConditionExposureReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "admitted by router default")
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c).Build()
		r := &Reconciler{Client: cl, APIReader: refusing, Features: features}
		want := "the operator may not read Routes in team-b: a Route " + c.WorkloadName() + " published before may remain; delete it by hand"
		messages := make([]string, 0, 2)
		for range 2 {
			_, leftovers, err := r.applyExposure(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if len(leftovers) != 1 || leftovers[0].Sentence != want || !leftovers[0].Unverified {
				t.Fatalf("leftovers = %+v, want %q, unverified", leftovers, want)
			}
			setLeftoverConfinement(c, false, leftovers)
			confined := meta.FindStatusCondition(c.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureConfined)
			if confined == nil || confined.Status != metav1.ConditionUnknown || confined.Reason != pgtoolboxv1alpha1.ReasonExposureOpen.String() {
				t.Fatalf("ExposureConfined = %+v, want Unknown ExposureOpen: the Route may be gone", confined)
			}
			r.observeExposure(context.Background(), c, leftovers)
			ready := meta.FindStatusCondition(c.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureReady)
			if ready == nil || !strings.Contains(ready.Message, want) || ready.Reason != pgtoolboxv1alpha1.ReasonNotRequested.String() {
				t.Fatalf("ExposureReady = %+v, want NotRequested naming the Route that may remain", ready)
			}
			messages = append(messages, ready.Message)
		}
		if messages[0] != messages[1] {
			t.Errorf("the message changed between loops: %q, %q", messages[0], messages[1])
		}
		if published, unverified, err := r.exposurePublished(context.Background(), c); err != nil || !published || !unverified {
			t.Fatalf("exposurePublished = %t (unverified %t), %v; an unreadable Route may still publish the console", published, unverified, err)
		}
		leftovers, err := r.withdrawExposure(context.Background(), c)
		if err != nil || len(leftovers) != 1 || leftovers[0].Sentence != want {
			t.Fatalf("withdrawExposure = %+v, %v; want the Route that may remain named", leftovers, err)
		}
	})

	t.Run("a Route found and kept says ExposureConfined False", func(t *testing.T) {
		c := newConsole()
		c.Spec.Exposure.Type = pgtoolboxv1alpha1.ExposureTypeRoute
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c, staleRoute(c)).Build()
		r := &Reconciler{Client: cl, APIReader: cl, Features: features}
		exposed, leftovers, err := r.applyExposure(context.Background(), c)
		if err != nil || exposed || len(leftovers) != 1 || leftovers[0].Unverified {
			t.Fatalf("applyExposure = %t, %+v, %v; want the requested Route found and kept", exposed, leftovers, err)
		}
		setLeftoverConfinement(c, exposed, leftovers)
		confined := meta.FindStatusCondition(c.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureConfined)
		if confined == nil || confined.Status != metav1.ConditionFalse || confined.Reason != pgtoolboxv1alpha1.ReasonExposureOpen.String() {
			t.Fatalf("ExposureConfined = %+v, want False ExposureOpen: the Route serves", confined)
		}
	})

	t.Run("a namespace that allows Routes reads none past the cache", func(t *testing.T) {
		c := newConsole()
		c.Namespace = "team-a"
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c).Build()
		reads := 0
		counting := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++
				return inner.Get(ctx, key, obj, opts...)
			},
		}).Build()
		r := &Reconciler{Client: cl, APIReader: counting, Features: features}
		if _, leftovers, err := r.applyExposure(context.Background(), c); err != nil || leftovers != nil || reads != 0 {
			t.Fatalf("applyExposure = %+v, %v with %d uncached reads; want none", leftovers, err, reads)
		}
	})
}
