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

package barmancloud

import "testing"

func TestMatcherValidatesBarmanScopeLayout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		configured []string
		key        string
		wantScope  string
		wantMatch  bool
		recognized bool
	}{
		{name: "discovers base layout", key: "alpha/base/20260727/backup.info", wantScope: "alpha", wantMatch: true, recognized: true},
		{name: "discovers WAL layout", key: "alpha/wals/000000010000000000000001", wantScope: "alpha", wantMatch: true, recognized: true},
		{name: "rejects pgBackRest layout", key: "backup/demo/backup.info", wantMatch: false},
		{name: "does not discover arbitrary root", key: "alpha/custom/value", wantMatch: false},
		{name: "keeps configured scope inventory without claiming layout", configured: []string{"alpha"}, key: "alpha/custom/value", wantScope: "alpha", wantMatch: true},
		{name: "confines explicit scope", configured: []string{"alpha"}, key: "beta/base/20260727/backup.info", wantMatch: false},
		{name: "rejects invalid name", key: "../base/value", wantMatch: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			matcher, err := New().NewScopeMatcher(test.configured)
			if err != nil {
				t.Fatal(err)
			}
			match, matched := matcher.Match(test.key)
			if matched != test.wantMatch || match.Name != test.wantScope || match.Recognized != test.recognized {
				t.Fatalf("Match(%q) = (%#v, %v)", test.key, match, matched)
			}
		})
	}
}

func TestMatcherRejectsInvalidConfiguredBarmanScopes(t *testing.T) {
	t.Parallel()
	for _, scopes := range [][]string{{"../escape"}, {"duplicate", "duplicate"}} {
		if _, err := New().NewScopeMatcher(scopes); err == nil {
			t.Fatalf("NewScopeMatcher(%v) error = nil", scopes)
		}
	}
}
