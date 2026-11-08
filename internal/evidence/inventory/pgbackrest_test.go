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

package inventory

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest/pgbackrestfixture"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

func scanPGBackRest(t *testing.T, reader store.Reader, configured []string, options *PGBackRestOptions) Snapshot {
	t.Helper()
	format := pgbackrest.New()
	cache, err := NewCache(Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := NewScanner(ScannerOptions{
		Store: reader, Format: format, ConfiguredScopes: configured, Cache: cache,
		Readiness:       readiness.New(true, time.Hour, time.Now),
		RefreshInterval: time.Hour, MaxObjects: 100_000, PageSize: 7, RecentLimit: 5,
		Now: time.Now, Logger: slog.New(slog.DiscardHandler), PGBackRest: options,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestScannerAnalysesPGBackRestStanzas(t *testing.T) {
	t.Parallel()
	plain := pgbackrestfixture.Load(t, "plain")
	encrypted := pgbackrestfixture.Load(t, "encrypted")
	agent := func(passphrase []byte) *PGBackRestOptions {
		return &PGBackRestOptions{Stanza: pgbackrestfixture.Stanza, ArchiveStanza: pgbackrestfixture.Stanza, Passphrase: passphrase}
	}
	tests := []struct {
		name       string
		repository *pgbackrestfixture.Repository
		configured []string
		options    *PGBackRestOptions
		state      evidence.State
		reports    int
		check      func(*testing.T, Snapshot)
	}{
		{
			name: "agent, plain", repository: plain, configured: []string{"demo"}, options: agent(nil),
			state: evidence.Healthy, reports: 1,
			check: func(t *testing.T, snapshot Snapshot) {
				for _, id := range []evidence.CapabilityID{evidence.CatalogListing, evidence.StructuralValidation, evidence.DependencyValidation, evidence.WALContinuity, evidence.TimelineTraversal, evidence.EncryptedMetadata, evidence.RecoveryCoverage} {
					assertCapability(t, snapshot, id, evidence.Supported, evidence.Healthy)
				}
				if len(snapshot.PGBackRest[0].Backups) != 4 || snapshot.PGBackRest[0].Window == nil {
					t.Fatalf("%d backups, window %v", len(snapshot.PGBackRest[0].Backups), snapshot.PGBackRest[0].Window)
				}
			},
		},
		{
			name: "agent, encrypted with the passphrase", repository: encrypted, configured: []string{"demo"},
			options: agent([]byte(pgbackrestfixture.Passphrase)), state: evidence.Warning, reports: 1,
			check: func(t *testing.T, snapshot Snapshot) {
				assertCapability(t, snapshot, evidence.EncryptedMetadata, evidence.Supported, evidence.Healthy)
			},
		},
		{
			name: "agent, encrypted without the passphrase", repository: encrypted, configured: []string{"demo"},
			options: agent(nil), state: evidence.Unknown, reports: 1,
			check: func(t *testing.T, snapshot Snapshot) {
				assertCapability(t, snapshot, evidence.EncryptedMetadata, evidence.Supported, evidence.Unknown)
			},
		},
		{
			// The archive stanza holds no WAL here: the split is reported,
			// and the WAL it cannot find makes the evidence unknown.
			name: "agent, split archive stanza", repository: plain, configured: []string{"demo", "demo-wal"},
			options: &PGBackRestOptions{Stanza: "demo", ArchiveStanza: "demo-wal"}, state: evidence.Unknown, reports: 1,
			check: func(t *testing.T, snapshot Snapshot) {
				report := snapshot.PGBackRest[0]
				if report.ArchiveStanza != "demo-wal" || !slices.ContainsFunc(report.Findings, func(finding pgbackrest.Finding) bool { return finding.Code == pgbackrest.FindingStanzaSplit }) {
					t.Fatalf("report %+v", report)
				}
			},
		},
		{
			// The agent's collector keeps only its two stanza directories:
			// another stanza in the same listing changes nothing.
			name:       "agent ignores other stanzas",
			repository: pgbackrestfixture.Merge(plain, renamed(t, pgbackrestfixture.Load(t, "format6"), "other")),
			configured: []string{"demo"}, options: agent(nil), state: evidence.Healthy, reports: 1,
		},
		{
			name: "viewer, every stanza", repository: pgbackrestfixture.Merge(plain, renamed(t, pgbackrestfixture.Load(t, "format6"), "other")),
			options: &PGBackRestOptions{Passphrase: []byte(pgbackrestfixture.Passphrase)}, state: evidence.Unknown, reports: 2,
			check: func(t *testing.T, snapshot Snapshot) {
				if snapshot.PGBackRest[0].Stanza != "demo" || snapshot.PGBackRest[1].Stanza != "other" {
					t.Fatalf("stanzas %s, %s", snapshot.PGBackRest[0].Stanza, snapshot.PGBackRest[1].Stanza)
				}
				for _, capability := range snapshot.Evidence.Capabilities {
					if capability.ID == evidence.CatalogListing && capability.Reason[:6] != "other:" {
						t.Fatalf("the worst stanza is not named: %+v", capability)
					}
				}
			},
		},
		{
			name: "no analysis without options", repository: plain, configured: []string{"demo"},
			state: evidence.Unknown, reports: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := scanPGBackRest(t, test.repository.Reader(t), test.configured, test.options)
			if snapshot.Evidence.State != test.state || len(snapshot.PGBackRest) != test.reports {
				t.Fatalf("state %s (%s), %d reports; want %s, %d", snapshot.Evidence.State, snapshot.Evidence.Reason, len(snapshot.PGBackRest), test.state, test.reports)
			}
			if test.check != nil {
				test.check(t, snapshot)
			}
		})
	}
}

func TestPGBackRestSnapshotCloneAndValidate(t *testing.T) {
	t.Parallel()
	snapshot := scanPGBackRest(t, pgbackrestfixture.Load(t, "plain").Reader(t), []string{"demo"}, &PGBackRestOptions{Stanza: "demo"})
	copied := clone(snapshot)
	copied.PGBackRest[0].Backups[0].Backup.Label = "x"
	if snapshot.PGBackRest[0].Backups[0].Backup.Label == "x" {
		t.Fatal("clone shares the pgBackRest reports")
	}
	unsorted := clone(snapshot)
	unsorted.PGBackRest = append(unsorted.PGBackRest, unsorted.PGBackRest[0])
	if unsorted.Validate() == nil {
		t.Fatal("duplicate stanza reports validated")
	}
}

func TestPGBackRestViewerStanzaCeiling(t *testing.T) {
	t.Parallel()
	objects := make([]store.Object, 0, MaxPGBackRestStanzas+1)
	for index := range MaxPGBackRestStanzas + 1 {
		objects = append(objects, store.Object{Key: "backup/s" + string(rune('A'+index/26)) + string(rune('a'+index%26)) + "/backup.info", Size: 1})
	}
	repository := &pgbackrestfixture.Repository{Objects: objects, Content: map[string][]byte{}}
	snapshot := scanPGBackRest(t, repository.Reader(t), nil, &PGBackRestOptions{})
	if snapshot.Evidence.State != evidence.Unknown || len(snapshot.PGBackRest) != 0 || snapshot.Evidence.Reason != "too many pgBackRest stanzas to analyse" {
		t.Fatalf("state %s (%s), %d reports", snapshot.Evidence.State, snapshot.Evidence.Reason, len(snapshot.PGBackRest))
	}
}

// renamed moves a fixture's stanza directories to stanza.
func renamed(t *testing.T, repository *pgbackrestfixture.Repository, stanza string) *pgbackrestfixture.Repository {
	t.Helper()
	moved := &pgbackrestfixture.Repository{Content: make(map[string][]byte)}
	rename := func(key string) string {
		for _, root := range []string{"backup/", "archive/"} {
			if len(key) > len(root)+len(pgbackrestfixture.Stanza) && key[:len(root)+len(pgbackrestfixture.Stanza)+1] == root+pgbackrestfixture.Stanza+"/" {
				return root + stanza + key[len(root)+len(pgbackrestfixture.Stanza):]
			}
		}
		return key
	}
	for _, object := range repository.Objects {
		object.Key = rename(object.Key)
		moved.Objects = append(moved.Objects, object)
	}
	for key, data := range repository.Content {
		moved.Content[rename(key)] = data
	}
	return moved
}
