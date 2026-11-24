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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// TestWorkloadReadyNamesTheMCPPodWhenOnlyMCPIsDown: with the console's own
// replicas available, WorkloadReady follows the <console>-mcp Deployment
// and its Pod (the one that serves MCP). A crash-looping MCP Pod must not
// leave the console reported WorkloadReady=True; a failing console Pod
// still wins while the console itself is not ready.
func TestWorkloadReadyNamesTheMCPPodWhenOnlyMCPIsDown(t *testing.T) {
	newConsole := func() *pgtoolboxv1alpha1.PgToolBoxConsole {
		c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "team-a", UID: "u"}}
		c.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: true}
		c.Spec.History.Persistent = new(false)
		return c
	}
	crashing := func(name string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Labels: labels},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "console", RestartCount: 7,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}}},
		}
	}
	// available sets a Deployment's status to n of its desired replicas.
	available := func(d *appsv1.Deployment, n int32) *appsv1.Deployment {
		d.Status = appsv1.DeploymentStatus{Replicas: n, UpdatedReplicas: n, AvailableReplicas: n}
		if d.Spec.Replicas != nil && n > 0 {
			d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas = *d.Spec.Replicas, *d.Spec.Replicas, *d.Spec.Replicas
		}
		return d
	}

	cases := []struct {
		name                  string
		consoleUp, mcpUp      bool
		pods                  func(c *pgtoolboxv1alpha1.PgToolBoxConsole) []*corev1.Pod
		status                metav1.ConditionStatus
		reason                pgtoolboxv1alpha1.Reason
		messageHas, messageNo string
	}{
		{
			name: "console ready, MCP Pod crash-looping", consoleUp: true,
			pods: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) []*corev1.Pod {
				return []*corev1.Pod{crashing("team-console-mcp-abc", mcpSelectorLabels(c))}
			},
			status: metav1.ConditionFalse, reason: pgtoolboxv1alpha1.ReasonCrashLoopBackOff,
			messageHas: "team-console-mcp-abc",
		},
		{
			name: "console ready, MCP Deployment unavailable, no Pod yet", consoleUp: true,
			pods:   func(*pgtoolboxv1alpha1.PgToolBoxConsole) []*corev1.Pod { return nil },
			status: metav1.ConditionFalse, reason: pgtoolboxv1alpha1.ReasonRolloutInProgress,
			messageHas: "team-console-mcp",
		},
		{
			name: "console and MCP ready", consoleUp: true, mcpUp: true,
			pods: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) []*corev1.Pod {
				// A stale crashing Pod of the MCP selector is not looked at once ready.
				return []*corev1.Pod{crashing("team-console-mcp-old", mcpSelectorLabels(c))}
			},
			status: metav1.ConditionTrue, reason: pgtoolboxv1alpha1.ReasonAsExpected,
			messageHas: "Deployment team-console:",
		},
		{
			name: "console down, MCP ready: the console's own Pod is reported", mcpUp: true,
			pods: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) []*corev1.Pod {
				return []*corev1.Pod{crashing("team-console-xyz", selectorLabels(c))}
			},
			status: metav1.ConditionFalse, reason: pgtoolboxv1alpha1.ReasonCrashLoopBackOff,
			messageHas: "team-console-xyz",
		},
		{
			name: "console and MCP both down: the console's own Pod comes first",
			pods: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) []*corev1.Pod {
				return []*corev1.Pod{crashing("team-console-xyz", selectorLabels(c)), crashing("team-console-mcp-abc", mcpSelectorLabels(c))}
			},
			status: metav1.ConditionFalse, reason: pgtoolboxv1alpha1.ReasonCrashLoopBackOff,
			messageHas: "team-console-xyz", messageNo: "team-console-mcp-abc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			console := newConsole()
			in := deploymentInputs{Image: specs.ResolveImage(nil, "registry.example/pgtoolbox-console:1.2.0")}
			objects := []client.Object{
				available(buildWorkload(console, in).(*appsv1.Deployment), boolN(tc.consoleUp)),
				available(buildMCPDeployment(console, in), boolN(tc.mcpUp)),
			}
			for _, d := range objects {
				d.SetOwnerReferences([]metav1.OwnerReference{ownerReference(console)})
			}
			for _, pod := range tc.pods(console) {
				objects = append(objects, pod)
			}
			c := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).
				WithStatusSubresource(&appsv1.Deployment{}).WithObjects(objects...).Build()
			r := &Reconciler{Client: c, APIReader: c, Scheme: cnpgScheme(t)}
			if err := r.reconcileWorkload(context.Background(), console, in); err != nil {
				t.Fatal(err)
			}
			got := meta.FindStatusCondition(console.Status.Conditions, pgtoolboxv1alpha1.ConditionWorkloadReady)
			if got == nil || got.Status != tc.status || got.Reason != string(tc.reason) || !strings.Contains(got.Message, tc.messageHas) ||
				(tc.messageNo != "" && strings.Contains(got.Message, tc.messageNo)) {
				t.Fatalf("WorkloadReady = %+v, want %s/%s mentioning %q", got, tc.status, tc.reason, tc.messageHas)
			}
		})
	}
}

func boolN(up bool) int32 {
	if up {
		return 1
	}
	return 0
}
