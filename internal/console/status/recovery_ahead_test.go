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
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// aheadPath is a path from a backup on timeline start, from -from,
// toward timeline 3, its frontier on timeline 3 three minutes ago.
func aheadPath(id string, from time.Duration, start uint64) evidence.RecoveryPath {
	p := tlPath(id, from, 3*time.Minute, evidencev1alpha1.CoverageFrontier)
	p.TargetTimeline, p.StartTimeline, p.FrontierTimeline = 3, pu64(start), pu64(3)
	return p
}

// TestRepositoryTimelineAheadOfTheLiveOne (round 12, PG P3-2, the
// reviewer's probe): the repository's newest timeline is 3 and the live
// primary writes timeline 2: another cluster archives here (a cluster
// restored by PITR, or a promoted replica cluster, with the empty-archive
// check skipped). A default restore follows timeline 3, so the restore
// points past the fork are not covered: from the newest lower bound of a
// backup on the live line (its on-target path ends it before the fork),
// and the bar is a warning that names both timelines.
func TestRepositoryTimelineAheadOfTheLiveOne(t *testing.T) {
	g := failoverGathered("db-1", time.Time{}, map[string][]failoverReading{})
	live := func(wal string) {
		g.swept = sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
			"db-1": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{IsPrimary: true, CurrentWAL: wal, ObservedAt: now}},
		}}
	}
	paths := []evidence.RecoveryPath{aheadPath("b0", 72*time.Hour, 2), aheadPath("b1", 48*time.Hour, 2), aheadPath("b2", 10*time.Hour, 3)}

	live("000000020000000000000007")
	b := failoverTimeline(g, paths...)
	tl := b.RecoveryTimeline
	if tl.State != string(evidencev1alpha1.Warning) || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.repositoryAheadOfLive" ||
		tl.Reasons[0].Params["timeline"] != "3" || tl.Reasons[0].Params["liveTimeline"] != "2" {
		t.Fatalf("state %s, reasons %+v, want a warning that timeline 3 is ahead of 2", tl.State, tl.Reasons)
	}
	if c := restoreCheck(b, now.Add(-60*time.Hour)); c.Answer != "covered" || c.BackupID != "b0" {
		t.Errorf("before b1's end, on the shared history: %s (%s), want covered by b0", c.Answer, c.Code)
	}
	for _, d := range []time.Duration{47 * time.Hour, 25 * time.Hour, 5 * time.Hour, 30 * time.Minute} {
		if c := restoreCheck(b, now.Add(-d)); c.Answer == "covered" || c.Code != "recovery.segment.repositoryAheadOfLive" {
			t.Errorf("now-%s, maybe past the fork: %s (%s), want unknown", d, c.Answer, c.Code)
		}
	}
	if tl.NewestCoveredTarget != nil && tl.NewestCoveredTarget.After(now.Add(-48*time.Hour)) {
		t.Errorf("newest covered target %s past the newest live-line backup", tl.NewestCoveredTarget)
	}

	// No backup on the live line: nothing is placed before the fork.
	b = failoverTimeline(g, aheadPath("b2", 10*time.Hour, 3))
	if c := restoreCheck(b, now.Add(-9*time.Hour)); c.Answer == "covered" {
		t.Errorf("only a timeline-3 backup: %+v", c)
	}

	// The live primary writes the target timeline, or the live one is
	// not known: nothing is said.
	for _, wal := range []string{"000000030000000000000007", ""} {
		live(wal)
		if wal == "" {
			g.swept = sweep.Snapshot{}
		}
		tl := failoverTimeline(g, paths...).RecoveryTimeline
		for _, r := range tl.Reasons {
			if r.Code == "recovery.reason.repositoryAheadOfLive" {
				t.Errorf("live WAL %q: %+v", wal, tl.Reasons)
			}
		}
		if segmentCoded(tl, "recovery.segment.repositoryAheadOfLive") != nil {
			t.Errorf("live WAL %q: segments marked ahead", wal)
		}
	}
}

// TestPostFailoverLagWithNoPrimaryToAct (round 12, CNPG P3-1, the
// reviewer's probe): after a failover the repository holds no WAL of the
// live timeline, and the Cluster is hibernated or its primary was not
// read (fenced, unreachable). The bar does not advise pg_switch_wal() on
// a primary that is not running or not read: hibernated says the
// hibernation (unknown); an unread primary keeps the lag warning without
// the advice.
func TestPostFailoverLagWithNoPrimaryToAct(t *testing.T) {
	p1 := now.Add(-10 * time.Minute)
	g := failoverGathered("db-2", p1, map[string][]failoverReading{
		"db-1": {{at: p1.Add(-20 * time.Minute), files: 0, timeline: 1}},
		"db-2": {{at: p1.Add(time.Minute), timeline: 2}},
	})
	receipt := p1.Add(-2 * time.Minute)
	p := tlPath("b1", 48*time.Hour, 0, evidencev1alpha1.CoverageFrontier)
	p.TargetTimeline, p.StartTimeline, p.FrontierTimeline, p.FrontierReceiptAt = 1, pu64(1), pu64(1), &receipt
	ev := timelineFixture{paths: []evidence.RecoveryPath{p}}.evidence()
	for _, c := range []struct {
		name               string
		hibernated, unread bool
		state, code        string
	}{
		{"read", false, false, string(evidencev1alpha1.Warning), "recovery.reason.liveTimelineNotArchived"},
		{"hibernated", true, true, string(evidencev1alpha1.Unknown), "recovery.reason.archiverHibernated"},
		{"primary not read", false, true, string(evidencev1alpha1.Warning), "recovery.reason.liveTimelineNotArchivedPrimaryUnread"},
	} {
		facts := failoverFacts(g)
		if facts.liveTimeline != 2 {
			t.Fatalf("live timeline %d", facts.liveTimeline)
		}
		facts.hibernated, facts.archiverUnread = c.hibernated, c.unread
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
		if tl.State != c.state || len(tl.Reasons) == 0 || tl.Reasons[0].Code != c.code {
			t.Errorf("%s: state %s, reasons %+v, want %s %s", c.name, tl.State, tl.Reasons, c.state, c.code)
		}
		if advises := strings.Contains(tl.Reason, "pg_switch_wal()"); advises != (c.name == "read") {
			t.Errorf("%s: advises pg_switch_wal() %v: %q", c.name, advises, tl.Reason)
		}
	}
}

// TestPostFailoverLagWhileArchivingFails (round 13, CNPG P3-1, the
// reviewer's probe), end to end through recoveryFactsOf: after a failover
// to db-2 the repository holds no WAL of timeline 2. Read with
// ContinuousArchiving True, the bar keeps the lag and its pg_switch_wal()
// advice. Not read with the condition True, it says the primary was not
// read. Not read with the condition False (archiving on the new primary
// broke, then it was fenced), it says archiving fails and gives no advice
// to run anything on the primary: the archiving reason is CNPG's own, not
// PrimaryUnread, and no readable instance reports the failure.
func TestPostFailoverLagWhileArchivingFails(t *testing.T) {
	p1 := now.Add(-10 * time.Minute)
	receipt := p1.Add(-2 * time.Minute)
	p := tlPath("b1", 48*time.Hour, 0, evidencev1alpha1.CoverageFrontier)
	p.TargetTimeline, p.StartTimeline, p.FrontierTimeline, p.FrontierReceiptAt = 1, pu64(1), pu64(1), &receipt
	ev := timelineFixture{paths: []evidence.RecoveryPath{p}}.evidence()
	for _, c := range []struct {
		name      string
		read      bool
		condition metav1.ConditionStatus
		reason    string
		code      string
		advises   bool
	}{
		{"read, archiving succeeds", true, metav1.ConditionTrue, "ContinuousArchivingSuccess", "recovery.reason.liveTimelineNotArchived", true},
		{"not read, archiving succeeded", false, metav1.ConditionTrue, "ContinuousArchivingSuccess", "recovery.reason.liveTimelineNotArchivedPrimaryUnread", false},
		// The condition cannot change while nothing archives: the last
		// attempt failed, with the condition's age (round 14, CNPG P3-b).
		{"not read, archiving fails", false, metav1.ConditionFalse, "ContinuousArchivingFailing", "recovery.reason.archivingFailedPrimaryUnread", false},
	} {
		g := failoverGathered("db-2", p1, map[string][]failoverReading{
			"db-1": {{at: p1.Add(-20 * time.Minute), files: 0, timeline: 1}},
			"db-2": {{at: p1.Add(time.Minute), timeline: 2}},
		})
		g.cluster.Status.Conditions = []metav1.Condition{{Type: cnpgstatus.ConditionContinuousArchiving, Status: c.condition, Reason: c.reason, LastTransitionTime: metav1.NewTime(p1)}}
		g.swept = sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{}}
		if c.read {
			archived := now.Add(-time.Minute)
			g.swept.Instances["db-2"] = sweep.Instance{State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{
				IsPrimary: true, IsArchivingWAL: true, CurrentWAL: "000000020000000000000007", LastArchivedWALTime: &archived, ObservedAt: now.Add(-10 * time.Second),
			}}
		}
		reading := cnpgstatus.Archiving(g.cluster, primaryArchiver(g))
		arch := viewmodel.Archiving{Health: health(reading.Class), Reason: reading.Reason, ConditionStatus: reading.ConditionStatus}
		facts := recoveryFactsOf(g, arch, now)
		if facts.liveTimeline != 2 || facts.primaryUnread == c.read {
			t.Fatalf("%s: live timeline %d, primary unread %v", c.name, facts.liveTimeline, facts.primaryUnread)
		}
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{Archiving: arch}, nil, facts)
		if tl.State != string(evidencev1alpha1.Warning) || len(tl.Reasons) == 0 || tl.Reasons[0].Code != c.code {
			t.Errorf("%s (archiving %s): state %s, reasons %+v, want a warning %s", c.name, reading.Reason, tl.State, tl.Reasons, c.code)
		}
		if advises := strings.Contains(tl.Reason, "pg_switch_wal()"); advises != c.advises {
			t.Errorf("%s: advises pg_switch_wal() %v: %q", c.name, advises, tl.Reason)
		}
		// The SPA's hint follows the same reasons (round 14, DBA P2-1):
		// no primary read, nothing to run on it.
		if want := map[bool]string{true: "", false: viewmodel.EdgeAdviceNone}[c.advises]; tl.EdgeAdvice != want {
			t.Errorf("%s: edgeAdvice %q, want %q", c.name, tl.EdgeAdvice, want)
		}
		if c.condition == metav1.ConditionFalse {
			if since := tl.Reasons[0].Params["sinceAt"]; since != p1.Format(time.RFC3339) || !strings.Contains(tl.Reason, "last archive attempt") || strings.Contains(tl.Reason, "is failing") {
				t.Errorf("%s: %q (since %q), want the last attempt with the condition's age, not the present tense", c.name, tl.Reason, since)
			}
		}
	}
}

// TestRepositoryAheadSiblingTimelineBackup (round 13, CNPG P3-2): the
// current primary writes timeline 4 (1, 2, 4) and another cluster archives
// timelines 3 and 5 (2, 3, 5) into the same repository. b0, on timeline 2
// before both forks, is on the live line: the agent evaluated a path from
// it toward 4. b1, on timeline 3, is not, though 3 is below 4: the start
// of the unknown span stays at b0's lower bound, not b1's, so the time
// between them, after the fork from 2, is not covered. When the agent
// gives no path toward 4 at all (pgBackRest publishes one per backup), no
// backup but one on 4 itself counts and the whole window is unknown.
func TestRepositoryAheadSiblingTimelineBackup(t *testing.T) {
	g := failoverGathered("db-1", time.Time{}, map[string][]failoverReading{})
	g.swept = sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
		"db-1": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{IsPrimary: true, CurrentWAL: "000000040000000000000007", ObservedAt: now}},
	}}
	toward := func(id string, from time.Duration, start, target uint64) evidence.RecoveryPath {
		p := tlPath(id, from, 3*time.Minute, evidencev1alpha1.CoverageFrontier)
		p.TargetTimeline, p.StartTimeline, p.FrontierTimeline = target, pu64(start), pu64(target)
		return p
	}
	towardLive := toward("b0", 72*time.Hour, 2, 4)
	towardLive.OffTarget = true // a sibling of timeline 5, the one a default restore follows
	paths := []evidence.RecoveryPath{toward("b0", 72*time.Hour, 2, 5), towardLive, toward("b1", 48*time.Hour, 3, 5), toward("b2", 10*time.Hour, 5, 5)}
	b := failoverTimeline(g, paths...)
	if tl := b.RecoveryTimeline; tl.TargetTimeline != 5 || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.repositoryAheadOfLive" {
		t.Fatalf("target %d, reasons %+v", tl.TargetTimeline, tl.Reasons)
	}
	for _, d := range []time.Duration{60 * time.Hour, 50 * time.Hour, 30 * time.Hour} {
		if c := restoreCheck(b, now.Add(-d)); c.Answer == "covered" || c.Code != "recovery.segment.repositoryAheadOfLive" {
			t.Errorf("now-%s, after b0 on the live line, b1 on a sibling: %s by %s (%s), want unknown", d, c.Answer, c.BackupID, c.Code)
		}
	}
	// b1 on the live line too (a path from it toward 4): the times before
	// its lower bound are on the shared history.
	b1Live := toward("b1", 48*time.Hour, 3, 4)
	b1Live.OffTarget = true
	b = failoverTimeline(g, append(slices.Clone(paths), b1Live)...)
	if c := restoreCheck(b, now.Add(-60*time.Hour)); c.Answer != "covered" || c.BackupID != "b0" {
		t.Errorf("before b1's end, both on the live line: %s (%s), want covered by b0", c.Answer, c.Code)
	}
	// No path toward 4: ancestry is not known, nothing before b2 counts.
	b = failoverTimeline(g, toward("b0", 72*time.Hour, 2, 5), toward("b1", 48*time.Hour, 3, 5), toward("b2", 10*time.Hour, 5, 5))
	if c := restoreCheck(b, now.Add(-70*time.Hour)); c.Answer == "covered" {
		t.Errorf("ancestry not known: now-70h %s by %s (%s), want unknown", c.Answer, c.BackupID, c.Code)
	}
}

// TestPurgeBandSpansTheOldestBackup (round 12, PG P3-3; round 13, PG
// P3-B and P3-C): retention drops a whole backup, with the WAL only it
// needs. Once a scan was missed (older than two refresh intervals), the
// band reaches the newest lower bound before first + the oldest backup's
// own span + the age + one refresh; within a normal cycle, a fresh scan or
// a single backup, it keeps the elapsed-time band.
func TestPurgeBandSpansTheOldestBackup(t *testing.T) {
	first, last := now.Add(-7*24*time.Hour), now.Add(-time.Minute)
	second := now.Add(-24 * time.Hour)
	two := []interval{{from: first, to: last, backupID: "old"}, {from: second, to: last, backupID: "new"}}
	one := two[:1]
	daily := func(n int) []interval {
		out := make([]interval, 0, n)
		for i := range n {
			out = append(out, interval{from: first.Add(time.Duration(i) * 24 * time.Hour), to: last, backupID: strconv.Itoa(i)})
		}
		return out
	}
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	for _, c := range []struct {
		name    string
		spans   []interval
		scanned *time.Time
		want    time.Duration
	}{
		{"fresh: one refresh plus the age", two, at(2 * time.Minute), 7 * time.Minute},
		{"a refresh old: unchanged", two, at(5 * time.Minute), 10 * time.Minute},
		{"past one refresh within a normal cycle: unchanged", two, at(6 * time.Minute), 11 * time.Minute},
		{"two refreshes old, the next scan not missed yet: unchanged", two, at(10 * time.Minute), 15 * time.Minute},
		{"a missed scan: the oldest backup's own span", two, at(11 * time.Minute), second.Sub(first)},
		{"a single backup is not purged by retention", one, at(2 * time.Hour), 2*time.Hour + 5*time.Minute},
		{"the elapsed band is longer: kept", []interval{{from: first, to: last, backupID: "old"}, {from: first.Add(time.Hour), to: last, backupID: "new"}}, at(3 * time.Hour), 3*time.Hour + 5*time.Minute},
		{"never past the window", []interval{{from: first, to: last, backupID: "old"}, {from: last.Add(time.Hour), to: last.Add(2 * time.Hour), backupID: "new"}}, at(time.Hour), last.Sub(first)},
		// Daily backups, scanned 30 h ago: the window's start may have
		// passed the second and third backups' lower bounds (24 h + 30 h
		// 5 min after first): the band reaches the third's.
		{"a long age: several backups", daily(5), at(30 * time.Hour), 48 * time.Hour},
		{"a shorter age: up to the next backup only", daily(5), at(12 * time.Hour), 24 * time.Hour},
		{"an absurd age stays within the window", daily(3), at(400 * 24 * time.Hour), last.Sub(first)},
	} {
		in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: c.scanned, refresh: 5 * time.Minute}
		if got := purgeBand(in, c.spans, first, last); got != c.want {
			t.Errorf("%s: band %s, want %s", c.name, got, c.want)
		}
	}
	// A short refresh interval: the console's own poll period bounds a
	// normal cycle, not two intervals.
	in := timelineInput{facts: recoveryFacts{now: now}, scannedAt: at(2*time.Minute + 30*time.Second), refresh: time.Minute}
	if got := purgeBand(in, two, first, last); got != 3*time.Minute+30*time.Second {
		t.Errorf("a one-minute refresh, scanned 2 min 30 s ago: band %s, want the elapsed band", got)
	}
}

// TestPurgeBandSumsTheAgeAndTheBackups (round 13, PG P3-B, the
// reviewer's probe): daily backups b0..b3 (120 h to 48 h ago), scanned 30
// h ago. With RECOVERY WINDOW 70h both b0 and b1 may be gone by now: the
// times only they reach are not covered, b2's are.
func TestPurgeBandSumsTheAgeAndTheBackups(t *testing.T) {
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second}
	paths := make([]evidence.RecoveryPath, 0, 4)
	for i, d := range []time.Duration{120, 96, 72, 48} {
		paths = append(paths, tlPath("b"+strconv.Itoa(i), d*time.Hour, 30*time.Hour+time.Minute, evidencev1alpha1.CoverageFrontier))
	}
	for _, stale := range []bool{false, true} {
		ev := timelineFixture{paths: paths, scannedAt: now.Add(-30 * time.Hour), stale: stale}.evidence()
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
		b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
		for _, d := range []time.Duration{110, 92, 85, 80, 75} {
			if c := restoreCheck(b, now.Add(-d*time.Hour)); c.Answer == "covered" || c.Code != "recovery.segment.purgeEdge" {
				t.Errorf("stale %v, now-%dh: %s by %s (%s), want the purge band", stale, d, c.Answer, c.BackupID, c.Code)
			}
		}
		if c := restoreCheck(b, now.Add(-60*time.Hour)); c.Answer != "covered" || c.BackupID != "b2" {
			t.Errorf("stale %v, now-60h: %s by %s (%s), want covered by b2", stale, c.Answer, c.BackupID, c.Code)
		}
	}
}

// TestPurgeBandDoesNotFlapWithinACycle (round 13, PG P3-C, the reviewer's
// probe): weekly backups, the agent refreshing every 5 minutes and the
// console polling every minute. Within a normal cycle, a scan 4 min or 5
// min 30 s old (the next one not landed yet), the oldest week answers the
// same; only a missed scan extends the band over it.
func TestPurgeBandDoesNotFlapWithinACycle(t *testing.T) {
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, sweepInterval: 30 * time.Second}
	paths := []evidence.RecoveryPath{
		tlPath("w0", 14*24*time.Hour, 6*time.Minute, evidencev1alpha1.CoverageFrontier),
		tlPath("w1", 7*24*time.Hour, 6*time.Minute, evidencev1alpha1.CoverageFrontier),
	}
	answer := func(age time.Duration) restoreAnswerOf {
		ev := timelineFixture{paths: paths, scannedAt: now.Add(-age)}.evidence()
		obs := now.Add(-20 * time.Second)
		ev.result.ObservedAt = &obs
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, facts)
		c := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-10*24*time.Hour))
		return restoreAnswerOf{c.Answer, c.Code}
	}
	want := restoreAnswerOf{"covered", ""}
	for _, age := range []time.Duration{0, 4 * time.Minute, 5*time.Minute + 30*time.Second, 6*time.Minute + 50*time.Second, 9 * time.Minute} {
		if got := answer(age); got.answer != want.answer {
			t.Errorf("scan %s old, within a cycle: now-10d answers %+v, want %+v", age, got, want)
		}
	}
	if got := answer(11 * time.Minute); got.answer == "covered" || got.code != "recovery.segment.purgeEdge" {
		t.Errorf("a missed scan: now-10d answers %+v, want the purge band", got)
	}
}

type restoreAnswerOf struct{ answer, code string }

// TestDroppedKeptPathsAreSaid (round 12, DBA): when the last good paths
// are dropped beside a failed read, the bar says why with a coded reason
// the SPA translates.
func TestDroppedKeptPathsAreSaid(t *testing.T) {
	snap := evidencev1alpha1.RepositoryEvidenceSnapshot{}
	for dropped, code := range map[string]string{
		evidence.KeptDroppedTooOld:            "recovery.reason.keptTooOld",
		evidence.KeptDroppedRepositoryChanged: "recovery.reason.keptRepositoryChanged",
		"":                                    "",
	} {
		ev := clusterEvidence{result: evidence.Result{Agent: "a", Snapshot: &snap, Recovery: evidence.Recovery{State: evidence.RecoveryUnavailable, Reason: "timeout", Dropped: dropped}}}
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{}, nil, recoveryFacts{now: now})
		var codes []string
		for _, r := range tl.Reasons {
			codes = append(codes, r.Code)
		}
		want := []string{"recovery.reason.pathsNotCoverage"}
		if code != "" {
			want = append(want, code)
		}
		if strings.Join(codes, ",") != strings.Join(want, ",") {
			t.Errorf("dropped %q: reasons %v, want %v", dropped, codes, want)
		}
	}
}

// TestRecoveryReasonsMatchTheSPAEnglish (round 13, DBA): the English Go
// writes as the fallback of a coded reason is the SPA's own English for
// that code (ui/messages/en.json), so a reader sees the same sentence
// whichever side says it.
func TestRecoveryReasonsMatchTheSPAEnglish(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "ui", "messages", "en.json"))
	if err != nil {
		t.Fatal(err)
	}
	var en map[string]any
	if err := json.Unmarshal(raw, &en); err != nil {
		t.Fatal(err)
	}
	tooOld, _ := keptDroppedReason(evidence.KeptDroppedTooOld)
	changed, _ := keptDroppedReason(evidence.KeptDroppedRepositoryChanged)
	out := viewmodel.RecoveryTimeline{}
	timelineVerdict(&out, timelineInput{rec: evidence.Recovery{Complete: true}, facts: recoveryFacts{now: now}}, timelineFindings{tail: evidencev1alpha1.CoverageFrontier, failing: true})
	if len(out.Reasons) != 1 || out.Reasons[0].Code != "recovery.reason.archivingFailing" {
		t.Fatalf("failing: reasons %+v", out.Reasons)
	}
	for key, phrase := range map[string]viewmodel.Phrase{
		"rp_reason_kept_too_old":            tooOld,
		"rp_reason_kept_repository_changed": changed,
		"rp_reason_archiving_failing":       out.Reasons[0],
	} {
		if en[key] != phrase.Text {
			t.Errorf("%s (%s): the SPA says %q, Go %q", key, phrase.Code, en[key], phrase.Text)
		}
	}
}

// TestArchiveFailureReadThroughTheGraceSweep (round 14, CNPG P3-a, the
// reviewer's probe): ContinuousArchiving True, the primary's last attempt
// failed with 5 .ready files. Read, then missed once (the grace sweep:
// the last good reading stands), the bar gives the same reason, the
// recent failure: the archiving fact and the failure come from the same
// reading. It must not say "archiving is failing" while the condition
// it points at says archiving works.
func TestArchiveFailureReadThroughTheGraceSweep(t *testing.T) {
	ev := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 3*time.Minute, evidencev1alpha1.CoverageFrontier)}}.evidence()
	failed, archived := now.Add(-2*time.Minute), now.Add(-5*time.Minute)
	reading := &instancestatus.Reading{
		IsPrimary: true, CurrentWAL: "000000020000000000000007", ReadyWALFiles: 5,
		LastFailedWAL: "000000020000000000000005", LastFailedWALTime: &failed, LastArchivedWALTime: &archived, ObservedAt: now.Add(-30 * time.Second),
	}
	for _, c := range []struct {
		name string
		inst sweep.Instance
	}{
		{"read", sweep.Instance{State: viewmodel.StatusReadOK, Since: now.Add(-time.Hour), Reading: reading}},
		{"missed once", sweep.Instance{State: viewmodel.StatusReadUnreachable, Since: now, Reading: reading}},
	} {
		g := failoverGathered("db-1", time.Time{}, map[string][]failoverReading{})
		g.cluster.Status.Conditions = []metav1.Condition{{Type: cnpgstatus.ConditionContinuousArchiving, Status: metav1.ConditionTrue, Reason: "ContinuousArchivingSuccess", LastTransitionTime: metav1.NewTime(now.Add(-time.Hour))}}
		g.swept = sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{"db-1": c.inst}}
		live := cnpgstatus.Archiving(g.cluster, primaryArchiver(g))
		if live.Reason != cnpgstatus.ArchivingReasonFailureWithBacklog {
			t.Fatalf("%s: archiving reason %s, want FailureWithBacklog", c.name, live.Reason)
		}
		arch := viewmodel.Archiving{Health: health(live.Class), Reason: live.Reason, ConditionStatus: live.ConditionStatus}
		facts := recoveryFactsOf(g, arch, now)
		tl := (&Service{}).recoveryTimeline(ev, viewmodel.BackupSummary{Archiving: arch}, nil, facts)
		if len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.failedRecently" {
			t.Errorf("%s: reasons %+v, want recovery.reason.failedRecently", c.name, tl.Reasons)
		}
	}
}

// TestEdgeAdvice (round 14, DBA P2-1; round 15, CNPG P2-1 and DBA P2-1):
// the SPA's hand checks at an uncertain edge follow the facts first, then
// one reason map. No primary read, or the Cluster hibernated: none,
// whatever the reasons. The archiving instance in recovery, where
// pg_switch_wal() fails, or archiving failing, lagging or failed
// recently: pg_stat_archiver only. The narrowest wins.
func TestEdgeAdvice(t *testing.T) {
	r := func(codes ...string) []viewmodel.Phrase {
		out := make([]viewmodel.Phrase, 0, len(codes))
		for _, c := range codes {
			out = append(out, viewmodel.Phrase{Code: c})
		}
		return out
	}
	for _, c := range []struct {
		name    string
		reasons []viewmodel.Phrase
		facts   recoveryFacts
		want    string
	}{
		{"nothing", nil, recoveryFacts{}, ""},
		{"post-failover lag", r("recovery.reason.liveTimelineNotArchived"), recoveryFacts{}, ""},
		{"failing", r("recovery.reason.archivingFailing"), recoveryFacts{}, viewmodel.EdgeAdviceArchiver},
		{"backlog", r("recovery.reason.archiveBacklog"), recoveryFacts{}, viewmodel.EdgeAdviceArchiver},
		{"failed recently", r("recovery.reason.failedRecently"), recoveryFacts{}, viewmodel.EdgeAdviceArchiver},
		{"hibernated reason", r("recovery.reason.archiverHibernated"), recoveryFacts{}, viewmodel.EdgeAdviceNone},
		{"lag, primary unread", r("recovery.reason.liveTimelineNotArchivedPrimaryUnread"), recoveryFacts{}, viewmodel.EdgeAdviceNone},
		{"failed, primary unread", r("recovery.reason.archivingFailedPrimaryUnread"), recoveryFacts{}, viewmodel.EdgeAdviceNone},
		{"failed recently and hibernated", r("recovery.reason.failedRecently", "recovery.reason.archiverHibernated"), recoveryFacts{}, viewmodel.EdgeAdviceNone},
		{"holes and backlog", r("recovery.reason.holes", "recovery.reason.archiveBacklog"), recoveryFacts{}, viewmodel.EdgeAdviceArchiver},
		// Reported window, archiving failing: no archiving reason, no switch.
		{"reported, failing", r("recovery.reason.reportedWindow"), recoveryFacts{failing: true}, viewmodel.EdgeAdviceArchiver},
		{"hibernated, failing", r("recovery.reason.archiverHibernated"), recoveryFacts{failing: true}, viewmodel.EdgeAdviceNone},
		// (a) Evidence, condition True, primary unread (fenced): the
		// reason is archiverUnread, PostgreSQL is stopped.
		{"evidence, archiver unread", r("recovery.reason.archiverUnread"), recoveryFacts{archiverUnread: true, primaryUnread: true}, viewmodel.EdgeAdviceNone},
		// The reason alone, too: it rests on no reading of the primary.
		{"archiver unread reason", r("recovery.reason.archiverUnread"), recoveryFacts{}, viewmodel.EdgeAdviceNone},
		// A graver reason ranks first and hides the archiver's: the fact
		// still holds.
		{"holes, primary unread", r("recovery.reason.holes"), recoveryFacts{primaryUnread: true}, viewmodel.EdgeAdviceNone},
		// (b) Reported basis, no agent, hibernated Cluster.
		{"reported, hibernated", r("recovery.reason.reportedWindow", "recovery.reason.noEvidence"), recoveryFacts{hibernated: true, archiverUnread: true, primaryUnread: true}, viewmodel.EdgeAdviceNone},
		{"reported, hibernated, primary read", r("recovery.reason.reportedWindow"), recoveryFacts{hibernated: true}, viewmodel.EdgeAdviceNone},
		// (c) Reported basis, condition False, primary unread: none, as on
		// the evidence basis, not archiver.
		{"reported, failing, primary unread", r("recovery.reason.reportedWindow", "recovery.reason.noEvidence"), recoveryFacts{failing: true, primaryUnread: true}, viewmodel.EdgeAdviceNone},
		// (d) A designated primary or a primary mid-promotion: in
		// recovery, pg_switch_wal() fails; its archiver still reads.
		{"not reported", r("recovery.reason.archiverNotReported"), recoveryFacts{notReported: true, archiverUnread: true}, viewmodel.EdgeAdviceArchiver},
		{"not reported reason", r("recovery.reason.archiverNotReported"), recoveryFacts{}, viewmodel.EdgeAdviceArchiver},
		{"reported, not reported", r("recovery.reason.reportedWindow"), recoveryFacts{notReported: true}, viewmodel.EdgeAdviceArchiver},
		{"holes, not reported", r("recovery.reason.holes"), recoveryFacts{notReported: true}, viewmodel.EdgeAdviceArchiver},
		// (e) The repository's newest timeline is another cluster's: a
		// switch here does not move it.
		{"repository ahead", r("recovery.reason.repositoryAheadOfLive"), recoveryFacts{}, viewmodel.EdgeAdviceArchiver},
		{"repository ahead, primary unread", r("recovery.reason.repositoryAheadOfLive"), recoveryFacts{primaryUnread: true}, viewmodel.EdgeAdviceNone},
	} {
		if got := edgeAdvice(c.reasons, c.facts); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// edgeAdviceBoth are the reasons that leave both hand checks (the map
// edgeAdviceOf has no entry for them): each is classified on purpose.
// None says a primary is unread or the Cluster hibernated, archiving
// fails or lags, or the archiving instance is in recovery; the facts
// narrow the advice whenever those hold beside them (edgeAdvice).
var edgeAdviceBoth = map[string]bool{
	"recovery.reason.backupsCut":                              true,
	"recovery.reason.reportedWindow":                          true,
	"recovery.reason.pathsStale":                              true,
	"recovery.reason.evidenceStale":                           true,
	"recovery.reason.timelineWarning":                         true,
	"recovery.reason.pathsCut":                                true,
	"recovery.reason.missingAfterLatestFormerPrimaryRejoined": true,
	"recovery.reason.missingAfterLatestFormerPrimary":         true,
	"recovery.reason.missingAfterLatest":                      true,
	"recovery.reason.holes":                                   true,
	"recovery.reason.candidateGaps":                           true,
	"recovery.reason.backupGone":                              true,
	"recovery.reason.backupUnconfirmed":                       true,
	"recovery.reason.backupNotListed":                         true,
	"recovery.reason.backupNotUsable":                         true,
	"recovery.reason.pathNotHealthy":                          true,
	"recovery.reason.liveTimelineNotArchived":                 true,
	"recovery.reason.orphanTimeline":                          true,
	"recovery.reason.undrawn":                                 true,
	// No paths, or none to draw: the window rests on no evidence edge.
	"recovery.reason.noEvidence":             true,
	"recovery.reason.noEvidenceBecause":      true,
	"recovery.reason.agentCoverage":          true,
	"recovery.reason.agentPredates":          true,
	"recovery.reason.noPaths":                true,
	"recovery.reason.pathsNotCoverage":       true,
	"recovery.reason.noConsistentPath":       true,
	"recovery.reason.timelineNotEstablished": true,
	"recovery.reason.keptTooOld":             true,
	"recovery.reason.keptRepositoryChanged":  true,
}

// TestEdgeAdviceClassifiesEveryReason (round 15, DBA P2-1): every
// recovery.reason code the package's code names is in edgeAdviceOf or in
// edgeAdviceBoth, never in both: a new reason cannot fall back to both
// checks unseen.
func TestEdgeAdviceClassifiesEveryReason(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	code := regexp.MustCompile(`"(recovery\.reason\.[A-Za-z]+)"`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range code.FindAllStringSubmatch(string(src), -1) {
			seen[m[1]] = true
		}
	}
	if len(seen) < len(edgeAdviceOf) {
		t.Fatalf("found %d reason codes, fewer than the map's %d: the scan is broken", len(seen), len(edgeAdviceOf))
	}
	for c := range seen {
		_, mapped := edgeAdviceOf[c]
		switch {
		case mapped && edgeAdviceBoth[c]:
			t.Errorf("%s is in edgeAdviceOf and edgeAdviceBoth", c)
		case !mapped && !edgeAdviceBoth[c]:
			t.Errorf("%s is not classified: add it to edgeAdviceOf (recovery.go) or to edgeAdviceBoth", c)
		}
	}
	for c := range edgeAdviceOf {
		if !seen[c] {
			t.Errorf("edgeAdviceOf names %s, which no code says", c)
		}
	}
	for c := range edgeAdviceBoth {
		if !seen[c] {
			t.Errorf("edgeAdviceBoth names %s, which no code says", c)
		}
	}
}

// TestEdgeAdviceFromFacts (round 15, CNPG P2-1, DBA P2-1 and CNPG P3-2):
// from a gathered Cluster, on both bases, the hint offers nothing to run
// on a primary that was not read or a hibernated Cluster, and no WAL
// switch on an archiving instance in recovery. A hibernated Cluster with
// ContinuousArchiving False says it is hibernated, not "until the primary
// is read again".
func TestEdgeAdviceFromFacts(t *testing.T) {
	p := tlPath("b1", 48*time.Hour, 0, evidencev1alpha1.CoverageFrontier)
	withEvidence := timelineFixture{paths: []evidence.RecoveryPath{p}}.evidence()
	first, last := now.Add(-48*time.Hour), now.Add(-6*time.Hour)
	window := &viewmodel.RecoveryWindow{First: &first, Last: &last, Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "Cluster.status"}}
	read := func(primary bool) sweep.Instance {
		return sweep.Instance{State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{
			IsPrimary: primary, IsArchivingWAL: true, LastArchivedWALTime: ago(time.Minute), ObservedAt: now.Add(-10 * time.Second),
		}}
	}
	for _, c := range []struct {
		name       string
		reported   bool
		condition  metav1.ConditionStatus
		hibernated bool
		primary    *sweep.Instance
		reason     string
		want       string
	}{
		{name: "evidence, True, primary unread (fenced)", condition: metav1.ConditionTrue, reason: "recovery.reason.archiverUnread", want: viewmodel.EdgeAdviceNone},
		{name: "evidence, False, hibernated", condition: metav1.ConditionFalse, hibernated: true, reason: "recovery.reason.archiverHibernated", want: viewmodel.EdgeAdviceNone},
		{name: "evidence, True, hibernated", condition: metav1.ConditionTrue, hibernated: true, reason: "recovery.reason.archiverHibernated", want: viewmodel.EdgeAdviceNone},
		{name: "evidence, designated primary", condition: metav1.ConditionTrue, primary: ptrTo(read(false)), reason: "recovery.reason.archiverNotReported", want: viewmodel.EdgeAdviceArchiver},
		{name: "reported, hibernated", reported: true, condition: metav1.ConditionTrue, hibernated: true, reason: "recovery.reason.reportedWindow", want: viewmodel.EdgeAdviceNone},
		{name: "reported, False, primary unread", reported: true, condition: metav1.ConditionFalse, reason: "recovery.reason.reportedWindow", want: viewmodel.EdgeAdviceNone},
		{name: "reported, designated primary", reported: true, condition: metav1.ConditionTrue, primary: ptrTo(read(false)), reason: "recovery.reason.reportedWindow", want: viewmodel.EdgeAdviceArchiver},
		{name: "reported, primary read, healthy", reported: true, condition: metav1.ConditionTrue, primary: ptrTo(read(true)), reason: "recovery.reason.reportedWindow", want: ""},
	} {
		g := failoverGathered("db-2", now.Add(-time.Hour), nil)
		g.cluster.Status.Conditions = []metav1.Condition{{Type: cnpgstatus.ConditionContinuousArchiving, Status: c.condition, Reason: "ContinuousArchivingFailing", LastTransitionTime: metav1.NewTime(now.Add(-2 * time.Hour))}}
		if c.hibernated {
			g.cluster.Annotations = map[string]string{cnpgstatus.AnnotationHibernation: "on"}
			g.cluster.Status.Conditions = append(g.cluster.Status.Conditions, metav1.Condition{Type: cnpgstatus.ConditionHibernation, Status: metav1.ConditionTrue, Reason: "Hibernated"})
		}
		g.swept = sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{}}
		if c.primary != nil {
			g.swept.Instances["db-2"] = *c.primary
		}
		reading := cnpgstatus.Archiving(g.cluster, primaryArchiver(g))
		arch := viewmodel.Archiving{Health: health(reading.Class), Reason: reading.Reason, ConditionStatus: reading.ConditionStatus}
		facts := recoveryFactsOf(g, arch, now)
		ev, summary := withEvidence, viewmodel.BackupSummary{Archiving: arch}
		if c.reported {
			ev, summary.RecoveryWindow = clusterEvidence{}, window
		}
		tl := (&Service{}).recoveryTimeline(ev, summary, nil, facts)
		if len(tl.Reasons) == 0 || tl.Reasons[0].Code != c.reason {
			t.Errorf("%s (archiving %s): reasons %+v, want %s first", c.name, reading.Reason, tl.Reasons, c.reason)
		}
		if tl.EdgeAdvice != c.want {
			t.Errorf("%s (archiving %s): edgeAdvice %q, want %q", c.name, reading.Reason, tl.EdgeAdvice, c.want)
		}
	}
}

func ptrTo[T any](v T) *T { return &v }
