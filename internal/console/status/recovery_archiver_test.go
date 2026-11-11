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
	"slices"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// edgeBand is the uncertain band at the newest edge whose code is one of
// codes; nil when there is none.
func edgeBand(tl viewmodel.RecoveryTimeline, codes ...string) *viewmodel.RecoverySegment {
	for i := range tl.Segments {
		if slices.Contains(codes, tl.Segments[i].Code) {
			return &tl.Segments[i]
		}
	}
	return nil
}

// TestBacklogOnsetFromTheQuietSweep: a backlog is dated from the newest
// sweep, at or before the scan, that read no .ready file on the primary,
// less one archive_timeout. A reading of one or two files proves nothing;
// a failed read between widens, never narrows; no empty reading, or no
// known sweep period, keeps failureLookback.
func TestBacklogOnsetFromTheQuietSweep(t *testing.T) {
	svc := &Service{}
	read := now.Add(-30 * time.Second)
	sweepBefore := func(n int) time.Time { return read.Add(-time.Duration(n) * 30 * time.Second) }
	for _, tc := range []struct {
		name     string
		ready    int
		earlier  []float64
		interval time.Duration
		// frontier is how long ago the newest segment reached the
		// repository (1 min when 0); scanned is when the agent read it
		// (now when zero).
		frontier time.Duration
		scanned  time.Time
		wantCode string
		wantFrom time.Time
	}{
		{
			// The round-6 ring: [0, 1, 2, 3] then 3, threshold 3.
			name: "[0,1...,2,3] then 3: from the empty sweep", ready: 3, earlier: append(append([]float64{0}, slices.Repeat([]float64{1}, 40)...), 2, 3), interval: 30 * time.Second,
			wantCode: "recovery.segment.archiveBacklogSince", wantFrom: sweepBefore(43).Add(-defaultArchiveTimeout),
		},
		{
			name: "[low, failed, high] then high: no empty sweep", ready: 6, earlier: []float64{1, -1, 5}, interval: 30 * time.Second,
			wantCode: "recovery.segment.archiveBacklog",
		},
		{
			name: "[0, failed..., high] then high: the failed reads do not move the onset", ready: 6, earlier: append(append([]float64{0}, slices.Repeat([]float64{-1}, 40)...), 5), interval: 30 * time.Second,
			wantCode: "recovery.segment.archiveBacklogSince", wantFrom: sweepBefore(42).Add(-defaultArchiveTimeout),
		},
		{
			name: "sweep period not known: a day's band", ready: 6, earlier: []float64{0, 5}, interval: 0,
			wantCode: "recovery.segment.archiveBacklog",
		},
		{
			name: "a console that just started: the count is not judged, a day's band", ready: 40, interval: 30 * time.Second,
			wantCode: "recovery.segment.archiveBacklog",
		},
		{
			// An empty sweep after the scan says nothing of what the scan saw.
			name: "the empty sweep after the scan is not used", ready: 6, interval: 30 * time.Second, frontier: 25 * time.Minute, scanned: now.Add(-20 * time.Minute),
			earlier:  append(append([]float64{0}, slices.Repeat([]float64{1}, 100)...), 0, 5),
			wantCode: "recovery.segment.archiveBacklogSince", wantFrom: sweepBefore(103).Add(-defaultArchiveTimeout),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := instancestatus.Reading{IsPrimary: true, ReadyWALFiles: tc.ready, IsArchivingWAL: true, ObservedAt: read}
			g := &gathered{
				cluster:    inTreeCluster(),
				swept:      sweep.Snapshot{Interval: tc.interval, Instances: map[string]sweep.Instance{"db-2": {State: viewmodel.StatusReadOK, Reading: &r}}},
				readyRings: map[string][]viewmodel.Point{"db-2": ring(read, tc.earlier...)},
			}
			facts := recoveryFactsOf(g, viewmodel.Archiving{Health: viewmodel.HealthOK}, now)
			frontier := max(tc.frontier, time.Minute)
			last := now.Add(-frontier)
			if tc.wantFrom.IsZero() {
				tc.wantFrom = last.Add(-failureLookback)
			}
			f := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, frontier, evidencev1alpha1.CoverageFrontier)}, scannedAt: tc.scanned}
			tl := svc.recoveryTimeline(f.evidence(), viewmodel.BackupSummary{}, nil, facts)
			band := edgeBand(tl, "recovery.segment.archiveBacklog", "recovery.segment.archiveBacklogSince")
			if band == nil || band.Code != tc.wantCode || !band.From.Equal(tc.wantFrom) {
				t.Fatalf("band %+v, want %s from %s (facts %+v)", band, tc.wantCode, tc.wantFrom, facts)
			}
			if tl.State == string(evidencev1alpha1.Healthy) {
				t.Errorf("state healthy with a backlog")
			}
			b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
			if got := restoreCheck(b, tc.wantFrom.Add(time.Second)); got.Answer == "covered" {
				t.Errorf("just after the band's start: %+v", got)
			}
			if got := restoreCheck(b, minTime(tc.wantFrom, last.Add(-minimumFrontierMargin)).Add(-time.Minute)); got.Answer != "covered" {
				t.Errorf("before the band: %+v", got)
			}
		})
	}
}

// TestUnreadArchiverIsNotHealthy: with the primary's archiver unread
// (archiving unknown PrimaryUnread, or hibernated), the window is never
// healthy, the newest edge is unknown from the newest sweep that read the
// primary with nothing waiting (else failureLookback), and every restore
// answer carries the archiverUnread caveat.
func TestUnreadArchiverIsNotHealthy(t *testing.T) {
	svc := &Service{}
	fx := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier)}}
	last := now.Add(-10 * time.Minute)
	read := now.Add(-time.Hour)
	unread := viewmodel.Archiving{Health: viewmodel.HealthUnknown, Reason: cnpgstatus.ArchivingReasonPrimaryUnread}
	for _, tc := range []struct {
		name       string
		arch       viewmodel.Archiving
		earlier    []float64
		promotedAt string
		wantCode   string
		wantFrom   time.Time
		wantReason string
		// wantState is the window's state; warning when empty.
		wantState string
	}{
		{
			name: "unread, the ring read it empty an hour ago", arch: unread, earlier: []float64{0, 0, 2, -1},
			wantCode: "recovery.segment.archiverUnreadSince", wantFrom: read.Add(-3 * 30 * time.Second).Add(-defaultArchiveTimeout),
			wantReason: "recovery.reason.archiverUnread",
		},
		{
			name: "unread, nothing kept", arch: unread,
			wantCode: "recovery.segment.archiverUnread", wantFrom: last.Add(-failureLookback), wantReason: "recovery.reason.archiverUnread",
		},
		{
			name: "unread, the empty sweeps were before it became primary", arch: unread, earlier: []float64{0, 0, 2, -1},
			promotedAt: read.Format(time.RFC3339Nano),
			wantCode:   "recovery.segment.archiverUnread", wantFrom: last.Add(-failureLookback), wantReason: "recovery.reason.archiverUnread",
		},
		{
			name: "no condition and the primary unread", arch: viewmodel.Archiving{Health: viewmodel.HealthUnknown, Reason: cnpgstatus.ArchivingReasonNoCondition},
			wantCode: "recovery.segment.archiverUnread", wantFrom: last.Add(-failureLookback), wantReason: "recovery.reason.archiverUnread",
		},
		{
			name: "hibernated", arch: viewmodel.Archiving{Health: viewmodel.HealthUnknown, Reason: cnpgstatus.ArchivingReasonHibernated},
			// Round 7, DBA P2-4: a hibernated Cluster is unknown, not a
			// warning that stays amber for as long as it sleeps.
			wantCode: "recovery.segment.archiverUnread", wantFrom: last.Add(-failureLookback), wantReason: "recovery.reason.archiverHibernated",
			wantState: string(evidencev1alpha1.Unknown),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := inTreeCluster()
			c.Status.CurrentPrimaryTimestamp = tc.promotedAt
			g := &gathered{
				cluster: c,
				// db-2, the primary, was not read this sweep.
				swept:      sweep.Snapshot{Interval: 30 * time.Second, Instances: map[string]sweep.Instance{"db-2": {State: viewmodel.StatusReadUnreachable}}},
				readyRings: map[string][]viewmodel.Point{"db-2": ring(read, tc.earlier...)},
			}
			facts := recoveryFactsOf(g, tc.arch, now)
			if !facts.archiverUnread {
				t.Fatalf("facts do not say the archiver is unread: %+v", facts)
			}
			tl := svc.recoveryTimeline(fx.evidence(), viewmodel.BackupSummary{Archiving: tc.arch}, nil, facts)
			wantState := tc.wantState
			if wantState == "" {
				wantState = string(evidencev1alpha1.Warning)
			}
			if tl.State != wantState || len(tl.Reasons) == 0 || tl.Reasons[0].Code != tc.wantReason {
				t.Errorf("state %s reasons %+v, want %s %s", tl.State, tl.Reasons, wantState, tc.wantReason)
			}
			band := edgeBand(tl, "recovery.segment.archiverUnread", "recovery.segment.archiverUnreadSince")
			if band == nil || band.Code != tc.wantCode || !band.From.Equal(tc.wantFrom) {
				t.Fatalf("band %+v, want %s from %s", band, tc.wantCode, tc.wantFrom)
			}
			if tl.NewestCoveredTarget == nil || !tl.NewestCoveredTarget.Before(tc.wantFrom.Add(time.Second)) {
				t.Errorf("newest covered target %v is inside the unread band from %s", tl.NewestCoveredTarget, tc.wantFrom)
			}
			b := viewmodel.Backups{RecoveryTimeline: tl, Summary: viewmodel.BackupSummary{Archiving: tc.arch}, GeneratedAt: now}
			got := restoreCheck(b, last.Add(-20*time.Minute))
			if got.Answer == "covered" {
				t.Errorf("20 min before the frontier with the archiver unread: %+v", got)
			}
			if !slices.ContainsFunc(got.Caveats, func(p viewmodel.Phrase) bool {
				// The newest archived WAL may hold older commits than its
				// arrival says (round 7, DBA P2-3: the copy was inverted).
				return p.Code == "recovery.caveat.archiverUnread" && strings.Contains(p.Text, "older than its arrival") &&
					!strings.Contains(p.Text, "later than the WAL")
			}) {
				t.Errorf("no archiverUnread caveat: %+v", got.Caveats)
			}
		})
	}
	// A primary read in good standing carries neither the fact nor the caveat.
	r := instancestatus.Reading{IsPrimary: true, IsArchivingWAL: true, ObservedAt: now.Add(-30 * time.Second)}
	g := &gathered{cluster: inTreeCluster(), swept: sweep.Snapshot{Interval: 30 * time.Second, Instances: map[string]sweep.Instance{"db-2": {State: viewmodel.StatusReadOK, Reading: &r}}}}
	live := true
	ok := viewmodel.Archiving{Health: viewmodel.HealthOK, Reason: "ContinuousArchivingSuccess", LiveArchiving: &live}
	if facts := recoveryFactsOf(g, ok, now); facts.archiverUnread {
		t.Errorf("a read primary reads as unread: %+v", facts)
	}
	tl := svc.recoveryTimeline(fx.evidence(), viewmodel.BackupSummary{Archiving: ok}, nil, recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout})
	if tl.State != string(evidencev1alpha1.Healthy) {
		t.Errorf("baseline: %s (%s)", tl.State, tl.Reason)
	}
	for _, c := range restoreCaveats(viewmodel.Backups{RecoveryTimeline: tl, Summary: viewmodel.BackupSummary{Archiving: ok}}) {
		if c.Code == "recovery.caveat.archiverUnread" {
			t.Errorf("caveat on a read archiver: %+v", c)
		}
	}
}

// TestDegradedPathIsNotCovered: a path that reaches its frontier but that
// the agent rates below healthy (an unproven pgBackRest segment size, or
// the console's cap at a roll-up that is not healthy) is drawn, never as
// covered; the bar is at most a warning with the path's reason, and no
// newest covered target is named when the newest span's path is degraded.
// A healthy path still covers the time it reaches.
func TestDegradedPathIsNotCovered(t *testing.T) {
	svc := &Service{}
	facts := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}
	warn := tlPath("b1", 48*time.Hour, 20*time.Hour, evidencev1alpha1.CoverageFrontier)
	warn.State = evidencev1alpha1.Warning
	warn.Reason = evidencev1alpha1.Reason{Code: "coverage-path-warning", Message: "the WAL segment size is not proven across log id 0/40"}

	tl := svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{warn}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	if tl.State != string(evidencev1alpha1.Warning) || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.pathNotHealthy" ||
		tl.Reasons[0].Params["backupId"] != "b1" || tl.Reasons[0].Params["state"] != "warning" {
		t.Errorf("state %s reasons %+v", tl.State, tl.Reasons)
	}
	if tl.NewestCoveredTarget != nil {
		t.Errorf("newest covered target %v on a warning path", tl.NewestCoveredTarget)
	}
	for _, seg := range tl.Segments {
		if seg.Kind == segmentCovered {
			t.Errorf("covered segment on a warning path: %+v", seg)
		}
	}
	got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-30*time.Hour))
	if got.Answer != "unknown" || got.Code != "recovery.segment.pathNotHealthy" || got.Params["detail"] != warn.Reason.Message {
		t.Errorf("inside the warning path: %+v", got)
	}

	// The roll-up cap (capAtRollup) yields the same.
	capped := tlPath("b1", 48*time.Hour, 20*time.Hour, evidencev1alpha1.CoverageFrontier)
	capped.State, capped.Reason.Code = evidencev1alpha1.Warning, "coverage-rollup-cap"
	if tl := svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{capped}}.evidence(), viewmodel.BackupSummary{}, nil, facts); tl.State == string(evidencev1alpha1.Healthy) || tl.NewestCoveredTarget != nil {
		t.Errorf("capped path: %s %v", tl.State, tl.NewestCoveredTarget)
	}

	// An older healthy path covers what it reaches; the newer warning path
	// alone covers nothing beyond it.
	healthy := tlPath("b0", 72*time.Hour, 30*time.Hour, evidencev1alpha1.CoverageFrontier)
	tl = svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{healthy, warn}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	b := viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}
	if got := restoreCheck(b, now.Add(-40*time.Hour)); got.Answer != "covered" || got.BackupID != "b0" {
		t.Errorf("inside both paths, the healthy one answers: %+v", got)
	}
	if got := restoreCheck(b, now.Add(-25*time.Hour)); got.Answer == "covered" {
		t.Errorf("past the healthy path, inside the warning one: %+v", got)
	}
	if tl.NewestCoveredTarget != nil {
		t.Errorf("newest span degraded, yet a newest covered target %v", tl.NewestCoveredTarget)
	}

	// A gap-limited path keeps its covered span up to the gap.
	gapped := tlPath("b1", 48*time.Hour, 20*time.Hour, evidencev1alpha1.CoverageGapLimited)
	tl = svc.recoveryTimeline(timelineFixture{paths: []evidence.RecoveryPath{gapped}}.evidence(), viewmodel.BackupSummary{}, nil, facts)
	if got := restoreCheck(viewmodel.Backups{RecoveryTimeline: tl, GeneratedAt: now}, now.Add(-30*time.Hour)); got.Answer != "covered" {
		t.Errorf("before the gap: %+v", got)
	}
}

// TestObjectStoreNameFollowsTheArchiver: the ObjectStore read (and so
// the wal.maxParallel the backlog threshold uses) is the one of the
// plugin CloudNativePG archives WAL with, not the first barman-cloud entry.
func TestObjectStoreNameFollowsTheArchiver(t *testing.T) {
	yes := true
	barman := func(store string, archiver bool) cnpgv1.PluginConfiguration {
		p := cnpgv1.PluginConfiguration{Name: cnpgstatus.BarmanCloudPlugin, Parameters: map[string]string{"barmanObjectName": store}}
		if archiver {
			p.IsWALArchiver = &yes
		}
		return p
	}
	for _, tc := range []struct {
		name    string
		plugins []cnpgv1.PluginConfiguration
		want    string
	}{
		{"one barman-cloud entry", []cnpgv1.PluginConfiguration{barman("store", false)}, "store"},
		{"backups-only first, the archiver second", []cnpgv1.PluginConfiguration{barman("backups", false), barman("wal", true)}, "wal"},
		{"pgBackRest archives: the barman-cloud store is still named", []cnpgv1.PluginConfiguration{barman("backups", false), {Name: cnpgstatus.PgBackRestDaliboPlugin, IsWALArchiver: &yes}}, "backups"},
	} {
		c := &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Plugins: tc.plugins}}
		if got := objectStoreName(c); got != tc.want {
			t.Errorf("%s: objectStoreName = %q, want %q", tc.name, got, tc.want)
		}
	}
	// With pgBackRest archiving, the barman-cloud store's maxParallel 16
	// does not raise the threshold.
	c := &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{barman("backups", false), {Name: cnpgstatus.PgBackRestDaliboPlugin, IsWALArchiver: &yes}}}}
	if got := cnpgstatus.ArchiveBacklogThreshold(c, pint(16)); got != cnpgstatus.ArchiveBacklogSegments {
		t.Errorf("threshold %d with pgBackRest archiving, want %d", got, cnpgstatus.ArchiveBacklogSegments)
	}
}

// TestDesignatedPrimaryArchiverIsNotOK (round 7, CNPG P1 and P2-2): a
// replica cluster's designated primary (status.currentPrimary, isPrimary
// false) archives but reports no readyWalFiles, so its 0 is no reading:
// with ContinuousArchiving True archiving is not ok, the recovery band is
// the day before the latest point with its own reason and caveat, and
// neither its zeros nor a demoted former primary's count toward a quiet
// sweep or a catch-up.
func TestDesignatedPrimaryArchiverIsNotOK(t *testing.T) {
	svc := &Service{}
	fx := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, 10*time.Minute, evidencev1alpha1.CoverageFrontier)}}
	last := now.Add(-10 * time.Minute)
	read := now.Add(-30 * time.Second)
	failed, archived := now.Add(-2*time.Hour), now.Add(-time.Hour)
	c := inTreeCluster()
	c.Status.Conditions = []metav1.Condition{{Type: cnpgstatus.ConditionContinuousArchiving, Status: metav1.ConditionTrue, Reason: "ContinuousArchivingSuccess"}}
	r := instancestatus.Reading{IsPrimary: false, IsArchivingWAL: true, ObservedAt: read}
	g := &gathered{
		cluster: c,
		swept:   sweep.Snapshot{Interval: 30 * time.Second, Instances: map[string]sweep.Instance{"db-2": {State: viewmodel.StatusReadOK, Reading: &r}}},
		// Zeros a former sweep recorded (before the sweep stopped recording
		// a standby's count) must not read as quiet.
		readyRings: map[string][]viewmodel.Point{"db-2": ring(read, 0, 0, 0, 0)},
	}
	reading := cnpgstatus.Archiving(c, primaryArchiver(g))
	if reading.Class == cnpgstatus.ClassOK || reading.Reason != cnpgstatus.ArchivingReasonBacklogNotReported {
		t.Fatalf("archiving of a designated primary: %+v", reading)
	}
	arch := viewmodel.Archiving{Health: health(reading.Class), Reason: reading.Reason, ConditionStatus: reading.ConditionStatus}
	facts := recoveryFactsOf(g, arch, now)
	if !facts.archiverUnread || !facts.notReported {
		t.Fatalf("facts %+v: want the archiver unread and not reported", facts)
	}
	tl := svc.recoveryTimeline(fx.evidence(), viewmodel.BackupSummary{Archiving: arch}, nil, facts)
	if tl.State == string(evidencev1alpha1.Healthy) || len(tl.Reasons) == 0 || tl.Reasons[0].Code != "recovery.reason.archiverNotReported" {
		t.Errorf("state %s reasons %+v", tl.State, tl.Reasons)
	}
	band := edgeBand(tl, "recovery.segment.archiverNotReported")
	if band == nil || !band.From.Equal(last.Add(-failureLookback)) {
		t.Fatalf("band %+v, want the day before the latest point", band)
	}
	b := viewmodel.Backups{RecoveryTimeline: tl, Summary: viewmodel.BackupSummary{Archiving: arch}, GeneratedAt: now}
	got := restoreCheck(b, now.Add(-2*time.Hour))
	if got.Answer == "covered" || !slices.ContainsFunc(got.Caveats, func(p viewmodel.Phrase) bool { return p.Code == "recovery.caveat.archiverNotReported" }) {
		t.Errorf("two hours ago on a designated primary: %+v", got)
	}
	if got := restoreCheck(b, now.Add(-30*time.Hour)); got.Answer != "covered" {
		t.Errorf("before the band: %+v", got)
	}
	// An instance that is not a primary now shows no catch-up from its
	// failure: its 0 ready files are no reading.
	r.LastFailedWALTime, r.LastArchivedWALTime = &failed, &archived
	if by := caughtUpBy(g, "db-2"); by != nil {
		t.Errorf("caught up by %v on a standby's zeros", by)
	}
	r.IsPrimary = true
	if by := caughtUpBy(g, "db-2"); by == nil {
		t.Errorf("a primary below the threshold since its failure shows no catch-up")
	}
}

// TestUnreadPrimaryKeepsItsRing (round 7, CNPG P2-4): with the current
// primary unread now, its ring is still read, so a sweep that read it
// with nothing waiting bounds the unread band (archiverUnreadEdge's quiet
// branch); an instance other than the primary is kept only when read now
// with a failure to report, or when its ring holds a reading taken while
// it was primary (a former primary, round 8 P2-2): a standby's ring holds
// gaps only.
func TestUnreadPrimaryKeepsItsRing(t *testing.T) {
	failed := now.Add(-time.Hour)
	g := &gathered{
		cluster: inTreeCluster(),
		swept: sweep.Snapshot{Interval: 30 * time.Second, Instances: map[string]sweep.Instance{
			"db-2": {State: viewmodel.StatusReadUnreachable},
			"db-1": {State: viewmodel.StatusReadUnreachable, Reading: &instancestatus.Reading{LastFailedWALTime: &failed}},
			"db-3": {State: viewmodel.StatusReadOK, Reading: &instancestatus.Reading{}},
		}},
	}
	standby := []viewmodel.Point{{At: now.Add(-30 * time.Second)}, {At: now}}
	sw := ringSweep{rings: map[string][]viewmodel.Point{"db-1": standby, "db-3": standby}}
	rings, _ := readyRings(sw, g)
	if _, ok := rings["db-2"]; !ok {
		t.Fatalf("the unread primary's ring is not read: %+v", rings)
	}
	if len(rings) != 1 {
		t.Errorf("rings of instances the facts do not use: %+v", rings)
	}
	sw.rings["db-3"] = []viewmodel.Point{{At: now.Add(-30 * time.Second), Value: new(2.0)}, {At: now}}
	if former, _ := readyRings(sw, g); len(former["db-3"]) != 2 {
		t.Errorf("a former primary's ring is not read: %+v", former)
	}
	g.readyRings = rings
	unread := viewmodel.Archiving{Health: viewmodel.HealthUnknown, Reason: cnpgstatus.ArchivingReasonPrimaryUnread}
	facts := recoveryFactsOf(g, unread, now)
	if q := facts.quietBy(nil); q == nil || !q.at.Equal(now.Add(-30*time.Second)) {
		t.Errorf("quiet sweep %v, want the ring's empty reading at %s", q, now.Add(-30*time.Second))
	}
}

// TestOneMissedPrimaryReadDoesNotFlicker (round 7, DBA P2-1): one sweep
// that failed to read the primary keeps its last good reading from the
// sweep before, so archiving and the recovery bar do not flip to unknown
// and back; a second miss in a row, or a reading older than one sweep,
// is unread.
func TestOneMissedPrimaryReadDoesNotFlicker(t *testing.T) {
	const interval = 30 * time.Second
	swept := now.Add(-5 * time.Second)
	c := inTreeCluster()
	c.Status.Conditions = []metav1.Condition{{Type: cnpgstatus.ConditionContinuousArchiving, Status: metav1.ConditionTrue, Reason: "ContinuousArchivingSuccess"}}
	for _, tc := range []struct {
		name     string
		state    viewmodel.StatusReadState
		observed time.Time
		since    time.Time
		read     bool
	}{
		{name: "read now", state: viewmodel.StatusReadOK, observed: swept, since: swept.Add(-time.Hour), read: true},
		{name: "one miss: the sweep before stands", state: viewmodel.StatusReadUnreachable, observed: swept.Add(-interval), since: swept, read: true},
		{name: "one miss with ticker jitter", state: viewmodel.StatusReadUnreachable, observed: swept.Add(-interval - 10*time.Second), since: swept, read: true},
		{name: "two misses in a row", state: viewmodel.StatusReadUnreachable, observed: swept.Add(-2 * interval), since: swept.Add(-interval), read: false},
		{name: "the failure began earlier", state: viewmodel.StatusReadUnreachable, observed: swept.Add(-interval), since: swept.Add(-interval), read: false},
		{name: "a reading much older than the sweep", state: viewmodel.StatusReadBadResponse, observed: swept.Add(-10 * time.Minute), since: swept, read: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := instancestatus.Reading{IsPrimary: true, IsArchivingWAL: true, ObservedAt: tc.observed}
			g := &gathered{cluster: c, swept: sweep.Snapshot{SweptAt: swept, Interval: interval, Instances: map[string]sweep.Instance{
				"db-2": {State: tc.state, Since: tc.since, Reading: &r},
			}}}
			reading := cnpgstatus.Archiving(c, primaryArchiver(g))
			if tc.read && (reading.Class != cnpgstatus.ClassOK || primaryArchiverReading(g) == nil) {
				t.Errorf("archiving %+v, want ok on the last good reading", reading)
			}
			if !tc.read && (reading.Class != cnpgstatus.ClassUnknown || reading.Reason != cnpgstatus.ArchivingReasonPrimaryUnread) {
				t.Errorf("archiving %+v, want unknown PrimaryUnread", reading)
			}
			arch := viewmodel.Archiving{Health: health(reading.Class), Reason: reading.Reason}
			if facts := recoveryFactsOf(g, arch, now); facts.archiverUnread == tc.read {
				t.Errorf("archiver unread %t, want %t", facts.archiverUnread, !tc.read)
			}
		})
	}
}

// busyPrimary is a primary with 16 MiB segments that never reads zero
// .ready files: over 60 sweeps (30 min, one every 30 s, before read) it
// switches a segment every 10 sweeps and keeps ready files waiting. Its
// reading names its current WAL file, from which the segment size
// follows.
func busyPrimary(read time.Time, ready int) (instancestatus.Reading, []viewmodel.Point, []viewmodel.Point) {
	const size, base = 16 << 20, 1300
	position := func(i int) uint64 { return uint64(base+i/10)*size + uint64(i)*4096 }
	counts, lsns := make([]float64, 0, 60), make([]float64, 0, 60)
	for i := range 60 {
		counts = append(counts, float64(ready))
		lsns = append(lsns, float64(position(i)))
	}
	current := position(60)
	segno := current / size
	r := instancestatus.Reading{
		IsPrimary: true, IsArchivingWAL: true, ReadyWALFiles: ready, ObservedAt: read,
		CurrentLSN: fmt.Sprintf("%X/%X", current>>32, current&0xFFFFFFFF), CurrentWAL: fmt.Sprintf("%08X%08X%08X", 1, segno/256, segno%256),
	}
	return r, ring(read, counts...), ring(read, lsns...)
}

// TestBusyPrimaryBacklogBoundByPositions (round 7, DBA P2-2): a busy
// primary may never read zero .ready files, which left a day-long band.
// At a sweep with k files waiting and the position in segment S, every
// segment before S-k was archived; S-k switched after the newest sweep
// whose position had not reached the start of S-k+1, and a commit in it
// is at most one archive_timeout older than its switch. The band starts
// there, for a backlog and for an unread primary; without the positions,
// or with a segment size the reading does not settle, it stays a day.
func TestBusyPrimaryBacklogBoundByPositions(t *testing.T) {
	svc := &Service{}
	read := now.Add(-30 * time.Second)
	at := func(i int) time.Time { return read.Add(-time.Duration(60-i) * 30 * time.Second) }
	// The newest sweep (i=59, segment base+5, 3 waiting) finds the start
	// of segment base+3 not reached at i=29 (segment base+2).
	wantFrom := at(29).Add(-defaultArchiveTimeout)
	last := now.Add(-time.Minute)
	fx := timelineFixture{paths: []evidence.RecoveryPath{tlPath("b1", 48*time.Hour, time.Minute, evidencev1alpha1.CoverageFrontier)}}
	for _, tc := range []struct {
		name      string
		unread    bool
		positions bool
		badWAL    bool
		wantCode  string
		wantFrom  time.Time
	}{
		{name: "backlog, positions known", positions: true, wantCode: "recovery.segment.archiveBacklogBound", wantFrom: wantFrom},
		{name: "unread, positions known", unread: true, positions: true, wantCode: "recovery.segment.archiverUnreadBound", wantFrom: wantFrom},
		{name: "backlog, no position ring", wantCode: "recovery.segment.archiveBacklog", wantFrom: last.Add(-failureLookback)},
		{name: "backlog, the segment size is not settled", positions: true, badWAL: true, wantCode: "recovery.segment.archiveBacklog", wantFrom: last.Add(-failureLookback)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ready, lsns := busyPrimary(read, 3)
			if tc.badWAL {
				r.CurrentWAL = "000000010000000000000000"
			}
			inst := sweep.Instance{State: viewmodel.StatusReadOK, Reading: &r}
			arch := viewmodel.Archiving{Health: viewmodel.HealthWarning, Reason: cnpgstatus.ArchivingReasonBacklog}
			if tc.unread {
				inst.State = viewmodel.StatusReadUnreachable
				arch = viewmodel.Archiving{Health: viewmodel.HealthUnknown, Reason: cnpgstatus.ArchivingReasonPrimaryUnread}
			}
			g := &gathered{
				cluster:    inTreeCluster(),
				swept:      sweep.Snapshot{SweptAt: now, Interval: 30 * time.Second, Instances: map[string]sweep.Instance{"db-2": inst}},
				readyRings: map[string][]viewmodel.Point{"db-2": ready},
			}
			if tc.positions {
				g.lsnRings = map[string][]viewmodel.Point{g.cluster.Status.CurrentPrimary: lsns}
			}
			facts := recoveryFactsOf(g, arch, now)
			tl := svc.recoveryTimeline(fx.evidence(), viewmodel.BackupSummary{Archiving: arch}, nil, facts)
			band := edgeBand(tl, "recovery.segment.archiveBacklog", "recovery.segment.archiveBacklogSince", "recovery.segment.archiveBacklogBound",
				"recovery.segment.archiverUnread", "recovery.segment.archiverUnreadSince", "recovery.segment.archiverUnreadBound")
			if band == nil || band.Code != tc.wantCode || !band.From.Equal(tc.wantFrom) {
				t.Fatalf("band %+v, want %s from %s (backlog %d, bounds %+v)", band, tc.wantCode, tc.wantFrom, facts.backlog, facts.bounds)
			}
			b := viewmodel.Backups{RecoveryTimeline: tl, Summary: viewmodel.BackupSummary{Archiving: arch}, GeneratedAt: now}
			if got := restoreCheck(b, tc.wantFrom.Add(time.Second)); got.Answer == "covered" {
				t.Errorf("just inside the band: %+v", got)
			}
			if got := restoreCheck(b, tc.wantFrom.Add(-time.Minute)); got.Answer != "covered" {
				t.Errorf("before the band: %+v", got)
			}
		})
	}
}

// TestWALSegmentSize: the size the current WAL file name and position
// agree on, at a segment boundary too (pg_walfile_name names the file
// before); none when several sizes fit or the name does not parse.
func TestWALSegmentSize(t *testing.T) {
	for _, tc := range []struct {
		lsn, wal string
		want     uint64
	}{
		{"5/2A000100", "00000001000000050000002A", 16 << 20},
		{"5/2B000000", "00000001000000050000002A", 16 << 20},
		{"5/2A000100", "0000000100000005000002A0", 1 << 20},
		{"5/40000100", "000000010000000500000001", 1 << 30},
		{"5/00000100", "000000010000000500000000", 0},
		{"5/2A000100", "not-a-wal-file-name-0000", 0},
		{"5/2A000100", "00000001000000060000002A", 0},
	} {
		got, ok := walSegmentSize(&instancestatus.Reading{CurrentLSN: tc.lsn, CurrentWAL: tc.wal})
		if got != tc.want || ok != (tc.want != 0) {
			t.Errorf("walSegmentSize(%s, %s) = %d %t, want %d", tc.lsn, tc.wal, got, ok, tc.want)
		}
	}
}

// ringSweep is fakeSweep with a readyWalFiles ring per instance; an
// instance it does not name gets fakeSweep's.
type ringSweep struct {
	fakeSweep
	rings map[string][]viewmodel.Point
}

func (r ringSweep) Series(key sweep.Key, instance string) []viewmodel.Series {
	if points, ok := r.rings[instance]; ok {
		return []viewmodel.Series{{Key: sweep.SeriesReadyWALFiles, Unit: "files", Points: points}}
	}
	return r.fakeSweep.Series(key, instance)
}
