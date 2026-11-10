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
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestObserveWorkloadCountsAgainstTheDesiredReplicas: WorkloadReady is
// True only when every desired replica is updated and available; a
// Deployment's ReplicaFailure (a quota refusing Pods) is reported whatever
// the counts say.
func TestObserveWorkloadCountsAgainstTheDesiredReplicas(t *testing.T) {
	const quota = `pods "c-x" is forbidden: exceeded quota: pods`
	ready := workloadFacts{Kind: "Deployment", Name: "c", Generation: 2, ObservedGeneration: 2, Desired: 3, Replicas: 3, Updated: 3, Available: 3}
	cases := []struct {
		name   string
		mutate func(*workloadFacts)
		status metav1.ConditionStatus
		reason pgtoolboxv1alpha1.Reason
	}{
		{"every desired replica available", func(*workloadFacts) {}, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected},
		{"one of three replicas, quota exceeded", func(w *workloadFacts) {
			w.Replicas, w.Updated, w.Available, w.Failure = 1, 1, 1, quota
		}, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonReplicaFailure},
		{
			"counts complete but ReplicaFailure set", func(w *workloadFacts) { w.Failure = quota },
			metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonReplicaFailure,
		},
		{
			"one of three replicas, no failure yet", func(w *workloadFacts) { w.Replicas, w.Updated, w.Available = 1, 1, 1 },
			metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress,
		},
		{
			"two of three available", func(w *workloadFacts) { w.Available = 2 },
			metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress,
		},
		{
			"an old replica still there", func(w *workloadFacts) { w.Replicas = 4 },
			metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress,
		},
		{
			"generation not observed", func(w *workloadFacts) { w.ObservedGeneration = 1 },
			metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress,
		},
		{
			"progress deadline exceeded", func(w *workloadFacts) { w.Available, w.Stuck = 2, "ReplicaSet c-1 has timed out progressing" },
			metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutStuck,
		},
	}
	r := &Reconciler{APIReader: fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxConsole{}).Build()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &pgtoolboxv1alpha1.PgToolBoxConsole{}
			c.Name, c.Namespace, c.Generation = "c", "ns", 1
			w := ready
			tc.mutate(&w)
			if err := r.observeWorkload(context.Background(), c, w); err != nil {
				t.Fatal(err)
			}
			got := meta.FindStatusCondition(c.Status.Conditions, pgtoolboxv1alpha1.ConditionWorkloadReady)
			if got == nil || got.Status != tc.status || got.Reason != string(tc.reason) {
				t.Fatalf("WorkloadReady = %+v, want %s/%s", got, tc.status, tc.reason)
			}
		})
	}
}
