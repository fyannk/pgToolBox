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

package status

import (
	"fmt"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// TestUnusablePgBackRestBackupIsNotCovered (round 8, P2-1, probe A): the
// agent judges a pgBackRest backup's recovery path from the WAL alone, so
// a backup it rates not usable (a broken chain, a manifest that does not
// verify, pgBackRest's error flag after a page-checksum failure) can have
// a healthy path to the frontier. Its span is drawn, never as covered,
// and a restore check there is not "covered". A usable newer full keeps
// its own span covered; reasons that come from the path itself (it
// already shows them) do not degrade it twice; a Barman backup (no
// pgBackRest item) is not judged by this.
func TestUnusablePgBackRestBackupIsNotCovered(t *testing.T) {
	svc := &Service{}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}
	frontier := evidencev1alpha1.CoverageFrontier
	consistent := true
	pg := func(id string, from, to time.Duration) evidence.RecoveryPath {
		p := tlPath(id, from, to, frontier)
		p.Consistent = &consistent
		return p
	}
	type backup struct {
		usable  bool
		reasons []string
		state   evidencev1alpha1.State
		message string
	}
	for _, c := range []struct {
		name    string
		paths   []evidence.RecoveryPath
		backups map[string]*backup
		// at is when a restore is checked; answer and backup what it says.
		at       time.Duration
		answer   string
		backupID string
		state    string
		code     string
	}{
		{
			name: "probe A: an incremental whose chain is broken", paths: []evidence.RecoveryPath{pg("20261110-incr", 6*time.Hour, 2*time.Minute)},
			backups: map[string]*backup{"20261110-incr": {reasons: []string{"chain-broken"}, state: evidencev1alpha1.Unhealthy, message: "the prior backup is missing"}},
			at:      3 * time.Hour, answer: "unknown", state: "warning", code: "recovery.segment.backupNotUsable",
		},
		{
			name: "a data-checksum error: every backup error:true", paths: []evidence.RecoveryPath{pg("full-1", 30*time.Hour, 2*time.Minute), pg("full-2", 6*time.Hour, 2*time.Minute)},
			backups: map[string]*backup{
				"full-1": {reasons: []string{"backup-error"}, state: evidencev1alpha1.Unhealthy, message: "pgBackRest recorded errors (page checksums) in this backup"},
				"full-2": {reasons: []string{"backup-error"}, state: evidencev1alpha1.Unhealthy, message: "pgBackRest recorded errors (page checksums) in this backup"},
			},
			at: 3 * time.Hour, answer: "unknown", state: "warning", code: "recovery.segment.backupNotUsable",
		},
		{
			name: "an unusable older chain before a usable newer full: before the full", paths: []evidence.RecoveryPath{pg("old-full", 48*time.Hour, 2*time.Minute), pg("new-full", 6*time.Hour, 2*time.Minute)},
			backups: map[string]*backup{"old-full": {reasons: []string{"manifest-unhealthy"}, state: evidencev1alpha1.Unhealthy}, "new-full": {usable: true}},
			at:      20 * time.Hour, answer: "unknown", state: "warning", code: "recovery.segment.backupNotUsable",
		},
		{
			name: "an unusable older chain before a usable newer full: after the full", paths: []evidence.RecoveryPath{pg("old-full", 48*time.Hour, 2*time.Minute), pg("new-full", 6*time.Hour, 2*time.Minute)},
			backups: map[string]*backup{"old-full": {reasons: []string{"manifest-unhealthy"}, state: evidencev1alpha1.Unhealthy}, "new-full": {usable: true}},
			at:      3 * time.Hour, answer: "covered", backupID: "new-full", state: "warning",
		},
		{
			name: "a manifest not inspected (older than the newest 64)", paths: []evidence.RecoveryPath{pg("full-1", 30*time.Hour, 2*time.Minute)},
			backups: map[string]*backup{"full-1": {reasons: []string{"manifest-not-inspected"}, state: evidencev1alpha1.Unknown}},
			at:      10 * time.Hour, answer: "unknown", state: "warning", code: "recovery.segment.backupNotUsable",
		},
		{
			name: "no reason given", paths: []evidence.RecoveryPath{pg("full-1", 30*time.Hour, 2*time.Minute)},
			backups: map[string]*backup{"full-1": {state: evidencev1alpha1.Unknown}},
			at:      10 * time.Hour, answer: "unknown", state: "warning", code: "recovery.segment.backupNotUsable",
		},
		{
			name: "a path-only reason on a healthy frontier path: not shown by the path", paths: []evidence.RecoveryPath{pg("full-1", 30*time.Hour, 2*time.Minute)},
			backups: map[string]*backup{"full-1": {reasons: []string{"wal-unknown"}, state: evidencev1alpha1.Unknown}},
			at:      10 * time.Hour, answer: "unknown", state: "warning", code: "recovery.segment.backupNotUsable",
		},
		{
			name: "a path-only reason the path shows: drawn by its own stop", paths: []evidence.RecoveryPath{func() evidence.RecoveryPath {
				p := tlPath("full-1", 30*time.Hour, 2*time.Hour, evidencev1alpha1.CoverageGapLimited)
				p.Consistent = &consistent
				return p
			}()},
			backups: map[string]*backup{"full-1": {reasons: []string{"wal-to-target-timeline-missing"}, state: evidencev1alpha1.Unhealthy}},
			at:      10 * time.Hour, answer: "covered", backupID: "full-1", state: "unhealthy",
		},
		{
			name: "a Barman backup is not judged by usability", paths: []evidence.RecoveryPath{tlPath("barman", 30*time.Hour, 2*time.Minute, frontier)},
			at: 10 * time.Hour, answer: "covered", backupID: "barman", state: "healthy",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ev := timelineFixture{paths: c.paths}.evidence()
			for i := range ev.result.Backups {
				b := c.backups[ev.result.Backups[i].ID]
				if b == nil {
					continue
				}
				item := &ev.result.Backups[i]
				item.State, item.Reason = b.state, evidencev1alpha1.Reason{Code: "backup-" + string(b.state), Message: b.message}
				if b.usable {
					item.State = evidencev1alpha1.Healthy
				}
				item.PgBackRest = &evidence.PgBackRestItem{Type: "full", Usable: b.usable, UnusableReasons: b.reasons}
			}
			tl := svc.recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
			got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-c.at))
			if got.Answer != c.answer || got.BackupID != c.backupID || (c.code != "" && got.Code != c.code) {
				t.Fatalf("restore check %s (%s, backup %q), want %s (%s, backup %q)", got.Answer, got.Code, got.BackupID, c.answer, c.code, c.backupID)
			}
			if tl.State != c.state {
				t.Fatalf("bar state %s (%s), want %s", tl.State, tl.Reason, c.state)
			}
			if c.code != "" && c.state == "warning" && (len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.backupNotUsable" || tl.Reasons[0].Params["reason"] == "") {
				t.Fatalf("bar reasons %+v do not say the backup is not usable", tl.Reasons)
			}
			if tl.NewestCoveredTarget != nil && c.answer != "covered" && c.name != "an unusable older chain before a usable newer full: before the full" {
				t.Fatalf("newest covered target %v with no usable backup", tl.NewestCoveredTarget)
			}
		})
	}
}

// TestPgBackRestPathWhoseBackupWasCutIsNotCovered (round 9, PG P2): the
// console keeps the newest 1,000 backup items but up to 2,000 recovery
// paths, and BackupsComplete stays true. With 1,001 pgBackRest backups,
// the oldest one's path has no item: its usability is not known, so its
// span is drawn, never as covered, and the bar says why. A listed usable
// backup's span stays covered.
func TestPgBackRestPathWhoseBackupWasCutIsNotCovered(t *testing.T) {
	svc := &Service{}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}
	consistent := true
	oldest := tlPath("full-0000", 48*time.Hour, 2*time.Minute, evidencev1alpha1.CoverageFrontier)
	oldest.Consistent = &consistent
	newest := tlPath("full-1000", 6*time.Hour, 2*time.Minute, evidencev1alpha1.CoverageFrontier)
	newest.Consistent = &consistent
	ev := timelineFixture{paths: []evidence.RecoveryPath{oldest, newest}}.evidence()
	// The format is the snapshot identity's (the console's routing in
	// evidence fetch), not whether the details carry a pgBackRest summary (round
	// 10, PG P3): none here.
	ev.result.Snapshot.Identity.Repository.Format = evidencev1alpha1.FormatPGBackRest
	items := make([]evidence.BackupItem, 0, 1001)
	for i := range 1001 {
		end := now.Add(-48*time.Hour + time.Duration(i)*2*time.Minute)
		if i == 1000 {
			end = now.Add(-6 * time.Hour)
		}
		id := fmt.Sprintf("full-%04d", i)
		items = append(items, evidence.BackupItem{
			ID: id, EndAt: &end, EndTimeline: pu64(2), EndPosition: pu64(50), State: evidencev1alpha1.Healthy,
			PgBackRest: &evidence.PgBackRestItem{Type: "full", Usable: true},
		})
	}
	// The console keeps the newest 1,000 (evidence fetch newestItems):
	// the oldest backup, whose path is still served, is cut.
	ev.result.Backups = items[1:]
	for _, item := range items {
		ev.result.BackupIDs[item.ID] = true
	}
	tl := svc.recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
	b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
	if got := restoreCheck(b, now.Add(-20*time.Hour)); got.Answer == "covered" || got.Code != "recovery.segment.backupNotListed" {
		t.Fatalf("restore check on the cut backup's span: %s (%s), want not covered, backupNotListed", got.Answer, got.Code)
	}
	if got := restoreCheck(b, now.Add(-3*time.Hour)); got.Answer != "covered" || got.BackupID != "full-1000" {
		t.Fatalf("restore check on the listed backup's span: %s (%s, %q), want covered by full-1000", got.Answer, got.Code, got.BackupID)
	}
	if tl.State != "warning" || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.backupNotListed" || tl.Reasons[0].Params["backupId"] != "full-0000" {
		t.Fatalf("bar %s, reasons %+v, want a warning naming the cut backup", tl.State, tl.Reasons)
	}
	// A Barman repository has no usability items: a path without one is
	// judged by its WAL alone, as before, whatever its details carry.
	ev.result.Snapshot.Identity.Repository.Format = evidencev1alpha1.FormatBarmanCloud
	ev.result.Snapshot.Details.PGBackRest = &evidencev1alpha1.PGBackRestSummary{}
	tl = svc.recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
	if got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-20*time.Hour)); got.Answer != "covered" {
		t.Fatalf("a Barman path without an item: %s (%s), want covered", got.Answer, got.Code)
	}
}
