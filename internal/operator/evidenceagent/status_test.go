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

package evidenceagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/conditions"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
)

func condition(agent *pgtoolboxv1alpha1.EvidenceAgent, conditionType string) metav1.Condition {
	found := meta.FindStatusCondition(agent.Status.Conditions, conditionType)
	if found == nil {
		return metav1.Condition{}
	}
	return *found
}

func drainEvents(recorder *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestPublish drives the EvidenceAgent's one status write through the
// shared condition model on a fake client with the status subresource:
// what the loop returned decides Stalled, Progressing and Ready over the
// agent's aspects; the write carries them together; Events follow the
// write; a repeat of the same state writes and emits nothing.
func TestPublish(t *testing.T) {
	type aspects map[string]metav1.ConditionStatus
	allTrue := aspects{"ConfigurationValid": "True", "ServingCertificateReady": "True", "WorkloadReady": "True"}
	reasons := map[string]pgtoolboxv1alpha1.Reason{
		"ConfigurationValid": pgtoolboxv1alpha1.ReasonClusterNotFound, "ServingCertificateReady": pgtoolboxv1alpha1.ReasonServiceCAPending,
		"WorkloadReady": pgtoolboxv1alpha1.ReasonRolloutInProgress,
	}
	tests := []struct {
		name        string
		aspects     aspects
		outcome     conditions.Outcome
		ready       metav1.ConditionStatus
		reason      pgtoolboxv1alpha1.Reason
		stalled     metav1.ConditionStatus
		progressing metav1.ConditionStatus
		returns     bool
		events      []string
	}{
		{
			name: "all true", aspects: allTrue, outcome: conditions.Outcome{Converged: true},
			ready: "True", reason: pgtoolboxv1alpha1.ReasonAsExpected, stalled: "False", progressing: "False",
			events: []string{"Normal AsExpected"},
		},
		{
			name: "configuration invalid stalls, one Warning", aspects: aspects{"ConfigurationValid": "False"},
			outcome: conditions.Outcome{Err: reconcile.Stall(pgtoolboxv1alpha1.ReasonClusterNotFound, "Cluster orders was not found")},
			ready:   "False", reason: pgtoolboxv1alpha1.ReasonClusterNotFound, stalled: "True", progressing: "False",
			events: []string{"Warning ClusterNotFound"},
		},
		{
			name: "rollout converging is quiet", aspects: aspects{"ConfigurationValid": "True", "ServingCertificateReady": "True", "WorkloadReady": "False"},
			outcome: conditions.Outcome{Converged: true},
			ready:   "False", reason: pgtoolboxv1alpha1.ReasonRolloutInProgress, stalled: "False", progressing: "True",
		},
		{
			name: "service CA pending is quiet", aspects: aspects{"ConfigurationValid": "True", "ServingCertificateReady": "False", "WorkloadReady": "True"},
			outcome: conditions.Outcome{Converged: true},
			ready:   "False", reason: pgtoolboxv1alpha1.ReasonServiceCAPending, stalled: "False", progressing: "True",
		},
		{
			name: "error wins and is returned", aspects: allTrue, outcome: conditions.Outcome{Err: errors.New("boom")},
			ready: "False", reason: pgtoolboxv1alpha1.ReasonReconcileError, stalled: "False", progressing: "True", returns: true,
			events: []string{"Warning ReconciliationFailed"},
		},
		{
			name: "paused", aspects: allTrue, outcome: conditions.Outcome{Paused: true},
			ready: "Unknown", reason: pgtoolboxv1alpha1.ReasonReconciliationPaused, stalled: "False", progressing: "False",
		},
		{
			name: "first touch", aspects: aspects{}, outcome: conditions.Outcome{},
			ready: "Unknown", reason: pgtoolboxv1alpha1.ReasonInitializing, stalled: "False", progressing: "True",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := pgtoolboxv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			stored := testAgent()
			writes := 0
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored).WithStatusSubresource(stored).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						writes++
						return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
					},
				}).Build()
			recorder := events.NewFakeRecorder(16)
			r := &Reconciler{Client: c, APIReader: c, Recorder: recorder}

			loop := func() (*pgtoolboxv1alpha1.EvidenceAgent, error) {
				agent := &pgtoolboxv1alpha1.EvidenceAgent{}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(stored), agent); err != nil {
					t.Fatal(err)
				}
				original := agent.DeepCopy()
				initializeConditions(agent)
				for conditionType, status := range test.aspects {
					reason := pgtoolboxv1alpha1.ReasonAsExpected
					if status != metav1.ConditionTrue {
						reason = reasons[conditionType]
					}
					setCondition(agent, conditionType, status, reason, "fact")
				}
				return agent, r.publish(context.Background(), agent, original, test.outcome)
			}

			agent, err := loop()
			if (err != nil) != test.returns {
				t.Fatalf("publish returned %v", err)
			}
			ready := condition(agent, pgtoolboxv1alpha1.ConditionReady)
			if ready.Status != test.ready || ready.Reason != test.reason.String() || ready.ObservedGeneration != agent.Generation {
				t.Fatalf("Ready = %+v", ready)
			}
			if got := condition(agent, pgtoolboxv1alpha1.ConditionStalled).Status; got != test.stalled {
				t.Fatalf("Stalled = %s", got)
			}
			if got := condition(agent, pgtoolboxv1alpha1.ConditionProgressing).Status; got != test.progressing {
				t.Fatalf("Progressing = %s", got)
			}
			if agent.Status.ObservedGeneration != agent.Generation {
				t.Fatalf("observedGeneration = %d", agent.Status.ObservedGeneration)
			}
			got := drainEvents(recorder)
			if len(got) != len(test.events) {
				t.Fatalf("events %q, want %q", got, test.events)
			}
			for i := range got {
				if !strings.HasPrefix(got[i], test.events[i]) {
					t.Fatalf("event %q, want %q", got[i], test.events[i])
				}
			}
			if writes != 1 {
				t.Fatalf("status writes = %d, want 1", writes)
			}
			if _, err := loop(); (err != nil) != test.returns {
				t.Fatalf("second publish returned %v", err)
			}
			if writes != 1 {
				t.Fatal("an unchanged status was written again")
			}
			if again := drainEvents(recorder); len(again) != 0 {
				t.Fatalf("an unchanged status emitted %q", again)
			}
		})
	}
}

func TestSetConditionRefusesUnknownReasonsAndBoundsMessages(t *testing.T) {
	agent := testAgent()
	setCondition(agent, pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse, "Waiting", strings.Repeat("é", 2000))
	got := condition(agent, pgtoolboxv1alpha1.ConditionWorkloadReady)
	if got.Reason != pgtoolboxv1alpha1.ReasonReconcileError.String() || len(got.Message) > reconcile.MaxMessageLength || !strings.HasSuffix(got.Message, "...") {
		t.Fatalf("condition = %s / %d bytes", got.Reason, len(got.Message))
	}
}
