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
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

func assessmentOf(t *testing.T, report Report, kind BackupType) Assessment {
	t.Helper()
	assessments := report.Assess()
	for index, backup := range report.Backups {
		if backup.Backup.Type == kind {
			return assessments[index]
		}
	}
	t.Fatalf("no %s backup", kind)
	return Assessment{}
}

func TestAspectsOfRealRepositories(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	encrypted := loadFixture(t, fixtureRoot, "encrypted")
	info, _ := loadInfoPair(t, "plain")
	diff := info.Backups[1]

	tests := []struct {
		name    string
		report  func(*testing.T) Report
		overall evidence.State
		check   func(*testing.T, Report, Aspects)
	}{
		{
			name:    "plain: every backup usable",
			report:  func(t *testing.T) Report { return analyzeFixture(t, plain, "plain") },
			overall: evidence.Healthy,
			check: func(t *testing.T, report Report, aspects Aspects) {
				for _, assessment := range report.Assess() {
					if !assessment.Usable || assessment.State != evidence.Healthy || len(assessment.Unusable) != 0 {
						t.Fatalf("assessment %+v", assessment)
					}
				}
				if aspects.Encryption.Reason != "the repository is not encrypted" || report.RepositoryFormat() != 5 {
					t.Fatalf("encryption %+v, format %d", aspects.Encryption, report.RepositoryFormat())
				}
			},
		},
		{
			// repo2 was not the repository restored: its incremental on
			// timeline 1 cannot reach timeline 2. A restore must name
			// timeline 1, so the backup is a warning, and so is the
			// catalog: the reader alone calls this repository healthy.
			name:    "encrypted: a backup on an abandoned branch",
			report:  func(t *testing.T) Report { return analyzeFixture(t, encrypted, "encrypted") },
			overall: evidence.Warning,
			check: func(t *testing.T, report Report, aspects Aspects) {
				branch := assessmentOf(t, report, BackupIncremental)
				if branch.Usable || branch.State != evidence.Warning || !reflect.DeepEqual(branch.Unusable, []UnusableReason{UnusableOffTargetTimeline}) {
					t.Fatalf("branch %+v", branch)
				}
				if aspects.Catalog.State != evidence.Warning || aspects.Coverage.State != evidence.Healthy || aspects.Encryption.State != evidence.Healthy || aspects.Encryption.Reason != "encrypted info files and manifests opened" {
					t.Fatalf("aspects %+v", aspects)
				}
			},
		},
		{
			name: "encrypted without a passphrase",
			report: func(t *testing.T) Report {
				return Analyze(context.Background(), encrypted.reader(t), encrypted.objects, Options{Stanza: "demo", Generation: 1})
			},
			overall: evidence.Unknown,
			check: func(t *testing.T, _ Report, aspects Aspects) {
				if aspects.Encryption.State != evidence.Unknown || aspects.Encryption.Reason != "the repository is encrypted and no passphrase is configured" || aspects.Catalog.State != evidence.Unknown {
					t.Fatalf("aspects %+v", aspects)
				}
			},
		},
		{
			name: "format 6 is detected and refused",
			report: func(t *testing.T) Report {
				return analyzeFixture(t, loadFixture(t, fixtureRoot, "format6"), "format6")
			},
			overall: evidence.Unknown,
			check: func(t *testing.T, report Report, aspects Aspects) {
				if report.RepositoryFormat() != 6 || aspects.Catalog.State != evidence.Unknown || aspects.WAL.State != evidence.Unknown {
					t.Fatalf("format %d, aspects %+v", report.RepositoryFormat(), aspects)
				}
			},
		},
		{
			// The roll-up question of B1, answered as Barman answers it: a
			// Barman repository with one FAILED backup is unhealthy even
			// when another backup gives it a recovery path (the catalog
			// aspect is the worst backup). So is this one: the
			// differential lost the WAL it needs to reach consistency, the
			// recovery window still exists, and the repository is
			// unhealthy, not healthy.
			name: "one unusable backup beside a recovery window",
			report: func(t *testing.T) Report {
				return analyzeFixture(t, plain.without(segmentKey(t, plain, diff.ArchiveStop)), "plain")
			},
			overall: evidence.Unhealthy,
			check: func(t *testing.T, report Report, aspects Aspects) {
				if report.Window == nil || report.State != evidence.Warning {
					t.Fatalf("the reader's own roll-up: window %+v, state %s", report.Window, report.State)
				}
				broken := assessmentOf(t, report, BackupDifferential)
				if broken.Usable || broken.State != evidence.Unhealthy || !reflect.DeepEqual(broken.Unusable, []UnusableReason{UnusableWALToConsistency}) {
					t.Fatalf("differential %+v", broken)
				}
				if aspects.Catalog.State != evidence.Unhealthy || aspects.Coverage.State == evidence.Unknown {
					t.Fatalf("aspects %+v", aspects)
				}
			},
		},
		{
			name: "split stanza: coverage is at most a warning",
			report: func(t *testing.T) Report {
				report := analyzeFixture(t, plain, "plain")
				report.AddFinding(StanzaSplitFinding("demo", "demo-wal"))
				return report
			},
			overall: evidence.Warning,
			check: func(t *testing.T, report Report, aspects Aspects) {
				if aspects.Coverage.State != evidence.Warning || !slices.ContainsFunc(report.Findings, func(finding Finding) bool { return finding.Code == FindingStanzaSplit }) {
					t.Fatalf("aspects %+v findings %+v", aspects, report.Findings)
				}
			},
		},
		{
			name: "no backup",
			report: func(t *testing.T) Report {
				report := analyzeFixture(t, plain, "plain")
				report.Backups, report.Window = nil, nil
				return report
			},
			overall: evidence.Unknown,
			check: func(t *testing.T, _ Report, aspects Aspects) {
				if aspects.Catalog.State != evidence.Unhealthy || aspects.Coverage.State != evidence.Unknown {
					t.Fatalf("aspects %+v", aspects)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := test.report(t)
			aspects := report.Aspects()
			if aspects.Overall.State != test.overall {
				t.Fatalf("overall %+v, want %s; aspects %+v", aspects.Overall, test.overall, aspects)
			}
			for index, assessment := range report.Assess() {
				if assessment.Usable != report.Backups[index].Usable || assessment.Usable != (len(assessment.Unusable) == 0) {
					t.Fatalf("assessment %+v disagrees with the reader's usability %v", assessment, report.Backups[index].Usable)
				}
			}
			test.check(t, report, aspects)
		})
	}
}

func TestCloneReportIsDeep(t *testing.T) {
	t.Parallel()
	original := analyzeFixture(t, loadFixture(t, fixtureRoot, "encrypted"), "encrypted")
	snapshot := CloneReport(original)
	clone := CloneReport(original)
	clone.Database.SystemID++
	clone.ArchiveIDs[0] = "x"
	clone.Backups[1].Backup.Reference[0] = "x"
	for key := range clone.Backups[0].Backup.Annotations {
		clone.Backups[0].Backup.Annotations[key] = "x"
	}
	clone.WAL[0].Ranges[0].Count++
	clone.Timelines[0].Edges[0].Parent++
	clone.Window.Assumptions[0] = "x"
	clone.Findings = append(clone.Findings, Finding{})
	if !reflect.DeepEqual(original, snapshot) {
		t.Fatal("mutating a clone changed the original report")
	}
}
