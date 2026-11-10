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

package viewmodel

import "time"

// Backups is the /backups screen: how the
// Cluster backs up, the Backup objects, the schedules as CloudNativePG
// reads them, the recovery window and the repository evidence
// cross-check. Under plugin-based backups the deprecated in-tree status
// fields are listed in Summary.NotReported and rendered "not reported by
// plugin-based backups", never "no backup".
type Backups struct {
	Ref         ClusterRef    `json:"ref"`
	GeneratedAt time.Time     `json:"generatedAt"`
	Summary     BackupSummary `json:"summary"`
	Method      BackupMethod  `json:"method"`
	// Items are the Backup objects of the Cluster, newest first.
	Items []BackupRow `json:"items"`
	// Schedules are the ScheduledBackup objects of the Cluster.
	Schedules []ScheduleRow `json:"schedules"`
	// Evidence is the independent cross-check against the backup
	// repository (the EvidenceAgent of the namespace).
	Evidence EvidenceCheck `json:"evidence"`
	// RecoveryTimeline is the recovery-window bar.
	RecoveryTimeline RecoveryTimeline `json:"recoveryTimeline"`
	// Drills are the restore drills of the Cluster.
	Drills     RestoreDrills `json:"drills"`
	Sources    []SourceState `json:"sources"`
	BlindSpots []BlindSpot   `json:"blindSpots"`
}

// BackupMethod is the declared backup configuration (spec: intent, not
// proof that a backup exists).
type BackupMethod struct {
	Configured  bool   `json:"configured"`
	PluginBased bool   `json:"pluginBased,omitempty"`
	PluginName  string `json:"pluginName,omitempty"`
	// ObjectStore is the barman-cloud plugin's ObjectStore name.
	ObjectStore       string `json:"objectStore,omitempty"`
	InTreeObjectStore bool   `json:"inTreeObjectStore,omitempty" doc:"spec.backup.barmanObjectStore is set (deprecated upstream)."`
	VolumeSnapshot    bool   `json:"volumeSnapshot,omitempty" doc:"spec.backup.volumeSnapshot is set."`
	// PgBackRest is set when the backup plugin is one of the two
	// pgBackRest plugins.
	PgBackRest *PgBackRestMethod `json:"pgbackrest,omitempty"`
	Provenance Provenance        `json:"provenance"`
}

// PgBackRestMethod is a pgBackRest plugin as the Cluster declares it,
// completed by the repository evidence where the spec does not say
// (Dalibo's stanza lives in its Stanza object).
type PgBackRestMethod struct {
	Plugin string `json:"plugin" doc:"pgbackrest.dalibo.com or pgbackrest.cnpg.opera.com."`
	Flavor string `json:"flavor" enum:"dalibo,opera"`
	// Display names the method: "pgBackRest (Dalibo)".
	Display    string `json:"display"`
	ObjectKind string `json:"objectKind" enum:"Stanza,Archive" doc:"The plugin's object the Cluster names: a Stanza (Dalibo) or an Archive (Opera)."`
	ObjectName string `json:"objectName,omitempty" untrusted:"true"`
	// Stanza holds the backups; ArchiveStanza the WAL when it differs.
	Stanza        string `json:"stanza,omitempty" untrusted:"true" doc:"The backup stanza: Opera's parameter (else the Cluster name), or the evidence agent's report."`
	ArchiveStanza string `json:"archiveStanza,omitempty" untrusted:"true" doc:"The WAL stanza, when it differs from stanza (a split stanza)."`
	Repository    *int64 `json:"repository,omitempty" doc:"The repository number (repo<n>) the evidence agent reads."`
	Encrypted     *bool  `json:"encrypted,omitempty" doc:"The repository is encrypted, per the evidence agent; absent until it read the info files."`
}

// PgBackRestBackupRef is the pgBackRest backup a Backup object took,
// joined by its label (Dalibo status.backupName, Opera status.backupId or
// the cnpg-backup-name annotation).
type PgBackRestBackupRef struct {
	Label string `json:"label" untrusted:"true"`
	Type  string `json:"type" enum:"full,diff,incr"`
	// Prior is the backup this one was taken against; Full the full
	// backup that heads its chain (the label itself for a full).
	Prior string `json:"prior,omitempty" untrusted:"true"`
	Full  string `json:"full" untrusted:"true"`
	// Usable is the evidence agent's claim; absent when the label is not
	// in the evidence read.
	Usable *bool  `json:"usable,omitempty"`
	Stanza string `json:"stanza" untrusted:"true"`
}

// BackupRow is one Backup object.
type BackupRow struct {
	Name string `json:"name" untrusted:"true"`
	// Phase is status.phase verbatim; absent while the operator has not
	// written one.
	Phase    string `json:"phase,omitempty"`
	Health   Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Known    bool   `json:"known" doc:"The phase is in the table of the observed CNPG minor."`
	Terminal bool   `json:"terminal,omitempty" doc:"Nothing moves the backup forward from this phase."`
	Method   string `json:"method,omitempty"`
	// PluginName is the plugin that took a plugin backup.
	PluginName string     `json:"pluginName,omitempty"`
	Instance   string     `json:"instance,omitempty" doc:"status.instanceID.podName: the instance the backup was taken from."`
	Online     *bool      `json:"online,omitempty"`
	BackupID   string     `json:"backupId,omitempty"`
	Error      string     `json:"error,omitempty" doc:"status.error, bounded, rendered escaped." untrusted:"true"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	StoppedAt  *time.Time `json:"stoppedAt,omitempty"`
	// Schedule is the ScheduledBackup that created it, when one did.
	Schedule string `json:"schedule,omitempty"`
	// NotReportedWhileRunning lists status fields a plugin backup sets
	// only at completion: render "reported at completion", not missing.
	NotReportedWhileRunning []string `json:"notReportedWhileRunning,omitempty"`
	// Repository is this backup's presence in the repository evidence.
	Repository RepositoryPresence `json:"repository" enum:"present,absent,unknown,notChecked"`
	// PgBackRest is the pgBackRest backup this object took: its label,
	// type and chain; absent for other methods or before completion.
	PgBackRest *PgBackRestBackupRef `json:"pgbackrest,omitempty"`
	// PredatesCluster: the Backup names this Cluster but was created
	// before it, or is owned by a Cluster with another UID (an earlier
	// Cluster of the same name). It counts in no recency, topology or
	// timeline of this Cluster.
	PredatesCluster bool       `json:"predatesCluster,omitempty"`
	Provenance      Provenance `json:"provenance"`
}

// RepositoryPresence is whether the evidence agent saw a backup in the
// repository.
type RepositoryPresence string

// The repository presence values. Absent is claimed only from a
// complete, untruncated inventory; anything less is unknown.
const (
	RepositoryPresent    RepositoryPresence = "present"
	RepositoryAbsent     RepositoryPresence = "absent"
	RepositoryUnknown    RepositoryPresence = "unknown"
	RepositoryNotChecked RepositoryPresence = "notChecked"
)

// ScheduleRow is one ScheduledBackup.
type ScheduleRow struct {
	Name string `json:"name" untrusted:"true"`
	// Schedule is spec.schedule verbatim: six fields, seconds first, as
	// CloudNativePG's cron reads it.
	Schedule string `json:"schedule,omitempty"`
	// Note explains a schedule that would not read as written (five
	// fields read with seconds first).
	Note            string     `json:"note,omitempty"`
	State           string     `json:"state" doc:"active, suspended, awaitingFirstCheck, overdue, invalidSchedule, admissionError or unknown."`
	Health          Health     `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Text            string     `json:"text,omitempty"`
	Suspended       bool       `json:"suspended,omitempty"`
	Immediate       bool       `json:"immediate,omitempty"`
	Method          string     `json:"method,omitempty"`
	Target          string     `json:"target,omitempty"`
	LastScheduledAt *time.Time `json:"lastScheduledAt,omitempty"`
	NextScheduledAt *time.Time `json:"nextScheduledAt,omitempty"`
	Error           string     `json:"error,omitempty" untrusted:"true"`
	Provenance      Provenance `json:"provenance"`
}

// EvidenceState is the state of the repository cross-check.
type EvidenceState string

// The evidence states.
const (
	// EvidenceNotConfigured: the Cluster archives to no repository an
	// agent could read (no backup method, or volume snapshots only).
	EvidenceNotConfigured EvidenceState = "notConfigured"
	// EvidenceNoAgent: no EvidenceAgent in the namespace serves this
	// Cluster's repository; the cross-check is a capability not
	// installed, not a fault.
	EvidenceNoAgent EvidenceState = "noAgent"
	// EvidencePending: an agent exists and has not answered yet.
	EvidencePending EvidenceState = "pending"
	// EvidenceUnavailable: the agent did not answer or its answer was
	// refused (TLS, identity, contract).
	EvidenceUnavailable EvidenceState = "unavailable"
	// EvidenceLive: the latest answer is current.
	EvidenceLive EvidenceState = "live"
	// EvidenceStale: the latest answer is retained after a failure, or
	// the agent itself says its snapshot is stale.
	EvidenceStale EvidenceState = "stale"
	// EvidenceDisabled: the console reads no evidence (configuration).
	EvidenceDisabled EvidenceState = "disabled"
)

// EvidenceCheck is the evidence agent's repository cross-check as the console
// shows it: the agent's conclusions, never the console's own reading of
// object storage.
type EvidenceCheck struct {
	State  EvidenceState `json:"state" enum:"notConfigured,noAgent,pending,unavailable,live,stale,disabled"`
	Health Health        `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Agent is the EvidenceAgent consulted, when one serves the Cluster.
	Agent  string `json:"agent,omitempty"`
	Reason string `json:"reason,omitempty" doc:"Why the cross-check is not live; never secret material."`
	// Repository is the agent's summary; absent until it answered.
	Repository *RepositoryEvidence `json:"repository,omitempty"`
	Provenance Provenance          `json:"provenance"`
}

// RepositoryEvidence is the agent's summary of the repository
// (evidence/v1alpha1). "Structurally usable" is the strongest claim the
// agent makes; nothing here says a restore will work.
type RepositoryEvidence struct {
	// Completeness is the agent's own completeness label, verbatim.
	Completeness string `json:"completeness,omitempty"`
	// AgentStale is the agent's own stale flag.
	AgentStale bool `json:"agentStale,omitempty"`
	// Overall is the snapshot's state.
	Overall            EvidenceFact `json:"overall"`
	WAL                EvidenceFact `json:"wal"`
	Timeline           EvidenceFact `json:"timeline"`
	Coverage           EvidenceFact `json:"coverage"`
	Retention          EvidenceFact `json:"retention"`
	Backups            *int64       `json:"backups,omitempty" doc:"Backups the agent saw in the repository."`
	StructurallyUsable *int64       `json:"structurallyUsable,omitempty" doc:"Of those, structurally usable per the agent."`
	// RepositoryOnly counts repository backups with no Backup object in
	// the Cluster's namespace (taken by another Cluster or deleted
	// objects); absent when the inventory was truncated.
	RepositoryOnly         *int       `json:"repositoryOnly,omitempty"`
	OldestCompletionAt     *time.Time `json:"oldestCompletionAt,omitempty"`
	NewestCompletionAt     *time.Time `json:"newestCompletionAt,omitempty"`
	LatestArchiveReceiptAt *time.Time `json:"latestArchiveReceiptAt,omitempty"`
	Truncated              bool       `json:"truncated,omitempty" doc:"The agent's lists were cut: absence conclusions are unknown."`
	CompletedAt            *time.Time `json:"completedAt,omitempty" doc:"When the agent completed the scan."`
	// PgBackRest is the pgBackRest part of the summary: the stanza, the
	// catalog's integrity, the agent's recovery window, its findings and
	// the repository's backups with their chains. Absent for Barman.
	PgBackRest *PgBackRestEvidence `json:"pgbackrest,omitempty"`
}

// PgBackRestEvidence is what the evidence agent reports about one
// pgBackRest stanza (pgbackrest/v1alpha1).
type PgBackRestEvidence struct {
	Stanza            string       `json:"stanza" untrusted:"true"`
	ArchiveStanza     string       `json:"archiveStanza" untrusted:"true" doc:"Where the WAL is read; differs from stanza for a split stanza (finding StanzaSplit)."`
	Repository        int64        `json:"repository" doc:"The repository number (repo<n>)."`
	RepositoryFormat  *int64       `json:"repositoryFormat,omitempty" doc:"backrest-format of the info files: 5 is read, 6 is refused."`
	Encrypted         *bool        `json:"encrypted,omitempty"`
	PgBackRestVersion string       `json:"pgbackrestVersion,omitempty" untrusted:"true"`
	Catalog           EvidenceFact `json:"catalog" doc:"The integrity of backup.info, archive.info and the manifests."`
	// Retention is the stanza against the repository's repo-retention-*
	// options; absent when the agent was given none.
	Retention *PgBackRestRetention `json:"retention,omitempty"`
	// RecoveryWindow is the agent's own window: from the oldest usable
	// backup's stop to the receipt of the last contiguous WAL segment.
	RecoveryWindow *PgBackRestRecoveryWindow `json:"recoveryWindow,omitempty"`
	Findings       []PgBackRestFinding       `json:"findings"`
	// FindingsTruncated: the agent cut its findings list.
	FindingsTruncated bool `json:"findingsTruncated,omitempty"`
	// Backups are the repository's backups, newest first, bounded;
	// BackupsTruncated says more exist than are listed.
	Backups          []PgBackRestRepoBackup `json:"backups"`
	BackupsTruncated bool                   `json:"backupsTruncated,omitempty"`
}

// PgBackRestRetention is the agent's comparison of a stanza with the
// repository's retention, as of the newest backup: pgBackRest expires at
// the end of each backup, never on its own.
type PgBackRestRetention struct {
	Configured bool `json:"configured" doc:"False when repo-retention-full is unset: pgBackRest never expires a full backup."`
	// Full is a count of full backups, or days when fullType is time.
	Full             *int64 `json:"full,omitempty" doc:"repo-retention-full: full backups kept, or days when fullType is time."`
	FullType         string `json:"fullType" enum:"count,time"`
	Diff             *int64 `json:"diff,omitempty" doc:"repo-retention-diff: differential backups kept."`
	Archive          *int64 `json:"archive,omitempty" doc:"repo-retention-archive, as configured."`
	ArchiveType      string `json:"archiveType" enum:"full,diff,incr"`
	EffectiveArchive *int64 `json:"effectiveArchive,omitempty" doc:"The archive retention expire applies, in backups of archiveType; absent when WAL is never expired."`
	// The backups of each type the stanza holds.
	FullBackups         int64 `json:"fullBackups"`
	DifferentialBackups int64 `json:"differentialBackups"`
	IncrementalBackups  int64 `json:"incrementalBackups"`
	// EvaluatedAt is the newest backup's stop, when the retention was last
	// applied; WindowStartAt is the start of a time-based window.
	EvaluatedAt   *time.Time   `json:"evaluatedAt,omitempty"`
	WindowStartAt *time.Time   `json:"windowStartAt,omitempty"`
	Result        EvidenceFact `json:"result" doc:"The comparison: RetentionNotConfigured, RetentionNotApplied and RetentionShortfall are among the findings."`
}

// PgBackRestRecoveryWindow is the agent's recovery window of a stanza.
type PgBackRestRecoveryWindow struct {
	OldestBackup string    `json:"oldestBackup" untrusted:"true"`
	StartAt      time.Time `json:"startAt" doc:"The first recoverability point: the oldest usable backup's stop time."`
	NewestBackup string    `json:"newestBackup" untrusted:"true"`
	NewestStopAt time.Time `json:"newestStopAt"`
	EndTimeline  int64     `json:"endTimeline"`
	EndWAL       string    `json:"endWal" untrusted:"true" doc:"The last contiguous WAL segment."`
	// EndReceiptAt is when that segment reached the repository: the
	// window's end in time.
	EndReceiptAt  *time.Time `json:"endReceiptAt,omitempty"`
	UsableBackups int64      `json:"usableBackups"`
	Assumptions   []string   `json:"assumptions"`
}

// PgBackRestFinding is one problem the agent's reader found.
type PgBackRestFinding struct {
	Code     string `json:"code" doc:"Stable UpperCamelCase code (ChecksumMismatch, CopyMismatch, StanzaSplit, WALGap, ...); the list grows."`
	Severity string `json:"severity" enum:"warning,error"`
	Health   Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Subject  string `json:"subject" untrusted:"true" doc:"An object key relative to the repository root, or a backup label."`
	Message  string `json:"message"`
}

// PgBackRestRepoBackup is one backup of the stanza as the agent read it.
type PgBackRestRepoBackup struct {
	Label string `json:"label" untrusted:"true"`
	Type  string `json:"type" enum:"full,diff,incr"`
	Prior string `json:"prior,omitempty" untrusted:"true"`
	Full  string `json:"full" untrusted:"true" doc:"The full backup that heads the chain."`
	// References counts the backups a restore of this one reads.
	References      int        `json:"references"`
	Usable          bool       `json:"usable"`
	UnusableReasons []string   `json:"unusableReasons"`
	State           string     `json:"state" untrusted:"true"`
	Health          Health     `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Failed          bool       `json:"failed,omitempty" doc:"pgBackRest recorded the backup with an error."`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	StoppedAt       *time.Time `json:"stoppedAt,omitempty"`
	Timeline        *int64     `json:"timeline,omitempty"`
	SizeBytes       int64      `json:"sizeBytes"`
	// RepositorySizeBytes is what it occupies in the repository.
	RepositorySizeBytes int64 `json:"repositorySizeBytes"`
	// BackupName is the CNPG Backup object that took it, when one in the
	// namespace matches.
	BackupName string `json:"backupName,omitempty" untrusted:"true"`
}

// EvidenceFact is one agent conclusion: its state verbatim, the reason
// code and message, and how it is drawn.
type EvidenceFact struct {
	State   string `json:"state,omitempty" doc:"The agent's state, verbatim." untrusted:"true"`
	Health  Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	// Phrase is Message as a translatable sentence, when the console
	// recognises its shape (the timeline reasons); absent otherwise.
	Phrase *Phrase `json:"phrase,omitempty" doc:"Message as a translatable Phrase, when the console recognises its shape; absent otherwise."`
}
