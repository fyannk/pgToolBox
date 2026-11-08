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

// rawEvent builds an unstructured event for the given involved object.
func rawEvent(name, kind, apiVersion, objectName string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "payments",
			"uid":       "u-" + name,
		},
		"involvedObject": map[string]any{
			"kind":       kind,
			"apiVersion": apiVersion,
			"name":       objectName,
			"namespace":  "payments",
		},
		"type":          "Warning",
		"reason":        "SomethingHappened",
		"message":       "detail",
		"count":         int64(3),
		"lastTimestamp": "2026-07-28T11:59:00Z",
	}
}

func TestConvertEventCandidateSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		event     map[string]any
		candidate bool
	}{
		{"cluster object", rawEvent("e1", "Cluster", "postgresql.cnpg.io/v1", "orders"), true},
		{"other cluster object", rawEvent("e2", "Cluster", "postgresql.cnpg.io/v1", "billing"), false},
		{"cluster kind of another group", rawEvent("e3", "Cluster", "example.com/v1", "orders"), false},
		{"member-prefixed pod", rawEvent("e4", "Pod", "v1", "orders-1"), true},
		{"prefix-matched stranger pod is still a candidate", rawEvent("e5", "Pod", "v1", "orders-api-1"), true},
		{"unrelated pod", rawEvent("e6", "Pod", "v1", "billing-1"), false},
		{"unrelated kind", rawEvent("e7", "Deployment", "apps/v1", "orders"), false},
		{"backup of the operator's group", rawEvent("e9", "Backup", "postgresql.cnpg.io/v1", "orders-20260917"), true},
		{"scheduled backup of the operator's group", rawEvent("e10", "ScheduledBackup", "postgresql.cnpg.io/v1", "nightly"), true},
		{"pooler of the operator's group", rawEvent("e11", "Pooler", "postgresql.cnpg.io/v1", "orders-rw"), true},
		{"backup kind of another group", rawEvent("e12", "Backup", "velero.io/v1", "orders"), false},
		{"other namespace object", func() map[string]any {
			e := rawEvent("e8", "Pod", "v1", "orders-1")
			e["involvedObject"].(map[string]any)["namespace"] = "elsewhere"
			return e
		}(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t)
			_, candidate := c.convertEvent(tc.event)
			if candidate != tc.candidate {
				t.Errorf("candidate = %v, want %v", candidate, tc.candidate)
			}
		})
	}
}

func TestConvertEventFacts(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	facts, candidate := c.convertEvent(rawEvent("e1", "Cluster", "postgresql.cnpg.io/v1", "orders"))
	if !candidate {
		t.Fatal("cluster event not a candidate")
	}
	if facts.Reason != "SomethingHappened" || facts.Type != "Warning" || facts.Count != 3 {
		t.Errorf("facts wrong: %+v", facts)
	}
	if facts.LastSeen.IsZero() {
		t.Error("LastSeen not derived")
	}
}

// TestConvertEventBoundsMessage proves the boundary bounds hostile
// message content.
func TestConvertEventBoundsMessage(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	e := rawEvent("e1", "Cluster", "postgresql.cnpg.io/v1", "orders")
	e["message"] = strings.Repeat("m", 100_000)
	facts, _ := c.convertEvent(e)
	if len(facts.Message) > maxEventMessage {
		t.Fatalf("message length %d exceeds bound", len(facts.Message))
	}
}

// TestConvertEventMalformedIsDropped proves a malformed event is not a
// candidate rather than an error that would end the stream.
func TestConvertEventMalformedIsDropped(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	_, candidate := c.convertEvent(map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]any{"name": "bad"},
		"count":      "not-a-number",
	})
	if candidate {
		t.Fatal("malformed event accepted as candidate")
	}
}

// TestConvertEventTimeFallback proves the occurrence time falls back
// through the timestamp fields.
func TestConvertEventTimeFallback(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	e := rawEvent("e1", "Cluster", "postgresql.cnpg.io/v1", "orders")
	delete(e, "lastTimestamp")
	e["eventTime"] = "2026-07-28T11:58:00.000000Z"
	facts, candidate := c.convertEvent(e)
	if !candidate {
		t.Fatal("event without lastTimestamp rejected")
	}
	if facts.LastSeen.IsZero() {
		t.Error("eventTime fallback not applied")
	}
}
