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

package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
)

// droppedRequired is what the start-up check asks for of the kinds the
// report touches.
func droppedRequired() []managerrole.Permission {
	group := pgtoolboxv1alpha1.GroupVersion.Group
	permissions := make([]managerrole.Permission, 0, 8)
	permissions = append(permissions,
		managerrole.Permission{Group: "apps", Resource: "deployments", Verbs: []string{"list", "watch"}},
		managerrole.Permission{Group: "events.k8s.io", Resource: "events", Verbs: []string{"create", "patch"}})
	for _, resource := range []string{"pgadmins", "pgtoolboxconsoles", "evidenceagents"} {
		permissions = append(permissions,
			managerrole.Permission{Group: group, Resource: resource, Verbs: []string{"get", "list", "watch"}},
			managerrole.Permission{Group: group, Resource: resource + "/status", Verbs: []string{"get", "patch", "update"}})
	}
	return permissions
}

// droppedObjects is one PgAdmin, PgToolBoxConsole and EvidenceAgent in
// team-b, each Ready=True from before the namespace was dropped, and a
// PgAdmin in the watched team-a.
func droppedObjects() []client.Object {
	ready := []metav1.Condition{{
		Type: pgtoolboxv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonAsExpected.String(),
		Message: "every aspect is as expected", LastTransitionTime: metav1.Now(), ObservedGeneration: 1,
	}}
	pgadmin := &pgtoolboxv1alpha1.PgAdmin{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-b", Generation: 1}}
	pgadmin.Status.Conditions = ready
	console := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-b", Generation: 1}}
	console.Status.Conditions = ready
	agent := &pgtoolboxv1alpha1.EvidenceAgent{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-b", Generation: 1}}
	agent.Status.Conditions = ready
	watched := &pgtoolboxv1alpha1.PgAdmin{ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "team-a", Generation: 1}}
	watched.Status.Conditions = ready
	return []client.Object{pgadmin, console, agent, watched}
}

// droppedClient is a fake client over droppedObjects that counts the
// requests made and refuses those refuse names ("verb resource").
type droppedClient struct {
	client.WithWatch

	mu       sync.Mutex
	requests []string
}

func (c *droppedClient) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.requests...)
}

func newDroppedClient(t *testing.T, refuse ...string) *droppedClient {
	t.Helper()
	dc := &droppedClient{}
	record := func(request string) error {
		dc.mu.Lock()
		dc.requests = append(dc.requests, request)
		dc.mu.Unlock()
		for _, refused := range refuse {
			if refused == request {
				return apierrors.NewForbidden(schema.GroupResource{Resource: request}, "", errors.New("narrowed Role"))
			}
		}
		return nil
	}
	resourceOf := func(obj any) string {
		switch obj.(type) {
		case *pgtoolboxv1alpha1.PgAdmin, *pgtoolboxv1alpha1.PgAdminList:
			return "pgadmins"
		case *pgtoolboxv1alpha1.PgToolBoxConsole, *pgtoolboxv1alpha1.PgToolBoxConsoleList:
			return "pgtoolboxconsoles"
		case *pgtoolboxv1alpha1.EvidenceAgent, *pgtoolboxv1alpha1.EvidenceAgentList:
			return "evidenceagents"
		case *eventsv1.Event:
			return "events"
		}
		return "other"
	}
	dc.WithWatch = fake.NewClientBuilder().WithScheme(scheme).WithObjects(droppedObjects()...).
		WithStatusSubresource(&pgtoolboxv1alpha1.PgAdmin{}, &pgtoolboxv1alpha1.PgToolBoxConsole{}, &pgtoolboxv1alpha1.EvidenceAgent{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, inner client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				o := &client.ListOptions{}
				o.ApplyOptions(opts)
				if err := record("list " + resourceOf(list) + " " + o.Namespace); err != nil {
					return err
				}
				return inner.List(ctx, list, opts...)
			},
			Create: func(ctx context.Context, inner client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := record("create " + resourceOf(obj) + " " + obj.GetNamespace()); err != nil {
					return err
				}
				return inner.Create(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, inner client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if err := record("patch " + resourceOf(obj) + "/" + subResource + " " + obj.GetNamespace()); err != nil {
					return err
				}
				return inner.SubResource(subResource).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	return dc
}

// listOf is every object of list in namespace.
func listOf[L client.ObjectList](t *testing.T, c client.Reader, list L, namespace string) L {
	t.Helper()
	if err := c.List(context.Background(), list, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	return list
}

func readyOf(t *testing.T, c client.Reader) []*metav1.Condition {
	t.Helper()
	ready := make([]*metav1.Condition, 0, 3)
	for _, p := range listOf(t, c, &pgtoolboxv1alpha1.PgAdminList{}, "team-b").Items {
		ready = append(ready, meta.FindStatusCondition(p.Status.Conditions, pgtoolboxv1alpha1.ConditionReady))
	}
	for _, p := range listOf(t, c, &pgtoolboxv1alpha1.PgToolBoxConsoleList{}, "team-b").Items {
		ready = append(ready, meta.FindStatusCondition(p.Status.Conditions, pgtoolboxv1alpha1.ConditionReady))
	}
	for _, p := range listOf(t, c, &pgtoolboxv1alpha1.EvidenceAgentList{}, "team-b").Items {
		ready = append(ready, meta.FindStatusCondition(p.Status.Conditions, pgtoolboxv1alpha1.ConditionReady))
	}
	return ready
}

// TestReportDroppedNamespaces: round 15, DBA P2-2. A namespace left out
// for a missing permission keeps a status nobody updates (possibly
// Ready=True); where the review shows the operator may still list the kind
// and patch its status, Ready turns Unknown (NamespaceNotWatched, naming
// only that namespace and what is missing), with one Warning Event on the
// transition; where it may not, nothing is attempted. A namespace whose
// permissions are unknown gets no request at all, and a watched one is
// never touched.
func TestReportDroppedNamespaces(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC) }
	missing := []managerrole.Permission{{Group: "apps", Resource: "deployments", Verbs: []string{"list", "watch"}}}
	narrowed := droppedNamespace{
		Namespace: "team-b", Cause: dropPermissions, Reason: "not allowed to list,watch deployments.apps in team-b",
		required: droppedRequired(), missing: missing,
	}

	t.Run("allowed: Ready Unknown and one Event per transition", func(t *testing.T) {
		c := newDroppedClient(t)
		reportDroppedNamespaces(t.Context(), c, []droppedNamespace{narrowed}, now)
		for _, ready := range readyOf(t, c) {
			if ready == nil || ready.Status != metav1.ConditionUnknown || ready.Reason != pgtoolboxv1alpha1.ReasonNamespaceNotWatched.String() {
				t.Fatalf("Ready = %+v, want Unknown NamespaceNotWatched", ready)
			}
			if !strings.Contains(ready.Message, "team-b") || !strings.Contains(ready.Message, "list,watch deployments.apps") || strings.Contains(ready.Message, "team-a") {
				t.Errorf("Ready message = %q, want team-b and the missing permission named, no other namespace", ready.Message)
			}
		}
		events := listOf(t, c, &eventsv1.EventList{}, "team-b").Items
		if len(events) != 3 {
			t.Fatalf("%d Events, want one per object (3)", len(events))
		}
		for _, e := range events {
			if e.Type != "Warning" || e.Reason != pgtoolboxv1alpha1.ReasonNamespaceNotWatched.String() || e.Regarding.Namespace != "team-b" ||
				e.ReportingController == "" || e.ReportingInstance == "" || e.Action == "" || e.EventTime.IsZero() {
				t.Errorf("Event = %+v, want a complete Warning NamespaceNotWatched regarding a team-b object", e)
			}
		}
		// The watched namespace is never touched.
		for _, request := range c.seen() {
			if strings.HasSuffix(request, " team-a") {
				t.Errorf("request in the watched namespace: %s", request)
			}
		}
		watched := listOf(t, c.WithWatch, &pgtoolboxv1alpha1.PgAdminList{}, "team-a").Items[0]
		if ready := meta.FindStatusCondition(watched.Status.Conditions, pgtoolboxv1alpha1.ConditionReady); ready.Status != metav1.ConditionTrue {
			t.Errorf("a watched namespace's PgAdmin was marked: %+v", ready)
		}

		// A second start: Ready already says so, nothing is written and no
		// Event repeats.
		before := len(c.seen())
		reportDroppedNamespaces(t.Context(), c, []droppedNamespace{narrowed}, now)
		for _, request := range c.seen()[before:] {
			if !strings.HasPrefix(request, "list ") {
				t.Errorf("second start wrote: %s", request)
			}
		}
		if events := listOf(t, c, &eventsv1.EventList{}, "team-b").Items; len(events) != 3 {
			t.Errorf("%d Events after a second start, want still 3", len(events))
		}
	})

	t.Run("status refused by the review: Events only, every start", func(t *testing.T) {
		noStatus := narrowed
		noStatus.missing = append([]managerrole.Permission{}, missing...)
		for _, resource := range []string{"pgadmins", "pgtoolboxconsoles", "evidenceagents"} {
			noStatus.missing = append(noStatus.missing, managerrole.Permission{Group: pgtoolboxv1alpha1.GroupVersion.Group, Resource: resource + "/status", Verbs: []string{"patch"}})
		}
		c := newDroppedClient(t)
		reportDroppedNamespaces(t.Context(), c, []droppedNamespace{noStatus}, now)
		for _, request := range c.seen() {
			if strings.HasPrefix(request, "patch ") {
				t.Errorf("status patched though the review refused it: %s", request)
			}
		}
		for _, ready := range readyOf(t, c) {
			if ready.Status != metav1.ConditionTrue {
				t.Errorf("Ready = %+v, want untouched", ready)
			}
		}
		if events := listOf(t, c, &eventsv1.EventList{}, "team-b").Items; len(events) != 3 {
			t.Errorf("%d Events, want 3", len(events))
		}
	})

	t.Run("list or writes refused by the review: nothing attempted", func(t *testing.T) {
		nothing := narrowed
		nothing.missing = append([]managerrole.Permission{}, missing...)
		nothing.missing = append(nothing.missing, managerrole.Permission{Group: "events.k8s.io", Resource: "events", Verbs: []string{"create"}})
		for _, resource := range []string{"pgadmins", "pgtoolboxconsoles", "evidenceagents"} {
			nothing.missing = append(nothing.missing, managerrole.Permission{Group: pgtoolboxv1alpha1.GroupVersion.Group, Resource: resource + "/status", Verbs: []string{"patch"}})
		}
		unlistable := narrowed
		unlistable.missing = []managerrole.Permission{{Group: pgtoolboxv1alpha1.GroupVersion.Group, Resource: "pgadmins", Verbs: []string{"list"}}}
		for name, d := range map[string]droppedNamespace{"no events, no status": nothing} {
			c := newDroppedClient(t)
			reportDroppedNamespaces(t.Context(), c, []droppedNamespace{d}, now)
			if requests := c.seen(); len(requests) != 0 {
				t.Errorf("%s: requests %q, want none", name, requests)
			}
		}
		c := newDroppedClient(t)
		reportDroppedNamespaces(t.Context(), c, []droppedNamespace{unlistable}, now)
		for _, request := range c.seen() {
			if strings.Contains(request, "pgadmins") {
				t.Errorf("PgAdmins listed or written though the review refused list: %s", request)
			}
		}
	})

	t.Run("permissions unknown: no request", func(t *testing.T) {
		for _, d := range []droppedNamespace{
			{Namespace: "team-b", Cause: dropClustersUnlistable, Reason: "forbidden"},
			{Namespace: "team-b", Cause: dropReviewFailed, Reason: "no answer"},
		} {
			c := newDroppedClient(t)
			reportDroppedNamespaces(t.Context(), c, []droppedNamespace{d}, now)
			if requests := c.seen(); len(requests) != 0 {
				t.Errorf("%s: requests %q, want none", d.Cause, requests)
			}
		}
	})

	t.Run("writes refused at the API: logged, not retried", func(t *testing.T) {
		c := newDroppedClient(t, "patch pgadmins/status team-b", "create events team-b")
		reportDroppedNamespaces(t.Context(), c, []droppedNamespace{narrowed}, now)
		patches := 0
		for _, request := range c.seen() {
			if request == "patch pgadmins/status team-b" {
				patches++
			}
		}
		if patches != 1 {
			t.Errorf("%d status patches of the PgAdmin, want 1 (no retry)", patches)
		}
	})
}

// raceList is a droppedAPI whose first List runs another replica's report
// once it has listed: every object it then patches has changed since.
type raceList struct {
	droppedAPI
	once  sync.Once
	other func()
}

func (r *raceList) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	err := r.droppedAPI.List(ctx, list, opts...)
	r.once.Do(r.other)
	return err
}

// TestReportDroppedNamespacesTwoReplicas: round 16, OPERATOR P3-1. Two
// replicas reporting at once: the second's patch from its stale List is a
// Conflict; it reads the object again, finds Ready already says so, and
// emits no second Event. Where it may not read the object again, the
// Conflict counts as already said.
func TestReportDroppedNamespacesTwoReplicas(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC) }
	narrowed := droppedNamespace{
		Namespace: "team-b", Cause: dropPermissions, Reason: "not allowed to list,watch deployments.apps in team-b",
		required: droppedRequired(), missing: []managerrole.Permission{{Group: "apps", Resource: "deployments", Verbs: []string{"list", "watch"}}},
	}
	noGet := narrowed
	noGet.missing = append(slices.Clone(narrowed.missing), managerrole.Permission{Group: pgtoolboxv1alpha1.GroupVersion.Group, Resource: "pgadmins", Verbs: []string{"get"}})
	for name, d := range map[string]droppedNamespace{"read again": narrowed, "no get": noGet} {
		t.Run(name, func(t *testing.T) {
			c := newDroppedClient(t)
			other := &raceList{droppedAPI: c}
			other.other = func() { reportDroppedNamespaces(t.Context(), c, []droppedNamespace{d}, now) }
			reportDroppedNamespaces(t.Context(), other, []droppedNamespace{d}, now)
			if events := listOf(t, c, &eventsv1.EventList{}, "team-b").Items; len(events) != 3 {
				t.Errorf("%d Events for one transition of 3 objects, want 3", len(events))
			}
			for _, ready := range readyOf(t, c) {
				if ready.Reason != pgtoolboxv1alpha1.ReasonNamespaceNotWatched.String() {
					t.Errorf("Ready = %+v, want NamespaceNotWatched", ready)
				}
			}
		})
	}
}

// TestDroppedReportNeedsLeaderElection: round 16, K8S P3-1. The report is
// a Runnable the manager starts on the elected leader only.
func TestDroppedReportNeedsLeaderElection(t *testing.T) {
	var runnable manager.Runnable = droppedReport{}
	elected, ok := runnable.(manager.LeaderElectionRunnable)
	if !ok || !elected.NeedLeaderElection() {
		t.Fatal("the dropped-namespace report would run on every replica")
	}
	c := newDroppedClient(t)
	narrowed := droppedNamespace{Namespace: "team-b", Cause: dropPermissions, Reason: "r", required: droppedRequired()}
	if err := (droppedReport{api: c, dropped: []droppedNamespace{narrowed}, now: time.Now}).Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if events := listOf(t, c, &eventsv1.EventList{}, "team-b").Items; len(events) != 3 {
		t.Errorf("%d Events, want 3", len(events))
	}
}

// TestCountDropped: every cause is exported, 0 when none.
func TestCountDropped(t *testing.T) {
	got := countDropped([]droppedNamespace{{Cause: dropPermissions}, {Cause: dropPermissions}, {Cause: dropReviewFailed}})
	want := map[dropCause]int{dropPermissions: 2, dropReviewFailed: 1, dropClustersUnlistable: 0}
	if len(got) != len(want) {
		t.Fatalf("counts = %v, want %v", got, want)
	}
	for cause, n := range want {
		if got[cause] != n {
			t.Errorf("%s = %d, want %d", cause, got[cause], n)
		}
	}
}
