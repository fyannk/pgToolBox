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
	"maps"
	"slices"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func mcpLease(holder string, renewed time.Time) *coordinationv1.Lease {
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "console-mcp", Namespace: "team-a"}}
	if holder != "" {
		lease.Spec = coordinationv1.LeaseSpec{
			HolderIdentity: new(holder), LeaseDurationSeconds: new(int32(15)), RenewTime: &metav1.MicroTime{Time: renewed},
		}
	}
	return lease
}

func TestMCPHolderPod(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		lease *coordinationv1.Lease
		want  string
	}{
		{"held", mcpLease("console-7d9f-abcde_0a1b2c3d4e5f", now.Add(-5*time.Second)), "console-7d9f-abcde"},
		{"no Lease", nil, ""},
		{"released", mcpLease("", time.Time{}), ""},
		{"expired", mcpLease("console-0_0a1b2c3d4e5f", now.Add(-16*time.Second)), ""},
		{"not a candidate identity", mcpLease("console-0", now), ""},
	} {
		if got := mcpHolderPod(tc.lease, now); got != tc.want {
			t.Errorf("%s: holder %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestLabelMCPLeader: the holder's Pod alone carries the label; with no
// holder, an expired Lease, no Lease or MCP off, none does; a Pod that is
// not this console's is never written; a Pod already right is not either.
func TestLabelMCPLeader(t *testing.T) {
	now := time.Now()
	labelled := map[string]string{pgtoolboxv1alpha1.LabelMCPLeader: pgtoolboxv1alpha1.MCPLeaderLabelValue}
	for _, tc := range []struct {
		name    string
		mcpOff  bool
		lease   *coordinationv1.Lease
		before  []string // the console's Pods that carry the label
		want    []string
		patches int
	}{
		{"holder found", false, mcpLease("console-b_00ff", now), nil, []string{"console-b"}, 1},
		{"holder moved", false, mcpLease("console-b_00ff", now), []string{"console-a"}, []string{"console-b"}, 2},
		{"already right", false, mcpLease("console-b_00ff", now), []string{"console-b"}, []string{"console-b"}, 0},
		{"holder gone", false, mcpLease("console-z_00ff", now), []string{"console-a"}, nil, 1},
		{"expired", false, mcpLease("console-b_00ff", now.Add(-time.Minute)), []string{"console-b"}, nil, 1},
		{"released", false, mcpLease("", now), []string{"console-b"}, nil, 1},
		{"no Lease", false, nil, []string{"console-a", "console-b"}, nil, 2},
		{"MCP off", true, mcpLease("console-b_00ff", now), []string{"console-b"}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			console := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a"}}
			console.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: !tc.mcpOff}
			objects := []client.Object{}
			for _, name := range []string{"console-a", "console-b"} {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Labels: selectorLabels(console)}}
				if slices.Contains(tc.before, name) {
					maps.Copy(pod.Labels, labelled)
				}
				objects = append(objects, pod)
			}
			// Not this console's: never written, whatever it carries.
			objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "console-z", Namespace: "team-a", Labels: maps.Clone(labelled)}})
			if tc.lease != nil {
				objects = append(objects, tc.lease)
			}
			patches := 0
			c := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patches++
					return c.Patch(ctx, obj, patch, opts...)
				},
			}).Build()
			r := &Reconciler{Client: c}
			if err := r.labelMCPLeader(t.Context(), console); err != nil {
				t.Fatal(err)
			}
			var pods corev1.PodList
			if err := c.List(t.Context(), &pods); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, pod := range pods.Items {
				if pod.Labels[pgtoolboxv1alpha1.LabelMCPLeader] == pgtoolboxv1alpha1.MCPLeaderLabelValue && pod.Name != "console-z" {
					got = append(got, pod.Name)
				}
				if pod.Name == "console-z" && pod.Labels[pgtoolboxv1alpha1.LabelMCPLeader] != pgtoolboxv1alpha1.MCPLeaderLabelValue {
					t.Error("a Pod of another owner lost its label")
				}
			}
			if !slices.Equal(got, tc.want) || patches != tc.patches {
				t.Errorf("labelled %v with %d patches, want %v with %d", got, patches, tc.want, tc.patches)
			}
		})
	}
}

func TestLeaseHolderChanged(t *testing.T) {
	now := time.Now()
	p := leaseHolderChanged()
	renewed := mcpLease("console-a_00", now.Add(2*time.Second))
	if p.Update(event.UpdateEvent{ObjectOld: mcpLease("console-a_00", now), ObjectNew: renewed}) {
		t.Error("a renewal reconciles")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: renewed, ObjectNew: mcpLease("console-b_00", now)}) {
		t.Error("a new holder does not reconcile")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: renewed, ObjectNew: mcpLease("", now)}) {
		t.Error("a release does not reconcile")
	}
}
