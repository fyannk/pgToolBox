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
)

// rawPod is a pod-shaped object with everything the boundary must
// strip or scrub: managed fields, the last-applied annotation, and an
// inline env value.
func rawPod() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":            "cluster-1",
			"namespace":       "db",
			"uid":             "u1",
			"generation":      int64(3),
			"resourceVersion": "12345",
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"env":[{"name":"PGPASSWORD","value":"hunter2"}]}}`,
				"kept": "value",
			},
			"managedFields": []any{
				map[string]any{
					"manager": "kubectl-client-side-apply", "operation": "Update", "time": "2026-08-01T10:00:00Z",
					"fieldsV1": map[string]any{
						"f:metadata": map[string]any{
							"f:annotations": map[string]any{"f:kept": map[string]any{}},
						},
					},
				},
				map[string]any{
					"manager": "cloudnative-pg", "operation": "Update", "time": "2026-08-01T11:00:00Z",
					"fieldsV1": map[string]any{
						"f:spec": map[string]any{
							"f:containers": map[string]any{
								`k:{"name":"postgres"}`: map[string]any{
									".":       map[string]any{},
									"f:image": map[string]any{},
								},
							},
						},
					},
				},
				map[string]any{
					"manager": "kubelet", "operation": "Update", "time": "2026-08-01T09:00:00Z",
					"fieldsV1": map[string]any{
						"f:status": map[string]any{
							"f:phase": map[string]any{},
							"f:conditions": map[string]any{
								`k:{"type":"Ready"}`: map[string]any{"f:status": map[string]any{}},
							},
						},
					},
				},
			},
		},
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name": "postgres",
					"env": []any{
						map[string]any{"name": "PGPASSWORD", "value": "hunter2"},
						map[string]any{"name": "EMPTY", "value": ""},
						map[string]any{"name": "FROM_SECRET", "valueFrom": map[string]any{
							"secretKeyRef": map[string]any{"name": "creds", "key": "password"},
						}},
					},
				},
			},
		},
		"status": map[string]any{"phase": "Running"},
	}
}

func TestObservationScrubsAndStrips(t *testing.T) {
	obs, err := observationFrom(scopePods, rawPod(), false)
	if err != nil {
		t.Fatalf("observationFrom: %v", err)
	}

	manifest := string(obs.Manifest)
	if strings.Contains(manifest, "hunter2") {
		t.Fatal("inline env value survived into the stored manifest")
	}
	if strings.Contains(manifest, "managedFields") || strings.Contains(manifest, "resourceVersion") {
		t.Fatal("volatile metadata survived into the stored manifest")
	}
	if strings.Contains(manifest, "last-applied-configuration") {
		t.Fatal("last-applied annotation survived into the stored manifest")
	}
	if !strings.Contains(manifest, "PGPASSWORD") || !strings.Contains(manifest, redactedEnvValue) {
		t.Fatal("env names and the redaction marker must survive")
	}
	if !strings.Contains(manifest, `"secretKeyRef"`) {
		t.Fatal("secret references are names and must survive")
	}
	if !strings.Contains(manifest, `"kept":"value"`) {
		t.Fatal("unrelated annotations must survive")
	}

	if obs.Kind != "Pod" || obs.Group != "" || obs.Version != "v1" {
		t.Fatalf("identity = %s/%s %s, want core v1 Pod", obs.Group, obs.Version, obs.Kind)
	}
	if obs.Namespace != "db" || obs.Name != "cluster-1" || obs.UID != "u1" || obs.Generation != 3 {
		t.Fatalf("identity fields wrong: %+v", obs)
	}
	if obs.Actor.Manager != "cloudnative-pg" {
		t.Fatalf("actor = %q, want the most recent manager", obs.Actor.Manager)
	}
}

func TestObservationLeavesTheOriginalUntouched(t *testing.T) {
	content := rawPod()
	if _, err := observationFrom(scopePods, content, false); err != nil {
		t.Fatalf("observationFrom: %v", err)
	}
	env := content["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)
	if env[0].(map[string]any)["value"] != "hunter2" {
		t.Fatal("capture mutated the pump's object")
	}
	if _, ok := content["metadata"].(map[string]any)["managedFields"]; !ok {
		t.Fatal("capture stripped the pump's object")
	}
}

func TestObservationHashesSplitSpecFromStatus(t *testing.T) {
	base, err := observationFrom(scopePods, rawPod(), false)
	if err != nil {
		t.Fatalf("observationFrom: %v", err)
	}

	statusChanged := rawPod()
	statusChanged["status"].(map[string]any)["phase"] = "Failed"
	after, err := observationFrom(scopePods, statusChanged, false)
	if err != nil {
		t.Fatalf("observationFrom: %v", err)
	}
	if after.SpecHash != base.SpecHash {
		t.Fatal("a status change moved the spec hash")
	}
	if after.StatusHash == base.StatusHash {
		t.Fatal("a status change left the status hash")
	}

	// Volatile churn alone must not move either hash.
	churned := rawPod()
	churned["metadata"].(map[string]any)["resourceVersion"] = "99999"
	same, err := observationFrom(scopePods, churned, false)
	if err != nil {
		t.Fatalf("observationFrom: %v", err)
	}
	if same.SpecHash != base.SpecHash || same.StatusHash != base.StatusHash {
		t.Fatal("resource-version churn moved a hash")
	}
}

func TestObservationCapturesFieldOwnership(t *testing.T) {
	obs, err := observationFrom(scopePods, rawPod(), false)
	if err != nil {
		t.Fatalf("observationFrom: %v", err)
	}
	if len(obs.Owners) != 3 {
		t.Fatalf("owners = %+v, want 3 managers", obs.Owners)
	}
	// Sorted by manager name.
	cnpg, kubectl, kubelet := obs.Owners[0], obs.Owners[1], obs.Owners[2]
	if cnpg.Manager != "cloudnative-pg" ||
		cnpg.Paths[0] != ".spec.containers[name=postgres]" ||
		cnpg.Paths[1] != ".spec.containers[name=postgres].image" {
		t.Fatalf("cnpg = %+v: named list elements must use the differ's encoding", cnpg)
	}
	if kubectl.Manager != "kubectl-client-side-apply" || kubectl.Paths[0] != ".metadata.annotations.kept" {
		t.Fatalf("kubectl = %+v", kubectl)
	}
	if kubelet.Manager != "kubelet" || len(kubelet.Paths) != 2 || kubelet.Paths[1] != ".status.phase" {
		t.Fatalf("kubelet = %+v", kubelet)
	}
	if kubelet.Paths[0] != `.status.conditions[type="Ready"].status` {
		t.Fatalf("multi-key element = %q: must render merge keys, which the differ never matches", kubelet.Paths[0])
	}
}

func TestOwnershipCaptureIsBounded(t *testing.T) {
	wide := map[string]any{}
	for i := 0; i < 300; i++ {
		wide["f:field"+string(rune('a'+i%26))+string(rune('a'+(i/26)%26))+string(rune('a'+i%7))] = map[string]any{}
	}
	pod := rawPod()
	pod["metadata"].(map[string]any)["managedFields"] = []any{
		map[string]any{"manager": "wide", "operation": "Update", "fieldsV1": map[string]any{"f:spec": wide}},
	}
	obs, err := observationFrom(scopePods, pod, false)
	if err != nil {
		t.Fatalf("observationFrom: %v", err)
	}
	total := 0
	for _, owner := range obs.Owners {
		total += len(owner.Paths)
	}
	if total > maxOwnedPaths {
		t.Fatalf("captured %d owned paths, want at most %d", total, maxOwnedPaths)
	}
}

func TestDeletionObservationCarriesNoOwnership(t *testing.T) {
	obs, err := observationFrom(scopePods, rawPod(), true)
	if err != nil {
		t.Fatalf("observationFrom: %v", err)
	}
	if obs.Owners != nil {
		t.Fatalf("owners = %+v, want none on a deletion", obs.Owners)
	}
}

func TestObservationRequiresIdentity(t *testing.T) {
	if _, err := observationFrom(scopePods, map[string]any{"kind": "Pod"}, false); err == nil {
		t.Fatal("object without metadata accepted")
	}
	noUID := rawPod()
	delete(noUID["metadata"].(map[string]any), "uid")
	if _, err := observationFrom(scopePods, noUID, false); err == nil {
		t.Fatal("object without uid accepted")
	}
}

func TestDeletionObservationCarriesIdentityOnly(t *testing.T) {
	obs, err := observationFrom(scopePods, rawPod(), true)
	if err != nil {
		t.Fatalf("observationFrom: %v", err)
	}
	if !obs.Deleted || obs.Manifest != nil || obs.SpecHash != "" {
		t.Fatalf("deletion = %+v, want identity only", obs)
	}
}
