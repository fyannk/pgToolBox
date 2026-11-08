//go:build catalogpins

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
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestFactPinsExistInVerifiedReleases is the pin test of the status facts
// read from Backup, storage, declared-object, replication and operation
// state (backupobjects.go, storage.go, declared.go, replication.go,
// operations.go): every literal they rely on must be in the operator
// source of each verified release. Run with -tags catalogpins.
func TestFactPinsExistInVerifiedReleases(t *testing.T) {
	for _, release := range verifiedReleases {
		corpus := operatorSources(t, release)
		literals := []string{
			// backupobjects.go
			`BackupPhasePending = "pending"`, `BackupPhaseStarted = "started"`, `BackupPhaseRunning = "running"`,
			`BackupPhaseFinalizing = "finalizing"`, `BackupPhaseCompleted = "completed"`, `BackupPhaseFailed = "failed"`,
			`BackupPhaseWalArchivingFailing = "walArchivingFailing"`,
			`BackupMethodPlugin BackupMethod = "plugin"`, `BackupMethodVolumeSnapshot BackupMethod = "volumeSnapshot"`,
			`BackupMethodBarmanObjectStore BackupMethod = "barmanObjectStore"`,
			`plugin backups, which never transition to the "running" phase`,
			"return backupStatus.Phase == BackupPhaseCompleted || backupStatus.Phase == BackupPhaseFailed",
			`json:"lastCheckTime,omitempty"`, `json:"lastScheduleTime,omitempty"`, `json:"nextScheduleTime,omitempty"`,
			`"github.com/robfig/cron"`, "len(strings.Fields(r.Spec.Schedule)) != 6",
			// storage.go
			`PvcRoleLabelName = MetadataNamespace + "/pvcRole"`, `InstanceNameLabelName = MetadataNamespace + "/instanceName"`,
			`TablespaceNameLabelName = "cnpg.io/tablespaceName"`, `PodRoleLabelName = MetadataNamespace + "/podRole"`,
			`PVCRolePgData PVCRole = "PG_DATA"`, `PVCRolePgWal PVCRole = "PG_WAL"`, `PVCRolePgTablespace PVCRole = "PG_TABLESPACE"`,
			`PodRoleInstance PodRole = "instance"`, `PodRolePooler PodRole = "pooler"`,
			`json:"healthyPVC,omitempty"`, `json:"initializingPVC,omitempty"`, `json:"resizingPVC,omitempty"`,
			`json:"danglingPVC,omitempty"`, `json:"unusablePVC,omitempty"`,
			"Podless resizing PVCs are classified as dangling instead",
			// declared.go
			`errClusterIsReplica = fmt.Errorf("waiting for the cluster to become primary")`,
			`RoleStatusReconciled RoleStatus = "reconciled"`, `RoleStatusNotManaged RoleStatus = "not-managed"`,
			`RoleStatusPendingReconciliation RoleStatus = "pending-reconciliation"`, `RoleStatusReserved RoleStatus = "reserved"`,
			`json:"cannotReconcile,omitempty"`, `json:"passwordStatus,omitempty"`,
			`TablespaceStatusReconciled TablespaceStatus = "reconciled"`, `TablespaceStatusPendingReconciliation TablespaceStatus = "pending"`,
			`PoolerTypeRW = PoolerType("rw")`, `PgBouncerPoolModeSession = PgBouncerPoolMode("session")`,
			"updatedStatus.Instances = resources.Deployment.Status.Replicas",
			// replication.go
			"WHERE application_name ~ $1 AND usename = $2", "FROM pg_catalog.pg_replication_slots",
			"WHERE application_name ~ '-join$'", `SentLsn         types.LSN ` + "`" + `json:"receivedLsn,omitempty"`,
			`json:"walStatus,omitempty"`, `Method:       "ANY"`,
			`DataDurabilityLevelRequired DataDurabilityLevel = "required"`, `DataDurabilityLevelPreferred DataDurabilityLevel = "preferred"`,
			`SynchronousReplicaConfigurationMethodFirst = SynchronousReplicaConfigurationMethod("first")`,
			`json:"failoverQuorum"`, `json:"standbyNumber,omitempty"`,
			// operations.go
			`ClusterRestartAnnotationName = "kubectl.kubernetes.io/restartedAt"`,
			`ClusterReloadAnnotationName = MetadataNamespace + "/reloadedAt"`,
			`"Primary instance restarted in-place"`, "cannot restart the primary in-place when a switchover is in progress",
			`cluster.Status.Phase = apiv1.PhaseSwitchover`, "func checkClusterHasDifferentRestartAnnotation",
			`"ClusterIsHibernated"`, `"ClusterHasNoBackupExecutorPlugin"`, `"ClusterHasBackupConfigured"`,
			`"ClusterHasNoVolumeSnapshotSection"`, `"ClusterHasNoBarmanSection"`,
			// The pooler phases, the backup definition phase and
			// DatabaseRole arrived in 1.30, the support floor.
			`BackupPhaseDefinitionInvalid = "invalid backup definition"`,
			`PoolerPhaseActive PoolerPhase = "active"`, `PoolerPhasePaused PoolerPhase = "paused"`,
			`PoolerPhaseInactive PoolerPhase = "inactive"`, `PoolerPhaseFailed PoolerPhase = "failed"`,
			"type DatabaseRoleStatus struct",
		}
		for _, literal := range literals {
			if !bytes.Contains(corpus, []byte(literal)) {
				t.Errorf("v%s: %q is not in the operator source", release, literal)
			}
		}
		// The reload annotation must stay write-only: if the operator
		// starts reading it, reload gains an observable completion.
		if n := bytes.Count(corpus, []byte("ClusterReloadAnnotationName")); n > 6 {
			t.Errorf("v%s: ClusterReloadAnnotationName appears %d times; reload may now be observable", release, n)
		}
	}
}

// TestEveryUpstreamBackupPhaseIsInTheTable is the inverse for Backup
// phases.
func TestEveryUpstreamBackupPhaseIsInTheTable(t *testing.T) {
	constant := regexp.MustCompile(`(?m)^\s*BackupPhase\w+\s*=\s*"([^"]+)"`)
	for minor, release := range verifiedReleases {
		types, err := os.ReadFile(filepath.Join(moduleDir(t, release), "api", "v1", "backup_types.go"))
		if err != nil {
			t.Fatal(err)
		}
		known := map[string]bool{}
		for _, row := range BackupPhaseTable(minor) {
			known[row.Phase] = true
		}
		for _, m := range constant.FindAllSubmatch(types, -1) {
			if !known[string(m[1])] {
				t.Errorf("v%s Backup phase %q has no row in the %s table", release, m[1], minor)
			}
		}
	}
}
