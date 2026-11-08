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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func rawPooler(name, cluster string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Pooler",
		"metadata": map[string]any{
			"name": name, "namespace": "payments", "uid": "u-" + name,
			"creationTimestamp": "2026-07-28T09:00:00Z",
		},
		"spec": map[string]any{
			"cluster":   map[string]any{"name": cluster},
			"type":      "rw",
			"instances": int64(2),
			"pgbouncer": map[string]any{"poolMode": "transaction"},
		},
		"status": map[string]any{
			"instances":   int64(2),
			"phase":       "active",
			"phaseReason": "the pooler is running",
			"image":       "ghcr.io/cloudnative-pg/pgbouncer:1.24",
		},
	}}
}

// TestConvertPoolerRequiresTheExactClusterReference proves selection is
// the spec.cluster.name reference and not the namespace. The watch is
// namespace-scoped because RBAC cannot pin one by name, so another
// cluster's pooler is expected traffic that must not be adopted.
func TestConvertPoolerRequiresTheExactClusterReference(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)

	facts, member, err := c.convertPooler(rawPooler("orders-rw", "orders").Object)
	if err != nil || !member {
		t.Fatalf("target Pooler conversion: member=%v err=%v", member, err)
	}
	if facts.Name != "orders-rw" || facts.Type != "rw" || facts.PoolMode != "transaction" {
		t.Errorf("facts = %+v, want the reported name, type and pool mode", facts)
	}
	if facts.Phase != "active" || facts.ReadyInstances != 2 {
		t.Errorf("facts = %+v, want the operator-reported phase and instance count", facts)
	}
	if facts.DesiredInstances == nil || *facts.DesiredInstances != 2 {
		t.Errorf("desired instances = %v, want the requested count", facts.DesiredInstances)
	}

	if _, member, err := c.convertPooler(rawPooler("other-rw", "other").Object); err != nil || member {
		t.Errorf("a pooler of another cluster was adopted: member=%v err=%v", member, err)
	}
}

// TestConvertPoolerDoesNotReadSecretReferences proves the credential
// wiring never crosses the adapter boundary. The console holds no Secret
// permission, and a pooler's Secret names are not review data.
func TestConvertPoolerDoesNotReadSecretReferences(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	raw := rawPooler("orders-rw", "orders")
	status, _ := raw.Object["status"].(map[string]any)
	status["secrets"] = map[string]any{
		"pgBouncerSecrets": map[string]any{
			"authQuery": map[string]any{"name": "orders-pooler-auth-canary"},
		},
	}

	facts, member, err := c.convertPooler(raw.Object)
	if err != nil || !member {
		t.Fatalf("conversion: member=%v err=%v", member, err)
	}
	for _, field := range []string{facts.Name, facts.Type, facts.PoolMode, facts.Phase, facts.PhaseReason, facts.Image} {
		if strings.Contains(field, "canary") {
			t.Fatalf("a Secret reference reached the facts: %q", field)
		}
	}
}

// TestConvertPoolerBoundsTheOperatorPhaseReason proves free text is cut
// at the adapter boundary, so no later layer can render or retain an
// unbounded operator message.
func TestConvertPoolerBoundsTheOperatorPhaseReason(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	raw := rawPooler("orders-rw", "orders")
	status, _ := raw.Object["status"].(map[string]any)
	status["phaseReason"] = strings.Repeat("x", maxConditionMessage*3)

	facts, _, err := c.convertPooler(raw.Object)
	if err != nil {
		t.Fatalf("convertPooler: %v", err)
	}
	if len(facts.PhaseReason) != maxConditionMessage {
		t.Errorf("phase reason length %d, want it cut at %d", len(facts.PhaseReason), maxConditionMessage)
	}
}
