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
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func stateConsole() *pgtoolboxv1alpha1.PgToolBoxConsole {
	return &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "team-a", UID: "c-uid"}}
}

func TestStateRBACNamesOneConfigMap(t *testing.T) {
	c := stateConsole()
	role, binding := buildStateRBAC(c)
	want := []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"configmaps"},
		ResourceNames: []string{"team-console-state"}, Verbs: []string{"get", "watch", "update"},
	}}
	if role.Name != "team-console-state" || len(role.Rules) != 1 || !slices.Equal(ruleSet(role.Rules), ruleSet(want)) {
		t.Fatalf("role %s rules %v, want %v", role.Name, role.Rules, want)
	}
	if binding.RoleRef.Name != role.Name || len(binding.Subjects) != 1 ||
		binding.Subjects[0] != (rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "team-console", Namespace: "team-a"}) {
		t.Fatalf("binding %+v", binding)
	}
	state := buildState(c)
	if state.Name != c.StateConfigMapName() || len(state.Data) != 0 || !metav1.IsControlledBy(state, c) {
		t.Fatalf("state ConfigMap %+v", state.ObjectMeta)
	}
}

// TestStateRoleMatchesTheChart: the operator grants a console what the
// pgtools chart grants it, on the same name.
func TestStateRoleMatchesTheChart(t *testing.T) {
	chart := chartObject[rbacv1.Role](t, renderChart(t), "Role/console-state")
	role, _ := buildStateRBAC(&pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a"}})
	if got, want := ruleSet(role.Rules), ruleSet(chart.Rules); !slices.Equal(got, want) {
		t.Errorf("state Role rules\n got %v\nwant %v (the chart's)", got, want)
	}
	chartRenders(t, "ConfigMap/console-state")
}

func chartRenders(t *testing.T, key string) {
	t.Helper()
	if _, ok := renderChart(t)[key]; !ok {
		t.Errorf("the chart renders no %s", key)
	}
}

func TestStateWritesDoNotReconcile(t *testing.T) {
	c := stateConsole()
	owned := func(name string, mutate func(*corev1.ConfigMap)) *corev1.ConfigMap {
		cm := buildState(c)
		cm.Name = name
		cm.ResourceVersion = "1"
		if mutate != nil {
			mutate(cm)
		}
		return cm
	}
	bump := func(cm *corev1.ConfigMap) { cm.ResourceVersion = "2" }
	cases := []struct {
		name     string
		old, new *corev1.ConfigMap
		want     bool
	}{
		{"a write of the state", owned("team-console-state", nil), owned("team-console-state", bump), false},
		{"a relabelled state", owned("team-console-state", nil), owned("team-console-state", func(cm *corev1.ConfigMap) {
			bump(cm)
			cm.Labels = map[string]string{"x": "y"}
		}), true},
		{"a state being deleted", owned("team-console-state", nil), owned("team-console-state", func(cm *corev1.ConfigMap) {
			now := metav1.Now()
			cm.DeletionTimestamp = &now
		}), true},
		{"an orphaned state", owned("team-console-state", nil), owned("team-console-state", func(cm *corev1.ConfigMap) {
			cm.OwnerReferences = nil
		}), true},
		{"the revocation list", owned("team-console-revocations", nil), owned("team-console-revocations", bump), true},
		{"the lifecycle table", owned("team-console-lifecycle", nil), owned("team-console-lifecycle", bump), true},
	}
	predicate := stateWritesIgnored()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := predicate.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}); got != tc.want {
				t.Fatalf("reconciles = %v, want %v", got, tc.want)
			}
		})
	}
	if !predicate.Create(event.CreateEvent{Object: owned("team-console-state", nil)}) ||
		!predicate.Delete(event.DeleteEvent{Object: owned("team-console-state", nil)}) {
		t.Fatal("creating or deleting the state must reconcile")
	}
}
