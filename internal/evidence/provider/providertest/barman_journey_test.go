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

package providertest

import (
	"encoding/json"
	"os"
	"testing"
)

func TestBarmanFixtureManifestMatchesJourneyProfile(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Profile    string `json:"profile"`
		Format     string `json:"format"`
		WALContext struct {
			PostgreSQLVersion int64 `json:"postgresql_version"`
			SegmentSize       int64 `json:"segment_size"`
		} `json:"wal_context"`
		Scenarios []struct {
			Name          string `json:"name"`
			ExpectedState string `json:"expected_state"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Profile != FixtureProfile || manifest.Format != "barman-cloud" || manifest.WALContext.PostgreSQLVersion != 170005 || manifest.WALContext.SegmentSize != 16<<20 || len(manifest.Scenarios) != 6 {
		t.Fatalf("fixture manifest = %#v", manifest)
	}
	wantStates := map[string]string{"completed": "healthy", "started": "warning", "failed": "unhealthy", "malformed": "unknown", "missing-artifact": "unhealthy", "missing-info": "unknown"}
	for _, scenario := range manifest.Scenarios {
		if wantStates[scenario.Name] != scenario.ExpectedState {
			t.Fatalf("scenario %q state = %q", scenario.Name, scenario.ExpectedState)
		}
		delete(wantStates, scenario.Name)
	}
	if len(wantStates) != 0 {
		t.Fatalf("fixture manifest is missing scenarios: %#v", wantStates)
	}
	objects := BarmanFixture(t)
	if len(objects) != 12 {
		t.Fatalf("fixture objects = %d, want 12", len(objects))
	}
}
