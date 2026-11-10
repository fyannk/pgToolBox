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
	"slices"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

func findingCodes(report Report) []FindingCode {
	codes := make([]FindingCode, 0, len(report.Findings))
	for _, finding := range report.Findings {
		if !slices.Contains(codes, finding.Code) {
			codes = append(codes, finding.Code)
		}
	}
	return codes
}

func backupOfType(t testing.TB, report Report, kind BackupType) BackupReport {
	t.Helper()
	for _, backup := range report.Backups {
		if backup.Backup.Type == kind {
			return backup
		}
	}
	t.Fatalf("no %s backup", kind)
	return BackupReport{}
}

// segmentKey returns the listed key of a segment by name.
func segmentKey(t testing.TB, repository *fixtureRepository, name string) string {
	t.Helper()
	for _, object := range repository.objects {
		if strings.Contains(object.Key, "/"+name+"-") {
			return object.Key
		}
	}
	t.Fatalf("segment %s is not listed", name)
	return ""
}

func nextWAL(t testing.TB, name string, delta uint64) string {
	t.Helper()
	timeline, position, err := ParseWALName(name, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	next, err := WALName(timeline, position+delta, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestAnalyzeRealRepositories(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"plain", "encrypted"} {
		t.Run(variant, func(t *testing.T) {
			t.Parallel()
			repository := loadFixture(t, fixtureRoot, variant)
			report := analyzeFixture(t, repository, variant)
			if report.State != evidence.Healthy || len(report.Findings) != 0 || report.Window == nil {
				t.Fatalf("state %s %q, findings %+v", report.State, report.Reason, report.Findings)
			}
			if report.SegmentSize != 16<<20 || report.SegmentSizeSource != SegmentSizeFromLSN || report.Encrypted != (variant == "encrypted") {
				t.Fatalf("segment size %d from %q, encrypted %v", report.SegmentSize, report.SegmentSizeSource, report.Encrypted)
			}
			full := backupOfType(t, report, BackupFull)
			if report.Window.OldestBackup != full.Backup.Label || !report.Window.Start.Equal(full.Backup.TimestampStop) || report.Window.EndTimeline != 2 {
				t.Fatalf("window %+v", report.Window)
			}
			for _, backup := range report.Backups {
				if backup.ChainState != evidence.Healthy || backup.Manifest.State != evidence.Healthy || !backup.Path.Consistent {
					t.Fatalf("%s: chain %s, manifest %s, path %+v", backup.Backup.Label, backup.ChainState, backup.Manifest.State, backup.Path)
				}
			}
			if len(report.Timelines) != 1 || report.Timelines[0].Timeline != 2 || report.Timelines[0].State != evidence.Healthy || report.Timelines[0].Edges[0].Parent != 1 {
				t.Fatalf("timelines %+v", report.Timelines)
			}
			for _, key := range repository.opened {
				if !strings.HasSuffix(key, ".info") && !strings.HasSuffix(key, ".info.copy") && !strings.HasSuffix(key, "backup.manifest") && !strings.HasSuffix(key, "backup.manifest.copy") && !strings.HasSuffix(key, ".history") {
					t.Fatalf("opened %s", key)
				}
			}
		})
	}
}

// The repository on repo2 was not the one restored: its incremental on
// timeline 1 ended after timeline 2 branched off, so it recovers only on
// timeline 1, and the window does not count it.
func TestAnalyzeBackupOnAnAbandonedBranch(t *testing.T) {
	t.Parallel()
	report := analyzeFixture(t, loadFixture(t, fixtureRoot, "encrypted"), "encrypted")
	branch := backupOfType(t, report, BackupIncremental)
	if !branch.Path.OffTarget || branch.Path.TargetTimeline != 1 || branch.Path.State != evidence.Warning || branch.Usable {
		t.Fatalf("path %+v usable %v", branch.Path, branch.Usable)
	}
	if report.Window.UsableBackups != 3 {
		t.Fatalf("window %+v", report.Window)
	}
}

func TestAnalyzeSyntheticDamage(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	encrypted := loadFixture(t, fixtureRoot, "encrypted")
	info, _ := loadInfoPair(t, "plain")
	full, diff := info.Backups[0], info.Backups[1]
	backupInfo := plain.content["backup/demo/backup.info"]
	withoutDiff := rewriteINI(t, backupInfo, func(body string) string {
		lines := strings.Split(body, "\n")
		return strings.Join(slices.DeleteFunc(lines, func(line string) bool { return strings.HasPrefix(line, diff.Label+"=") }), "\n")
	})
	otherCopy := rewriteINI(t, backupInfo, func(body string) string {
		return strings.Replace(body, `"backup-info-size":`, `"backup-info-size":1`, 1)
	})
	formatSix := func(data []byte) []byte { return append([]byte("PGBR006_"), data[8:]...) }
	corruptManifest := bytes.Replace(plain.content[ManifestKey("demo", diff.Label)], []byte("backup-type=\"diff\""), []byte("backup-type=\"diff\" "), 1)

	tests := []struct {
		name       string
		repository *fixtureRepository
		variant    string
		options    func(*Options)
		state      evidence.State
		codes      []FindingCode
		check      func(*testing.T, Report)
	}{
		{
			name:       "a WAL gap after the full backup",
			repository: plain.without(segmentKey(t, plain, nextWAL(t, full.ArchiveStop, 1))),
			variant:    "plain", state: evidence.Warning, codes: []FindingCode{FindingWALGap},
			check: func(t *testing.T, report Report) {
				first := backupOfType(t, report, BackupFull)
				if first.Usable || first.Path.State != evidence.Warning || !first.Path.Consistent || first.Path.Stop != CoverageCandidateLimited || first.Path.FrontierWAL != full.ArchiveStop {
					t.Fatalf("full path %+v usable %v", first.Path, first.Usable)
				}
				if report.Window.OldestBackup != diff.Label {
					t.Fatalf("window starts at %s", report.Window.OldestBackup)
				}
			},
		},
		{
			name:       "a WAL gap inside a backup",
			repository: plain.without(segmentKey(t, plain, diff.ArchiveStop)),
			variant:    "plain", state: evidence.Warning, codes: []FindingCode{FindingWALGap},
			check: func(t *testing.T, report Report) {
				backup := backupOfType(t, report, BackupDifferential)
				if backup.Path.Consistent || backup.Usable || backup.Path.State != evidence.Unhealthy {
					t.Fatalf("diff path %+v", backup.Path)
				}
				// The incremental needs the differential's files, not its WAL.
				if !backupOfType(t, report, BackupIncremental).Usable {
					t.Fatal("the incremental is not usable")
				}
				if !slices.ContainsFunc(report.Findings, func(finding Finding) bool {
					return finding.Code == FindingWALGap && finding.Subject == diff.Label && finding.Severity == SeverityError
				}) {
					t.Fatalf("findings %+v", report.Findings)
				}
			},
		},
		{
			name:       "a .copy that disagrees",
			repository: plain.with("backup/demo/backup.info.copy", otherCopy),
			variant:    "plain", state: evidence.Warning, codes: []FindingCode{FindingCopyMismatch},
		},
		{
			name:       "a main file that does not verify",
			repository: plain.with("backup/demo/backup.info", bytes.Replace(backupInfo, []byte(`"db-id":1`), []byte(`"db-id":2`), 1)),
			variant:    "plain", state: evidence.Warning, codes: []FindingCode{FindingChecksumMismatch, FindingCopyMismatch},
			check: func(t *testing.T, report Report) {
				if report.BackupInfo.Used != SourceCopy {
					t.Fatalf("used %s", report.BackupInfo.Used)
				}
			},
		},
		{
			name:       "a missing prior",
			repository: plain.with("backup/demo/backup.info", withoutDiff).with("backup/demo/backup.info.copy", withoutDiff),
			variant:    "plain", state: evidence.Warning, codes: []FindingCode{FindingMissingPrior, FindingMissingReference},
			check: func(t *testing.T, report Report) {
				for _, backup := range report.Backups {
					if (backup.Backup.Type == BackupFull) != (backup.ChainState == evidence.Healthy) || (backup.Backup.Type == BackupFull) != backup.Usable {
						t.Fatalf("%s: chain %s usable %v", backup.Backup.Label, backup.ChainState, backup.Usable)
					}
				}
				if report.Window.OldestBackup != full.Label || report.Window.NewestBackup != full.Label {
					t.Fatalf("window %+v", report.Window)
				}
			},
		},
		{
			name: "a format 6 header",
			repository: encrypted.with("backup/demo/backup.info", formatSix(encrypted.content["backup/demo/backup.info"])).
				with("backup/demo/backup.info.copy", formatSix(encrypted.content["backup/demo/backup.info.copy"])),
			variant: "encrypted", state: evidence.Unknown, codes: []FindingCode{FindingRepositoryFormatUnsupported},
		},
		{
			name:       "a missing manifest",
			repository: plain.without(ManifestKey("demo", diff.Label), ManifestKey("demo", diff.Label)+".copy"),
			variant:    "plain", state: evidence.Warning, codes: []FindingCode{FindingManifestMissing},
			check: func(t *testing.T, report Report) {
				for _, backup := range report.Backups {
					if backup.Usable != (backup.Backup.Type == BackupFull) {
						t.Fatalf("%s usable %v", backup.Backup.Label, backup.Usable)
					}
				}
			},
		},
		{
			name:       "a manifest that does not verify",
			repository: plain.with(ManifestKey("demo", diff.Label), corruptManifest),
			variant:    "plain", state: evidence.Warning, codes: []FindingCode{FindingChecksumMismatch, FindingCopyMismatch},
			check: func(t *testing.T, report Report) {
				if backup := backupOfType(t, report, BackupDifferential); backup.Manifest.Pair.Used != SourceCopy || !backup.Usable {
					t.Fatalf("manifest %+v", backup.Manifest)
				}
			},
		},
		{
			name:       "no timeline history",
			repository: plain.without("archive/demo/17-1/00000002.history"),
			variant:    "plain", state: evidence.Warning, codes: []FindingCode{FindingWALUnknown},
			check: func(t *testing.T, report Report) {
				for _, backup := range report.Backups {
					onTwo := strings.HasPrefix(backup.Backup.ArchiveStart, "00000002")
					if backup.Usable != onTwo || (!onTwo && backup.Path.State != evidence.Unknown) {
						t.Fatalf("%s: path %+v", backup.Backup.Label, backup.Path)
					}
				}
			},
		},
		{
			name: "an independent full backup beyond the manifest bound",
			repository: plain.with("backup/demo/backup.info", rewriteINI(t, backupInfo, func(body string) string {
				_, after, _ := strings.Cut(body, "\n"+full.Label+"=")
				line, _, _ := strings.Cut(full.Label+"="+after, "\n")
				older := strings.Replace(strings.Replace(line, full.Label, "20200101-000000F", 1), `"backup-timestamp-start":17`, `"backup-timestamp-start":16`, 1)
				older = strings.Replace(older, `"backup-timestamp-stop":17`, `"backup-timestamp-stop":16`, 1)
				return strings.Replace(body, "[backup:current]\n", "[backup:current]\n"+older+"\n", 1)
			})).without("backup/demo/backup.info.copy"),
			variant: "plain", options: func(options *Options) { options.MaxManifests = 1 },
			state: evidence.Warning, codes: []FindingCode{FindingCopyMismatch},
			check: func(t *testing.T, report Report) {
				old := report.Backups[0]
				if old.Backup.Label != "20200101-000000F" || old.Manifest.Inspected || old.Manifest.State != evidence.Unknown || old.Usable {
					t.Fatalf("old full %+v", old.Manifest)
				}
				for _, backup := range report.Backups[1:] {
					if !backup.Manifest.Inspected {
						t.Fatalf("%s was not inspected", backup.Backup.Label)
					}
				}
			},
		},
		{
			name:       "an encrypted repository without its passphrase",
			repository: encrypted, variant: "plain", state: evidence.Unknown, codes: []FindingCode{FindingInfoUnreadable},
		},
		{
			name:       "an encrypted repository with a wrong passphrase",
			repository: encrypted, variant: "encrypted", options: func(options *Options) { options.Passphrase = []byte("wrong") },
			state: evidence.Unknown, codes: []FindingCode{FindingInfoUnreadable},
		},
		{
			name:       "WAL under another stanza",
			repository: plain, variant: "plain", options: func(options *Options) { options.ArchiveStanza = "other" },
			state: evidence.Unknown, codes: []FindingCode{FindingInfoUnreadable},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := Options{Stanza: "demo", Passphrase: fixturePassphraseFor(test.variant), Generation: 1}
			if test.options != nil {
				test.options(&options)
			}
			report := Analyze(context.Background(), test.repository.reader(t), test.repository.objects, options)
			if report.State != test.state || !slices.Equal(findingCodes(report), test.codes) {
				t.Fatalf("state %s %q, findings %+v", report.State, report.Reason, report.Findings)
			}
			if test.check != nil {
				test.check(t, report)
			}
			for _, finding := range report.Findings {
				if strings.Contains(finding.Message, fixturePassphrase) || strings.Contains(finding.Subject, fixturePassphrase) {
					t.Fatal("a finding carries the passphrase")
				}
			}
		})
	}
}

func TestAnalyzeRefusesInvalidStanzaNames(t *testing.T) {
	t.Parallel()
	for _, options := range []Options{{Stanza: ""}, {Stanza: "../x"}, {Stanza: "demo", ArchiveStanza: "a/b"}} {
		if report := Analyze(context.Background(), nil, nil, options); report.State != evidence.Unknown {
			t.Fatalf("Analyze(%+v) = %s", options, report.State)
		}
	}
}

func TestAnalyzeWithoutBackups(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	empty := rewriteINI(t, plain.content["backup/demo/backup.info"], func(body string) string {
		start := strings.Index(body, "[backup:current]")
		end := strings.Index(body, "[db]")
		return body[:start] + body[end:]
	})
	repository := plain.with("backup/demo/backup.info", empty).with("backup/demo/backup.info.copy", empty)
	report := analyzeFixture(t, repository, "plain")
	if report.State != evidence.Unhealthy || report.Window != nil || report.SegmentSizeSource != SegmentSizeAssumed {
		t.Fatalf("state %s %q window %+v source %q", report.State, report.Reason, report.Window, report.SegmentSizeSource)
	}
}

// TestGapStopFollowsTheGapLifecycle: as the wire contract says, a path stops
// candidate-limited at a gap seen by one scan, and gap-limited only once a
// second complete scan confirmed it (or the WAL is proven lost).
func TestGapStopFollowsTheGapLifecycle(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	info, _ := loadInfoPair(t, "plain")
	full, diff := info.Backups[0], info.Backups[1]
	for name, test := range map[string]struct {
		missing string
		label   string
		state   evidence.State
		stop    CoverageStop
	}{
		// The full's walk stops at the gap after its consistency point.
		"gap after the backup": {missing: nextWAL(t, full.ArchiveStop, 1), label: full.Label},
		// The differential's own WAL is lost whatever the gap's status:
		// pgBackRest archived it before finishing the backup.
		"gap inside the backup": {missing: diff.ArchiveStop, label: diff.Label, state: evidence.Unhealthy, stop: CoverageGapLimited},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repository := plain.without(segmentKey(t, plain, test.missing))
			first := Analyze(context.Background(), repository.reader(t), repository.objects, Options{Stanza: "demo", Generation: 1})
			second := Analyze(context.Background(), repository.reader(t), repository.objects, Options{Stanza: "demo", Generation: 2, Previous: &first})
			for _, scan := range []struct {
				report Report
				state  evidence.State
				stop   CoverageStop
			}{
				{report: first, state: evidence.Warning, stop: CoverageCandidateLimited},
				{report: second, state: evidence.Unhealthy, stop: CoverageGapLimited},
			} {
				if test.stop != "" {
					scan.state, scan.stop = test.state, test.stop
				}
				var path RecoveryPath
				for _, backup := range scan.report.Backups {
					if backup.Backup.Label == test.label {
						path = backup.Path
					}
				}
				if path.State != scan.state || path.Stop != scan.stop {
					t.Fatalf("generation %d: path %+v", scan.report.WAL[0].Gaps[0].LastObservedGeneration, path)
				}
			}
			if first.WAL[0].Gaps[0].Status != GapCandidate || second.WAL[0].Gaps[0].Status != GapConfirmed {
				t.Fatalf("gaps %+v then %+v", first.WAL[0].Gaps, second.WAL[0].Gaps)
			}
		})
	}
}
