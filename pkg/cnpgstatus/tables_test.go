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
	"slices"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
)

func TestWorst(t *testing.T) {
	tests := []struct {
		name string
		in   []Class
		want Class
	}{
		{"empty is unknown", nil, ClassUnknown},
		{"single info", []Class{ClassInfo}, ClassInfo},
		{"ok over info", []Class{ClassInfo, ClassOK}, ClassOK},
		{"inactive over ok", []Class{ClassOK, ClassInactive}, ClassInactive},
		{"progressing over inactive", []Class{ClassInactive, ClassProgressing}, ClassProgressing},
		{"unknown over progressing", []Class{ClassProgressing, ClassUnknown, ClassOK}, ClassUnknown},
		{"warning over unknown", []Class{ClassUnknown, ClassWarning}, ClassWarning},
		{"critical over all", []Class{ClassCritical, ClassWarning, ClassOK, ClassInfo}, ClassCritical},
		{"foreign counts as unknown", []Class{ClassOK, "green"}, ClassUnknown},
		{"foreign alone is unknown", []Class{""}, ClassUnknown},
		{"foreign does not hide critical", []Class{"red", ClassCritical}, ClassCritical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Worst(tt.in...); got != tt.want {
				t.Errorf("Worst(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
	for _, c := range []Class{ClassOK, ClassProgressing, ClassWarning, ClassCritical, ClassInactive, ClassUnknown, ClassInfo} {
		if !c.Valid() {
			t.Errorf("%q is not Valid", c)
		}
	}
	if Class("healthy").Valid() {
		t.Error(`"healthy" is Valid`)
	}
}

func TestMinorOf(t *testing.T) {
	tests := []struct {
		in    string
		want  Minor
		found bool
	}{
		{"1.30.1", Minor130, true},
		{"v1.30.1", Minor130, true},
		{"1.29.2", "", false}, // below the support floor: no table
		{"1.31.0", Minor131, true},
		{"1.30.0-rc1", Minor130, true},
		{"1.30.0-dev24", Minor130, true},
		{">=1.30", Minor130, true},
		{">= 1.29", "", false},
		{"1.30", Minor130, true},
		{" 1.30.0 ", Minor130, true},
		{"1.28.4", "", false},
		{"1.32.0", "", false},
		{"2.0.0", "", false},
		{"", "", false},
		{"latest", "", false},
		{"main", "", false},
		{"1", "", false},
		{"1.30.0.1", "", false},
		{"1.x.0", "", false},
		{"-1.30.0", "", false},
		{"1.30.0-\x00", "", false},
		{"1.30.0-" + string(make([]byte, 80)), "", false},
		{"+1.30.0", "", false},
		{"1.30.0 ; rm -rf", "", false},
		{"9999999.30.0", "", false},
		{"16", "", false}, // app.kubernetes.io/version carries the PostgreSQL major
	}
	for _, tt := range tests {
		got, found := MinorOf(tt.in)
		if got != tt.want || found != tt.found {
			t.Errorf("MinorOf(%q) = %q, %v; want %q, %v", tt.in, got, found, tt.want, tt.found)
		}
	}
	if got := SupportedMinors(); !slices.Equal(got, []Minor{"1.30", "1.31"}) {
		t.Errorf("SupportedMinors() = %v", got)
	}
}

func TestClassifyPhase(t *testing.T) {
	tests := []struct {
		name  string
		minor Minor
		phase string
		want  PhaseReading
	}{
		{"healthy", Minor130, cnpgv1.PhaseHealthy, PhaseReading{Class: ClassOK, Known: true}},
		{"healthy by its literal", Minor130, "Cluster in healthy state", PhaseReading{Class: ClassOK, Known: true}},
		{"empty is unknown", Minor130, "", PhaseReading{Class: ClassUnknown, Known: false}},
		{"empty with unknown minor", "", "", PhaseReading{Class: ClassUnknown, Known: false}},
		{"unknown phase stays warning", Minor130, "Cluster is dancing", PhaseReading{Class: ClassWarning, Known: false}},
		{"case matters", Minor130, "cluster in healthy state", PhaseReading{Class: ClassWarning, Known: false}},
		{"trailing space is foreign", Minor130, "Cluster in healthy state ", PhaseReading{Class: ClassWarning, Known: false}},
		{"1.29 has no table and reads 1.30", "1.29", cnpgv1.PhaseDefinitionInvalid, PhaseReading{Class: ClassCritical, Known: true}},
		{"1.30 phase under 1.30", Minor130, cnpgv1.PhaseDefinitionInvalid, PhaseReading{Class: ClassCritical, Known: true}},
		{"1.30 phase under 1.31", Minor131, cnpgv1.PhaseDefinitionInvalid, PhaseReading{Class: ClassCritical, Known: true}},
		{"failover critical", Minor130, cnpgv1.PhaseFailOver, PhaseReading{Class: ClassCritical, Known: true}},
		{"switchover progressing", Minor130, cnpgv1.PhaseSwitchover, PhaseReading{Class: ClassProgressing, Known: true}},
		{"waiting for user warning", Minor130, cnpgv1.PhaseWaitingForUser, PhaseReading{Class: ClassWarning, Known: true}},
		{"setting up primary progressing", Minor130, cnpgv1.PhaseFirstPrimary, PhaseReading{Class: ClassProgressing, Known: true}},
		{"unknown minor uses 1.30", "1.28", cnpgv1.PhaseDefinitionInvalid, PhaseReading{Class: ClassCritical, Known: true}},
		{"unknown minor, unknown phase", "", "Something else", PhaseReading{Class: ClassWarning, Known: false}},
		{"garbage minor, healthy", "garbage", cnpgv1.PhaseHealthy, PhaseReading{Class: ClassOK, Known: true}},
		{"undetected minor, healthy", "", cnpgv1.PhaseHealthy, PhaseReading{Class: ClassOK, Known: true}},
		// A kept string can change its meaning in a minor the table was
		// not verified at: healthy is capped at warning there, with the
		// reason; non-ok rows keep their class.
		{"planned 1.31 healthy is capped", Minor131, cnpgv1.PhaseHealthy, PhaseReading{Class: ClassWarning, Known: true, Reason: "phase table not verified for CNPG 1.31"}},
		{"newer 1.32 healthy is capped", "1.32", cnpgv1.PhaseHealthy, PhaseReading{Class: ClassWarning, Known: true, Reason: "phase table not verified for CNPG 1.32"}},
		{"2.0 healthy is capped", "2.0", cnpgv1.PhaseHealthy, PhaseReading{Class: ClassWarning, Known: true, Reason: "phase table not verified for CNPG 2.0"}},
		{"1.29 healthy is capped", "1.29", cnpgv1.PhaseHealthy, PhaseReading{Class: ClassWarning, Known: true, Reason: "phase table not verified for CNPG 1.29"}},
		{"newer 1.32 failover stays critical", "1.32", cnpgv1.PhaseFailOver, PhaseReading{Class: ClassCritical, Known: true}},
		{"newer 1.32 switchover stays progressing", "1.32", cnpgv1.PhaseSwitchover, PhaseReading{Class: ClassProgressing, Known: true}},
		{"newer 1.32 unknown phase stays warning", "1.32", "Cluster is dancing", PhaseReading{Class: ClassWarning, Known: false}},
		{"verified 1.30 healthy is ok", Minor130, cnpgv1.PhaseHealthy, PhaseReading{Class: ClassOK, Known: true}},
		{"patch-level string is not a minor", "1.32.0", cnpgv1.PhaseHealthy, PhaseReading{Class: ClassOK, Known: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyPhase(tt.minor, tt.phase); got != tt.want {
				t.Errorf("ClassifyPhase(%q, %q) = %+v, want %+v", tt.minor, tt.phase, got, tt.want)
			}
		})
	}
}

// TestPhaseTableMatchesAPIModule pins the 1.30 table to the phase
// constants of github.com/cloudnative-pg/api v1.30.0 (go.mod), at
// compile time and by value: every constant is in the table, and the
// table has nothing else.
func TestPhaseTableMatchesAPIModule(t *testing.T) {
	constants := []string{
		cnpgv1.PhaseSwitchover, cnpgv1.PhaseFailOver, cnpgv1.PhaseFirstPrimary,
		cnpgv1.PhaseCreatingReplica, cnpgv1.PhaseUpgrade, cnpgv1.PhaseMajorUpgrade,
		cnpgv1.PhaseUpgradeDelayed, cnpgv1.PhaseWaitingForUser, cnpgv1.PhaseInplacePrimaryRestart,
		cnpgv1.PhaseInplaceDeletePrimaryRestart, cnpgv1.PhaseHealthy, cnpgv1.PhaseUnknownPlugin,
		cnpgv1.PhaseFailurePlugin, cnpgv1.PhaseImageCatalogError, cnpgv1.PhaseUnrecoverable,
		cnpgv1.PhaseArchitectureBinaryMissing, cnpgv1.PhaseWaitingForInstancesToBeActive,
		cnpgv1.PhaseOnlineUpgrading, cnpgv1.PhaseApplyingConfiguration,
		cnpgv1.PhaseReplicaClusterPromotion, cnpgv1.PhaseCannotCreateClusterObjects,
		cnpgv1.PhaseDefinitionInvalid,
	}
	rows := PhaseTable(Minor130)
	table := make([]string, 0, len(rows))
	for _, row := range rows {
		table = append(table, row.Phase)
	}
	slices.Sort(constants)
	slices.Sort(table)
	if !slices.Equal(constants, table) {
		t.Errorf("1.30 phase table and api v1.30.0 constants differ:\ntable: %q\napi:   %q", table, constants)
	}
	if PhaseTable("1.29") != nil {
		t.Error("1.29 is below the support floor but has a phase table")
	}
	if !slices.Equal(PhaseTable(Minor131), PhaseTable(Minor130)) {
		t.Error("planned 1.31 phase table differs from 1.30, but main (21e24f6) has the same constants")
	}
	if PhaseTable("1.28") != nil {
		t.Error("an unsupported minor has a phase table")
	}
	// kubectl cnpg status colours (internal/cmd/plugin/status/status.go
	// v1.30.0:478-484) and the class floor they imply.
	for _, row := range PhaseTable(Minor130) {
		switch row.Phase {
		case cnpgv1.PhaseHealthy, cnpgv1.PhaseFirstPrimary, cnpgv1.PhaseCreatingReplica:
			if row.Kubectl != kubectlGreen {
				t.Errorf("%q: kubectl colour %q, want green", row.Phase, row.Kubectl)
			}
		case cnpgv1.PhaseUpgrade, cnpgv1.PhaseWaitingForUser:
			if row.Kubectl != kubectlYellow {
				t.Errorf("%q: kubectl colour %q, want yellow", row.Phase, row.Kubectl)
			}
		default:
			if row.Kubectl != kubectlRed {
				t.Errorf("%q: kubectl colour %q, want red", row.Phase, row.Kubectl)
			}
			if row.Class == ClassOK {
				t.Errorf("%q: red in kubectl but ok here", row.Phase)
			}
		}
		if !row.Class.Valid() || row.Source == "" {
			t.Errorf("%q: class %q, source %q", row.Phase, row.Class, row.Source)
		}
	}
}

func TestClassifyCondition(t *testing.T) {
	tests := []struct {
		name                string
		minor               Minor
		fork                bool
		typ, status, reason string
		want                ConditionReading
	}{
		{"ready true", Minor130, false, "Ready", "True", "ClusterIsReady", ConditionReading{ClassOK, true}},
		{"ready false", Minor130, false, "Ready", "False", "ClusterIsNotReady", ConditionReading{ClassWarning, true}},
		{"ready unknown", Minor130, false, "Ready", "Unknown", "", ConditionReading{ClassUnknown, true}},
		{"garbage status", Minor130, false, "Ready", "yes", "", ConditionReading{ClassUnknown, false}},
		{"empty status", Minor130, false, "Ready", "", "", ConditionReading{ClassUnknown, false}},
		{"archiving failing", Minor130, false, "ContinuousArchiving", "False", "ContinuousArchivingFailing", ConditionReading{ClassCritical, true}},
		{"backup started", Minor130, false, "LastBackupSucceeded", "False", "BackupStarted", ConditionReading{ClassProgressing, true}},
		{"backup failed", Minor130, false, "LastBackupSucceeded", "False", "LastBackupFailed", ConditionReading{ClassWarning, true}},
		{"system id mismatch", Minor130, false, "ConsistentSystemID", "False", "Mismatch", ConditionReading{ClassCritical, true}},
		{"system id not found", Minor130, false, "ConsistentSystemID", "False", "NotFound", ConditionReading{ClassWarning, true}},
		{"initialized pending", Minor130, false, "Initialized", "False", "BootstrapPending", ConditionReading{ClassProgressing, true}},
		{"1.29 has no table and reads 1.30", "1.29", false, "Initialized", "True", "BootstrapCompleted", ConditionReading{ClassOK, true}},
		{"topology under 1.30 is foreign", Minor130, false, "SyncReplicationTopologySatisfied", "False", "InsufficientCrossDomainReplicas", ConditionReading{ClassInfo, false}},
		{"topology under 1.31", Minor131, false, "SyncReplicationTopologySatisfied", "False", "InsufficientCrossDomainReplicas", ConditionReading{ClassWarning, true}},
		{"hibernated", Minor130, false, "cnpg.io/hibernation", "True", "Hibernated", ConditionReading{ClassInactive, true}},
		{"hibernating", Minor130, false, "cnpg.io/hibernation", "False", "DeletingPods", ConditionReading{ClassProgressing, true}},
		{"hibernation waits", Minor130, false, "cnpg.io/hibernation", "False", "WaitingForHealthy", ConditionReading{ClassWarning, true}},
		{"hibernation wrong value", Minor130, false, "cnpg.io/hibernation", "False", "WrongAnnotationValue", ConditionReading{ClassWarning, true}},
		{"unknown type", Minor130, false, "SomethingNew", "True", "", ConditionReading{ClassInfo, false}},
		{"unknown type, unknown status", Minor130, false, "SomethingNew", "Unknown", "", ConditionReading{ClassInfo, false}},
		{"fork type without pin", Minor130, false, "VolumeExpansionRefused", "True", "", ConditionReading{ClassWarning, false}},
		{"fork type pinned", Minor130, true, "VolumeExpansionRefused", "True", "", ConditionReading{ClassWarning, true}},
		{"fork type cleared, pinned", Minor131, true, "TopologyUnavailable", "False", "", ConditionReading{ClassOK, true}},
		{"fork type unknown status", Minor130, false, "VolumeAutoGrowStopped", "Unknown", "", ConditionReading{ClassUnknown, false}},
		{"unknown minor uses 1.30", "1.32", false, "Initialized", "True", "", ConditionReading{ClassOK, true}},
		{"unknown minor, 1.31 type", "", false, "SyncReplicationTopologySatisfied", "True", "", ConditionReading{ClassInfo, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyConditionPinned(tt.minor, tt.fork, tt.typ, tt.status, tt.reason)
			if got != tt.want {
				t.Errorf("ClassifyConditionPinned(%q, %v, %q, %q, %q) = %+v, want %+v", tt.minor, tt.fork, tt.typ, tt.status, tt.reason, got, tt.want)
			}
			if !tt.fork {
				if plain := ClassifyCondition(tt.minor, tt.typ, tt.status, tt.reason); plain != got {
					t.Errorf("ClassifyCondition disagrees with the unpinned form: %+v vs %+v", plain, got)
				}
			}
		})
	}
}

// TestConditionTableShape: every type has a wildcard row for True and
// for False, every row a valid class and a source, and the upstream
// types match the api module's constants.
func TestConditionTableShape(t *testing.T) {
	wild := map[string]map[string]bool{}
	for _, rule := range conditionRules {
		if !rule.Class.Valid() || rule.Source == "" {
			t.Errorf("rule %+v: invalid class or no source", rule)
		}
		if (rule.Fork == "") == (rule.Since == "") {
			t.Errorf("rule %+v: exactly one of Since and Fork must be set", rule)
		}
		if rule.Reason == anyReason {
			if wild[rule.Type] == nil {
				wild[rule.Type] = map[string]bool{}
			}
			wild[rule.Type][rule.Status] = true
		}
	}
	for typ, statuses := range wild {
		if !statuses["True"] || !statuses["False"] {
			t.Errorf("type %q lacks a wildcard row for True or False: %v", typ, statuses)
		}
	}
	for _, typ := range []cnpgv1.ClusterConditionType{
		cnpgv1.ConditionClusterReady, cnpgv1.ConditionContinuousArchiving, cnpgv1.ConditionBackup,
		cnpgv1.ConditionConsistentSystemID, cnpgv1.ConditionInitialized,
	} {
		found := false
		for _, rule := range ConditionTable(Minor130, false) {
			found = found || rule.Type == string(typ)
		}
		if !found {
			t.Errorf("api v1.30.0 condition %q missing from the 1.30 table", typ)
		}
	}
	reasons := map[string]string{
		"BackupStarted": string(cnpgv1.ConditionBackupStarted),
	}
	for ours, theirs := range reasons {
		if ours != theirs {
			t.Errorf("reason %q is %q in api v1.30.0", ours, theirs)
		}
	}
	if ConditionTable("1.28", true) != nil {
		t.Error("an unsupported minor has a condition table")
	}
	for _, rule := range ConditionTable(Minor130, false) {
		if rule.Fork != "" {
			t.Errorf("fork rule %q in the unpinned table", rule.Type)
		}
	}
}

// TestNamedMinorCapsAnUnsupportedVersion: the console classifies phases
// with the minor the detected version names, so a newer CloudNativePG
// without tables is capped instead of read as an undetected version.
func TestNamedMinorCapsAnUnsupportedVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    Minor
		class   Class
	}{
		{"1.30.1", "1.30", ClassOK},
		{"v1.32.0", "1.32", ClassWarning},
		{">=1.30", "1.30", ClassOK},
		{"", "", ClassOK},
		{"not-a-version", "", ClassOK},
	} {
		got := NamedMinor(tc.version)
		if got != tc.want {
			t.Fatalf("NamedMinor(%q) = %q, want %q", tc.version, got, tc.want)
		}
		if c := ClassifyPhase(got, cnpgv1.PhaseHealthy).Class; c != tc.class {
			t.Fatalf("ClassifyPhase(NamedMinor(%q), healthy) = %v, want %v", tc.version, c, tc.class)
		}
	}
}
