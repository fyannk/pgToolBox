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

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

// A manifest listed exactly as the last scan read it is not read again, and
// the scan reports what a fresh read would; a manifest whose listing changed,
// or that the store lists without an ETag, is read again.
func TestUnchangedManifestIsNotReadAgain(t *testing.T) {
	t.Parallel()
	tagged := loadFixture(t, fixtureRoot, "plain").without()
	for index := range tagged.objects {
		tagged.objects[index].ETag = "etag-" + tagged.objects[index].Key
	}
	first := Analyze(context.Background(), tagged.reader(t), tagged.objects, Options{Stanza: "demo", Generation: 1})
	if len(first.Backups) < 2 {
		t.Fatalf("fixture has %d backups, want at least 2", len(first.Backups))
	}
	label := first.Backups[0].Backup.Label
	key := ManifestKey("demo", label)

	manifestsOpened := func(repository *fixtureRepository) []string {
		var keys []string
		for _, opened := range repository.opened {
			if strings.Contains(opened, "/backup.manifest") {
				keys = append(keys, opened)
			}
		}
		return keys
	}
	untagged := tagged.without()
	for index := range untagged.objects {
		untagged.objects[index].ETag = ""
	}
	changed := tagged.with(key, []byte("[backrest]\nbackrest-format=5\n"))
	for index := range changed.objects {
		if changed.objects[index].Key == key {
			changed.objects[index].ETag = "etag-rewritten"
		}
	}
	for name, test := range map[string]struct {
		repository *fixtureRepository
		reread     []string
		state      evidence.State
		used       FileSource
		previous   func(Report) Report
	}{
		"unchanged": {repository: tagged.without(), state: first.Backups[0].Manifest.State},
		"no ETag":   {repository: untagged, reread: []string{"*"}, state: first.Backups[0].Manifest.State},
		// The inventory cache hands the next scan a CloneReport copy; the
		// clone keeps what reuse needs.
		"through the cache clone": {repository: tagged.without(), state: first.Backups[0].Manifest.State, previous: CloneReport},
		// A manifest decrypted under another cipher-pass is read again.
		"other cipher-pass": {repository: tagged.without(), reread: []string{key, key + ".copy"}, state: first.Backups[0].Manifest.State, previous: func(report Report) Report {
			report.Backups = slices.Clone(report.Backups)
			report.Backups[0].Manifest.decryption[0] ^= 0xff
			return report
		}},
		// The pair is read again; the rewritten main no longer verifies, so
		// the .copy is used, as pgBackRest does.
		"rewritten": {repository: changed, reread: []string{key, key + ".copy"}, state: evidence.Healthy, used: SourceCopy},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			previous := first
			if test.previous != nil {
				previous = test.previous(first)
			}
			second := Analyze(context.Background(), test.repository.reader(t), test.repository.objects, Options{Stanza: "demo", Generation: 2, Previous: &previous})
			opened := manifestsOpened(test.repository)
			want := test.reread
			if len(want) == 1 && want[0] == "*" {
				want = manifestsOpened(tagged)
			}
			if !reflect.DeepEqual(opened, want) {
				t.Errorf("read %v again, want %v", opened, want)
			}
			if got := second.Backups[0].Manifest.State; got != test.state {
				t.Errorf("manifest state %s, want %s", got, test.state)
			}
			if test.used != "" && second.Backups[0].Manifest.Pair.Used != test.used {
				t.Errorf("used %q, want %q", second.Backups[0].Manifest.Pair.Used, test.used)
			}
			if len(test.reread) == 0 && !reflect.DeepEqual(second.Findings, first.Findings) {
				t.Errorf("findings %v, want those of a fresh read %v", second.Findings, first.Findings)
			}
		})
	}
}
