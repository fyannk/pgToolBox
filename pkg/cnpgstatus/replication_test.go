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
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
)

func TestClassifyReplicationValues(t *testing.T) {
	sender := map[string]Class{
		WalSenderStreaming: ClassOK, WalSenderCatchup: ClassProgressing, WalSenderStartup: ClassProgressing,
		WalSenderBackup: ClassProgressing, WalSenderStopping: ClassWarning, "": ClassUnknown, "Streaming": ClassUnknown,
	}
	for state, want := range sender {
		got := ClassifyWalSenderState(state)
		if got.Class != want || got.Known != (want != ClassUnknown) {
			t.Errorf("ClassifyWalSenderState(%q) = %+v, want %s", state, got, want)
		}
	}
	sync := map[string]struct {
		class Class
		sync  bool
	}{
		SyncStateSync: {ClassOK, true}, SyncStateQuorum: {ClassOK, true},
		SyncStatePotential: {ClassInfo, false}, SyncStateAsync: {ClassInfo, false}, "": {ClassUnknown, false},
	}
	for state, want := range sync {
		got := ClassifySyncState(state)
		if got.Class != want.class || got.Synchronous != want.sync {
			t.Errorf("ClassifySyncState(%q) = %+v", state, got)
		}
	}
	slots := map[string]Class{
		SlotWALReserved: ClassOK, SlotWALExtended: ClassWarning, SlotWALUnreserved: ClassCritical,
		SlotWALLost: ClassCritical, "": ClassUnknown, "gone": ClassUnknown,
	}
	for status, want := range slots {
		if got := ClassifySlotWALStatus(status); got.Class != want {
			t.Errorf("ClassifySlotWALStatus(%q) = %+v, want %s", status, got, want)
		}
	}
}

func TestSyncConfig(t *testing.T) {
	if got := SyncConfig(Minor130, nil); got.Mode != SyncModeNone {
		t.Errorf("nil: %+v", got)
	}
	legacy := &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{MinSyncReplicas: 1, MaxSyncReplicas: 2}}
	if got := SyncConfig(Minor130, legacy); got.Mode != SyncModeLegacy || got.Method != "any" || got.MinSync != 1 || got.MaxSync != 2 {
		t.Errorf("legacy: %+v", got)
	}
	if got := SyncConfig(Minor130, &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{MinSyncReplicas: 1}}); got.Mode != SyncModeNone {
		t.Errorf("min without max is not synchronous: %+v", got)
	}
	explicit := &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{
		MaxSyncReplicas: 2, // ignored: synchronous wins
		PostgresConfiguration: cnpgv1.PostgresConfiguration{Synchronous: &cnpgv1.SynchronousReplicaConfiguration{
			Method: cnpgv1.SynchronousReplicaConfigurationMethodFirst, Number: 1,
			MaxStandbyNamesFromCluster: new(2), StandbyNamesPre: []string{"ext-1"}, FailoverQuorum: true,
		}},
	}}
	got := SyncConfig(Minor130, explicit)
	if got.Mode != SyncModeExplicit || got.Method != "first" || got.Number != 1 || got.DataDurability != "required" ||
		!got.FailoverQuorum || got.FailureDomainKeysUnreadable || len(got.StandbyNamesPre) != 1 {
		t.Errorf("explicit: %+v", got)
	}
	if got := SyncConfig(Minor131, explicit); !got.FailureDomainKeysUnreadable {
		t.Error("1.31 must flag the failure-domain keys the typed API cannot read")
	}
	explicit.Spec.PostgresConfiguration.Synchronous.DataDurability = cnpgv1.DataDurabilityLevelPreferred
	if got := SyncConfig(Minor130, explicit); got.DataDurability != "preferred" {
		t.Errorf("preferred: %+v", got)
	}
}

func TestFailoverQuorumState(t *testing.T) {
	if got := FailoverQuorumState(nil, "pg-1"); got.Class != ClassUnknown {
		t.Errorf("nil: %+v", got)
	}
	if got := FailoverQuorumState(&cnpgv1.FailoverQuorum{}, "pg-1"); got.Class != ClassWarning {
		t.Errorf("empty status must warn (failover blocked): %+v", got)
	}
	fq := &cnpgv1.FailoverQuorum{Status: cnpgv1.FailoverQuorumStatus{Method: "ANY", StandbyNumber: 1, StandbyNames: []string{"pg-2", "pg-3"}, Primary: "pg-1"}}
	if got := FailoverQuorumState(fq, "pg-1"); got.Class != ClassOK || got.Number != 1 || len(got.StandbyNames) != 2 {
		t.Errorf("current: %+v", got)
	}
	if got := FailoverQuorumState(fq, "pg-2"); got.Class != ClassWarning {
		t.Errorf("stale writer: %+v", got)
	}
}
