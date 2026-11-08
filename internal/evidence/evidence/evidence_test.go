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

package evidence

import "testing"

func TestInitialSnapshotIsUnknownAndDeterministic(t *testing.T) {
	t.Parallel()
	snapshot := InitialSnapshot("synthetic", "vault", []CapabilityID{WALContinuity, CatalogListing})
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	if snapshot.State != Unknown || snapshot.Compatibility != Unknown || snapshot.Completeness != Unscanned || snapshot.Reason != "no completed scan" {
		t.Fatalf("initial snapshot overstates evidence: %#v", snapshot)
	}
	if snapshot.Capabilities[0].ID != CatalogListing || snapshot.Capabilities[1].ID != WALContinuity {
		t.Fatalf("capabilities are not sorted: %#v", snapshot.Capabilities)
	}
}

func TestSnapshotRejectsHealthyIncompleteEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{name: "incomplete rollup", mutate: func(snapshot *Snapshot) { snapshot.State = Healthy }},
		{name: "unscanned warning", mutate: func(snapshot *Snapshot) { snapshot.State = Warning }},
		{name: "unsupported healthy capability", mutate: func(snapshot *Snapshot) {
			snapshot.Capabilities[0].Support = Unsupported
			snapshot.Capabilities[0].State = Unhealthy
		}},
		{name: "unbounded reason", mutate: func(snapshot *Snapshot) { snapshot.Reason = string(make([]byte, 257)) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := InitialSnapshot("synthetic", "vault", []CapabilityID{CatalogListing})
			test.mutate(&snapshot)
			if err := snapshot.Validate(); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}
