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
	"strconv"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// BarmanCloudPlugin is the Barman Cloud CNPG-I plugin's name, as
// internal/console/kube/convert.go recognises it.
const BarmanCloudPlugin = "barman-cloud.cloudnative-pg.io"

// The two pgBackRest CNPG-I plugins and the Cluster
// parameters that name their objects: Dalibo's stanzaRef names a Stanza
// (cnpg-i-pgbackrest v0.0.3 internal/config), Opera's pgbackrestObjectName
// names an Archive and its optional stanza the backup stanza, the
// Cluster's name when absent (cnpg-plugin-pgbackrest v0.9.0
// internal/cnpgi/operator/config NewFromCluster). Neither needs
// isWALArchiver: CloudNativePG sends WAL to every enabled plugin that
// can archive it.
const (
	PgBackRestDaliboPlugin  = "pgbackrest.dalibo.com"
	PgBackRestOperaPlugin   = "pgbackrest.cnpg.opera.com"
	PgBackRestDaliboObject  = "stanzaRef"
	PgBackRestOperaObject   = "pgbackrestObjectName"
	PgBackRestOperaStanza   = "stanza"
	PgBackRestFlavorDalibo  = "dalibo"
	PgBackRestFlavorOpera   = "opera"
	PgBackRestKindStanza    = "Stanza"
	PgBackRestKindArchive   = "Archive"
	pgBackRestDaliboDisplay = "pgBackRest (Dalibo)"
	pgBackRestOperaDisplay  = "pgBackRest (Opera)"
)

// PgBackRestBackupKey is the key a Backup object's backup has in the
// repository evidence: the pgBackRest label for the pgBackRest plugins
// (Dalibo writes it to status.backupName, Opera to status.backupId), the
// Barman backup_id otherwise.
func PgBackRestBackupKey(pluginName, backupID, backupName string) string {
	if pluginName == PgBackRestDaliboPlugin {
		return backupName
	}
	return backupID
}

// PgBackRestConfig is a Cluster's pgBackRest plugin as its spec declares
// it: which plugin, the object it names, and (Opera) the backup stanza.
// Dalibo's stanza lives in the Stanza object, which the Cluster does not
// spell: Stanza is empty then, and the repository evidence names it.
type PgBackRestConfig struct {
	Plugin string `json:"plugin"`
	// Flavor is dalibo or opera.
	Flavor string `json:"flavor"`
	// Display is how the console names the method: "pgBackRest (Dalibo)".
	Display string `json:"display"`
	// ObjectKind is Stanza (Dalibo) or Archive (Opera); ObjectName is
	// empty when the parameter is missing.
	ObjectKind string `json:"objectKind"`
	ObjectName string `json:"objectName,omitempty"`
	// Stanza is Opera's backup stanza (the parameter, else the Cluster's
	// name); empty for Dalibo.
	Stanza string `json:"stanza,omitempty"`
}

// PgBackRestPlugin reads one spec.plugins entry as a pgBackRest plugin;
// false for any other plugin.
func PgBackRestPlugin(clusterName string, plugin cnpgv1.PluginConfiguration) (PgBackRestConfig, bool) {
	switch plugin.Name {
	case PgBackRestDaliboPlugin:
		return PgBackRestConfig{
			Plugin: plugin.Name, Flavor: PgBackRestFlavorDalibo, Display: pgBackRestDaliboDisplay,
			ObjectKind: PgBackRestKindStanza, ObjectName: bound(plugin.Parameters[PgBackRestDaliboObject], maxAnnotationValue),
		}, true
	case PgBackRestOperaPlugin:
		stanza := plugin.Parameters[PgBackRestOperaStanza]
		if stanza == "" {
			stanza = clusterName
		}
		return PgBackRestConfig{
			Plugin: plugin.Name, Flavor: PgBackRestFlavorOpera, Display: pgBackRestOperaDisplay,
			ObjectKind: PgBackRestKindArchive, ObjectName: bound(plugin.Parameters[PgBackRestOperaObject], maxAnnotationValue),
			Stanza: bound(stanza, maxAnnotationValue),
		}, true
	}
	return PgBackRestConfig{}, false
}

// IsBackupPlugin reports whether an enabled plugin entry is one the
// console reads as the Cluster's backup method: declared the WAL
// archiver, or one of the known backup plugins (barman-cloud and the two
// pgBackRest plugins), which archive WAL whether or not isWALArchiver is
// spelled out.
func IsBackupPlugin(plugin cnpgv1.PluginConfiguration) bool {
	if plugin.Enabled != nil && !*plugin.Enabled {
		return false
	}
	switch {
	case plugin.IsWALArchiver != nil && *plugin.IsWALArchiver:
		return true
	case plugin.Name == BarmanCloudPlugin, plugin.Name == PgBackRestDaliboPlugin, plugin.Name == PgBackRestOperaPlugin:
		return true
	}
	return false
}

// NotReported keys: the Cluster.status fields marked "Deprecated: the
// field is not set for backup plugins" (api/v1/cluster_types.go
// v1.30.0:1026-1056; the same five notes at main), then the
// instance exporter's gauges fed from them
// (pkg/management/postgres/webserver/metricserver/pg_collector.go
// v1.30.0:181-199, both "(Deprecated)").
var pluginNotReported = []string{
	"status.firstRecoverabilityPoint",
	"status.firstRecoverabilityPointByMethod",
	"status.lastSuccessfulBackup",
	"status.lastSuccessfulBackupByMethod",
	"status.lastFailedBackup",
	"metric:cnpg_collector_first_recoverability_point",
	"metric:cnpg_collector_last_available_backup_timestamp",
	"metric:cnpg_collector_last_failed_backup_timestamp",
}

// BackupConfig says how a Cluster backs up, from its spec (operator
// claim of intent, not proof that a backup exists).
type BackupConfig struct {
	// Configured is true when any backup method is declared: an archiver
	// plugin, spec.backup.barmanObjectStore or spec.backup.volumeSnapshot.
	Configured bool `json:"configured"`
	// PluginBased is true when an enabled spec.plugins entry is the WAL
	// archiver or is the Barman Cloud plugin.
	PluginBased bool   `json:"pluginBased"`
	PluginName  string `json:"pluginName,omitempty"`
	// NotReported are the stable keys of the deprecated in-tree fields
	// that stay empty under plugin backups: the UI renders them as "not
	// reported by plugin-based backups", never as "no backup".
	// Recoverability comes from ObjectStore.status.serverRecoveryWindow,
	// Backup objects and repository evidence instead.
	NotReported []string `json:"notReported,omitempty"`
	// PgBackRest is set when the backup plugin is a pgBackRest one:
	// recoverability then comes from repository evidence only.
	PgBackRest *PgBackRestConfig `json:"pgbackrest,omitempty"`
}

// Backups reads the Cluster's backup configuration. PluginBased mirrors
// backupArchiverPlugin in internal/console/kube/convert.go: the first
// enabled plugin (spec order) that IsBackupPlugin accepts. A nil Cluster
// returns the zero value.
func Backups(c *cnpgv1.Cluster) BackupConfig {
	if c == nil {
		return BackupConfig{}
	}
	var config BackupConfig
	for _, plugin := range c.Spec.Plugins {
		if !IsBackupPlugin(plugin) {
			continue
		}
		config.PluginBased = true
		config.PluginName = bound(plugin.Name, maxAnnotationValue)
		if pgbr, ok := PgBackRestPlugin(c.Name, plugin); ok {
			config.PgBackRest = &pgbr
		}
		break
	}
	inTree := c.Spec.Backup != nil && (c.Spec.Backup.BarmanObjectStore != nil || c.Spec.Backup.VolumeSnapshot != nil)
	config.Configured = config.PluginBased || inTree
	if config.PluginBased {
		config.NotReported = append([]string(nil), pluginNotReported...)
	}
	return config
}

// walArchivingConfigured reports whether some component archives WAL,
// by the instance manager's own rule (WALArchiver).
func walArchivingConfigured(c *cnpgv1.Cluster) bool {
	a := WALArchiver(c)
	return a.InTree || a.Plugin != ""
}

// ArchiverLive is the primary's archiver as its /pg/status reports it
// (pkg/postgres/status.go v1.30.0:48 isArchivingWAL, :60 lastFailedWAL,
// :68 readyWalFiles). IsArchivingWAL is pg_stat_archiver's
// last_archived_time > last_failed_time with both defaulting to
// -infinity (pkg/management/postgres/probes.go fillArchiverStatus
// v1.30.0:367-385), so a primary that never archived and an idle
// primary with one old failure and nothing since both read false.
type ArchiverLive struct {
	IsArchivingWAL bool   `json:"isArchivingWAL"`
	LastFailedWAL  string `json:"lastFailedWAL,omitempty"`
	ReadyWALFiles  int    `json:"readyWalFiles"`
	// LastArchivedAt is pg_stat_archiver's last_archived_time (the
	// instance's clock); nil when it reports none.
	LastArchivedAt *time.Time `json:"lastArchivedAt,omitempty"`
	// ObservedAt is when this reading was taken (the sweep's read).
	ObservedAt time.Time `json:"observedAt,omitzero"`
	// Earlier are the same instance's readyWalFiles at the sweeps before
	// this reading, oldest first (the console's sweep ring); empty when
	// none are kept. A backlog counts only when it persists across them
	// (ArchiveBacklogOf).
	Earlier []ReadySample `json:"earlier,omitempty"`
	// ReadyNotReported: the archiving instance is not a primary by its own
	// report (isPrimary false: a replica cluster's designated primary,
	// which archives with archive_mode=always), and CloudNativePG fills
	// readyWalFiles only on a primary (pkg/management/postgres/probes.go
	// v1.30.1:461-463 returns before GetWALArchiveCounters at :520), so
	// ReadyWALFiles is no reading (ArchivingReasonBacklogNotReported).
	ReadyNotReported bool `json:"readyNotReported,omitempty"`
	// PluginMaxParallel is the barman-cloud plugin's ObjectStore
	// spec.configuration.wal.maxParallel (0 when unset) when the caller
	// read that ObjectStore; nil when it did not, or the archiver is not
	// that plugin.
	PluginMaxParallel *int `json:"pluginMaxParallel,omitempty"`
}

// The reasons Archiving infers itself, beside the condition's own: a
// caller that branches on one compares against these, never a literal.
const (
	ArchivingReasonNoCluster             = "NoCluster"
	ArchivingReasonNotConfigured         = "NotConfigured"
	ArchivingReasonSkipped               = "ArchivingSkipped"
	ArchivingReasonNoCondition           = "NoCondition"
	ArchivingReasonLiveArchiving         = "LiveArchiving"
	ArchivingReasonRecoveredSinceFailure = "RecoveredSinceFailure"
	ArchivingReasonFailureWithBacklog    = "FailureWithBacklog"
	ArchivingReasonBacklog               = "ArchiveBacklog"
	ArchivingReasonPrimaryUnread         = "PrimaryUnread"
	// ArchivingReasonHibernated: the Cluster is hibernated, so no
	// primary runs to read or to archive.
	ArchivingReasonHibernated = "Hibernated"
	// ArchivingReasonBacklogUnconfirmed: readyWalFiles is at the backlog
	// threshold, but the console has too few sweeps to tell a backlog
	// that persists from a burst (ArchiveBacklog.Unconfirmed).
	ArchivingReasonBacklogUnconfirmed = "ArchiveBacklogUnconfirmed"
	// ArchivingReasonBacklogNotReported: the archiving instance is a
	// designated primary (ArchiverLive.ReadyNotReported), which does not
	// report readyWalFiles, so a backlog cannot be seen.
	ArchivingReasonBacklogNotReported = "ArchiveBacklogNotReported"
)

// ArchiveBacklogSegments is the floor of the readyWalFiles count at
// which the archiver may be behind (ArchiveBacklogThreshold). A
// serial archiver (one segment per archive_command) shows zero or one
// .ready file, two on a segment switch caught mid-copy. A parallel one
// shows more while healthy: in-tree wal.maxParallel and the barman-cloud
// plugin upload up to maxParallel segments at once and keep the ones
// uploaded ahead as .ready until PostgreSQL asks for them, and
// pgBackRest's asynchronous archive-push does the same. So one reading
// at the threshold proves nothing: a backlog counts only when it
// persists across sweeps (ArchiveBacklogOf). The waiting segments are
// WAL the repository does not hold yet, so recovery screens read a
// persistent backlog as "the newest restore points are not
// established". The diagnostics findings on the same count
// (cnpg-wal-archive-backlog, cnpg-instance-archive-backlog) fire much
// later, at 32: by then the recovery screens already say it.
const ArchiveBacklogSegments = 3

// ArchivingReading is the WAL-archiving verdict.
type ArchivingReading struct {
	Class           Class  `json:"class"`
	ConditionStatus string `json:"conditionStatus,omitempty"`
	// Reason is the condition's reason, or the console's inference when
	// the condition is absent or overridden by the live archiver (the
	// ArchivingReason constants).
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Archiving combines the ContinuousArchiving condition (written by the
// instance on each archive_command, pkg/management/postgres/webserver/local.go
// v1.30.0:247-263, v1.30.1 and main:264-280) with the primary's live archiver (nil when the
// primary's /pg/status was not read).
//
//   - No archiver configured and no condition: info.
//   - cnpg.io/skipWalArchiving=enabled: warning (EscapeHatches carries
//     the critical flag).
//   - Condition True: ok, unless the live archiver shows the last
//     attempt failed with WAL waiting (ready files > 0): warning
//     FailureWithBacklog; or, without a failure, a backlog that persists
//     across sweeps (ArchiveBacklogOf): warning ArchiveBacklog (a hung
//     or slow archive_command fails nothing, and the condition stays
//     True while segments pile up). An idle primary with an old failure
//     (isArchivingWAL=false, no backlog) is not made worse than ok by
//     that alone. With the primary unread: unknown PrimaryUnread, with
//     the condition's age (the condition alone confirms nothing now),
//     or inactive Hibernated when the Cluster is hibernated. A count at
//     the backlog threshold that too few sweeps saw to judge is unknown
//     ArchiveBacklogUnconfirmed, never ok. A designated primary (a
//     replica cluster's archiver) reports no readyWalFiles, so no backlog
//     can be seen on it: unknown ArchiveBacklogNotReported, never ok.
//   - Condition False: critical, unless the live archiver shows a
//     success after the failure: warning (the condition catches up on
//     the next archive).
//   - Condition absent while configured: unknown, or ok when the live
//     archiver is archiving (warning ArchiveBacklog with a persistent
//     backlog, unknown ArchiveBacklogNotReported on a designated
//     primary).
//   - Condition Unknown: unknown.
//
// The stalled archiver whose last_archived_time ages while WAL is
// written needs no rule of its own: with archive_timeout set, written
// WAL completes a segment within it, and a segment the archiver does
// not take is a .ready file, so the stall shows as the backlog.
func Archiving(c *cnpgv1.Cluster, primary *ArchiverLive) ArchivingReading {
	return ArchivingOf(c, primary, false, false)
}

// ArchivingOf is Archiving, told why a primary given as nil was not
// read. primaryMasked: its instance manager answered /pg/status without
// reaching PostgreSQL (fenced, restarting or rewinding,
// instancestatus.Reading.Masked); the reading is the same, PrimaryUnread
// with condition True, worded so that a DBA does not look for a sweep
// that failed. notChecked: the console reads no instance
// (INSTANCE_STATUS_ENABLED=false), so a condition True is shown as
// CloudNativePG reports it, ok, still PrimaryUnread so that nothing
// downstream takes it for a checked reading.
func ArchivingOf(c *cnpgv1.Cluster, primary *ArchiverLive, primaryMasked, notChecked bool) ArchivingReading {
	if c == nil {
		return ArchivingReading{Class: ClassUnknown, Reason: ArchivingReasonNoCluster}
	}
	condition := meta.FindStatusCondition(c.Status.Conditions, ConditionContinuousArchiving)
	if c.Annotations[AnnotationSkipWalArchiving] == valueEnabled {
		return ArchivingReading{
			Class:   ClassWarning,
			Reason:  ArchivingReasonSkipped,
			Message: "cnpg.io/skipWalArchiving is enabled: PostgreSQL does not archive WAL",
		}
	}
	// Before the condition: with no archiver, CloudNativePG's
	// archive_command returns success without copying anything
	// (pkg/management/postgres/archiver/archiver.go v1.30.1:179-186) and
	// the instance manager then writes ContinuousArchiving True
	// (internal/cmd/manager/walarchive/cmd.go v1.30.1:78), so True there
	// means nothing reaches a repository. A False condition still
	// reads below: a failure is never played down.
	if !walArchivingConfigured(c) && (condition == nil || condition.Status != statusFalse) {
		return ArchivingReading{Class: ClassInfo, Reason: ArchivingReasonNotConfigured, Message: "no WAL archiver is configured"}
	}
	b := ArchiveBacklogOf(c, primary)
	backlog := b.Behind()
	hibernated := primary == nil && Hibernation(c).State == HibernationHibernated
	unconfirmed := func(reading ArchivingReading) ArchivingReading {
		if b.Unconfirmed == 0 {
			return reading
		}
		reading.Class = ClassUnknown
		reading.Reason = ArchivingReasonBacklogUnconfirmed
		reading.Message = strconv.Itoa(b.Unconfirmed) + " WAL segments are waiting to be archived on the primary, and the console has not swept it long enough to tell a backlog that persists from a burst"
		return reading
	}
	notReported := primary != nil && primary.ReadyNotReported
	notReportedReading := func(reading ArchivingReading) ArchivingReading {
		reading.Class = ClassUnknown
		reading.Reason = ArchivingReasonBacklogNotReported
		reading.Message = "the archiving instance is a designated primary, which does not report readyWalFiles: whether WAL waits to be archived on it is not known"
		if !IsReplicaCluster(c) {
			// Mid-promotion the current primary can briefly report
			// isPrimary false (CNPG P3-6).
			reading.Message = "the current primary does not report itself primary yet (a promotion may be in progress), so it does not report readyWalFiles: whether WAL waits to be archived on it is not known"
		}
		return reading
	}
	hibernatedMessage := "the Cluster is hibernated: no instance runs, so nothing archives WAL, and whether every segment written before it reached the repository is not confirmed"
	backlogReading := func(reading ArchivingReading) ArchivingReading {
		reading.Class = ClassWarning
		reading.Reason = ArchivingReasonBacklog
		reading.Message = strconv.Itoa(primary.ReadyWALFiles) + " WAL segments are waiting to be archived on the primary: the archiver is behind"
		if b.StalledSince != nil {
			reading.Message = strconv.Itoa(primary.ReadyWALFiles) + " WAL segments have waited to be archived on the primary over " + strconv.Itoa(BacklogSweepsUnknownParallelism) +
				" sweeps, and it archived none since " + b.StalledSince.UTC().Format(time.RFC3339) + ": the archiver is stalled"
		}
		return reading
	}
	if condition == nil {
		if primary != nil && primary.IsArchivingWAL {
			if notReported {
				return notReportedReading(ArchivingReading{})
			}
			if backlog {
				return backlogReading(ArchivingReading{})
			}
			return unconfirmed(ArchivingReading{Class: ClassOK, Reason: ArchivingReasonLiveArchiving, Message: "the primary reports WAL archiving; the operator has not written ContinuousArchiving yet"})
		}
		if hibernated {
			return ArchivingReading{Class: ClassInactive, Reason: ArchivingReasonHibernated, Message: hibernatedMessage}
		}
		return ArchivingReading{Class: ClassUnknown, Reason: ArchivingReasonNoCondition, Message: "a WAL archiver is configured but ContinuousArchiving is not reported"}
	}
	reading := ArchivingReading{
		ConditionStatus: string(condition.Status),
		Reason:          bound(condition.Reason, maxAnnotationValue),
		Message:         bound(condition.Message, maxMessage),
	}
	switch condition.Status {
	case statusTrue:
		reading.Class = ClassOK
		switch {
		case hibernated:
			// Deliberately asleep: inactive, as the Cluster is (DBA, round
			// 8), not unknown, which would outrank it in the headline. The
			// recovery window's newest edge stays unknown on its own.
			reading.Class = ClassInactive
			reading.Reason = ArchivingReasonHibernated
			reading.Message = "ContinuousArchiving is True" + since(condition.LastTransitionTime.Time) + ", but " + hibernatedMessage
		case primary == nil:
			// The condition changes only when an archive_command's
			// outcome changes: written by an instance that may be gone,
			// True says nothing about WAL reaching the repository now.
			reading.Class = ClassUnknown
			reading.Reason = ArchivingReasonPrimaryUnread
			if notChecked {
				// Info, not ok: CloudNativePG's word, weaker than a checked reading.
				reading.Class = ClassInfo
				reading.Message = "ContinuousArchiving is True" + since(condition.LastTransitionTime.Time) +
					" per the Cluster status, not checked on the instance: the instance status sweep is switched off"
				break
			}
			unread := "the primary's archiver was not read"
			if primaryMasked {
				unread = "the primary's instance manager answered without reading PostgreSQL (fenced, restarting or rewinding)"
			}
			reading.Message = "ContinuousArchiving is True" + since(condition.LastTransitionTime.Time) +
				", but " + unread + ": whether WAL reaches the repository now is not confirmed"
		case primary != nil && !primary.IsArchivingWAL && primary.LastFailedWAL != "" && primary.ReadyWALFiles > 0:
			reading.Class = ClassWarning
			reading.Reason = ArchivingReasonFailureWithBacklog
			reading.Message = "the primary's last archive attempt failed (" + bound(primary.LastFailedWAL, maxAnnotationValue) + ") and WAL files are waiting"
		case notReported:
			reading = notReportedReading(reading)
		case backlog:
			reading = backlogReading(reading)
		default:
			reading = unconfirmed(reading)
		}
	case statusFalse:
		reading.Class = ClassCritical
		if primary != nil && primary.IsArchivingWAL {
			reading.Class = ClassWarning
			reading.Reason = ArchivingReasonRecoveredSinceFailure
		}
	default:
		reading.Class = ClassUnknown
	}
	return reading
}

// since is " since <t>" for a condition's last transition, or "" when
// it is not set.
func since(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " since " + t.UTC().Format(time.RFC3339)
}

// IsReplicaCluster is CloudNativePG's Cluster.IsReplica
// (api/v1/cluster_funcs.go v1.30.1:1087, not in the api module the
// console builds against): spec.replica.enabled when set, else the
// distributed topology's self (the Cluster's name by default) differing
// from replica.primary.
func IsReplicaCluster(c *cnpgv1.Cluster) bool {
	if c == nil || c.Spec.ReplicaCluster == nil {
		return false
	}
	r := c.Spec.ReplicaCluster
	if r.Enabled != nil {
		return *r.Enabled
	}
	self := r.Self
	if self == "" {
		self = c.Name
	}
	return self != r.Primary
}
