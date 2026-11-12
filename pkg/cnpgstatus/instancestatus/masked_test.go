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

package instancestatus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMaskedReports (round 16, CNPG P2-1): the 200 answers a fenced and a
// restarting primary give, in CloudNativePG's wire shape (PostgresqlStatus
// as json.Marshal writes it from GetStatus's deferred mask), parse as
// masked, with isPrimary kept and every PostgreSQL field at its zero; a
// manager that flags MightBeUnavailable but read PostgreSQL is not masked.
func TestMaskedReports(t *testing.T) {
	for _, name := range []string{"fenced-primary.json", "restarting-primary.json"} {
		body, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		r, err := Parse(strings.NewReader(string(body)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !r.Masked() || !r.IsPrimary || !r.MightBeUnavailable {
			t.Errorf("%s: masked %v, isPrimary %v, mightBeUnavailable %v; want all true", name, r.Masked(), r.IsPrimary, r.MightBeUnavailable)
		}
		if r.IsArchivingWAL || r.ReadyWALFiles != 0 || r.LastArchivedWALTime != nil || r.CurrentWAL != "" || r.TimelineID != 0 {
			t.Errorf("%s: PostgreSQL fields %+v, want their zeros (not read)", name, r)
		}
	}
	r, err := Parse(strings.NewReader(`{"isPrimary":true,"mightBeUnavailable":true,"readyWalFiles":2,"isArchivingWAL":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Masked() {
		t.Error("a report read with MightBeUnavailable set and no masked error is masked")
	}
	r, err = Parse(strings.NewReader(`{"isPgRewindRunning":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Masked() {
		t.Error("a report sent while pg_rewind runs (PostgreSQL not read) is not masked")
	}
}
