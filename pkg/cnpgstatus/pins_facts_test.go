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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestFactPinsExistInVerifiedReleases is the pin test of the status facts
// read from Backup, storage, declared-object, replication and operation
// state (backupobjects.go, storage.go, declared.go, replication.go,
// operations.go): every literal they rely on must be in the operator
// source of each verified release. Run with -tags catalogpins.
func TestFactPinsExistInVerifiedReleases(t *testing.T) {
	for _, v := range verifiedReleases {
		release := v.tag
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
			// backlog.go WALArchiver: the archiver is the first enabled
			// isWALArchiver plugin, else the in-tree object store, and
			// every enabled plugin is still asked first.
			"if plugin.IsEnabled() && plugin.IsWALArchiver != nil && *plugin.IsWALArchiver {",
			"if err := archiveWALViaPlugins(ctx, cluster, pgData, walName); err != nil {",
			`if cluster.GetEnabledWALArchivePluginName() != "" {`,
			"if cluster.Spec.Backup == nil || cluster.Spec.Backup.BarmanObjectStore == nil {",
			// backups.go ArchiverLive.ReadyNotReported: readyWalFiles is
			// filled only on a primary (a designated primary is not one),
			// and the recovery facts' WAL positions and segment size come
			// from pg_current_wal_lsn() and its file name.
			"func (instance *Instance) fillWalStatusFromConnection(result *postgres.PostgresqlStatus, superUserDB *sql.DB) error {\n\tif !result.IsPrimary {\n\t\treturn nil\n\t}",
			"result.ReadyWALFiles, _, err = GetWALArchiveCounters()",
			"pg_catalog.pg_walfile_name(pg_catalog.pg_current_wal_lsn()) as current_wal,",
			// backups.go IsReplicaCluster follows Cluster.IsReplica, and
			// the archiver archives on a replica cluster only from the
			// designated primary (views.go liveStatus).
			"func (cluster Cluster) IsReplica() bool {", "return clusterName != r.Primary",
			"if podName != cluster.Status.CurrentPrimary && podName != cluster.Status.TargetPrimary {",
			`configuration.OverwriteConfig("archive_mode", "always")`,
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
	for _, v := range verifiedReleases {
		minor, release := v.minor, v.tag
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

// TestPinContinuousArchivingSince pins what the recovery facts read
// from ContinuousArchiving's lastTransitionTime (internal/console/status
// recoveryFacts failingSince, round 14, CNPG P3-b; pinned round 15, CNPG
// P3-3): the instance builds the condition with no LastTransitionTime
// (pkg/management/postgres/webserver/local.go
// getContinuousArchivingCondition, v1.30.0:247-263, v1.30.1:264-280),
// and the operator merges it with meta.SetStatusCondition
// (pkg/resources/status/conditions.go PatchConditionsWithOptimisticLock),
// which sets the time only when the status changes: "False since T" is
// the start of the current failing streak.
func TestPinContinuousArchivingSince(t *testing.T) {
	for _, v := range verifiedReleases {
		release := v.tag
		dir := moduleDir(t, release)
		local, err := os.ReadFile(filepath.Join(dir, "pkg", "management", "postgres", "webserver", "local.go"))
		if err != nil {
			t.Fatal(err)
		}
		build := functionBody(t, local, "func (asr *ArchiveStatusRequest) getContinuousArchivingCondition() metav1.Condition {")
		if !bytes.Contains(build, []byte("Type:    string(apiv1.ConditionContinuousArchiving),")) || !bytes.Contains(build, []byte("Status:  metav1.ConditionFalse,")) {
			t.Errorf("v%s: getContinuousArchivingCondition no longer builds ContinuousArchiving False", release)
		}
		if bytes.Contains(build, []byte("LastTransitionTime")) {
			t.Errorf("v%s: getContinuousArchivingCondition now sets LastTransitionTime: failingSince may no longer be the streak's start", release)
		}
		conditions, err := os.ReadFile(filepath.Join(dir, "pkg", "resources", "status", "conditions.go"))
		if err != nil {
			t.Fatal(err)
		}
		patch := functionBody(t, conditions, "func PatchConditionsWithOptimisticLock(")
		if !bytes.Contains(patch, []byte("meta.SetStatusCondition(&cluster.Status.Conditions, c)")) {
			t.Errorf("v%s: PatchConditionsWithOptimisticLock no longer merges with meta.SetStatusCondition", release)
		}
	}
}

// TestPinArchiverIdentityAndTimeline pins two facts the recovery facts
// read (round 9): the instance manager archives only on
// status.currentPrimary (a switchover's target primary or a pod that
// still reports itself primary is refused: internal/console/status
// statusRead), and /pg/status names the primary's current WAL file with
// pg_walfile_name(pg_current_wal_lsn()), the timeline the sweep stamps
// (sweep SeriesTimeline), while its timelineId comes from
// pg_control_checkpoint(), which lags a promotion.
func TestPinArchiverIdentityAndTimeline(t *testing.T) {
	for _, v := range verifiedReleases {
		release := v.tag
		archiver, err := os.ReadFile(filepath.Join(moduleDir(t, release), "pkg", "management", "postgres", "archiver", "archiver.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, literal := range []string{"if cluster.Status.CurrentPrimary != podName {", "return errSwitchoverInProgress"} {
			if !bytes.Contains(archiver, []byte(literal)) {
				t.Errorf("v%s: archiver.go no longer has %q", release, literal)
			}
		}
		probes, err := os.ReadFile(filepath.Join(moduleDir(t, release), "pkg", "management", "postgres", "probes.go"))
		if err != nil {
			t.Fatal(err)
		}
		body := functionBody(t, probes, "func (instance *Instance) fillStatusFromPrimary(result *postgres.PostgresqlStatus) error {")
		for _, literal := range []string{"pg_catalog.pg_walfile_name(pg_catalog.pg_current_wal_lsn()) as current_wal", "FROM pg_catalog.pg_control_checkpoint()) as timeline_id"} {
			if !bytes.Contains(body, []byte(literal)) {
				t.Errorf("v%s: fillStatusFromPrimary no longer reads %q", release, literal)
			}
		}
		// instancestatus neverArchived (round 11, CNPG P3): a NULL
		// last_archived_time reaches /pg/status as "-infinity".
		archiverStatus := functionBody(t, probes, "func fillArchiverStatus(superUserDB *sql.DB, result *postgres.PostgresqlStatus) error {")
		if !bytes.Contains(archiverStatus, []byte("COALESCE(last_archived_time,'-infinity'),")) {
			t.Errorf("v%s: fillArchiverStatus no longer coalesces last_archived_time to '-infinity'", release)
		}
	}
}

// TestPinFormerPrimaryArchivesBeforeRejoining pins the fact the console's
// FormerPrimaryRejoined advice rests on (internal/console/status
// rejoinedSince, round 11, CNPG P3): a former primary that starts again
// archives every .ready WAL file before pg_rewind and its demotion, and a
// failure stops the start, so a Ready standby read after the promotion
// no longer holds WAL waiting to be archived.
func TestPinFormerPrimaryArchivesBeforeRejoining(t *testing.T) {
	for _, v := range verifiedReleases {
		release := v.tag
		startup, err := os.ReadFile(filepath.Join(moduleDir(t, release), "internal", "management", "controller", "instance_startup.go"))
		if err != nil {
			t.Fatal(err)
		}
		archive := bytes.Index(startup, []byte("archiver.ArchiveAllReadyWALs(ctx, cluster, r.instance.PgData)"))
		failure := bytes.Index(startup, []byte("while ensuring all WAL files are archived"))
		rewind := bytes.Index(startup, []byte("r.instance.Rewind(ctx)"))
		switch {
		case archive < 0:
			t.Errorf("v%s: instance_startup.go no longer archives the ready WAL files on start", release)
		case failure < archive:
			t.Errorf("v%s: instance_startup.go no longer fails the start when archiving the ready WAL files fails", release)
		case rewind < failure:
			t.Errorf("v%s: instance_startup.go no longer archives the ready WAL files before pg_rewind", release)
		}
	}
}

// TestPinMaskedStatusIsA200 pins the fact instancestatus Reading.Masked
// rests on (round 16, CNPG P2-1): while fencing or a restart sets
// MightBeUnavailable (SetFencing, RequestAndWaitRestartSmartFast), GetStatus
// defers a mask that saves the error as MightBeUnavailableMaskedError and
// re-reads only isPrimary (pkg/management/postgres/probes.go
// v1.30.1:47-62), so pgStatus (webserver/remote.go) answers 200 with every
// other PostgreSQL field at zero.
func TestPinMaskedStatusIsA200(t *testing.T) {
	for _, v := range verifiedReleases {
		release := v.tag
		dir := moduleDir(t, release)
		probes, err := os.ReadFile(filepath.Join(dir, "pkg", "management", "postgres", "probes.go"))
		if err != nil {
			t.Fatal(err)
		}
		get := functionBody(t, probes, "func (instance *Instance) GetStatus() (result *postgres.PostgresqlStatus, err error) {")
		mask := bytes.Index(get, []byte("result.MightBeUnavailableMaskedError = err.Error()"))
		primary := bytes.Index(get, []byte("result.IsPrimary, err = instance.IsPrimary()"))
		if !bytes.Contains(get, []byte("defer func() {")) || mask < 0 || primary < mask {
			t.Errorf("v%s: GetStatus no longer masks the error and re-reads only isPrimary", release)
		}
		remote, err := os.ReadFile(filepath.Join(dir, "pkg", "management", "postgres", "webserver", "remote.go"))
		if err != nil {
			t.Fatal(err)
		}
		status := functionBody(t, remote, "func (ws *remoteWebserverEndpoints) pgStatus(w http.ResponseWriter, _ *http.Request) {")
		for _, literal := range []string{"status, err := ws.instance.GetStatus()", "js, err := json.Marshal(status)", "_, _ = w.Write(js)"} {
			if !bytes.Contains(status, []byte(literal)) {
				t.Errorf("v%s: pgStatus no longer has %q", release, literal)
			}
		}
		instance, err := os.ReadFile(filepath.Join(dir, "pkg", "management", "postgres", "instance.go"))
		if err != nil {
			t.Fatal(err)
		}
		for signature, literal := range map[string]string{
			"func (instance *Instance) SetFencing(enabled bool) {":                                                         "instance.SetMightBeUnavailable(true)",
			"func (instance *Instance) RequestAndWaitRestartSmartFast(ctx context.Context, timeout time.Duration) error {": "instance.SetMightBeUnavailable(true)",
		} {
			if !bytes.Contains(functionBody(t, instance, signature), []byte(literal)) {
				t.Errorf("v%s: %s no longer sets MightBeUnavailable", release, signature)
			}
		}
	}
}

// functionBody is the text from a function's signature line to the next
// top-level closing brace.
func functionBody(t *testing.T, source []byte, signature string) []byte {
	t.Helper()
	start := bytes.Index(source, []byte(signature))
	if start < 0 {
		t.Fatalf("%q is not in the source", signature)
	}
	rest := source[start:]
	end := bytes.Index(rest, []byte("\n}\n"))
	if end < 0 {
		t.Fatalf("%q has no end", signature)
	}
	return rest[:end]
}

// TestPinPluginBackupTimesFromUnix pins the fact observe.SaneBackupTimes
// rests on (round 18, CNPG P3): CloudNativePG converts a plugin's start
// and stop seconds with time.Unix (internal/cnpi/plugin/client/backup.go)
// and keeps the result unless IsZero (webserver/plugin_backup.go), which
// a time.Unix(0, 0) never is: a plugin that leaves one unset stamps the
// Backup 1970-01-01.
func TestPinPluginBackupTimesFromUnix(t *testing.T) {
	for _, v := range verifiedReleases {
		release := v.tag
		dir := moduleDir(t, release)
		client, err := os.ReadFile(filepath.Join(dir, "internal", "cnpi", "plugin", "client", "backup.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, literal := range []string{"time.Unix(result.StartedAt, 0)", "time.Unix(result.StoppedAt, 0)"} {
			if !bytes.Contains(client, []byte(literal)) {
				t.Errorf("v%s: backup.go no longer has %q", release, literal)
			}
		}
		webserver, err := os.ReadFile(filepath.Join(dir, "pkg", "management", "postgres", "webserver", "plugin_backup.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, literal := range []string{"if !response.StartedAt.IsZero() {", "if !response.StoppedAt.IsZero() {"} {
			if !bytes.Contains(webserver, []byte(literal)) {
				t.Errorf("v%s: plugin_backup.go no longer guards with %q", release, literal)
			}
		}
	}
}

// TestPinBackupStampsAfterTheTool pins the facts observe.SaneBackupTimes
// and the purge band's expire timing rest on (round 20, CNPG P3-4):
// SetAsCompleted and SetAsFailed stamp status.reconciliationTerminatedAt
// and SetAsStarted status.reconciliationStartedAt with the writer's
// clock; both instance sites (in-tree barman and plugin) call
// SetAsCompleted only after the tool returned, a failure returning
// first, so the stamp is an upper bound on the end and comes after
// pgBackRest's expire; a volume snapshot's stoppedAt is that stamp.
func TestPinBackupStampsAfterTheTool(t *testing.T) {
	read := func(t *testing.T, dir string, path ...string) []byte {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(append([]string{dir}, path...)...))
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	// ordered says that in body each literal comes after the one before.
	ordered := func(t *testing.T, release, what string, body []byte, literals ...string) {
		t.Helper()
		at := 0
		for _, literal := range literals {
			i := bytes.Index(body[at:], []byte(literal))
			if i < 0 {
				t.Errorf("v%s: %s no longer has %q after %q", release, what, literal, body[max(0, at-40):at])
				return
			}
			at += i + len(literal)
		}
	}
	for _, v := range verifiedReleases {
		release := v.tag
		dir := moduleDir(t, release)
		funcs := read(t, dir, "api", "v1", "backup_funcs.go")
		for _, c := range []struct{ signature, stamp string }{
			{"func (backupStatus *BackupStatus) SetAsCompleted() {", "backupStatus.ReconciliationTerminatedAt = ptr.To(metav1.Now())"},
			{"func (backupStatus *BackupStatus) SetAsFailed(", "backupStatus.ReconciliationTerminatedAt = ptr.To(metav1.Now())"},
			{"func (backupStatus *BackupStatus) SetAsStarted(podName, containerID, sessionID string, method BackupMethod) {", "backupStatus.ReconciliationStartedAt = ptr.To(metav1.Now())"},
		} {
			ordered(t, release, c.signature, functionBody(t, funcs, c.signature), c.stamp)
		}
		barman := functionBody(t, read(t, dir, "pkg", "management", "postgres", "backup.go"), "func (b *BackupCommand) takeBackup(ctx context.Context) error {")
		ordered(t, release, "takeBackup", barman, "err := b.barmanBackup.Take(", "if err != nil {", "return err", "b.Backup.Status.SetAsCompleted()")
		plugin := functionBody(t, read(t, dir, "pkg", "management", "postgres", "webserver", "plugin_backup.go"), "func (b *PluginBackupCommand) invokeStart(ctx context.Context) {")
		ordered(t, release, "invokeStart", plugin, "response, err := cli.Backup(", "if err != nil {", "b.markBackupAsFailed(ctx, err)", "return", "b.Backup.Status.SetAsCompleted()")
		snapshot := read(t, dir, "pkg", "reconciler", "backup", "volumesnapshot", "reconciler.go")
		ordered(t, release, "volumesnapshot reconciler.go", snapshot, "backup.Status.SetAsCompleted()\n\tbackup.Status.StoppedAt = backup.Status.ReconciliationTerminatedAt.DeepCopy()")
		// reconciliationStartedAt is on the operator's clock: SetAsStarted
		// is called only by the Backup controller (backup_controller.go
		// v1.30.1:702 and 985; round 21, CNPG P3-3), so a start stamp kept
		// beside dropped instance times says the operator started that
		// Backup live (observe.SaneBackupTimes ClockBehind).
		controller := filepath.Join("internal", "controller", "backup_controller.go")
		callers := setAsStartedCallers(t, dir)
		if len(callers) == 0 {
			t.Errorf("v%s: nothing calls SetAsStarted any more", release)
		}
		for _, file := range callers {
			if file != controller {
				t.Errorf("v%s: SetAsStarted is now called from %s, not only by the operator's %s", release, file, controller)
			}
		}
		// A Completed or Failed Backup is not reconciled again, so the
		// operator never re-stamps one (backup_controller.go v1.30.1:155).
		reconcile := functionBody(t, read(t, dir, controller), "func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {")
		ordered(t, release, "BackupReconciler.Reconcile", reconcile, "switch backup.Status.Phase {", "case apiv1.BackupPhaseFailed, apiv1.BackupPhaseCompleted:", "return ctrl.Result{}, nil", "EnsureResourceIsAdmitted(")
		if skip := bytes.Index(reconcile, []byte("case apiv1.BackupPhaseFailed, apiv1.BackupPhaseCompleted:")); skip >= 0 {
			for _, write := range []string{"SetAs", "Status()", "Patch(", "Update("} {
				if bytes.Contains(reconcile[:skip], []byte(write)) {
					t.Errorf("v%s: BackupReconciler.Reconcile has %q before its terminal-phase skip", release, write)
				}
			}
		}
	}
}

// setAsStartedCallers are the non-test Go files of the module at dir,
// relative to it, that call SetAsStarted (its definition excluded).
func setAsStartedCallers(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte(".SetAsStarted(")) {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestPinReadyWALFilesCountedOnEveryPrimary pins the fact the console's
// LiveStatus.readyWalFiles rests on (round 18, DBA): CloudNativePG counts
// the .ready files on every primary it reads, failing the status
// otherwise, and omits a zero from the JSON, so a primary read through
// PostgreSQL without the field has none waiting.
func TestPinReadyWALFilesCountedOnEveryPrimary(t *testing.T) {
	for _, v := range verifiedReleases {
		release := v.tag
		dir := moduleDir(t, release)
		probes, err := os.ReadFile(filepath.Join(dir, "pkg", "management", "postgres", "probes.go"))
		if err != nil {
			t.Fatal(err)
		}
		fill := functionBody(t, probes, "func (instance *Instance) fillWalStatusFromConnection(result *postgres.PostgresqlStatus, superUserDB *sql.DB) error {")
		count := bytes.Index(fill, []byte("result.ReadyWALFiles, _, err = GetWALArchiveCounters()"))
		if count < 0 || !bytes.Contains(fill[count:], []byte("return err")) {
			t.Errorf("v%s: fillWalStatusFromConnection no longer counts the ready WAL files on the primary, failing on error", release)
		}
		status, err := os.ReadFile(filepath.Join(dir, "pkg", "postgres", "status.go"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(status, []byte("`json:\"readyWalFiles,omitempty\"`")) {
			t.Errorf("v%s: PostgresqlStatus.ReadyWALFiles is no longer omitempty", release)
		}
	}
}
