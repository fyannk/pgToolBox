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
		return &snapshot
	}
	for _, test := range []struct {
		name     string
		snapshot func(*testing.T) *evidencev1alpha1.RepositoryEvidenceSnapshot
		mutate   func(*evidencev1alpha1.RepositoryEvidenceSnapshot)
		capped   bool
		reason   string
	}{
		{name: "Barman, current producer, healthy timeline", snapshot: barman},
		{name: "pgBackRest, current producer, healthy timeline", snapshot: pgbackrest},
		{name: "Barman, producer without recovery-timeline-latest", snapshot: barman, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) { s.Producer.Features = nil }, capped: true, reason: "older build"},
		{name: "pgBackRest, producer without recovery-timeline-latest", snapshot: pgbackrest, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			s.Producer.Features = []evidencev1alpha1.Feature{"future-guarantee"}
		}, capped: true, reason: "older build"},
		{name: "Barman, timeline unknown", snapshot: barman, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			s.Details.BarmanCloud.Timeline = evidencev1alpha1.StateReason{State: evidencev1alpha1.Unknown, Reason: evidencev1alpha1.Reason{Code: "timeline-unknown", Message: "history 2 is missing"}}
		}, capped: true, reason: "timeline evidence is unknown (history 2 is missing)"},
		{name: "pgBackRest, timeline unknown", snapshot: pgbackrest, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			s.Details.PGBackRest.Timeline = evidencev1alpha1.StateReason{State: evidencev1alpha1.Unknown, Reason: evidencev1alpha1.Reason{Code: "timeline-unknown", Message: "a timeline history file is missing or unreadable"}}
		}, capped: true, reason: "timeline evidence is unknown"},
		{name: "Barman, timeline warning", snapshot: barman, mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
			s.Details.BarmanCloud.Timeline.State = evidencev1alpha1.Warning
		}, capped: true, reason: "timeline evidence is warning"},
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
