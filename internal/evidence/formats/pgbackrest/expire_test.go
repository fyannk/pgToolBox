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
	"os"
	"slices"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

// expireRetention is what the generator's expire ran with
// (hack/evidence/pgbackrest-generator: repo1-retention-full=1).
var expireRetention = Retention{Full: 1}

// checkRetentionFollowsExpire reads the repository a real pgBackRest
// 2.60.0 left before and after `expire --repo1-retention-full=1` on two full
// backups, and evaluates it as the agent does (ApplyRetention): before, the
// older full and its WAL are what expire removes (RetentionNotApplied);
// after, the stanza matches the retention.
func checkRetentionFollowsExpire(t *testing.T, root string) {
	t.Helper()
	analyze := func(variant string) Report {
		report := analyzeFixture(t, loadFixture(t, root, variant), variant)
		if report.State != evidence.Healthy || report.Window == nil {
			t.Fatalf("%s: state %s %q, findings %+v", variant, report.State, report.Reason, report.Findings)
		}
		report.ApplyRetention(expireRetention, nil)
		return report
	}
	before, after := analyze("retention-before"), analyze("retention-after")
	if len(before.Backups) != 2 || len(after.Backups) != 1 {
		t.Fatalf("backups before %d, after %d; want two fulls, then one", len(before.Backups), len(after.Backups))
	}
	newest := after.Backups[0].Backup
	older := slices.IndexFunc(before.Backups, func(backup BackupReport) bool { return backup.Backup.Label != newest.Label })
	if newest.Type != BackupFull || older < 0 || before.Backups[older].Backup.Type != BackupFull {
		t.Fatalf("before %+v, after %+v", before.Backups, after.Backups)
	}

	if before.Retention.State != evidence.Warning || !slices.Contains(retentionCodes(before), FindingRetentionNotApplied) || slices.Contains(retentionCodes(before), FindingRetentionShortfall) {
		t.Fatalf("before expire: retention %s %q, findings %+v", before.Retention.State, before.Retention.Reason, before.Retention.Findings)
	}
	subjects := map[string]bool{}
	for _, finding := range before.Retention.Findings {
		subjects[finding.Subject] = true
	}
	if !subjects[before.Backups[older].Backup.Label] {
		t.Fatalf("before expire: no finding names the older full %s: %+v", before.Backups[older].Backup.Label, before.Retention.Findings)
	}
	if aspects := before.Aspects(); aspects.Retention.State != evidence.Warning {
		t.Fatalf("before expire: aspects %+v", aspects)
	}

	if after.Retention.State != evidence.Healthy || len(after.Retention.Findings) != 0 || after.State != evidence.Healthy {
		t.Fatalf("after expire: retention %s %q, findings %+v, report %s", after.Retention.State, after.Retention.Reason, after.Retention.Findings, after.State)
	}
	if aspects := after.Aspects(); aspects.Retention.State != evidence.Healthy {
		t.Fatalf("after expire: aspects %+v", aspects)
	}
	// Expire kept the WAL from the kept full's archive start, nothing older.
	if first := after.WAL[0].Ranges[0].First; first != newest.ArchiveStart {
		t.Fatalf("after expire the WAL starts at %s, want %s", first, newest.ArchiveStart)
	}
}

func retentionCodes(report Report) []FindingCode {
	codes := make([]FindingCode, 0, len(report.Retention.Findings))
	for _, finding := range report.Retention.Findings {
		codes = append(codes, finding.Code)
	}
	return codes
}

func TestRetentionFollowsARealExpire(t *testing.T) {
	t.Parallel()
	checkRetentionFollowsExpire(t, fixtureRoot)
}

// TestRegeneratedRetentionFollowsExpire is the same check on the tree that
// make test-pgbackrest-fixtures just generated.
func TestRegeneratedRetentionFollowsExpire(t *testing.T) {
	root := os.Getenv("PGBACKREST_REGENERATED_FIXTURES")
	if root == "" {
		t.Skip("PGBACKREST_REGENERATED_FIXTURES is not set; make test-pgbackrest-fixtures sets it")
	}
	checkRetentionFollowsExpire(t, root)
}
