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
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/event"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
)

// TestStatusFieldManagerIsTheConsolesContract pins the field manager of
// the operator's status writes. The console calls an evidence agent only
// when this manager owns status.endpoint and status.caBundle
// (internal/console/evidence.OperatorStatusManager); the
// console may not import the operator, so renaming the manager here
// without the console would silently stop every evidence read.
func TestStatusFieldManagerIsTheConsolesContract(t *testing.T) {
	if reconcile.StatusFieldOwner != "pgtoolbox-status" {
		t.Fatalf("StatusFieldOwner = %q; the console trusts EvidenceAgent status written by \"pgtoolbox-status\" only", reconcile.StatusFieldOwner)
	}
}

func TestConsumerStatusChanged(t *testing.T) {
	base := &pgtoolboxv1alpha1.EvidenceAgent{Status: pgtoolboxv1alpha1.EvidenceAgentStatus{
		Endpoint: "https://a.team-a.svc:8443", CABundle: []byte("ca"), Image: "x",
	}}
	changed := func(mutate func(*pgtoolboxv1alpha1.EvidenceAgent)) *pgtoolboxv1alpha1.EvidenceAgent {
		next := base.DeepCopy()
		mutate(next)
		return next
	}
	predicate := consumerStatusChanged()
	for _, tc := range []struct {
		name string
		next *pgtoolboxv1alpha1.EvidenceAgent
		want bool
	}{
		{"nothing changed", base.DeepCopy(), false},
		{"another status field", changed(func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Status.Image = "y" }), false},
		{"endpoint", changed(func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Status.Endpoint = "https://b.team-a.svc:8443" }), true},
		{"CA bundle", changed(func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Status.CABundle = []byte("other") }), true},
	} {
		if got := predicate.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: tc.next}); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if predicate.Create(event.CreateEvent{Object: base}) || predicate.Delete(event.DeleteEvent{Object: base}) || predicate.Generic(event.GenericEvent{Object: base}) {
		t.Error("only updates are this predicate's business")
	}
}
