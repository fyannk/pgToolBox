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
	"slices"
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
		wantGone       bool
	}{
		{name: "nothing there", want: "", wantGone: true},
		{name: "owned, removed", objects: []client.Object{route("owner-uid")}, remove: true, want: "", wantDeleted: true, wantGone: true},
		{
			name: "owned, kept (the requested kind)", objects: []client.Object{route("owner-uid")},
			want: "Route team-pgadmin published before remains, no longer managed: the operator may not list Routes in team-b; delete it by hand",
		},
		{name: "someone else's, never touched", objects: []client.Object{route("other-uid")}, remove: true, want: "", wantGone: true},
		{name: "unowned, never touched", objects: []client.Object{route("")}, remove: true, want: "", wantGone: true},
		{
			name: "unreadable", getErr: forbidden, remove: true,
			want:           "the operator may not read Routes in team-b: a Route team-pgadmin published before may remain; delete it by hand",
			wantUnverified: true,
		},
		// Round 15, k8s P3-2: nothing in the owner's history says it ever
		// published a Route; an unreadable one is not reported.
		{name: "unreadable, never published", getErr: forbidden, remove: true, notPublished: true, want: ""},
		// Round 16, OPERATOR P3-3: the kind served at start answers
		// NoKindMatch (an aggregated API unavailable for a moment while
		// the Route remains): an error, retried, never "nothing left".
		{name: "kind not served now", getErr: noKind, remove: true, wantErr: true},
		{name: "kind not served now, at delete", objects: []client.Object{route("owner-uid")}, deleteErr: noKind, remove: true, wantErr: true, wantDeleted: false},
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
			if got != tt.want || remnant.Unverified != tt.wantUnverified || remnant.Gone != tt.wantGone {
				t.Errorf("leftover = %q (unverified %t, gone %t), want %q (unverified %t, gone %t)", got, remnant.Unverified, remnant.Gone, tt.want, tt.wantUnverified, tt.wantGone)
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

// TestMayHavePublished: round 16, OPERATOR P3-2. Only the spec's request
// or the status record of a published kind says an owner may have
// published a Route or an HTTPRoute; no word in a condition message does
// ("504 Gateway Timeout", "the Route API is unavailable", nor the leftover
// sentence itself, which made an Unknown stick).
func TestMayHavePublished(t *testing.T) {
	for _, tt := range []struct {
		name      string
		requested bool
		published []string
		kind      string
		want      bool
	}{
		{"requested now", true, nil, "Route", true},
		{"no record", false, nil, "Route", false},
		{"a Route recorded", false, []string{"Route"}, "Route", true},
		{"an HTTPRoute is no Route", false, []string{"HTTPRoute"}, "Route", false},
		{"a Route is no HTTPRoute", false, []string{"Route"}, "HTTPRoute", false},
		{"both recorded", false, []string{"HTTPRoute", "Route"}, "HTTPRoute", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := MayHavePublished(tt.requested, tt.published, tt.kind); got != tt.want {
				t.Fatalf("MayHavePublished = %t, want %t", got, tt.want)
			}
		})
	}
}

// TestPublishedRecord: MarkPublished and MarkGone keep a sorted set,
// nil when empty (so a no-op loop writes nothing).
func TestPublishedRecord(t *testing.T) {
	var published []string
	MarkGone(&published, "Route")
	if published != nil {
		t.Fatalf("MarkGone on nothing = %q", published)
	}
	MarkPublished(&published, "Route")
	MarkPublished(&published, "HTTPRoute")
	MarkPublished(&published, "Route")
	if !slices.Equal(published, []string{"HTTPRoute", "Route"}) {
		t.Fatalf("published = %q, want [HTTPRoute Route]", published)
	}
	MarkGone(&published, "Route")
	if !slices.Equal(published, []string{"HTTPRoute"}) {
		t.Fatalf("published = %q, want [HTTPRoute]", published)
	}
	MarkGone(&published, "HTTPRoute")
	if published != nil {
		t.Fatalf("published = %q, want nil", published)
	}
}

func TestAllUnverified(t *testing.T) {
	if !AllUnverified([]Remnant{{Unverified: true}, {Unverified: true}}) || AllUnverified([]Remnant{{Unverified: true}, {}}) || AllUnverified(nil) {
		t.Error("AllUnverified: true only for a non-empty list of unverified remnants")
	}
}
