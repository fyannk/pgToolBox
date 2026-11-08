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

// walArchivingConfigured reports whether some component archives WAL:
// the in-tree Barman object store or an archiver plugin.
func walArchivingConfigured(c *cnpgv1.Cluster, backups BackupConfig) bool {
	return backups.PluginBased || (c.Spec.Backup != nil && c.Spec.Backup.BarmanObjectStore != nil)
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
}

// ArchivingReading is the WAL-archiving verdict.
type ArchivingReading struct {
	Class           Class  `json:"class"`
	ConditionStatus string `json:"conditionStatus,omitempty"`
	// Reason is the condition's reason, or the console's inference when
	// the condition is absent or overridden by the live archiver
	// (NotConfigured, ArchivingSkipped, NoCondition, LiveArchiving,
	// RecoveredSinceFailure, FailureWithBacklog).
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Archiving combines the ContinuousArchiving condition (written by the
// instance on each archive_command, pkg/management/postgres/webserver/local.go
// v1.30.0:247-263) with the primary's live archiver (nil when the
// primary's /pg/status was not read).
//
//   - No archiver configured and no condition: info.
//   - cnpg.io/skipWalArchiving=enabled: warning (EscapeHatches carries
//     the critical flag).
//   - Condition True: ok, unless the live archiver shows the last
//     attempt failed with WAL waiting (ready files > 0): warning. An idle
//     primary with an old failure (isArchivingWAL=false, no backlog) is
//     not made worse than ok by that alone.
//   - Condition False: critical, unless the live archiver shows a
//     success after the failure: warning (the condition catches up on
//     the next archive).
//   - Condition absent while configured: unknown, or ok when the live
//     archiver is archiving.
//   - Condition Unknown: unknown.
func Archiving(c *cnpgv1.Cluster, primary *ArchiverLive) ArchivingReading {
	if c == nil {
		return ArchivingReading{Class: ClassUnknown, Reason: "NoCluster"}
	}
	backups := Backups(c)
	condition := meta.FindStatusCondition(c.Status.Conditions, ConditionContinuousArchiving)
	if c.Annotations[AnnotationSkipWalArchiving] == valueEnabled {
		return ArchivingReading{
			Class:   ClassWarning,
			Reason:  "ArchivingSkipped",
			Message: "cnpg.io/skipWalArchiving is enabled: PostgreSQL does not archive WAL",
		}
	}
	if condition == nil {
		if !walArchivingConfigured(c, backups) {
			return ArchivingReading{Class: ClassInfo, Reason: "NotConfigured", Message: "no WAL archiver is configured"}
		}
		if primary != nil && primary.IsArchivingWAL {
			return ArchivingReading{Class: ClassOK, Reason: "LiveArchiving", Message: "the primary reports WAL archiving; the operator has not written ContinuousArchiving yet"}
		}
		return ArchivingReading{Class: ClassUnknown, Reason: "NoCondition", Message: "a WAL archiver is configured but ContinuousArchiving is not reported"}
	}
	reading := ArchivingReading{
		ConditionStatus: string(condition.Status),
		Reason:          bound(condition.Reason, maxAnnotationValue),
		Message:         bound(condition.Message, maxMessage),
	}
	switch condition.Status {
	case statusTrue:
		reading.Class = ClassOK
		if primary != nil && !primary.IsArchivingWAL && primary.LastFailedWAL != "" && primary.ReadyWALFiles > 0 {
			reading.Class = ClassWarning
			reading.Reason = "FailureWithBacklog"
			reading.Message = "the primary's last archive attempt failed (" + bound(primary.LastFailedWAL, maxAnnotationValue) + ") and WAL files are waiting"
		}
	case statusFalse:
		reading.Class = ClassCritical
		if primary != nil && primary.IsArchivingWAL {
			reading.Class = ClassWarning
			reading.Reason = "RecoveredSinceFailure"
		}
	default:
		reading.Class = ClassUnknown
	}
	return reading
}
