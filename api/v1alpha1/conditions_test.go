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

package v1alpha1_test

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

var updateGolden = flag.Bool("update", false, "rewrite the reason catalogue golden files")

// TestReasonCatalogueFrozen holds every registered reason against
// testdata/reasons/<owner>.golden. A reason that disappears or is renamed
// fails (reasons are API surface: extend, never rename); a new reason
// fails until the golden file is regenerated with -update, so additions
// are deliberate and reviewed. One golden file per owner, so a new kind
// adds a file and never edits another's.
func TestReasonCatalogueFrozen(t *testing.T) {
	dir := filepath.Join("testdata", "reasons")
	owners := pgtoolboxv1alpha1.CatalogueOwners()
	for _, owner := range owners {
		got := pgtoolboxv1alpha1.Catalogue(owner)
		path := filepath.Join(dir, owner+".golden")
		if *updateGolden {
			var b strings.Builder
			for _, reason := range got {
				b.WriteString(string(reason) + "\n")
			}
			if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		raw, err := os.ReadFile(path) // #nosec G304 -- test fixture path
		if err != nil {
			t.Fatalf("owner %s has no golden file (go test ./api/v1alpha1 -run TestReasonCatalogueFrozen -update): %v", owner, err)
		}
		want := strings.Fields(string(raw))
		for _, reason := range want {
			if !slices.Contains(got, pgtoolboxv1alpha1.Reason(reason)) {
				t.Errorf("%s: reason %q was removed or renamed; reasons are frozen API surface", owner, reason)
			}
		}
		for _, reason := range got {
			if !slices.Contains(want, string(reason)) {
				t.Errorf("%s: reason %q is new; regenerate the golden file with -update", owner, reason)
			}
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		owner := strings.TrimSuffix(entry.Name(), ".golden")
		if !slices.Contains(owners, owner) {
			t.Errorf("golden file %s has no registered owner: its reasons were removed", entry.Name())
		}
	}
}

func TestIsKnownReason(t *testing.T) {
	cases := []struct {
		reason pgtoolboxv1alpha1.Reason
		want   bool
	}{
		{pgtoolboxv1alpha1.ReasonAsExpected, true},
		{pgtoolboxv1alpha1.ReasonReconcileError, true},
		{"Waiting", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := pgtoolboxv1alpha1.IsKnownReason(tc.reason); got != tc.want {
			t.Errorf("IsKnownReason(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}
