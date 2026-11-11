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

package exposure

import (
	"context"
	"errors"
	"strings"
	"testing"

	routev1 "github.com/openshift/api/route/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestLeftover: what an exposure object published before its kind was
// refused in the namespace leaves in the status, and whether it is deleted.
func TestLeftover(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := routev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := &pgtoolboxv1alpha1.PgAdmin{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "team-b", UID: "owner-uid"}}
	route := func(controller apitypes.UID) *routev1.Route {
		r := &routev1.Route{ObjectMeta: metav1.ObjectMeta{Name: "team-pgadmin", Namespace: "team-b", UID: "route-uid"}}
		if controller != "" {
			r.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: pgtoolboxv1alpha1.GroupVersion.String(), Kind: "PgAdmin", Name: "team", UID: controller, Controller: new(true),
			}}
		}
		return r
	}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: routev1.GroupName, Resource: "routes"}, "team-pgadmin", errors.New("narrowed Role"))
	noKind := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: routev1.GroupName, Kind: "Route"}, SearchedVersions: []string{"v1"}}
	tests := []struct {
		name           string
		objects        []client.Object
		getErr         error
		deleteErr      error
		remove         bool
		notPublished   bool
		want           string
		wantUnverified bool
		wantErr        bool
		wantDeleted    bool
	}{
		{name: "nothing there", want: ""},
		{name: "owned, removed", objects: []client.Object{route("owner-uid")}, remove: true, want: "", wantDeleted: true},
		{
			name: "owned, kept (the requested kind)", objects: []client.Object{route("owner-uid")},
			want: "Route team-pgadmin published before remains, no longer managed: the operator may not list Routes in team-b; delete it by hand",
		},
		{name: "someone else's, never touched", objects: []client.Object{route("other-uid")}, remove: true, want: ""},
		{name: "unowned, never touched", objects: []client.Object{route("")}, remove: true, want: ""},
		{
			name: "unreadable", getErr: forbidden, remove: true,
			want:           "the operator may not read Routes in team-b: a Route team-pgadmin published before may remain; delete it by hand",
			wantUnverified: true,
		},
		// Round 15, k8s P3-2: nothing in the owner's history says it ever
		// published a Route; an unreadable one is not reported.
		{name: "unreadable, never published", getErr: forbidden, remove: true, notPublished: true, want: ""},
		// Round 15, operator note: the Route CRD removed while the operator
		// runs. Nothing of it can remain; the loop does not fail.
		{name: "kind no longer served", getErr: noKind, remove: true, want: ""},
		{name: "kind no longer served at delete", objects: []client.Object{route("owner-uid")}, deleteErr: noKind, remove: true, want: ""},
		{
			name: "deletion refused", objects: []client.Object{route("owner-uid")}, deleteErr: forbidden, remove: true,
			want: "the operator may not delete Routes in team-b: Route team-pgadmin published before remains; delete it by hand",
		},
		{name: "another read error", getErr: errors.New("connection refused"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deleted := false
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if tt.getErr != nil {
						return tt.getErr
					}
					return c.Get(ctx, key, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if tt.deleteErr != nil {
						return tt.deleteErr
					}
					o := &client.DeleteOptions{}
					o.ApplyOptions(opts)
					if o.Preconditions == nil || o.Preconditions.UID == nil || *o.Preconditions.UID != "route-uid" {
						t.Errorf("delete without the object's UID precondition: %+v", o.Preconditions)
					}
					deleted = true
					return c.Delete(ctx, obj, opts...)
				},
			}).Build()
			remnant, err := Leftover(context.Background(), c, c, owner, &routev1.Route{}, "Route", "team-pgadmin", tt.remove, !tt.notPublished)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %t", err, tt.wantErr)
			}
			got := remnant.Sentence
			if got != tt.want || remnant.Unverified != tt.wantUnverified {
				t.Errorf("leftover = %q (unverified %t), want %q (unverified %t)", got, remnant.Unverified, tt.want, tt.wantUnverified)
			}
			if deleted != tt.wantDeleted {
				t.Errorf("deleted = %t, want %t", deleted, tt.wantDeleted)
			}
			if strings.Contains(got, "team-a") {
				t.Errorf("the sentence names another namespace: %q", got)
			}
		})
	}
	if got := WithLeftovers("in-cluster Service only", nil); got != "in-cluster Service only" {
		t.Errorf("WithLeftovers without leftovers = %q", got)
	}
	if got := WithLeftovers("a", []Remnant{{Sentence: "b"}, {}, {Sentence: "c", Unverified: true}}); got != "a; b; c" {
		t.Errorf("WithLeftovers = %q", got)
	}
}

// TestMayHavePublished: what in an owner's history says it may have
// published a Route or an HTTPRoute, so that a Leftover it may not read is
// reported only then.
func TestMayHavePublished(t *testing.T) {
	ready := func(message string) []metav1.Condition {
		return []metav1.Condition{{Type: pgtoolboxv1alpha1.ConditionExposureReady, Message: message}}
	}
	leftover := "in-cluster Service only (exposure type clusterIP); the operator may not read Routes in team-b: a Route orders published before may remain; delete it by hand"
	for _, tt := range []struct {
		name       string
		requested  bool
		conditions []metav1.Condition
		kind       string
		want       bool
	}{
		{"requested now", true, nil, "Route", true},
		{"no history", false, nil, "Route", false},
		{"only ever clusterIP", false, ready("in-cluster Service only (exposure type clusterIP)"), "Route", false},
		{"an Ingress", false, ready("served at orders.example.com"), "Route", false},
		{"admitted by a router", false, ready("admitted by router default"), "Route", true},
		{"router refused it", false, ready("router default: HostAlreadyClaimed: taken"), "Route", true},
		{"Route not observed yet", false, ready("the Route is not observed yet"), "Route", true},
		{"its own leftover sentence (sticky)", false, ready(leftover), "Route", true},
		{"an HTTPRoute is no Route", false, ready("accepted by Gateway public; no Gateway has reported on the HTTPRoute yet"), "Route", false},
		{"a Route is no HTTPRoute", false, ready("admitted by router default"), "HTTPRoute", false},
		{"accepted by a Gateway", false, ready("accepted by Gateway public"), "HTTPRoute", true},
		{"HTTPRoute not observed yet", false, ready("the HTTPRoute is not observed yet"), "HTTPRoute", true},
		{"in another condition", false, []metav1.Condition{{Type: "ExposureConfined", Message: leftover}}, "Route", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := MayHavePublished(tt.requested, tt.conditions, tt.kind); got != tt.want {
				t.Fatalf("MayHavePublished = %t, want %t", got, tt.want)
			}
		})
	}
	if !AllUnverified([]Remnant{{Unverified: true}, {Unverified: true}}) || AllUnverified([]Remnant{{Unverified: true}, {}}) || AllUnverified(nil) {
		t.Error("AllUnverified: true only for a non-empty list of unverified remnants")
	}
}
