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

package metadata

import (
	"maps"
	"strings"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func TestBoundedName(t *testing.T) {
	long := strings.Repeat("a", 40)
	cases := []struct {
		name, base, suffix string
		wantLen            int
		wantExact          string
	}{
		{name: "short", base: "team", suffix: "-pgadmin", wantExact: "team-pgadmin"},
		{name: "exactly 63", base: strings.Repeat("b", 55), suffix: "-pgadmin", wantExact: strings.Repeat("b", 55) + "-pgadmin"},
		{name: "too long", base: long, suffix: "-pgadmin-servers-and-more", wantLen: 63},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BoundedName(tc.base, tc.suffix)
			if tc.wantExact != "" && got != tc.wantExact {
				t.Fatalf("got %q, want %q", got, tc.wantExact)
			}
			if tc.wantLen != 0 && len(got) != tc.wantLen {
				t.Fatalf("len(%q) = %d, want %d", got, len(got), tc.wantLen)
			}
			if len(got) > 63 {
				t.Fatalf("%q exceeds 63", got)
			}
		})
	}
	if BoundedName(long, "-pgadmin-servers-and-more-x") == BoundedName(long, "-pgadmin-servers-and-more-y") {
		t.Fatal("distinct long names collided")
	}
}

func TestLabelsCarryCacheSelector(t *testing.T) {
	id := Identity{Application: "pgadmin", Instance: "team", InstanceLabelKey: pgtoolboxv1alpha1.PgAdminLabelKey}
	labels := id.Labels("pgadmin")
	for key, want := range map[string]string{
		pgtoolboxv1alpha1.LabelReconcile:  "true",
		pgtoolboxv1alpha1.LabelManagedBy:  "pgtoolbox",
		pgtoolboxv1alpha1.PgAdminLabelKey: "team",
		LabelComponent:                    "pgadmin",
	} {
		if labels[key] != want {
			t.Errorf("label %s = %q, want %q", key, labels[key], want)
		}
	}
	selector := id.SelectorLabels("pgadmin")
	if _, ok := selector[pgtoolboxv1alpha1.LabelReconcile]; ok {
		t.Error("selector labels must not carry the mutable cache label")
	}
}

func TestPropagate(t *testing.T) {
	rules := []pgtoolboxv1alpha1.MetadataPropagation{
		{Target: pgtoolboxv1alpha1.PropagationTargetAll, Labels: map[string]string{"cost": "a", "app.kubernetes.io/name": "evil"}, Annotations: map[string]string{"pgtoolbox.fyannk.dev/spec-hash": "evil", "note": "all"}},
		{Target: pgtoolboxv1alpha1.PropagationTargetPod, Labels: map[string]string{"cost": "b"}, Annotations: map[string]string{"sidecar.istio.io/inject": "false"}},
		{Target: pgtoolboxv1alpha1.PropagationTargetService, Labels: map[string]string{"svc": "yes"}},
	}
	own := map[string]string{"app.kubernetes.io/name": "pgadmin"}
	ownAnnotations := map[string]string{"pgtoolbox.fyannk.dev/spec-hash": "real"}
	cases := []struct {
		name            string
		target          pgtoolboxv1alpha1.PropagationTarget
		wantLabels      map[string]string
		wantAnnotations map[string]string
	}{
		{
			name: "pod", target: pgtoolboxv1alpha1.PropagationTargetPod,
			wantLabels:      map[string]string{"cost": "b", "app.kubernetes.io/name": "pgadmin"},
			wantAnnotations: map[string]string{"note": "all", "sidecar.istio.io/inject": "false", "pgtoolbox.fyannk.dev/spec-hash": "real"},
		},
		{
			name: "storage", target: pgtoolboxv1alpha1.PropagationTargetStorage,
			wantLabels:      map[string]string{"cost": "a", "app.kubernetes.io/name": "pgadmin"},
			wantAnnotations: map[string]string{"note": "all", "pgtoolbox.fyannk.dev/spec-hash": "real"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels, annotations := Propagate(tc.target, rules, own, ownAnnotations)
			if !maps.Equal(labels, tc.wantLabels) {
				t.Errorf("labels = %v, want %v", labels, tc.wantLabels)
			}
			if !maps.Equal(annotations, tc.wantAnnotations) {
				t.Errorf("annotations = %v, want %v", annotations, tc.wantAnnotations)
			}
		})
	}
}

func TestFilteredOverlay(t *testing.T) {
	got := FilteredOverlay(map[string]string{"a": "1", "pgtoolbox.fyannk.dev/x": "2"})
	if !maps.Equal(got, map[string]string{"a": "1"}) {
		t.Fatalf("got %v", got)
	}
	if FilteredOverlay(nil) != nil {
		t.Fatal("nil in, nil out")
	}
}
