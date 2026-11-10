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

package evidence

import (
	"context"
	"strings"
	"testing"
	"time"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func capFixturePaths() []RecoveryPath {
	receipt := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	path := func(state evidencev1alpha1.State, stop evidencev1alpha1.CoverageStop, message string, offTarget bool) RecoveryPath {
		at := receipt
		return RecoveryPath{
			BackupID: "b", TargetTimeline: 1, State: state, Stop: stop, FrontierReceiptAt: &at, OffTarget: offTarget,
			Reason: evidencev1alpha1.Reason{Code: "coverage-path-" + string(state), Message: message}, Assumptions: []string{"segment-name-presence-only"},
		}
	}
	return []RecoveryPath{
		path(evidencev1alpha1.Healthy, evidencev1alpha1.CoverageFrontier, "observed contiguous segment-name coverage reaches the current archive frontier", false),
		path(evidencev1alpha1.Warning, evidencev1alpha1.CoverageFrontier, "timeline 2 branched before the backup reached consistency; the backup recovers only on its own timeline, which a restore must name", true),
		path(evidencev1alpha1.Unhealthy, evidencev1alpha1.CoverageGapLimited, "confirmed required WAL gap stops coverage; a restore must name timeline 1", true),
	}
}

// TestCapRecovery: an older producer, or a timeline that is not healthy,
// leaves no path the console may draw as coverage, for Barman Cloud and
// pgBackRest alike; the paths' own reasons survive the cap.
func TestCapRecovery(t *testing.T) {
	t.Parallel()
	barman := func(t *testing.T) *evidencev1alpha1.RepositoryEvidenceSnapshot {
		snapshot := fixtureSnapshot(t)
		return &snapshot
	}
	pgbackrest := func(t *testing.T) *evidencev1alpha1.RepositoryEvidenceSnapshot {
		snapshot, _, _ := pgbackrestFixture(t)
		// The fixture's coverage is a warning; the roll-up cap has its own
		// test.
		snapshot.Details.PGBackRest.Coverage.State = evidencev1alpha1.Healthy
		return &snapshot
	}
	for _, test := range []struct {
		name     string
		snapshot func(*testing.T) *evidencev1alpha1.RepositoryEvidenceSnapshot
		mutate   func(*evidencev1alpha1.RepositoryEvidenceSnapshot)
		capped   bool
		reason   string
		warning  string
	}{
		{name: "Barman, current producer, healthy timeline", snapshot: barman},
		{name: "pgBackRest, current producer, healthy timeline", snapshot: pgbackrest},
		{name: "Barman, producer without recovery-timeline-latest", snapshot: barman, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) { s.Producer.Features = nil }, capped: true, reason: "older build"},
		// A producer that resolves 'latest' but not its ancestry may call
		// a sibling branch healthy (round 4, P1-6).
		{name: "Barman, producer without recovery-timeline-ancestry", snapshot: barman, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			s.Producer.Features = []evidencev1alpha1.Feature{evidencev1alpha1.FeatureRecoveryTimelineLatest}
		}, capped: true, reason: "older build"},
		{name: "pgBackRest, producer without recovery-timeline-latest", snapshot: pgbackrest, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			s.Producer.Features = []evidencev1alpha1.Feature{"future-guarantee"}
		}, capped: true, reason: "older build"},
		{name: "Barman, timeline unknown", snapshot: barman, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			s.Details.BarmanCloud.Timeline = evidencev1alpha1.StateReason{State: evidencev1alpha1.Unknown, Reason: evidencev1alpha1.Reason{Code: "timeline-unknown", Message: "history 2 is missing"}}
		}, capped: true, reason: "timeline evidence is unknown (history 2 is missing)"},
		{name: "pgBackRest, timeline unknown", snapshot: pgbackrest, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			s.Details.PGBackRest.Timeline = evidencev1alpha1.StateReason{State: evidencev1alpha1.Unknown, Reason: evidencev1alpha1.Reason{Code: "timeline-unknown", Message: "a timeline history file is missing or unreadable"}}
		}, capped: true, reason: "timeline evidence is unknown"},
		// A history no default restore reads is invalid: a warning that
		// names the file, kept beside the paths, capping none (round 4,
		// P2-2).
		{name: "Barman, timeline warning", snapshot: barman, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			s.Details.BarmanCloud.Timeline = evidencev1alpha1.StateReason{State: evidencev1alpha1.Warning, Reason: evidencev1alpha1.Reason{Code: "timeline-warning", Message: "00000002.history: timeline history is malformed; no default restore reads it"}}
		}, warning: "00000002.history"},
		{name: "Barman, timeline unhealthy", snapshot: barman, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			s.Details.BarmanCloud.Timeline.State = evidencev1alpha1.Unhealthy
		}, capped: true, reason: "timeline evidence is unhealthy"},
		{name: "no details", snapshot: barman, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) { s.Details.BarmanCloud = nil }, capped: true, reason: "timeline evidence is unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := test.snapshot(t)
			if test.mutate != nil {
				test.mutate(snapshot)
			}
			in := Recovery{State: RecoveryAvailable, Complete: true, Paths: capFixturePaths()}
			got := capRecovery(snapshot, in)
			if in.Paths[0].State != evidencev1alpha1.Healthy || in.Paths[0].FrontierReceiptAt == nil {
				t.Fatal("capRecovery changed its input")
			}
			if !test.capped {
				if got.State != RecoveryAvailable || got.Paths[0].State != evidencev1alpha1.Healthy || got.Paths[0].FrontierReceiptAt == nil {
					t.Fatalf("an uncapped recovery changed: %+v", got)
				}
				if !strings.Contains(got.TimelineWarning, test.warning) || (test.warning == "") != (got.TimelineWarning == "") {
					t.Fatalf("timeline warning %q, want %q", got.TimelineWarning, test.warning)
				}
				return
			}
			if got.State != RecoveryUncertain || !strings.Contains(got.Reason, test.reason) || len(got.Reason) > maxCapReasonBytes {
				t.Fatalf("recovery %s %q, want uncertain with %q", got.State, got.Reason, test.reason)
			}
			for index, path := range got.Paths {
				if path.State == evidencev1alpha1.Healthy || path.State == evidencev1alpha1.Warning || path.FrontierReceiptAt != nil {
					t.Errorf("path %d still claims coverage: %+v", index, path)
				}
				if !strings.HasPrefix(path.Reason.Message, in.Paths[index].Reason.Message) || !strings.Contains(path.Reason.Message, "not coverage") || len(path.Reason.Message) > maxCapReasonBytes {
					t.Errorf("path %d reason %q lost its own or the cap's", index, path.Reason.Message)
				}
			}
			if got.Paths[2].State != evidencev1alpha1.Unhealthy || !got.Paths[1].OffTarget {
				t.Errorf("a proven loss or the off-target flag changed: %+v", got.Paths)
			}
		})
	}
	for _, state := range []string{RecoveryUnavailable, RecoveryUnsupported, RecoveryNotScanned} {
		snapshot := fixtureSnapshot(t)
		snapshot.Producer.Features = nil
		if got := capRecovery(&snapshot, Recovery{State: state, Reason: "kept"}); got.State != state || got.Reason != "kept" {
			t.Errorf("a %s recovery was capped: %+v", state, got)
		}
	}
}

// TestCapRecoveryAtTheCoverageRollup (round 4): no path claims more than
// the agent's own coverage roll-up. When the agent says a default restore
// succeeds from no backup (a warning), a path that reads healthy is drawn
// as a warning, keeping its frontier and its reason; an unknown roll-up
// (conservative over every backup) caps a healthy path at a warning too,
// never lower; a healthy roll-up changes nothing.
func TestCapRecoveryAtTheCoverageRollup(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		state evidencev1alpha1.State
		want  evidencev1alpha1.State
	}{
		{name: "healthy roll-up", state: evidencev1alpha1.Healthy, want: evidencev1alpha1.Healthy},
		{name: "warning roll-up", state: evidencev1alpha1.Warning, want: evidencev1alpha1.Warning},
		{name: "unknown roll-up", state: evidencev1alpha1.Unknown, want: evidencev1alpha1.Warning},
		{name: "unhealthy roll-up", state: evidencev1alpha1.Unhealthy, want: evidencev1alpha1.Warning},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := fixtureSnapshot(t)
			snapshot.Details.BarmanCloud.Coverage = evidencev1alpha1.StateReason{State: test.state, Reason: evidencev1alpha1.Reason{Code: "coverage-" + string(test.state), Message: "no backup on or before the newest timeline's branch point: a default restore succeeds from none"}}
			in := Recovery{State: RecoveryAvailable, Complete: true, Paths: capFixturePaths()}
			got := capRecovery(&snapshot, in)
			if in.Paths[0].State != evidencev1alpha1.Healthy {
				t.Fatal("capRecovery changed its input")
			}
			healthy := got.Paths[0]
			if got.State != RecoveryAvailable || healthy.State != test.want || healthy.FrontierReceiptAt == nil || !strings.HasPrefix(healthy.Reason.Message, in.Paths[0].Reason.Message) {
				t.Fatalf("recovery %s, healthy path %+v", got.State, healthy)
			}
			if test.state != evidencev1alpha1.Healthy && !strings.Contains(healthy.Reason.Message, "succeeds from none") {
				t.Fatalf("capped path does not carry the roll-up reason: %q", healthy.Reason.Message)
			}
			if got.Paths[1].State != evidencev1alpha1.Warning || got.Paths[2].State != evidencev1alpha1.Unhealthy {
				t.Fatalf("the warning or the proven loss changed: %+v", got.Paths)
			}
		})
	}
}

// TestCapRecoveryCarriesTheCoverageReason: when the agent's coverage
// roll-up is not healthy, its reason reaches the read model, which says it
// instead of a generic "no path reaches consistency".
func TestCapRecoveryCarriesTheCoverageReason(t *testing.T) {
	t.Parallel()
	snapshot := fixtureSnapshot(t)
	if got := capRecovery(&snapshot, Recovery{State: RecoveryAvailable, Paths: capFixturePaths()}); snapshot.Details.BarmanCloud.Coverage.State == evidencev1alpha1.Healthy && got.CoverageReason != "" {
		t.Fatalf("a healthy coverage carried a reason: %q", got.CoverageReason)
	}
	want := "no backup on or before the newest timeline's branch point"
	snapshot.Details.BarmanCloud.Coverage = evidencev1alpha1.StateReason{State: evidencev1alpha1.Unknown, Reason: evidencev1alpha1.Reason{Code: "coverage-unknown", Message: want}}
	if got := capRecovery(&snapshot, Recovery{State: RecoveryAvailable, Paths: capFixturePaths()}); got.CoverageReason != want {
		t.Fatalf("coverage reason %q, want %q", got.CoverageReason, want)
	}
}

// TestRunnerCapsRecoveryOfAnOlderAgent: the cap is applied at ingest, so
// the read model receives an uncertain Recovery from an agent that does not
// declare recovery-timeline-latest.
func TestRunnerCapsRecoveryOfAnOlderAgent(t *testing.T) {
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	fake := newFakeAgent(t, ca)
	snapshot := fixtureSnapshot(t)
	snapshot.Producer.Features = nil
	fake.pages = backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
	fake.recovery = fixtureRecovery(t, snapshot)
	fake.snapshot = snapshot
	runner := newTestRunner(t, newFakeLister(t, testEvidenceAgent(testAgent, ca, snapshot, testNow)), fake, writeToken(t, testToken))
	runner.refresh(context.Background())
	got := runner.ForCluster(testNamespace, testCluster).Recovery
	if got.State != RecoveryUncertain || len(got.Paths) != 1 || got.Paths[0].State == evidencev1alpha1.Healthy || got.Paths[0].FrontierReceiptAt != nil {
		t.Fatalf("recovery of an older agent = %+v", got)
	}
}
