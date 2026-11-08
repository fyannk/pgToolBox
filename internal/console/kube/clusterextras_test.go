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

package kube

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

func rawQuorum(primary string, standbys []any, number int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "FailoverQuorum",
		"metadata": map[string]any{"name": "orders", "namespace": "payments"},
		"status": map[string]any{
			"method": "any", "primary": primary,
			"standbyNames": standbys, "standbyNumber": number,
		},
	}}
}

// TestConvertFailoverQuorumBoundsTheStandbyList proves the list is cut
// at the adapter boundary, so no later layer can render an unbounded
// one.
func TestConvertFailoverQuorumBoundsTheStandbyList(t *testing.T) {
	t.Parallel()
	standbys := make([]any, observe.MaxQuorumStandbys+10)
	for i := range standbys {
		standbys[i] = "standby"
	}
	facts, err := convertFailoverQuorum(rawQuorum("orders-1", standbys, 2).Object)
	if err != nil {
		t.Fatalf("convertFailoverQuorum: %v", err)
	}
	if len(facts.Standbys) != observe.MaxQuorumStandbys || !facts.StandbysTruncated {
		t.Errorf("standbys len=%d truncated=%v, want the bound applied and visible", len(facts.Standbys), facts.StandbysTruncated)
	}
}
