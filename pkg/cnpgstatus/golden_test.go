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

package cnpgstatus

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

// assertGolden compares v, rendered as indented JSON, with
// testdata/<name>.golden.json, rewriting it under -update. A table
// change therefore shows up as a reviewed diff of that file.
func assertGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name+".golden.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run go test -update to create it): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file; review the table change and rerun with -update.\ngot:\n%s", path, got)
	}
}

// phaseGolden is the golden document of one minor's phase table, with
// each row's classification by ClassifyPhase alongside it.
type phaseGolden struct {
	Minor  Minor            `json:"minor"`
	Phases []phaseGoldenRow `json:"phases"`
	// Probes are classifications of strings outside the table.
	Probes []phaseProbe `json:"probes"`
}

type phaseGoldenRow struct {
	PhaseEntry
	Reading PhaseReading `json:"reading"`
}

type phaseProbe struct {
	Phase   string       `json:"phase"`
	Reading PhaseReading `json:"reading"`
}

func TestPhaseTablesGolden(t *testing.T) {
	for _, minor := range SupportedMinors() {
		doc := phaseGolden{Minor: minor}
		for _, row := range PhaseTable(minor) {
			doc.Phases = append(doc.Phases, phaseGoldenRow{PhaseEntry: row, Reading: ClassifyPhase(minor, row.Phase)})
		}
		for _, phase := range []string{"", "Cluster in healthy state ", "cluster in healthy state", "Invalid cluster definition", "Hibernated"} {
			doc.Probes = append(doc.Probes, phaseProbe{Phase: phase, Reading: ClassifyPhase(minor, phase)})
		}
		assertGolden(t, "phases-"+string(minor), doc)
	}
}

// conditionGolden is the golden document of one minor's condition
// table (fork rows included, marked), with probes of each type.
type conditionGolden struct {
	Minor  Minor              `json:"minor"`
	Rules  []ConditionRule    `json:"rules"`
	Probes []conditionProbeIO `json:"probes"`
}

type conditionProbeIO struct {
	Type       string           `json:"type"`
	Status     string           `json:"status"`
	Reason     string           `json:"reason"`
	Upstream   ConditionReading `json:"upstream"`
	ForkPinned ConditionReading `json:"forkPinned"`
}

func TestConditionTablesGolden(t *testing.T) {
	types := []string{
		ConditionReady, ConditionContinuousArchiving, ConditionLastBackupSucceeded,
		ConditionConsistentSystemID, ConditionInitialized, ConditionSyncReplicationTopologySatisfied,
		ConditionHibernation, ConditionTopologyUnavailable, ConditionVolumeExpansionRefused,
		ConditionVolumeAutoGrowStopped, "SomethingNew",
	}
	type probe struct{ typ, status, reason string }
	probes := make([]probe, 0, 4*len(types)+4)
	for _, conditionType := range types {
		for _, status := range []string{"True", "False", "Unknown", "Maybe"} {
			probes = append(probes, probe{conditionType, status, ""})
		}
	}
	probes = append(probes,
		probe{ConditionLastBackupSucceeded, "False", "BackupStarted"},
		probe{ConditionConsistentSystemID, "False", "Mismatch"},
		probe{ConditionHibernation, "False", "DeletingPods"},
		probe{ConditionHibernation, "False", "WaitingForHealthy"},
	)
	for _, minor := range SupportedMinors() {
		doc := conditionGolden{Minor: minor, Rules: ConditionTable(minor, true)}
		for _, p := range probes {
			doc.Probes = append(doc.Probes, conditionProbeIO{
				Type: p.typ, Status: p.status, Reason: p.reason,
				Upstream:   ClassifyCondition(minor, p.typ, p.status, p.reason),
				ForkPinned: ClassifyConditionPinned(minor, true, p.typ, p.status, p.reason),
			})
		}
		assertGolden(t, "conditions-"+string(minor), doc)
	}
}
