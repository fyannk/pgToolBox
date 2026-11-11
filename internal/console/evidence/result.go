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

// Package evidence is the console's consumer of the evidence agents: it
// discovers the EvidenceAgent objects of
// the watched namespaces from the informer cache, calls each agent over
// in-cluster HTTPS (TLS 1.3, the CA from EvidenceAgent.status.caBundle,
// the server name of its Service) with the console's bound
// ServiceAccount token (audience pgtoolbox-evidence-agent), validates
// every answer against the evidence/v1alpha1 contract, and publishes the
// latest good result per agent. Fetching happens in a background runner,
// never in a request; the read model only reads Results. Object-store
// credentials never enter the console: it receives conclusions only.
//
// Without the operator, the pgtools chart hands the console a fixed list of
// agents instead (EVIDENCE_AGENTS_FILE, static.go): same Results, read with
// a shared token over plain HTTP or HTTPS against a mounted CA.
package evidence

import (
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Result is what the console knows about the evidence for one Cluster.
type Result struct {
	// Agent is the EvidenceAgent whose spec.clusterRef names the Cluster;
	// empty when none does (State is then EvidenceNoAgent).
	Agent string
	// State is the cross-check state (viewmodel.Evidence*).
	State viewmodel.EvidenceState
	// Reason says why State is not live, display-safe and secret-free.
	Reason string
	// ObservedAt is when the retained snapshot was fetched.
	ObservedAt *time.Time
	// Snapshot is the latest good snapshot; nil until one was accepted.
	// A failed refresh keeps the previous one (State stale).
	Snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot
	// BackupIDs are the backup_id values of the repository's backups for
	// the agent's server; BackupsComplete is false when the collection
	// was truncated or not fully read, in which case absence proves
	// nothing.
	BackupIDs       map[string]bool
	BackupsComplete bool
	// Backups are the same collection's items, compact, newest last;
	// bounded to MaximumBackupItems (the newest are kept).
	Backups []BackupItem
	// Recovery is the agent's observed recovery coverage of the same
	// publication.
	Recovery Recovery
	// PgBackRestPlugin and PgBackRestObject are the pgBackRest plugin and
	// the Stanza or Archive the EvidenceAgent's status says it resolved
	// when the snapshot was fetched; empty for a Barman agent and for a
	// static agent (no EvidenceAgent status).
	PgBackRestPlugin, PgBackRestObject string
	// RefreshInterval is how often the agent re-reads the repository
	// (spec.refreshInterval, else the agent's default): a retention purge
	// can remove a backup up to that long before the evidence shows it.
	RefreshInterval time.Duration
}

// DefaultAgentRefreshInterval is the agent's own default cadence
// (CATALOG_REFRESH_INTERVAL), used when spec.refreshInterval is unset.
const DefaultAgentRefreshInterval = 5 * time.Minute

// BackupItem is one repository backup as the recovery window needs it.
type BackupItem struct {
	ID      string
	Status  *string
	State   evidencev1alpha1.State
	Reason  evidencev1alpha1.Reason
	BeginAt *time.Time
	EndAt   *time.Time
	// EndTimeline and EndPosition locate the backup's end WAL segment
	// (the position counts segments, as the agent's recovery paths do);
	// nil when the repository does not say or the name does not parse.
	EndTimeline *uint64
	EndPosition *uint64
	// Failed is the repository's own failure mark: Barman status FAILED,
	// pgBackRest backup-error.
	Failed bool
	// PgBackRest carries what only a pgBackRest backup has (ID is then
	// its label); nil for a Barman backup.
	PgBackRest *PgBackRestItem
}

// PgBackRestItem is a pgBackRest backup's chain and identity: its type,
// the backup it was taken against, the backups a restore of it reads, and
// the CNPG Backup name Opera's plugin stores as an annotation.
type PgBackRestItem struct {
	Stanza string
	// Type is full, diff or incr.
	Type string
	// Prior is the backup this one was taken against (nil for a full).
	Prior *string
	// References are every backup a restore of this one reads.
	References []string
	// Usable is the agent's strongest claim: chain, manifests and WAL to
	// consistency verified; UnusableReasons say why not.
	Usable          bool
	UnusableReasons []string
	Timeline        *uint64
	// CNPGBackupName is the cnpg-backup-name annotation (Opera).
	CNPGBackupName *string
	SizeBytes      uint64
	// RepositorySizeBytes is what the backup occupies in the repository
	// (compressed, deduplicated against its prior).
	RepositorySizeBytes uint64
}

// Recovery states.
const (
	// RecoveryAvailable: the recovery paths of the publication were read.
	RecoveryAvailable = "available"
	// RecoveryUnsupported: the agent does not claim observed recovery
	// coverage for this repository.
	RecoveryUnsupported = "unsupported"
	// RecoveryUnavailable: reading them failed; Reason says why.
	RecoveryUnavailable = "unavailable"
	// RecoveryNotScanned: the agent has no completed scan yet.
	RecoveryNotScanned = "notScanned"
	// RecoveryUncertain: the paths were read but the console does not
	// take them as coverage (an older agent, or a timeline a default
	// restore follows that is not established; capRecovery); Reason says
	// why. The paths are kept, capped: none healthy, none with a frontier
	// receipt to draw a covered span to.
	RecoveryUncertain = "uncertain"
)

// Recovery is the agent's observed recovery coverage: from each backup,
// how far contiguous archived WAL reaches, judged from segment names
// only (the agent never reads WAL bytes nor runs a restore).
type Recovery struct {
	State  string
	Reason string
	Paths  []RecoveryPath
	// Complete is false when the collection was cut at a bound.
	Complete bool
	// Stale is set when reading the paths failed and these are the last
	// good ones, of an earlier publication; StaleReason says why.
	Stale       bool
	StaleReason string
	// ScannedAt is when the agent completed the scan these paths come
	// from (the snapshot's completedAt, else when the console fetched
	// it): kept with stale paths, it stays that earlier scan's while the
	// snapshot beside them is fresher. Nil when not known.
	ScannedAt *time.Time
	// CoverageReason is the agent's own coverage roll-up reason, set when
	// that roll-up is not healthy: it says why no path places a restore
	// point better than the console can from the paths alone.
	CoverageReason string
	// TimelineWarning is the agent's timeline warning, which caps no path:
	// a history file that no default restore reads is invalid (the reason
	// names the file).
	TimelineWarning string
	// Dropped says why the last good paths were not kept beside a failed
	// read (KeptDroppedTooOld, KeptDroppedRepositoryChanged); empty when
	// none were dropped.
	Dropped string
}

// Why the last good recovery paths were dropped instead of kept stale
// (keepRecovery).
const (
	// KeptDroppedTooOld: they are more than three refresh intervals (at
	// least an hour) older than the latest scan.
	KeptDroppedTooOld = "too-old"
	// KeptDroppedRepositoryChanged: the repository's identity changed
	// since they were read; they are another repository's.
	KeptDroppedRepositoryChanged = "repository-changed"
)

// RecoveryPath is one backup's observed coverage toward a timeline.
type RecoveryPath struct {
	BackupID       string
	TargetTimeline uint64
	State          evidencev1alpha1.State
	Reason         evidencev1alpha1.Reason
	Stop           evidencev1alpha1.CoverageStop
	// LowerBoundAt is the backup's end: the earliest restore point.
	LowerBoundAt *time.Time
	// StartTimeline is the backup's timeline; FrontierTimeline and
	// FrontierPosition locate the last contiguous WAL segment the path
	// reaches (segments, as BackupItem.EndPosition). Nil for a path the
	// agent could not evaluate.
	StartTimeline    *uint64
	FrontierTimeline *uint64
	FrontierPosition *uint64
	// FrontierReceiptAt is when the last contiguous WAL segment reached
	// the repository: commits in it are older, so it bounds the latest
	// restore point from above.
	FrontierReceiptAt *time.Time
	Assumptions       []string
	// Consistent is the agent's own answer to whether the path reaches
	// its backup's consistency point (pgBackRest paths state it); nil
	// when the console must work it out from positions (Barman).
	Consistent *bool
	// OffTarget is true when the agent proved a default restore
	// (recovery_target_timeline 'latest') does not follow the path: on the
	// backup's own timeline, the newest timeline left the backup behind;
	// on a newer one, that timeline is a sibling of the line a default
	// restore follows. A restore along it must name TargetTimeline.
	OffTarget bool
}

// Reader is the read model's view of the evidence runner.
type Reader interface {
	// ForCluster returns the evidence for a Cluster.
	ForCluster(namespace, cluster string) Result
	// State is the runner's own source state ("evidence").
	State() viewmodel.SourceState
}
