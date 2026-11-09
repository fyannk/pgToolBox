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

package sharedstate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func passed(name string, finished time.Time) DrillResult {
	return DrillResult{
		Namespace: "team-a", Name: name, SourceNamespace: "team-a", Source: "orders",
		Backup: "orders-nightly-1", Result: "passed",
		StartedAt: finished.Add(-time.Hour).Unix(), HealthyAt: finished.Add(-30 * time.Minute).Unix(), FinishedAt: finished.Unix(),
	}
}

// TestDrillResultsSurviveARestart: a finished drill's outcome written by
// one console is read back by the next one, which has no memory of it.
func TestDrillResultsSurviveARestart(t *testing.T) {
	api := newAPIServer()
	clock := &testClock{now: t0}
	before := newStore(api, clock, nil)
	run(t, before)
	before.StartKeepingDrills(t0.Add(-48 * time.Hour))
	before.KeepDrill(passed("drill-orders-1", t0))
	eventually(t, "the result written", func() bool { return strings.Contains(api.data(KeyDrills), "drill-orders-1") })

	after := newStore(api, clock, nil)
	if len(after.KeptDrills()) != 0 {
		t.Fatal("a new store knows a drill before reading the ConfigMap")
	}
	run(t, after)
	eventually(t, "the result read back", func() bool { return len(after.KeptDrills()) == 1 })
	if got := after.KeptDrills()[0]; got != passed("drill-orders-1", t0) {
		t.Errorf("kept = %+v", got)
	}
	if since := after.DrillsKeptSince(); !since.Equal(t0.Add(-48 * time.Hour)) {
		t.Errorf("kept since = %v, want the earliest start written", since)
	}
	if !after.KeepsDrills() {
		t.Error("a live ConfigMap does not keep drills")
	}
}

func TestDrillResultsRejectWhatTheConsoleWouldNotWrite(t *testing.T) {
	s := newStore(nil, &testClock{now: t0}, nil)
	running := passed("running", t0)
	running.Result = "running"
	backwards := passed("backwards", t0)
	backwards.FinishedAt = backwards.StartedAt - 1
	noSource := passed("no-source", t0)
	noSource.Source = ""
	long := passed(strings.Repeat("x", 254), t0)
	for _, r := range []DrillResult{running, backwards, noSource, long} {
		s.KeepDrill(r)
	}
	if got := s.KeptDrills(); len(got) != 0 {
		t.Fatalf("kept %+v, want none", got)
	}
	// A reason over its bound is cut, never refused, and stays UTF-8.
	reasoned := passed("reasoned", t0)
	reasoned.Result, reasoned.Reason = "failed", strings.Repeat("é", maxDrillReason)
	s.KeepDrill(reasoned)
	got := s.KeptDrills()
	if len(got) != 1 || len(got[0].Reason) > maxDrillReason || !strings.HasPrefix(reasoned.Reason, got[0].Reason) {
		t.Fatalf("reason kept as %d bytes", len(got[0].Reason))
	}

	// A remote document is held to the same rules.
	remote := drillsDoc{Version: formatVersion, Results: map[string]DrillResult{
		"team-a/ok":       passed("ok", t0),
		"team-a/mismatch": passed("other", t0),
		"team-a/running":  running,
	}}
	raw, err := json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.mergeLocked(&corev1.ConfigMap{Data: map[string]string{KeyDrills: string(raw)}})
	s.mu.Unlock()
	names := map[string]bool{}
	for _, r := range s.KeptDrills() {
		names[r.Name] = true
	}
	if !names["ok"] || names["other"] || names["running"] {
		t.Errorf("merged %v", names)
	}
}

func TestDrillResultsMergeAndBounds(t *testing.T) {
	s := newStore(nil, &testClock{now: t0}, nil)
	// One pass long ago, then more failures than the per-source bound.
	s.KeepDrill(passed("drill-old-pass", t0.Add(-200*24*time.Hour)))
	for i := range maxDrillsPerSource + 5 {
		r := passed(fmt.Sprintf("drill-%03d", i), t0.Add(time.Duration(i)*time.Hour))
		r.Result = "failed"
		s.KeepDrill(r)
	}
	kept := s.KeptDrills()
	names := map[string]bool{}
	for _, r := range kept {
		names[r.Name] = true
	}
	if len(kept) != maxDrillsPerSource+1 {
		t.Errorf("kept %d, want the bound plus the newest pass", len(kept))
	}
	if !names["drill-old-pass"] {
		t.Error("the source's newest pass was dropped for the bound")
	}
	if names["drill-000"] || !names[fmt.Sprintf("drill-%03d", maxDrillsPerSource+4)] {
		t.Error("want the oldest dropped first, the newest kept")
	}

	// The later result of one drill wins, whichever replica holds it.
	early, late := passed("drill-x", t0), passed("drill-x", t0.Add(time.Minute))
	early.Source, late.Source = "billing", "billing"
	late.Result = "failed"
	s.KeepDrill(late)
	s.KeepDrill(early)
	for _, r := range s.KeptDrills() {
		if r.Name == "drill-x" && r.Result != "failed" {
			t.Errorf("drill-x = %s, want the later result", r.Result)
		}
	}
}

func TestDrillResultsNotKeptWithoutAConfigMap(t *testing.T) {
	for name, setup := range map[string]func(*apiServer){
		"missing":   func(a *apiServer) { a.cm = nil },
		"forbidden": func(a *apiServer) { a.forbidden = true },
	} {
		t.Run(name, func(t *testing.T) {
			api := newAPIServer()
			setup(api)
			s := newStore(api, &testClock{now: t0}, nil)
			run(t, s)
			eventually(t, "the ConfigMap judged", s.MarksKnown)
			if s.KeepsDrills() {
				t.Error("drills reported kept with no ConfigMap to keep them in")
			}
		})
	}
	if New(Options{}).KeepsDrills() {
		t.Error("a disabled store keeps drills")
	}
}
