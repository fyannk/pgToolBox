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
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

// budgetRepository is a backup list and a store that serves body for every
// manifest and records which keys were opened.
type budgetRepository struct {
	reports []BackupReport
	byLabel map[string]int
	listed  map[string]store.Object
	body    []byte
	mutex   sync.Mutex
	opened  []string
}

func (r *budgetRepository) add(backup Backup, listedSize int64) {
	backup.TimestampStart = time.Unix(int64(1_700_000_000+len(r.reports)), 0).UTC()
	backup.TimestampStop = backup.TimestampStart
	r.byLabel[backup.Label] = len(r.reports)
	r.reports = append(r.reports, BackupReport{Backup: backup})
	key := ManifestKey("demo", backup.Label)
	r.listed[key] = store.Object{Key: key, Size: listedSize}
	r.listed[key+".copy"] = store.Object{Key: key + ".copy", Size: listedSize}
}

func (r *budgetRepository) reader() store.Reader {
	return &storetest.Fake{OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
		r.mutex.Lock()
		r.opened = append(r.opened, request.Key)
		r.mutex.Unlock()
		return io.NopCloser(bytes.NewReader(r.body)), nil
	}}
}

func (r *budgetRepository) check(options Options) *findingSet {
	findings := &findingSet{}
	checkManifests(context.Background(), r.reader(), r.listed, options, &BackupInfo{}, r.reports, newChainIndex(r.reports, r.byLabel), findings)
	return findings
}

func newBudgetRepository() *budgetRepository {
	return &budgetRepository{byLabel: map[string]int{}, listed: map[string]store.Object{}, body: []byte("not a manifest\n")}
}

func fullLabel(index int) string {
	return fmt.Sprintf("2026%02d%02d-%02d%02d%02dF", 1+index/(28*24*60)%12, 1+index/(24*60)%28, index/60%24, index%60, 0)
}

func budgetFindings(findings *findingSet) []Finding {
	return slices.DeleteFunc(findings.sorted(), func(finding Finding) bool { return finding.Code != FindingManifestBudgetExceeded })
}

// The manifests a scan reads are bounded whatever
// backup.info says, dependencies included.
func TestManifestReadsAreBoundedPerScan(t *testing.T) {
	t.Parallel()
	t.Run("references outside the chain are not followed", func(t *testing.T) {
		t.Parallel()
		repository := newBudgetRepository()
		references := make([]string, 0, 501)
		for index := range 500 {
			repository.add(Backup{Label: fullLabel(index), Type: BackupFull}, 10)
			references = append(references, fullLabel(index))
		}
		full := fullLabel(500)
		repository.add(Backup{Label: full, Type: BackupFull}, 10)
		// A differential whose backup.info claims 500 other fulls as references.
		repository.add(Backup{Label: full + "_20270101-000000D", Type: BackupDifferential, Prior: full, Reference: append(references, full)}, 10)
		findings := repository.check(Options{Stanza: "demo", MaxManifests: 1})
		if want := 4; len(repository.opened) != want { // the differential and its full, each with its .copy
			t.Fatalf("opened %d manifests (%v), want %d", len(repository.opened), repository.opened[:min(6, len(repository.opened))], want)
		}
		if got := budgetFindings(findings); len(got) != 0 {
			t.Fatalf("budget findings %+v", got)
		}
	})
	t.Run("a long chain stops at the read cap with a finding", func(t *testing.T) {
		t.Parallel()
		repository := newBudgetRepository()
		full := fullLabel(0)
		repository.add(Backup{Label: full, Type: BackupFull}, 10)
		prior := full
		for index := range 5000 {
			label := fmt.Sprintf("%s_2027%02d%02d-%02d%02d%02dI", full, 1+index/(28*24*60)%12, 1+index/(24*60)%28, index/60%24, index%60, 0)
			repository.add(Backup{Label: label, Type: BackupIncremental, Prior: prior}, 10)
			prior = label
		}
		findings := repository.check(Options{Stanza: "demo"})
		if limit := 2 * (DefaultMaxManifests + DefaultMaxManifests); len(repository.opened) > limit {
			t.Fatalf("opened %d manifests, want at most %d", len(repository.opened), limit)
		}
		if got := budgetFindings(findings); len(got) != 1 || got[0].Severity != SeverityWarning {
			t.Fatalf("budget findings %+v, want one warning", got)
		}
		read := 0
		for _, report := range repository.reports {
			if report.Manifest.Inspected {
				read++
			} else if report.Manifest.State != evidence.Unknown {
				t.Fatalf("%s: a manifest not read is %s", report.Backup.Label, report.Manifest.State)
			}
		}
		if want := 2 * DefaultMaxManifests; read != want {
			t.Fatalf("%d manifests inspected, want %d: the newest %d and as much of their chain as fits", read, want, DefaultMaxManifests)
		}
	})
	t.Run("the byte budget stops reading, newest first", func(t *testing.T) {
		t.Parallel()
		repository := newBudgetRepository()
		for index := range 10 {
			repository.add(Backup{Label: fullLabel(index), Type: BackupFull}, 1000)
		}
		findings := repository.check(Options{Stanza: "demo", MaxManifestScanBytes: 4500})
		inspected := []string{}
		for _, report := range repository.reports {
			if report.Manifest.Inspected {
				inspected = append(inspected, report.Backup.Label)
			}
		}
		if want := []string{fullLabel(8), fullLabel(9)}; !slices.Equal(inspected, want) {
			t.Fatalf("inspected %v, want the two newest %v", inspected, want)
		}
		if got := budgetFindings(findings); len(got) != 1 {
			t.Fatalf("budget findings %+v, want one", got)
		}
	})
	t.Run("an understated listing does not escape the byte budget", func(t *testing.T) {
		t.Parallel()
		repository := newBudgetRepository()
		repository.body = append(bytes.Repeat([]byte("x"), 999), '\n')
		for index := range 10 {
			repository.add(Backup{Label: fullLabel(index), Type: BackupFull}, 0)
		}
		repository.check(Options{Stanza: "demo", MaxManifestScanBytes: 4500})
		// Five files of 1000 bytes reach the budget; the pair being read when
		// it is passed finishes, nothing after it starts.
		if len(repository.opened) > 6 {
			t.Fatalf("opened %d manifests with understated sizes, want at most 6", len(repository.opened))
		}
	})
}
