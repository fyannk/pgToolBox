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
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func TestRestoreCheckReadsTheTimeline(t *testing.T) {
	ctx := context.Background()
	svc := newScreenService(t)
	cases := []struct {
		name     string
		at       time.Time
		answer   string
		backupID string
	}{
		{"before the oldest backup's end", now.Add(-31 * time.Hour), "notCovered", ""},
		{"inside the older covered span", now.Add(-25 * time.Hour), "covered", "20261028T060000"},
		{"inside the missing WAL", now.Add(-10 * time.Hour), "gap", ""},
		{"inside the newer covered span", now.Add(-3 * time.Hour), "covered", "20261029T060000"},
		{"just before the end of the archive", now.Add(-10 * time.Minute), "unknown", ""},
		{"just before the hole", now.Add(-20*time.Hour - 5*time.Minute), "unknown", ""},
		// The archive ends at -2min and the agent last read it at -40s: in
		// between, the WAL had not reached the repository when it was read,
		// but a segment open at the scan may have been archived since.
		{"after the newest archived WAL, before the last read", now.Add(-90 * time.Second), "unknown", ""},
		// After the last read, the evidence says nothing about now.
		{"after the last read of the repository", now, "unknown", ""},
	}
	for _, c := range cases {
		got, err := svc.RestoreCheck(ctx, "team-a", "orders", c.at)
		if err != nil {
			t.Fatal(err)
		}
		if got.Answer != c.answer || got.BackupID != c.backupID || got.Basis != "evidence" || len(got.Caveats) < 2 || got.Text == "" {
			t.Errorf("%s: %+v", c.name, got)
		}
		// No drill is recorded for orders here: never restore-tested.
		if !slices.ContainsFunc(got.Caveats, func(c viewmodel.Phrase) bool {
			return strings.HasPrefix(c.Text, "Not restore-tested") && c.Code == "recovery.caveat.notRestoreTested"
		}) {
			t.Errorf("%s: caveats %q do not say the restore was not tested", c.name, got.Caveats)
		}
	}
	got, err := svc.RestoreCheck(ctx, "team-a", "billing", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Answer != "unknown" || got.Basis != "none" {
		t.Errorf("billing has no evidence: %+v", got)
	}
}

// timelineFixture builds evidence for the timeline tests: every path is
// toward timeline 2, starts on it, and its backup ends at position 50.
type timelineFixture struct {
	paths     []evidence.RecoveryPath
	cut       bool
	stale     bool
	scannedAt time.Time
}

func ago(d time.Duration) *time.Time { v := now.Add(-d); return &v }

func pu64(v uint64) *uint64 { return &v }

// tlPath is a path from -from to -to reaching position 100.
func tlPath(id string, from, to time.Duration, stop evidencev1alpha1.CoverageStop) evidence.RecoveryPath {
	state := evidencev1alpha1.Healthy
	switch stop {
	case evidencev1alpha1.CoverageGapLimited:
		state = evidencev1alpha1.Unhealthy
	case evidencev1alpha1.CoverageCandidateLimited:
		state = evidencev1alpha1.Warning
	case evidencev1alpha1.CoverageUnknownLimited:
		state = evidencev1alpha1.Unknown
	case evidencev1alpha1.CoverageFrontier:
		// The path reaches its frontier: the state set above stands.
	}
	return evidence.RecoveryPath{
		BackupID: id, TargetTimeline: 2, State: state, Stop: stop, LowerBoundAt: ago(from), FrontierReceiptAt: ago(to),
		StartTimeline: pu64(2), FrontierTimeline: pu64(2), FrontierPosition: pu64(100),
	}
}

func (f timelineFixture) evidence() clusterEvidence {
	scanned := f.scannedAt
	if scanned.IsZero() {
		scanned = now
	}
	snap := evidencev1alpha1.RepositoryEvidenceSnapshot{CompletedAt: &scanned}
	backups := make([]evidence.BackupItem, 0, len(f.paths))
	ids := make(map[string]bool, len(f.paths))
	for _, p := range f.paths {
		backups = append(backups, evidence.BackupItem{ID: p.BackupID, EndAt: p.LowerBoundAt, EndTimeline: pu64(2), EndPosition: pu64(50)})
		ids[p.BackupID] = true
	}
	ev := clusterEvidence{result: evidence.Result{
		Agent: "a", Snapshot: &snap, BackupsComplete: true, BackupIDs: ids, Backups: backups, RefreshInterval: 5 * time.Minute,
		Recovery: evidence.Recovery{State: evidence.RecoveryAvailable, Complete: !f.cut, Paths: f.paths},
	}}
	if f.stale {
		ev.result.Recovery.Stale, ev.result.Recovery.StaleReason = true, "the evidence agent did not answer"
	}
	return ev
}

func TestRecoveryTimelineStates(t *testing.T) {
	svc := &Service{}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}
	frontier, gap := evidencev1alpha1.CoverageFrontier, evidencev1alpha1.CoverageGapLimited
	candidate, unknownStop := evidencev1alpha1.CoverageCandidateLimited, evidencev1alpha1.CoverageUnknownLimited
	overlapping := []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, frontier), tlPath("b2", 24*time.Hour, time.Minute, frontier)}
	short := func(p evidence.RecoveryPath) evidence.RecoveryPath { p.FrontierPosition = pu64(40); return p }
	orphan := evidence.RecoveryPath{BackupID: "b1", TargetTimeline: 3, State: evidencev1alpha1.Unknown, Stop: unknownStop, LowerBoundAt: ago(48 * time.Hour)}
	type answer struct {
		at       time.Duration
		answer   string
		backupID string
	}
	cases := []struct {
		name    string
		fixture timelineFixture
		facts   recoveryFacts
		state   string
		answers []answer
	}{
		{
			"overlapping paths name the backup whose own span holds the time",
			timelineFixture{paths: overlapping},
			facts, "healthy",
			[]answer{
				// b2 ends at -24h: a time before it cannot be restored from b2.
				{30 * time.Hour, "covered", "b1"},
				{47 * time.Hour, "covered", "b1"},
				{22 * time.Hour, "covered", "b2"},
				{time.Hour, "covered", "b2"},
				{48*time.Hour - time.Minute, "unknown", ""}, // a purge may have removed b1
				{10 * time.Minute, "unknown", ""},           // the newest edge
				{0, "unknown", ""},                          // after the frontier: WAL archived since may cover it
				{49 * time.Hour, "notCovered", ""},
			},
		},
		{
			"a gap at the recent end is unhealthy",
			timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, gap)}},
			facts, "unhealthy",
			[]answer{{30 * time.Hour, "covered", "b1"}, {10 * time.Hour, "gap", ""}},
		},
		{
			// pgBackRest and Barman stop a path candidate-limited at a gap one
			// scan saw: the recent end is unconfirmed, not missing.
			"a candidate gap at the recent end warns, it is not missing WAL",
			timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, candidate)}},
			facts, "warning",
			[]answer{{30 * time.Hour, "covered", "b1"}, {10 * time.Hour, "unknown", ""}},
		},
		{
			"a candidate gap inside warns",
			timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 30*time.Hour, candidate), tlPath("b2", 24*time.Hour, time.Minute, frontier)}},
			facts, "warning",
			[]answer{{27 * time.Hour, "gap", ""}, {30*time.Hour + 5*time.Minute, "unknown", ""}, {35 * time.Hour, "covered", "b1"}},
		},
		{
			"an older timeline is not drawn",
			timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, frontier), func() evidence.RecoveryPath {
				p := tlPath("b0", 90*time.Hour, 80*time.Hour, frontier)
				p.TargetTimeline = 1
				return p
			}()}},
			facts, "healthy",
			[]answer{{85 * time.Hour, "notCovered", ""}, {30 * time.Hour, "covered", "b1"}},
		},
		{
			"no path past its backup",
			timelineFixture{paths: []evidence.RecoveryPath{{BackupID: "b1", TargetTimeline: 1, Stop: unknownStop, LowerBoundAt: ago(time.Hour)}}},
			facts, "unknown",
			[]answer{{30 * time.Minute, "unknown", ""}},
		},
		{
			// Negative: the frontier receipt is after the backup's end, but
			// the WAL stops before the backup's end segment.
			"a path stopped before its backup's end is not drawn",
			timelineFixture{paths: []evidence.RecoveryPath{short(tlPath("b1", 48*time.Hour, 20*time.Hour, gap))}},
			facts, "unknown",
			[]answer{{30 * time.Hour, "unknown", ""}},
		},
		{
			"an unknown-limited tail answers unknown, not notCovered",
			timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 20*time.Hour, unknownStop)}},
			facts, "warning",
			[]answer{{30 * time.Hour, "covered", "b1"}, {10 * time.Hour, "unknown", ""}},
		},
		{
			"a cut collection answers unknown everywhere",
			timelineFixture{paths: overlapping, cut: true},
			facts, "unknown",
			[]answer{{30 * time.Hour, "unknown", ""}, {50 * time.Hour, "unknown", ""}, {0, "unknown", ""}},
		},
		{
			"an unconnected newer timeline is not the target",
			timelineFixture{paths: append([]evidence.RecoveryPath{orphan}, overlapping...)},
			facts, "warning",
			[]answer{{30 * time.Hour, "covered", "b1"}, {time.Hour, "covered", "b2"}},
		},
		{
			"stale paths are not healthy and widen the oldest edge",
			timelineFixture{paths: overlapping, stale: true, scannedAt: now.Add(-6 * time.Hour)},
			facts, "warning",
			[]answer{{45 * time.Hour, "unknown", ""}, {30 * time.Hour, "covered", "b1"}, {0, "unknown", ""}},
		},
		{
			// The frontier is a fact about the scan: WAL archived since
			// may cover the time after it, so that span is not notCovered.
			"a fresh frontier read a while ago leaves the time since unknown",
			timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 30*time.Minute, frontier)}, scannedAt: now.Add(-10 * time.Minute)},
			facts, "healthy",
			[]answer{{20 * time.Minute, "unknown", ""}, {5 * time.Minute, "unknown", ""}, {0, "unknown", ""}, {2 * time.Hour, "covered", "b1"}},
		},
		{
			"a recent archiving failure makes the newest day unknown",
			timelineFixture{paths: overlapping},
			recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, lastFailedWAL: ago(2 * time.Hour)},
			"warning",
			[]answer{{3 * time.Hour, "unknown", ""}, {25 * time.Hour, "unknown", ""}, {30 * time.Hour, "covered", "b1"}},
		},
		{
			"an old archiving failure changes nothing",
			timelineFixture{paths: overlapping},
			recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, lastFailedWAL: ago(72 * time.Hour)},
			"healthy",
			[]answer{{3 * time.Hour, "covered", "b2"}},
		},
		{
			"a longer archive_timeout widens the newest edge",
			timelineFixture{paths: overlapping},
			recoveryFacts{now: now, archiveTimeout: 10 * time.Minute},
			"healthy",
			[]answer{{25 * time.Minute, "unknown", ""}, {35 * time.Minute, "covered", "b2"}},
		},
		{
			"archive_timeout disabled leaves the last hour unknown",
			timelineFixture{paths: overlapping},
			recoveryFacts{now: now},
			"healthy",
			[]answer{{50 * time.Minute, "unknown", ""}, {2 * time.Hour, "covered", "b2"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := svc.recoveryTimeline(c.fixture.evidence(), viewmodel.BackupSummary{}, nil, c.facts)
			if got.Basis != "evidence" || got.State != c.state {
				t.Fatalf("basis %s state %s (%s): %+v", got.Basis, got.State, got.Reason, got.Segments)
			}
			for _, a := range c.answers {
				check := restoreCheck(viewmodel.Backups{RecoveryTimeline: got}, now.Add(-a.at))
				if check.Answer != a.answer || check.BackupID != a.backupID {
					t.Errorf("-%s: %s %s (%s)", a.at, check.Answer, check.BackupID, check.Text)
				}
			}
			for _, seg := range got.Segments {
				if seg.BackupID == "b2" && seg.From.Before(now.Add(-24*time.Hour)) {
					t.Errorf("b2 is drawn before its own end: %+v", seg)
				}
			}
		})
	}

	t.Run("the operator's window is drawn as reported, never covered", func(t *testing.T) {
		first, last := now.Add(-48*time.Hour), now.Add(-6*time.Hour)
		summary := viewmodel.BackupSummary{RecoveryWindow: &viewmodel.RecoveryWindow{First: &first, Last: &last, Provenance: viewmodel.Provenance{
			Origin: viewmodel.OriginOperator, Source: "ObjectStore.status.serverRecoveryWindow",
		}}}
		got := svc.recoveryTimeline(clusterEvidence{}, summary, nil, facts)
		if got.Basis != "reported" || len(got.Segments) != 2 || got.Segments[0].Kind != segmentReported || got.State != "unknown" {
			t.Fatalf("%+v", got)
		}
		if tail := got.Segments[1]; tail.Kind != segmentUnknown || !tail.From.Equal(last) || !tail.To.Equal(now) {
			t.Errorf("the time after the last base backup is not unknown: %+v", tail)
		}
		check := restoreCheck(viewmodel.Backups{RecoveryTimeline: got}, now.Add(-7*time.Hour))
		if check.Answer != "reported" {
			t.Errorf("check = %+v", check)
		}
		// lastSuccessfulBackupTime is the last base backup, not the WAL
		// frontier: with healthy archiving the hours after it are
		// restorable, so the check must not say notCovered.
		for _, at := range []time.Time{now.Add(-time.Hour), now, now.Add(time.Hour)} {
			check = restoreCheck(viewmodel.Backups{RecoveryTimeline: got}, at)
			if check.Answer != "unknown" || !strings.Contains(check.Text, "last base backup") || check.Code == "" {
				t.Errorf("after the last base backup (%s): %+v", at, check)
			}
		}
	})
}

func TestPostgresDuration(t *testing.T) {
	//nolint:gocritic // mapKey: the padded " 90 s " is the input under test (whitespace around number and unit is accepted).
	for in, want := range map[string]time.Duration{"5min": 5 * time.Minute, "300": 300 * time.Second, "1h": time.Hour, "0": 0, " 90 s ": 90 * time.Second, "500ms": 500 * time.Millisecond, "300000000us": 5 * time.Minute} {
		if got, ok := postgresDuration(in); !ok || got != want {
			t.Errorf("%q = %s %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "abc", "-1", "5 weeks", "9999999999"} {
		if _, ok := postgresDuration(in); ok {
			t.Errorf("%q parsed", in)
		}
	}
}
