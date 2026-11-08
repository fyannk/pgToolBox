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

package v1alpha1

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type wireValidator interface {
	Validate() error
}

func TestWireGoldensRoundTripAndIgnoreAdditiveFields(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() wireValidator
	}{
		{name: "health", file: "health.json", new: func() wireValidator { return &ServiceStatus{} }},
		{name: "readiness ready", file: "readiness-ready.json", new: func() wireValidator { return &ServiceStatus{} }},
		{name: "readiness not ready", file: "readiness-not-ready.json", new: func() wireValidator { return &ServiceStatus{} }},
		{name: "snapshot", file: "snapshot.json", new: func() wireValidator { return &RepositoryEvidenceSnapshot{} }},
		{name: "backups", file: "backups.json", new: func() wireValidator { return &BarmanBackupPage{} }},
		{name: "WAL ranges", file: "wal-ranges.json", new: func() wireValidator { return &BarmanWALRangePage{} }},
		{name: "WAL gaps", file: "wal-gaps.json", new: func() wireValidator { return &BarmanWALGapPage{} }},
		{name: "recovery paths", file: "recovery-paths.json", new: func() wireValidator { return &BarmanRecoveryPathPage{} }},
		{name: "error", file: "error.json", new: func() wireValidator { return &EvidenceAPIError{} }},
		{name: "pgBackRest snapshot", file: "pgbackrest-snapshot.json", new: func() wireValidator { return &RepositoryEvidenceSnapshot{} }},
		{name: "pgBackRest backups", file: "pgbackrest-backups.json", new: func() wireValidator { return &PGBackRestBackupPage{} }},
		{name: "pgBackRest WAL ranges", file: "pgbackrest-wal-ranges.json", new: func() wireValidator { return &PGBackRestWALRangePage{} }},
		{name: "pgBackRest WAL gaps", file: "pgbackrest-wal-gaps.json", new: func() wireValidator { return &PGBackRestWALGapPage{} }},
		{name: "pgBackRest recovery paths", file: "pgbackrest-recovery-paths.json", new: func() wireValidator { return &PGBackRestRecoveryPathPage{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join("testdata", "wire", test.file))
			if err != nil {
				t.Fatal(err)
			}
			resource := test.new()
			if err := json.Unmarshal(golden, resource); err != nil {
				t.Fatal(err)
			}
			if err := resource.Validate(); err != nil {
				t.Fatal(err)
			}
			roundTrip, err := json.MarshalIndent(resource, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			roundTrip = append(roundTrip, '\n')
			if !bytes.Equal(golden, roundTrip) {
				t.Fatalf("wire golden changed after Go round trip\nwant:\n%s\ngot:\n%s", golden, roundTrip)
			}

			var additive map[string]json.RawMessage
			if err := json.Unmarshal(golden, &additive); err != nil {
				t.Fatal(err)
			}
			additive["future_field"] = json.RawMessage(`{"ignored":true}`)
			withUnknown, err := json.Marshal(additive)
			if err != nil {
				t.Fatal(err)
			}
			future := test.new()
			if err := json.Unmarshal(withUnknown, future); err != nil {
				t.Fatal(err)
			}
			if err := future.Validate(); err != nil {
				t.Fatalf("additive unknown field invalidated resource: %v", err)
			}
			encoded, err := json.Marshal(future)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte("future_field")) {
				t.Fatal("unknown field was retained in the typed resource")
			}
		})
	}
}

func TestGeneratedSchemaIsCanonicalJSON(t *testing.T) {
	data, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(data, canonical) {
		t.Fatal("generated schema is not canonical deterministic JSON")
	}
}

func TestServiceStatusRejectsCrossedKindAndStatus(t *testing.T) {
	for _, value := range []ServiceStatus{
		{APIVersion: APIVersion, Kind: HealthKind, Status: ReadinessReady},
		{APIVersion: APIVersion, Kind: ReadinessKind, Status: HealthLive},
		{APIVersion: APIVersion, Kind: "FutureStatus", Status: HealthLive},
	} {
		if err := value.Validate(); err == nil {
			t.Fatalf("invalid service status accepted: %#v", value)
		}
	}
}
