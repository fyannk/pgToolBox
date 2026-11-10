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

package status

import (
	"context"
	"slices"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	cnpgevidence "github.com/fyannk/pgToolBox/pkg/cnpgstatus/evidence"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// sourceEvidence is the evidence runner's source key.
const sourceEvidence = "evidence"

// clusterEvidence is the cross-check as the backups screen uses it.
type clusterEvidence struct {
	view   viewmodel.EvidenceCheck
	source viewmodel.SourceState
	result evidence.Result
}

// evidenceHealth draws an agent state. The agent's vocabulary is its own
// contract (evidence/v1alpha1 State): healthy, warning, unhealthy,
// unknown; anything else is unknown.
func evidenceHealth(s evidencev1alpha1.State) viewmodel.Health {
	switch s {
	case evidencev1alpha1.Healthy:
		return viewmodel.HealthOK
	case evidencev1alpha1.Warning:
		return viewmodel.HealthWarning
	case evidencev1alpha1.Unhealthy:
		return viewmodel.HealthCritical
	default:
		return viewmodel.HealthUnknown
	}
}

func evidenceFact(s evidencev1alpha1.State, r evidencev1alpha1.Reason) viewmodel.EvidenceFact {
	f := viewmodel.EvidenceFact{State: string(s), Health: evidenceHealth(s), Code: bound(r.Code), Message: bound(r.Message)}
	if p, ok := timelinePhrase(f.Message); ok {
		f.Phrase = &p
	}
	return f
}

// utc is a time in UTC, or nil.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func u64(p *uint64) *int64 {
	if p == nil {
		return nil
	}
	v := u64v(*p)
	return &v
}

// u64v is u64 for a present value.
func u64v(v uint64) int64 { return int64(min(v, uint64(1<<62))) }

// retentionNotEvaluated is the code of a pgBackRest retention the agent
// had no expectation to compare with; the message is the agent's own.
const retentionNotEvaluated = "not-evaluated"

// walCountFacts is the format-neutral projection of a format's WAL counts
// (the two wire types have the same fields).
func walCountFacts[C evidencev1alpha1.BarmanWALCounts | evidencev1alpha1.PGBackRestWALCounts](c *C) *cnpgevidence.WALCountFacts {
	if c == nil {
		return nil
	}
	f := cnpgevidence.WALCountFacts(*c)
	return &f
}

// evidenceFor reads the cross-check of a Cluster.
func (s *Service) evidenceFor(_ context.Context, g *gathered) clusterEvidence {
	out := clusterEvidence{view: viewmodel.EvidenceCheck{Health: viewmodel.HealthUnknown, Provenance: viewmodel.Provenance{Origin: viewmodel.OriginEvidence}}}
	reader := s.opts.Evidence
	if reader == nil {
		out.source = viewmodel.SourceState{Source: sourceEvidence, State: viewmodel.SourceDisabled, Reason: "the console reads no repository evidence"}
		out.view.State, out.view.Health, out.view.Reason = viewmodel.EvidenceDisabled, viewmodel.HealthInactive, out.source.Reason
		return out
	}
	out.source = reader.State()
	cfg := cnpgstatus.Backups(g.cluster)
	r := reader.ForCluster(g.key.Namespace, g.key.Name)
	out.result = r
	out.view.Agent, out.view.State, out.view.Reason = r.Agent, r.State, bound(r.Reason)
	if r.Agent == "" && !cfg.Configured {
		out.view.State, out.view.Health = viewmodel.EvidenceNotConfigured, viewmodel.HealthInfo
		out.view.Reason = "the Cluster declares no backup method an evidence agent could read"
		return out
	}
	switch r.State {
	case viewmodel.EvidenceNoAgent:
		// A capability not installed, not a fault.
		out.view.Health = viewmodel.HealthInfo
		if out.view.Reason == "" {
			out.view.Reason = "no EvidenceAgent in the namespace names this Cluster"
		}
	case viewmodel.EvidencePending:
		out.view.Health = viewmodel.HealthProgressing
	case viewmodel.EvidenceUnavailable:
		out.view.Health = viewmodel.HealthUnknown
	case viewmodel.EvidenceNotConfigured, viewmodel.EvidenceLive, viewmodel.EvidenceStale, viewmodel.EvidenceDisabled:
		// Live and stale are judged from the repository below; not
		// configured and disabled carry no health of their own.
	}
	if r.Agent != "" {
		out.view.Provenance.Source = "EvidenceAgent " + r.Agent + " " + evidencev1alpha1.SnapshotPath
	}
	out.view.Provenance.ObservedAt = r.ObservedAt
	if r.State != viewmodel.EvidenceLive {
		out.view.Provenance.Stale = r.Snapshot != nil
		out.view.Provenance.StaleReason = out.view.Reason
	}
	snap := r.Snapshot
	if snap == nil {
		return out
	}
	repo := &viewmodel.RepositoryEvidence{
		Completeness: string(snap.Completeness), AgentStale: snap.Stale,
		Overall:     evidenceFact(snap.State, snap.Reason),
		CompletedAt: snap.CompletedAt,
	}
	if snap.Stale && r.State == viewmodel.EvidenceLive {
		out.view.State = viewmodel.EvidenceStale
	}
	if b := snap.Details.BarmanCloud; b != nil {
		repo.WAL = evidenceFact(b.WAL.State, b.WAL.Reason)
		repo.Timeline = evidenceFact(b.Timeline.State, b.Timeline.Reason)
		repo.Coverage = evidenceFact(b.Coverage.State, b.Coverage.Reason)
		repo.Retention = evidenceFact(b.Retention.State, b.Retention.Reason)
		repo.Backups, repo.StructurallyUsable = u64(b.BackupItems), u64(b.StructurallyUsableBackups)
		repo.OldestCompletionAt, repo.NewestCompletionAt = b.Retention.OldestCompletionAt, b.Retention.NewestCompletionAt
		repo.LatestArchiveReceiptAt = b.LatestArchiveReceiptAt
		repo.Truncated = b.RangesTruncated || b.DiagnosticsTruncated || !r.BackupsComplete
	} else if p := snap.Details.PGBackRest; p != nil {
		repo.WAL = evidenceFact(p.WAL.State, p.WAL.Reason)
		repo.Timeline = evidenceFact(p.Timeline.State, p.Timeline.Reason)
		repo.Coverage = evidenceFact(p.Coverage.State, p.Coverage.Reason)
		repo.Retention = viewmodel.EvidenceFact{
			State: string(evidencev1alpha1.Unknown), Health: viewmodel.HealthUnknown, Code: retentionNotEvaluated,
			Message: "no retention expectation is configured",
		}
		if retention := p.Retention; retention != nil {
			repo.Retention = evidenceFact(retention.State, retention.Reason)
		}
		repo.Backups, repo.StructurallyUsable = u64(p.BackupItems), u64(p.UsableBackups)
		// For pgBackRest the "oldest" time is the recovery window's start:
		// the stop time of the oldest usable backup that reaches the newest
		// WAL frontier, not the oldest backup in the stanza.
		if w := p.RecoveryWindow; w != nil {
			start, newest := w.StartAt, w.NewestStopAt
			repo.OldestCompletionAt, repo.NewestCompletionAt = &start, &newest
		}
		repo.LatestArchiveReceiptAt = p.LatestArchiveReceiptAt
		repo.Truncated = p.RangesTruncated || p.DiagnosticsTruncated || !r.BackupsComplete
		repo.PgBackRest = pgbackrestEvidence(*p, r.Backups, g.backups)
	} else {
		repo.WAL, repo.Timeline, repo.Coverage, repo.Retention = unknownFact(), unknownFact(), unknownFact(), unknownFact()
		repo.Truncated = !r.BackupsComplete
	}
	if r.BackupsComplete {
		known := map[string]bool{}
		// A Backup of an earlier Cluster of the same name is still a
		// Backup object: its repository entry is not repository-only.
		for _, b := range slices.Concat(g.backups, g.priorBackups) {
			if id := backupKey(b); id != "" {
				known[id] = true
			}
		}
		only := 0
		for id := range r.BackupIDs {
			if !known[id] {
				only++
			}
		}
		repo.RepositoryOnly = &only
	}
	out.view.Repository = repo
	switch out.view.State {
	case viewmodel.EvidenceLive:
		out.view.Health = repo.Overall.Health
	case viewmodel.EvidenceStale:
		// The retained answer is shown, but its judgement is not current.
		out.view.Health = viewmodel.HealthUnknown
	case viewmodel.EvidenceNotConfigured, viewmodel.EvidenceNoAgent, viewmodel.EvidencePending, viewmodel.EvidenceUnavailable, viewmodel.EvidenceDisabled:
		// No repository answer to judge: the health set above stands.
	}
	return out
}

func unknownFact() viewmodel.EvidenceFact {
	return viewmodel.EvidenceFact{State: string(evidencev1alpha1.Unknown), Health: viewmodel.HealthUnknown}
}

// presence is a Backup object's presence in the repository: present when
// its repository key (backupKeyOf) is listed, absent only from a complete
// list of a live or stale snapshot, unknown otherwise; a backup without a
// key (not completed, or a volume snapshot) is not checked.
func (e clusterEvidence) presence(r cnpgstatus.BackupObjectReading) viewmodel.RepositoryPresence {
	key := backupKeyOf(r)
	switch {
	case key == "" || r.Method == "volumeSnapshot":
		return viewmodel.RepositoryNotChecked
	case e.result.Snapshot == nil:
		return viewmodel.RepositoryUnknown
	case e.result.BackupIDs[key]:
		return viewmodel.RepositoryPresent
	case e.result.BackupsComplete && r.StoppedAt != nil && e.result.Snapshot.CompletedAt != nil &&
		r.StoppedAt.Before(*e.result.Snapshot.CompletedAt):
		// Absent only from a complete list scanned after the backup
		// finished; a backup newer than the scan is not judged.
		return viewmodel.RepositoryAbsent
	default:
		return viewmodel.RepositoryUnknown
	}
}

// backupKeyOf is the key a Backup object's backup has in the repository
// evidence (cnpgstatus.PgBackRestBackupKey).
func backupKeyOf(r cnpgstatus.BackupObjectReading) string {
	return cnpgstatus.PgBackRestBackupKey(r.PluginName, r.BackupID, r.BackupName)
}

// backupKey is backupKeyOf of a Backup object.
func backupKey(b *cnpgv1.Backup) string {
	name := ""
	if b.Spec.PluginConfiguration != nil {
		name = b.Spec.PluginConfiguration.Name
	}
	return backupKeyOf(cnpgstatus.BackupObjectReading{PluginName: name, BackupID: b.Status.BackupID, BackupName: b.Status.BackupName})
}

// pgbackrestRef is the pgBackRest backup a Backup object took: the item
// its label names, else (Opera) the item whose cnpg-backup-name
// annotation is the object's name. Nil when neither matches or the
// evidence holds no pgBackRest items.
func (e clusterEvidence) pgbackrestRef(name string, r cnpgstatus.BackupObjectReading) *viewmodel.PgBackRestBackupRef {
	key := backupKeyOf(r)
	for i := range e.result.Backups {
		item := &e.result.Backups[i]
		p := item.PgBackRest
		if p == nil {
			continue
		}
		if (key != "" && item.ID == key) || (p.CNPGBackupName != nil && *p.CNPGBackupName == name) {
			usable := p.Usable
			ref := &viewmodel.PgBackRestBackupRef{Label: item.ID, Type: p.Type, Full: fullLabel(item.ID, p), Usable: &usable, Stanza: p.Stanza}
			if p.Prior != nil {
				ref.Prior = *p.Prior
			}
			return ref
		}
	}
	return nil
}

// fullLabel is the full backup heading a pgBackRest backup's chain: the
// label itself for a full, the first 16 characters of a differential or
// incremental label (<full>_<stamp>{D,I}).
func fullLabel(label string, p *evidence.PgBackRestItem) string {
	if p.Type == evidencev1alpha1.PGBackRestFull || len(label) < 16 {
		return label
	}
	return label[:16]
}

// maximumPgBackRestBackups bounds the repository backups listed on the
// Backups screen (the evidence holds up to a thousand).
const maximumPgBackRestBackups = 100

// pgbackrestEvidence is the pgBackRest part of the repository summary:
// the stanza, the catalog fact, the agent's window and findings, and the
// newest backups with their chains, each joined to the Backup object
// that took it.
func pgbackrestEvidence(p evidencev1alpha1.PGBackRestSummary, items []evidence.BackupItem, objects []*cnpgv1.Backup) *viewmodel.PgBackRestEvidence {
	out := &viewmodel.PgBackRestEvidence{
		Stanza: bound(p.Stanza), ArchiveStanza: bound(p.ArchiveStanza), Repository: int64(min(p.Repository, 1<<16)),
		Encrypted: p.Encrypted, Catalog: evidenceFact(p.Catalog.State, p.Catalog.Reason),
		FindingsTruncated: p.FindingsTruncated, Findings: []viewmodel.PgBackRestFinding{}, Backups: []viewmodel.PgBackRestRepoBackup{},
	}
	if p.RepositoryFormat != nil {
		out.RepositoryFormat = u64(p.RepositoryFormat)
	}
	if p.PGBackRestVersion != nil {
		out.PgBackRestVersion = bound(*p.PGBackRestVersion)
	}
	if r := p.Retention; r != nil {
		out.Retention = &viewmodel.PgBackRestRetention{
			Configured: r.Configured, Full: u64(r.Full), FullType: r.FullType, Diff: u64(r.Diff),
			Archive: u64(r.Archive), ArchiveType: r.ArchiveType, EffectiveArchive: u64(r.EffectiveArchive),
			FullBackups: u64v(r.FullBackups), DifferentialBackups: u64v(r.DifferentialBackups), IncrementalBackups: u64v(r.IncrementalBackups),
			EvaluatedAt: utc(r.EvaluatedAt), WindowStartAt: utc(r.WindowStartAt),
			Result: evidenceFact(r.State, r.Reason),
		}
	}
	if w := p.RecoveryWindow; w != nil {
		out.RecoveryWindow = &viewmodel.PgBackRestRecoveryWindow{
			OldestBackup: w.OldestBackup, StartAt: w.StartAt.UTC(), NewestBackup: w.NewestBackup, NewestStopAt: w.NewestStopAt.UTC(),
			EndTimeline: u64v(w.EndTimeline), EndWAL: w.EndWAL, EndReceiptAt: w.EndReceiptAt, UsableBackups: u64v(w.UsableBackups),
			Assumptions: append([]string{}, w.AssumptionCodes...),
		}
	}
	for _, f := range p.Findings {
		health := viewmodel.HealthWarning
		if f.Severity == evidencev1alpha1.FindingError {
			health = viewmodel.HealthCritical
		}
		out.Findings = append(out.Findings, viewmodel.PgBackRestFinding{Code: f.Code, Severity: f.Severity, Health: health, Subject: bound(f.Subject), Message: bound(f.Message)})
	}
	byKey, byName := map[string]string{}, map[string]bool{}
	for _, b := range objects {
		if key := backupKey(b); key != "" {
			byKey[key] = b.Name
		}
		byName[b.Name] = true
	}
	// items are newest last; the screen lists newest first.
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		pb := item.PgBackRest
		if pb == nil {
			continue
		}
		if len(out.Backups) == maximumPgBackRestBackups {
			out.BackupsTruncated = true
			break
		}
		row := viewmodel.PgBackRestRepoBackup{
			Label: item.ID, Type: pb.Type, Full: fullLabel(item.ID, pb), References: len(pb.References),
			Usable: pb.Usable, UnusableReasons: append([]string{}, pb.UnusableReasons...),
			State: string(item.State), Health: evidenceHealth(item.State), Failed: item.Failed,
			StartedAt: item.BeginAt, StoppedAt: item.EndAt, Timeline: u64(pb.Timeline),
			SizeBytes: u64v(pb.SizeBytes), RepositorySizeBytes: u64v(pb.RepositorySizeBytes),
			BackupName: byKey[item.ID],
		}
		if row.BackupName == "" && pb.CNPGBackupName != nil && byName[*pb.CNPGBackupName] {
			row.BackupName = *pb.CNPGBackupName
		}
		if pb.Prior != nil {
			row.Prior = *pb.Prior
		}
		out.Backups = append(out.Backups, row)
	}
	return out
}

// pgbackrestMethod is the pgBackRest method of the Backups screen: the
// spec's plugin and object, the stanza and repository the evidence
// agent reports (the spec names Dalibo's stanza only through its Stanza
// object, which the console does not read).
func pgbackrestMethod(cfg *cnpgstatus.PgBackRestConfig, snap *evidencev1alpha1.RepositoryEvidenceSnapshot) *viewmodel.PgBackRestMethod {
	if cfg == nil {
		return nil
	}
	out := &viewmodel.PgBackRestMethod{
		Plugin: cfg.Plugin, Flavor: cfg.Flavor, Display: cfg.Display, ObjectKind: cfg.ObjectKind, ObjectName: cfg.ObjectName, Stanza: cfg.Stanza,
	}
	if snap == nil || snap.Details.PGBackRest == nil {
		return out
	}
	p := snap.Details.PGBackRest
	out.Stanza = bound(p.Stanza)
	if p.ArchiveStanza != p.Stanza {
		out.ArchiveStanza = bound(p.ArchiveStanza)
	}
	repository := int64(min(p.Repository, 1<<16))
	out.Repository, out.Encrypted = &repository, p.Encrypted
	return out
}

// evidenceStatus projects the agent channel's result onto the evidence
// status the diagnostics catalog reads (pkg/cnpgstatus/evidence). A Cluster without an agent, or a
// console reading no evidence, feeds nothing: the repository checks say
// the consumer is not configured, which is the truth.
func evidenceStatus(r evidence.Result) (cnpgevidence.Status, bool) {
	switch r.State {
	case viewmodel.EvidenceNoAgent, viewmodel.EvidenceNotConfigured, viewmodel.EvidenceDisabled, "":
		return cnpgevidence.Status{}, false
	case viewmodel.EvidencePending, viewmodel.EvidenceUnavailable, viewmodel.EvidenceLive, viewmodel.EvidenceStale:
		// An agent answers, or should: projected below.
	}
	st := cnpgevidence.Status{}
	if r.State == viewmodel.EvidenceUnavailable || r.State == viewmodel.EvidenceStale {
		st.Failure = cnpgevidence.FailureUnavailable
	}
	if r.Snapshot == nil {
		return st, true
	}
	snap := cnpgevidence.Snapshot{
		Report: projectReport(*r.Snapshot), Stale: r.State == viewmodel.EvidenceStale, Failure: st.Failure,
		BackupsTruncated: !r.BackupsComplete,
	}
	if r.ObservedAt != nil {
		snap.ObservedAt = *r.ObservedAt
	}
	st.HasReport, st.Snapshot = true, snap
	return st, true
}

// projectReport reduces an evidence/v1alpha1 snapshot to the report the
// catalog reads, field for field as the v1alpha1 projection does.
func projectReport(s evidencev1alpha1.RepositoryEvidenceSnapshot) cnpgevidence.Report {
	report := cnpgevidence.Report{
		ProducerVersion: s.Producer.Version, ClusterNamespace: s.Identity.Cluster.Namespace, ClusterUID: s.Identity.Cluster.UID,
		Provider: s.Identity.Repository.Provider, Format: s.Identity.Repository.Format,
		ScopeKind: s.Identity.Repository.Scope.Kind, ScopeName: s.Identity.Repository.Scope.Name,
		Fingerprint: s.Identity.Repository.DestinationFingerprint, ProducerInstance: s.ProducerInstance,
		Revision: s.Revision, EvidenceGeneration: s.EvidenceGeneration,
		StartedAt: s.StartedAt, CompletedAt: s.CompletedAt, LastAttemptAt: s.LastAttemptAt,
		Completeness: string(s.Completeness), SourceStale: s.Stale,
		Overall:     cnpgevidence.StateFact{State: string(s.State), Code: s.Reason.Code},
		DetailsType: s.Details.Type,
		Inventory: cnpgevidence.InventoryFacts{
			Known: s.Inventory.Known, ObjectCount: s.Inventory.ObjectCount, StoredBytes: s.Inventory.StoredBytes,
			UnscopedObjectCount: s.Inventory.UnscopedObjectCount, PagesExamined: s.Inventory.PagesExamined,
			ObjectsExamined: s.Inventory.ObjectsExamined,
		},
	}
	if s.Identity.Cluster.Name != nil {
		report.ClusterName = *s.Identity.Cluster.Name
	}
	if s.Inventory.LastFailureCategory != nil {
		report.Inventory.LastFailureCategory = string(*s.Inventory.LastFailureCategory)
	}
	for _, c := range s.Capabilities {
		report.Capabilities = append(report.Capabilities, cnpgevidence.CapabilityFact{
			ID: string(c.ID), Support: string(c.Support), State: string(c.State), Code: c.Reason.Code,
		})
	}
	switch {
	case s.Details.BarmanCloud != nil:
		report.Facts = barmanFacts(*s.Details.BarmanCloud)
	case s.Details.PGBackRest != nil:
		report.Facts = pgbackrestFacts(*s.Details.PGBackRest)
	}
	return report
}

func stateFact(r evidencev1alpha1.StateReason) cnpgevidence.StateFact {
	return cnpgevidence.StateFact{State: string(r.State), Code: r.Reason.Code}
}

func stateCounts(c *evidencev1alpha1.StateCounts) *cnpgevidence.StateCountFacts {
	if c == nil {
		return nil
	}
	return &cnpgevidence.StateCountFacts{Healthy: c.Healthy, Warning: c.Warning, Unhealthy: c.Unhealthy, Unknown: c.Unknown}
}

// barmanFacts is the format-neutral projection of a Barman summary.
func barmanFacts(b evidencev1alpha1.BarmanCloudSummary) *cnpgevidence.RepositoryFacts {
	f := &cnpgevidence.RepositoryFacts{
		Format:      cnpgevidence.FormatBarmanCloud,
		BackupItems: b.BackupItems, WALRangeItems: b.WALRangeItems, WALGapItems: b.WALGapItems,
		RecoveryPathItems: b.RecoveryPathItems, UsableBackups: b.StructurallyUsableBackups,
		BackupStates: stateCounts(b.BackupStates),
		WAL:          stateFact(b.WAL), Timeline: stateFact(b.Timeline), Coverage: stateFact(b.Coverage),
		Retention: &cnpgevidence.RetentionFacts{
			VisibleBackups: b.Retention.VisibleBackups, StructurallyUsableBackups: b.Retention.StructurallyUsableBackups,
			OldestCompletionAt: b.Retention.OldestCompletionAt, NewestCompletionAt: b.Retention.NewestCompletionAt,
			MinimumConfigured: b.Retention.MinimumConfigured, MinimumRedundancy: b.Retention.MinimumRedundancy,
			Result: cnpgevidence.StateFact{State: string(b.Retention.State), Code: b.Retention.Reason.Code},
		},
		OldestBackupAt: b.Retention.OldestCompletionAt, NewestBackupAt: b.Retention.NewestCompletionAt,
		LatestArchiveReceiptAt: b.LatestArchiveReceiptAt, RangesTruncated: b.RangesTruncated, DiagnosticsTruncated: b.DiagnosticsTruncated,
	}
	f.WALCounts = walCountFacts(b.WALCounts)
	return f
}

// pgbackrestFacts is the format-neutral projection of a pgBackRest
// summary: the catalog's integrity has its own fact, retention is present
// only when the agent compared the stanza with a retention, and the
// agent's recovery window and findings come along.
func pgbackrestFacts(p evidencev1alpha1.PGBackRestSummary) *cnpgevidence.RepositoryFacts {
	catalog := stateFact(p.Catalog)
	f := &cnpgevidence.RepositoryFacts{
		Format:      cnpgevidence.FormatPGBackRest,
		BackupItems: p.BackupItems, WALRangeItems: p.WALRangeItems, WALGapItems: p.WALGapItems,
		RecoveryPathItems: p.RecoveryPathItems, UsableBackups: p.UsableBackups,
		BackupStates: stateCounts(p.BackupStates), Catalog: &catalog,
		WAL: stateFact(p.WAL), Timeline: stateFact(p.Timeline), Coverage: stateFact(p.Coverage),
		LatestArchiveReceiptAt: p.LatestArchiveReceiptAt, RangesTruncated: p.RangesTruncated, DiagnosticsTruncated: p.DiagnosticsTruncated,
		Findings: []cnpgevidence.FindingFact{}, FindingsTruncated: p.FindingsTruncated,
	}
	f.WALCounts = walCountFacts(p.WALCounts)
	// OldestBackupAt (and the retention's OldestCompletionAt below) is the
	// recovery window's start, the stop time of the oldest usable backup
	// that reaches the newest frontier, not the oldest backup: an older
	// backup that cannot reach the frontier does not widen the window.
	if w := p.RecoveryWindow; w != nil {
		start, newest := w.StartAt, w.NewestStopAt
		f.OldestBackupAt, f.NewestBackupAt = &start, &newest
		f.RecoveryWindow = &cnpgevidence.RecoveryWindowFacts{
			OldestBackup: w.OldestBackup, NewestBackup: w.NewestBackup, StartAt: w.StartAt, NewestStopAt: w.NewestStopAt,
			EndTimeline: w.EndTimeline, EndWAL: w.EndWAL, EndReceiptAt: w.EndReceiptAt, UsableBackups: w.UsableBackups,
		}
	}
	for _, finding := range p.Findings {
		f.Findings = append(f.Findings, cnpgevidence.FindingFact{Code: finding.Code, Severity: finding.Severity, Subject: finding.Subject})
	}
	if r := p.Retention; r != nil {
		visible := r.FullBackups + r.DifferentialBackups + r.IncrementalBackups
		f.Retention = &cnpgevidence.RetentionFacts{
			VisibleBackups: &visible, StructurallyUsableBackups: p.UsableBackups,
			OldestCompletionAt: f.OldestBackupAt, NewestCompletionAt: r.EvaluatedAt,
			MinimumConfigured: r.Configured,
			Result:            cnpgevidence.StateFact{State: string(r.State), Code: r.Reason.Code},
			PGBackRest: &cnpgevidence.PGBackRestRetentionFacts{
				Configured: r.Configured, Full: r.Full, FullType: r.FullType, Diff: r.Diff,
				Archive: r.Archive, ArchiveType: r.ArchiveType, EffectiveArchive: r.EffectiveArchive,
				FullBackups: r.FullBackups, DifferentialBackups: r.DifferentialBackups, IncrementalBackups: r.IncrementalBackups,
				EvaluatedAt: r.EvaluatedAt, WindowStartAt: r.WindowStartAt,
			},
		}
	}
	return f
}
