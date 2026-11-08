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

package pgbackrest

import "testing"

func TestMatcherValidatesPGBackRestScopeLayout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		configured []string
		key        string
		wantScope  string
		wantMatch  bool
	}{
		{name: "discovers backup layout", key: "backup/demo/backup.info", wantScope: "demo", wantMatch: true},
		{name: "discovers archive layout", key: "archive/demo/16-1/000000010000000000000001", wantScope: "demo", wantMatch: true},
		{name: "rejects Barman layout", key: "demo/base/20260727/backup.info"},
		{name: "rejects arbitrary root", key: "custom/demo/value"},
		{name: "confines explicit stanza", configured: []string{"demo"}, key: "backup/other/backup.info"},
		{name: "accepts explicit stanza", configured: []string{"demo"}, key: "backup/demo/backup.info", wantScope: "demo", wantMatch: true},
		{name: "rejects invalid name", key: "backup/../backup.info"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			matcher, err := New().NewScopeMatcher(test.configured)
			if err != nil {
				t.Fatal(err)
			}
			match, matched := matcher.Match(test.key)
			if matched != test.wantMatch || match.Name != test.wantScope {
				t.Fatalf("Match(%q) = (%#v, %v)", test.key, match, matched)
			}
		})
	}
}

func TestMatcherRejectsInvalidConfiguredPGBackRestScopes(t *testing.T) {
	t.Parallel()
	for _, scopes := range [][]string{{"../escape"}, {"duplicate", "duplicate"}} {
		if _, err := New().NewScopeMatcher(scopes); err == nil {
			t.Fatalf("NewScopeMatcher(%v) error = nil", scopes)
		}
	}
}
