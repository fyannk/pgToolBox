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

package pgadmin

import (
	"encoding/json"
	"testing"
)

// FuzzParseReport: the registration Job's termination message is written
// inside a Pod, so it is untrusted. Whatever it holds, the version that
// reaches status is empty or at most 32 bytes of [0-9A-Za-z.+_-] (no line
// break, control character or space can be smuggled into kubectl output),
// the server count is never negative, and a report re-encoded from what
// was accepted parses to itself.
func FuzzParseReport(f *testing.F) {
	for _, seed := range []string{
		`{"pgAdminVersion":"9.18","servers":4}`,
		`{"pgAdminVersion":"9.18\nInjected: x"}`,
		`{"pgAdminVersion":"9.18\t","servers":-1}`,
		`{"pgAdminVersion":"` + "\u202e" + `81.9"}`,
		`not json`,
		``,
		`{"servers":9223372036854775807}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, message string) {
		report := parseReport(message)
		if !validVersion(report.PgAdminVersion) {
			t.Fatalf("version %q reached status", report.PgAdminVersion)
		}
		if report.Servers < 0 {
			t.Fatalf("negative server count %d", report.Servers)
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if again := parseReport(string(encoded)); again != report {
			t.Fatalf("not a fixed point: %+v -> %+v", report, again)
		}
	})
}
