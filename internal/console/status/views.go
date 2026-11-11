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
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/diag"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// maxRowContributors is how many contributors a list row carries.
const maxRowContributors = 3

// maxText bounds operator-provided text the views carry.
const maxText = 1024

func bound(s string) string {
	if len(s) > maxText {
		return s[:maxText] + "…"
	}
	return s
}

func (s *Service) latestRun(key cache.ClusterKey) *diag.Run {
	d := s.diagnostics()
	if d == nil {
		return nil
	}
	if run, ok := d.Latest(key); ok {
		return &run
	}
	return nil
}

// ClusterList is the "/" screen: every watched Cluster, before the API
// filters it by what the caller may get.
func (s *Service) ClusterList(ctx context.Context) (viewmodel.ClusterList, error) {
	out := viewmodel.ClusterList{GeneratedAt: s.now().UTC(), Items: []viewmodel.ClusterRow{}}
	keys, failed := s.keysRead(ctx)
	for _, ns := range s.cache.Namespaces() {
		state := []viewmodel.SourceState{s.cache.Source(ns, cache.SourceClusters)}
		if failed[ns] {
			// The informer may say live while its list failed: the
			// namespace's rows are the last known ones, not a reading.
			state = unread(state, cache.SourceClusters)
		}
		out.Namespaces = append(out.Namespaces, viewmodel.NamespaceState{Name: ns, Sources: state})
	}
	sources := map[string]string{}
	for _, key := range keys {
		if _, ok := sources[key.Namespace]; !ok {
			sources[key.Namespace] = s.namespaceSources(key.Namespace)
		}
		// The stamp is read before the cache: a change landing in
		// between moves the generation on, and the next list rebuilds.
		stamp, reusable := s.rowStamp(key, sources[key.Namespace])
		now := s.now()
		if reusable {
			if row, ok := s.rows.get(key, stamp, now); ok {
				out.Items = append(out.Items, row)
				continue
			}
		}
		g, err := s.gather(ctx, key, false)
		if errors.Is(err, ErrNotFound) {
			// Deleted between the list and the read.
			continue
		}
		if err != nil {
			// The Cluster was listed but could not be read: it keeps
			// its row, unknown, and its namespace says it was not read.
			out.Items = append(out.Items, s.unreadRow(key))
			for i := range out.Namespaces {
				if out.Namespaces[i].Name == key.Namespace {
					out.Namespaces[i].Sources = unread(out.Namespaces[i].Sources, cache.SourceClusters)
				}
			}
			continue
		}
		row := s.row(g)
		if reusable {
			// The memoized row carries its JSON, so a list request
			// copies the bytes of an unchanged row instead of encoding
			// it again (the encoding was most of a fleet list's CPU).
			// It is the row's content only: authorization filters
			// whole rows afterwards, per caller.
			row = row.WithJSON()
			s.rows.put(key, stamp, now, row)
		}
		out.Items = append(out.Items, row)
	}
	s.rows.keep(keys)
	return out, nil
}

// unreadRow is the row of a Cluster the cache listed but failed to
// read: every judgement unknown, never dropped from the list.
func (s *Service) unreadRow(key cache.ClusterKey) viewmodel.ClusterRow {
	prov := viewmodel.Provenance{
		Origin: viewmodel.OriginKubernetes, Source: "Cluster", Stale: true,
		StaleReason: "the console's cache could not be read for this Cluster; retry",
	}
	return viewmodel.ClusterRow{
		Ref: viewmodel.ClusterRef{Namespace: key.Namespace, Name: key.Name},
		Verdict: viewmodel.Verdict{Health: viewmodel.HealthUnknown, Stale: true, Contributors: []viewmodel.Contributor{{
			ID: "source:" + cache.SourceClusters, Health: viewmodel.HealthUnknown, Label: "Source " + cache.SourceClusters,
			Detail: string(viewmodel.SourceUnavailable) + reasonSuffix(prov.StaleReason), Provenance: prov,
		}}},
		Phase:      viewmodel.Phase{Health: viewmodel.HealthUnknown, Provenance: prov},
		Primary:    viewmodel.PrimarySummary{State: viewmodel.PrimaryUnknown},
		Instances:  viewmodel.InstanceCounts{Health: viewmodel.HealthUnknown},
		Backup:     viewmodel.BackupRecency{Health: viewmodel.HealthUnknown, Provenance: prov},
		Findings:   findingCounts(s.latestRun(key)),
		Version:    viewmodel.VersionInfo{Confidence: viewmodel.VersionUnknown},
		Provenance: prov,
	}
}

func (s *Service) row(g *gathered) viewmodel.ClusterRow {
	run := s.latestRun(g.key)
	return s.rowWith(g, run, s.verdict(g, run))
}

// rowWith builds the row from a verdict already computed; the row keeps
// only its first contributors.
func (s *Service) rowWith(g *gathered, run *diag.Run, verdict viewmodel.Verdict) viewmodel.ClusterRow {
	if len(verdict.Contributors) > maxRowContributors {
		verdict.Contributors = verdict.Contributors[:maxRowContributors]
	}
	now := s.now().UTC()
	return viewmodel.ClusterRow{
		Ref:        ref(g.cluster),
		Labels:     maps.Clone(g.cluster.Labels),
		Verdict:    verdict,
		Phase:      s.phase(g),
		Primary:    primarySummary(g.cluster),
		Instances:  s.instanceCounts(g),
		Backup:     s.backupRecency(g),
		Findings:   findingCounts(run),
		Version:    versionInfo(g, run),
		Provenance: s.provenance(g.key.Namespace, cache.SourceClusters, viewmodel.OriginOperator, "Cluster", now),
	}
}

func ref(c *cnpgv1.Cluster) viewmodel.ClusterRef {
	return viewmodel.ClusterRef{Namespace: c.Namespace, Name: c.Name, UID: string(c.UID)}
}

func (s *Service) phase(g *gathered) viewmodel.Phase {
	reading := cnpgstatus.ClassifyPhase(g.phaseMinor, g.cluster.Status.Phase)
	return viewmodel.Phase{
		Text:       bound(g.cluster.Status.Phase),
		Reason:     bound(g.cluster.Status.PhaseReason),
		Health:     health(reading.Class),
		Known:      reading.Known,
		Cap:        tableCap("phase", g.phaseMinor, reading.Reason),
		Since:      s.phaseSince(g),
		Provenance: s.provenance(g.key.Namespace, cache.SourceClusters, viewmodel.OriginOperator, "Cluster.status.phase", s.now().UTC()),
	}
}

// tableCap says why a phase or condition reading of kind ("phase",
// "condition") was capped below its table's class; nil when it was not.
// The minor named is the one the version detection gave, or, when none
// was detected, the table the reading was made against.
func tableCap(kind string, minor cnpgstatus.Minor, reason string) *viewmodel.Phrase {
	if reason == "" {
		return nil
	}
	if cnpgstatus.NamesMinor(minor) {
		p := say(kind+".cap.unverifiedMinor", reason, map[string]string{"minor": string(minor)})
		return &p
	}
	p := say(kind+".cap.undetectedMinor", reason, map[string]string{"table": string(cnpgstatus.TableMinor(minor))})
	return &p
}

func primarySummary(c *cnpgv1.Cluster) viewmodel.PrimarySummary {
	p := cnpgstatus.Primary(c)
	return viewmodel.PrimarySummary{Current: p.Current, Target: p.Target, State: viewmodel.PrimaryState(p.State), Since: p.Since}
}

func versionInfo(g *gathered, run *diag.Run) viewmodel.VersionInfo {
	v := viewmodel.VersionInfo{
		CNPG:       g.version.Version,
		CNPGMinor:  string(g.version.Minor),
		Confidence: viewmodel.VersionConfidence(g.version.Confidence),
		DetectedBy: g.version.DetectedBy,
		Image:      g.cluster.Status.Image,
		// UpgradeInFlight: the instance managers and the operator's
		// cnpg.io/operatorVersion annotation disagree.
		UpgradeInFlight: g.upgradeInFlight,
	}
	if v.Confidence == "" {
		v.Confidence = viewmodel.VersionUnknown
	}
	if major := g.cluster.Status.PGDataImageInfo; major != nil && major.MajorVersion > 0 {
		m := major.MajorVersion
		v.PostgresMajor = &m
	}
	v.Components = componentVersions(run, g.cluster.Status.Image)
	return v
}

// endingSoon is how far ahead a line's end of life marks it.
const endingSoon = 180 * 24 * time.Hour

// componentVersions states the versions the last diagnostics run saw and
// places each in the lifecycle table that run used, so the overview and
// the findings never disagree on a date.
func componentVersions(run *diag.Run, image string) []viewmodel.ComponentVersion {
	if run == nil {
		return nil
	}
	facts := diagnose.Versions(run.Input)
	var out []viewmodel.ComponentVersion
	for _, c := range []diagnose.Component{diagnose.ComponentKubernetes, diagnose.ComponentCNPG, diagnose.ComponentBarman, diagnose.ComponentPostgreSQL} {
		f, ok := facts[c]
		if !ok {
			continue
		}
		version := f.Version
		// The Cluster reports the major only; its image tag usually says the minor too.
		if tag, ok := diagnose.ImageTagVersion(image); ok && c == diagnose.ComponentPostgreSQL && strings.HasPrefix(tag, version+".") {
			version = tag
		}
		cv := viewmodel.ComponentVersion{Component: string(c), Version: version, Source: f.Object, Support: viewmodel.SupportUnlisted}
		s := run.Input.Lifecycle.Status(c, f.Version)
		if s.HasEOL {
			cv.EndsOn = s.EOL.Format(time.DateOnly)
		}
		switch {
		case !s.Listed:
		case s.Past(run.At):
			cv.Support = viewmodel.SupportEnded
		case s.HasEOL && s.EOL.Sub(run.At) <= endingSoon:
			cv.Support = viewmodel.SupportEndingSoon
		default:
			cv.Support = viewmodel.SupportSupported
		}
		out = append(out, cv)
	}
	return out
}

// backupRecency is the newest completed and failed Backup of the Cluster.
func (s *Service) backupRecency(g *gathered) viewmodel.BackupRecency {
	cfg := cnpgstatus.Backups(g.cluster)
	r := viewmodel.BackupRecency{
		Configured:  cfg.Configured || len(g.scheduled) > 0,
		PluginBased: cfg.PluginBased,
		Provenance:  s.provenance(g.key.Namespace, cache.SourceBackups, viewmodel.OriginOperator, "Backup.status", s.now().UTC()),
	}
	if !g.listed(cache.SourceBackups) {
		r.Health = viewmodel.HealthUnknown
		return r
	}
	for _, b := range g.backups {
		stopped := b.Status.StoppedAt
		switch b.Status.Phase {
		case cnpgv1.BackupPhaseCompleted:
			if stopped != nil && (r.LastCompletedAt == nil || stopped.After(*r.LastCompletedAt)) {
				t := stopped.UTC()
				r.LastCompletedAt = &t
			}
		case cnpgv1.BackupPhaseFailed:
			at := b.CreationTimestamp.UTC()
			if stopped != nil {
				at = stopped.UTC()
			}
			if r.LastFailedAt == nil || at.After(*r.LastFailedAt) {
				r.LastFailedAt = &at
			}
		}
	}
	if r.LastFailedAt != nil && r.LastCompletedAt != nil && !r.LastFailedAt.After(*r.LastCompletedAt) {
		r.LastFailedAt = nil
	}
	if r.Configured {
		r.Cadence = backupCadence(g, s.now().UTC(), r.LastCompletedAt)
	}
	switch {
	case !r.Configured:
		r.Health = viewmodel.HealthInfo
	case r.LastFailedAt != nil:
		r.Health = viewmodel.HealthWarning
	case r.LastCompletedAt == nil:
		r.Health = viewmodel.HealthWarning
	case r.Cadence != nil && r.Cadence.Overdue:
		r.Health = viewmodel.HealthWarning
	default:
		r.Health = viewmodel.HealthOK
	}
	return r
}

// defaultBackupThreshold is the age past which a last completed backup
// is overdue when no schedule period is known: a week, conservative
// enough for a weekly full, so a configured Cluster is never "ok"
// forever on one old backup.
const defaultBackupThreshold = 7 * 24 * time.Hour

// backupCadence derives the overdue threshold. The period is the
// operator's own: ScheduledBackup.status.nextScheduleTime minus
// lastScheduleTime (CNPG sets both on each run: scheduledbackup_controller.go
// v1.30.0 and v1.30.1:389-390; the first reconcile sets only the next one,
// line 306, so the period is unknown until a run), the shortest over the
// active schedules. Twice the period absorbs one late or retried run.
func backupCadence(g *gathered, now time.Time, last *time.Time) *viewmodel.BackupCadence {
	var period time.Duration
	for _, sb := range g.scheduled {
		if (sb.Spec.Suspend != nil && *sb.Spec.Suspend) || sb.Status.LastScheduleTime == nil || sb.Status.NextScheduleTime == nil {
			continue
		}
		d := sb.Status.NextScheduleTime.Sub(sb.Status.LastScheduleTime.Time)
		if d > 0 && (period == 0 || d < period) {
			period = d
		}
	}
	c := &viewmodel.BackupCadence{Basis: "default", ThresholdSeconds: int64(defaultBackupThreshold / time.Second)}
	if period > 0 {
		threshold := max(2*period, period+time.Hour)
		c = &viewmodel.BackupCadence{Basis: "schedule", IntervalSeconds: int64(period / time.Second), ThresholdSeconds: int64(threshold / time.Second)}
	}
	threshold := time.Duration(c.ThresholdSeconds) * time.Second
	switch {
	case last != nil:
		c.Overdue = now.Sub(*last) > threshold
	case !g.cluster.CreationTimestamp.IsZero():
		c.Overdue = now.Sub(g.cluster.CreationTimestamp.Time) > threshold
	}
	return c
}

func backupDetail(r viewmodel.BackupRecency, now time.Time) string {
	switch {
	case !r.Configured:
		return "no backup configured"
	case r.Health == viewmodel.HealthUnknown:
		return "Backups are not readable"
	case r.LastFailedAt != nil:
		return "failed " + age(now, *r.LastFailedAt) + " ago, after the last completed one"
	case r.LastCompletedAt == nil:
		return "no completed Backup observed"
	case r.Cadence != nil && r.Cadence.Overdue && r.Cadence.Basis == "schedule":
		return "completed " + age(now, *r.LastCompletedAt) + " ago, overdue: the schedule runs every " +
			age(now, now.Add(-time.Duration(r.Cadence.IntervalSeconds)*time.Second))
	case r.Cadence != nil && r.Cadence.Overdue:
		return "completed " + age(now, *r.LastCompletedAt) + " ago, older than " +
			age(now, now.Add(-time.Duration(r.Cadence.ThresholdSeconds)*time.Second)) + " (no backup cadence known)"
	default:
		return "completed " + age(now, *r.LastCompletedAt) + " ago"
	}
}

func age(now, at time.Time) string {
	d := now.Sub(at).Round(time.Minute)
	if d < time.Minute {
		return "<1m"
	}
	return strings.TrimSuffix(d.String(), "0s")
}

// Overview is the /c/<ns>/<cluster> screen.
func (s *Service) Overview(ctx context.Context, namespace, name string) (viewmodel.ClusterOverview, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, true)
	if err != nil {
		return viewmodel.ClusterOverview{}, err
	}
	now := s.now().UTC()
	run := s.latestRun(g.key)
	verdict := s.verdict(g, run)
	row := s.rowWith(g, run, verdict)
	ov := viewmodel.ClusterOverview{
		Ref:         row.Ref,
		Labels:      row.Labels,
		GeneratedAt: now,
		Present:     true,
		Verdict:     verdict,
		Phase:       row.Phase,
		Primary:     s.primaryElection(g, now),
		Instances:   row.Instances,
		Cards:       s.cards(g),
		Conditions:  conditions(g),
		Backups:     s.backupSummary(g, now),
		Version:     row.Version,
		Findings:    row.Findings,
		Sources:     g.markFailed(s.sources(g.key.Namespace)),
		BlindSpots:  blindSpots(g),
	}
	ov.RecentWarnings = []viewmodel.EventRow{}
	for _, e := range g.events {
		if e.Type == corev1.EventTypeWarning {
			ov.RecentWarnings = append(ov.RecentWarnings, eventRow(e))
		}
	}
	ov.EscapeHatches = []viewmodel.EscapeHatch{}
	for _, hatch := range cnpgstatus.EscapeHatches(g.cluster) {
		ov.EscapeHatches = append(ov.EscapeHatches, viewmodel.EscapeHatch{Annotation: hatch.Annotation, Value: hatch.Value, Effect: hatch.Effect, Health: health(hatch.Class)})
	}
	for _, hatch := range cnpgstatus.PodEscapeHatches(g.pods) {
		ov.EscapeHatches = append(ov.EscapeHatches, viewmodel.EscapeHatch{Annotation: hatch.Annotation + " on Pod/" + hatch.Pod, Value: hatch.Value, Effect: hatch.Effect, Health: health(hatch.Class)})
	}
	fencing := cnpgstatus.Fencing(g.cluster)
	ov.Fencing = viewmodel.Fencing{State: viewmodel.FencingState(fencing.State), Instances: fencing.Instances}
	ov.Failover = s.failover(g, now)
	hib := cnpgstatus.Hibernation(g.cluster)
	ov.Hibernation = viewmodel.Hibernation{State: viewmodel.HibernationState(hib.State), Reason: hib.Reason, Message: bound(hib.Message)}
	if waiting := waitingFor(g, ov.RecentWarnings); waiting != nil {
		ov.Waiting = waiting
	}
	return ov, nil
}

// waitCauses are the Warning reasons that name why the operator waits.
var waitCauses = map[string]bool{
	"QuotaExceeded": true, "AdmissionDenied": true, "ReconciliationFailed": true, "BackupPending": true,
	"BackupDelayed": true, "VolumeSnapshotError": true, "FailedScheduling": true, "FailedCreate": true,
}

func waitingFor(g *gathered, warnings []viewmodel.EventRow) *viewmodel.OperatorWaiting {
	phase := cnpgstatus.ClassifyPhase(g.phaseMinor, g.cluster.Status.Phase)
	if phase.Class == cnpgstatus.ClassOK || phase.Reason != "" {
		// A capped reading is an ok row of an unverified table: the
		// operator is not waiting because of it.
		return nil
	}
	w := &viewmodel.OperatorWaiting{PhaseReason: bound(g.cluster.Status.PhaseReason), Causes: []viewmodel.EventRow{}, Reasons: []viewmodel.WaitReason{}}
	for _, e := range warnings {
		if waitCauses[e.Reason] {
			w.Causes = append(w.Causes, e)
		}
	}
	w.FilteredCacheTrap = cnpgstatus.FilteredCacheTrap(g.cluster, g.pods)
	for _, r := range cnpgstatus.WaitReasons(cnpgstatus.WaitInput{
		Minor: g.minor, Cluster: g.cluster, Pods: g.pods, Events: g.events, Backups: g.backups,
	}) {
		w.Reasons = append(w.Reasons, viewmodel.WaitReason{ID: r.ID, Text: r.Text, Heuristic: r.Heuristic, Evidence: r.Evidence})
	}
	if w.PhaseReason == "" && len(w.Causes) == 0 && !w.FilteredCacheTrap && len(w.Reasons) == 0 {
		return nil
	}
	return w
}

func (s *Service) sources(namespace string) []viewmodel.SourceState {
	out := s.cache.Sources(namespace)
	if sw := s.sweeper(); sw != nil {
		out = append(out, sw.State())
	}
	if d := s.diagnostics(); d != nil {
		out = append(out, d.State())
	}
	return out
}

// BlindSpotVolumeTrendsAtCapacity marks a Cluster the trend store turned
// away at its cap: some of its volumes have no forecast.
const BlindSpotVolumeTrendsAtCapacity = "storage.volumeTrendsAtCapacity"

func blindSpots(g *gathered) []viewmodel.BlindSpot {
	out := []viewmodel.BlindSpot{}
	for _, b := range cnpgstatus.BlindSpotsFor(g.minor, g.cluster, cnpgstatus.Backups(g.cluster)) {
		if b.ID == cnpgstatus.BlindSpotVolumeUsageNotReported && g.volumesReported {
			// The instances report their volumes: detected from the data.
			continue
		}
		out = append(out, viewmodel.BlindSpot{ID: b.ID, Panel: b.Panel, Text: b.Text})
	}
	if g.volumesRefused {
		out = append(out, viewmodel.BlindSpot{
			ID: BlindSpotVolumeTrendsAtCapacity, Panel: "storage",
			Text: "The console's volume trend store is at its series cap (VOLUME_TREND_MAX_SERIES): some volumes of this cluster are not trended, so their forecasts are missing. Raise the cap or split the fleet across consoles.",
		})
	}
	return out
}

func conditions(g *gathered) []viewmodel.Condition {
	out := make([]viewmodel.Condition, 0, len(g.cluster.Status.Conditions))
	for _, c := range g.cluster.Status.Conditions {
		reading := cnpgstatus.ClassifyCondition(g.phaseMinor, c.Type, string(c.Status), c.Reason)
		cond := viewmodel.Condition{
			Type: c.Type, Status: string(c.Status), Reason: bound(c.Reason), Message: bound(c.Message),
			Health: health(reading.Class), Known: reading.Known, Cap: tableCap("condition", g.phaseMinor, reading.Reason),
		}
		if !c.LastTransitionTime.IsZero() {
			t := c.LastTransitionTime.UTC()
			cond.LastTransition = &t
		}
		out = append(out, cond)
	}
	return out
}

func (s *Service) primaryElection(g *gathered, now time.Time) viewmodel.PrimaryElection {
	p := cnpgstatus.Primary(g.cluster)
	e := viewmodel.PrimaryElection{
		PrimarySummary: primarySummary(g.cluster),
		Provenance:     s.provenance(g.key.Namespace, cache.SourceClusters, viewmodel.OriginOperator, "Cluster.status.currentPrimary", now),
	}
	if p.State == cnpgstatus.PrimaryFailover || p.State == cnpgstatus.PrimaryFailoverPending {
		if t, err := time.Parse(time.RFC3339Nano, g.cluster.Status.CurrentPrimaryFailingSinceTimestamp); err == nil {
			t = t.UTC()
			e.FailingSince = &t
		}
	}
	for _, pod := range g.pods {
		if pod.Labels["cnpg.io/instanceRole"] == "primary" || pod.Labels["role"] == "primary" {
			e.LabelPrimaries = append(e.LabelPrimaries, pod.Name)
		}
	}
	for name, inst := range g.swept.Instances {
		if inst.State == viewmodel.StatusReadOK && inst.Reading != nil && inst.Reading.IsPrimary {
			e.LivePrimaries = append(e.LivePrimaries, name)
		}
	}
	sort.Strings(e.LivePrimaries)
	if g.lease != nil {
		lease := &viewmodel.Lease{Provenance: s.provenance(g.key.Namespace, cache.SourceLeases, viewmodel.OriginKubernetes, "Lease/"+g.lease.Name, now)}
		if g.lease.Spec.HolderIdentity != nil {
			lease.Holder = *g.lease.Spec.HolderIdentity
		}
		if g.lease.Spec.RenewTime != nil {
			t := g.lease.Spec.RenewTime.UTC()
			lease.RenewedAt = &t
		}
		e.Lease = lease
	}
	return e
}

// role is the instance's role per the operator's currentPrimary.
func role(c *cnpgv1.Cluster, name string) viewmodel.InstanceRole {
	switch c.Status.CurrentPrimary {
	case "":
		return viewmodel.RoleUnknown
	case name:
		return viewmodel.RolePrimary
	default:
		return viewmodel.RoleReplica
	}
}

func (s *Service) cards(g *gathered) []viewmodel.InstanceCard {
	rows := s.instanceRows(g, false)
	out := make([]viewmodel.InstanceCard, 0, len(rows))
	for _, r := range rows {
		card := viewmodel.InstanceCard{Name: r.Name, Role: r.Role.Operator, Health: r.Health, StatusRead: r.StatusRead.State, Fenced: r.Fenced}
		if r.Pod != nil {
			ready := r.Pod.Ready
			card.Ready = &ready
			card.Detail = r.Pod.Waiting
		}
		if card.Detail == "" && r.StatusRead.State != viewmodel.StatusReadOK {
			card.Detail = r.StatusRead.Reason
		}
		if card.Detail == "" && r.Live != nil && r.Live.LagBytes != nil {
			card.Detail = fmt.Sprintf("lag %d B", *r.Live.LagBytes)
		}
		out = append(out, card)
	}
	return out
}

// Instances is the /instances screen.
func (s *Service) Instances(ctx context.Context, namespace, name string) (viewmodel.InstanceList, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return viewmodel.InstanceList{}, err
	}
	list := viewmodel.InstanceList{Ref: ref(g.cluster), GeneratedAt: s.now().UTC(), Items: s.instanceRows(g, true), BlindSpots: []viewmodel.BlindSpot{}}
	if sw := s.sweeper(); sw != nil {
		list.Sweep = sw.State()
	} else {
		list.Sweep = viewmodel.SourceState{Source: "instanceStatus", State: viewmodel.SourceDisabled}
	}
	for _, b := range blindSpots(g) {
		if b.Panel == "instances" || b.Panel == "replication" {
			list.BlindSpots = append(list.BlindSpots, b)
		}
	}
	return list, nil
}

// instanceRows lists one row per member pod, primary first, then one per
// instance the operator reports without a pod (status.instanceNames).
func (s *Service) instanceRows(g *gathered, withSeries bool) []viewmodel.InstanceRow {
	now := s.now().UTC()
	fencing := cnpgstatus.Fencing(g.cluster)
	fenced := func(name string) bool {
		if fencing.State == cnpgstatus.FencingAll {
			return true
		}
		for _, n := range fencing.Instances {
			if n == name {
				return true
			}
		}
		return false
	}
	var primaryLSN string
	for _, inst := range g.swept.Instances {
		if inst.State == viewmodel.StatusReadOK && inst.Reading != nil && inst.Reading.IsPrimary {
			primaryLSN = inst.Reading.CurrentLSN
		}
	}
	seen := map[string]bool{}
	rows := []viewmodel.InstanceRow{}
	for _, pod := range g.pods {
		seen[pod.Name] = true
		row := viewmodel.InstanceRow{
			Name: pod.Name, Fenced: fenced(pod.Name),
			Role: viewmodel.Roles{Operator: role(g.cluster, pod.Name), Label: labelRole(pod), Live: viewmodel.RoleUnknown},
			Pod:  s.podView(g, pod, now),
		}
		row.StatusRead, row.Live = s.statusRead(g, pod.Name, primaryLSN)
		if row.Live != nil && row.StatusRead.State == viewmodel.StatusReadOK {
			row.Role.Live = viewmodel.RoleReplica
			if row.Live.IsPrimary {
				row.Role.Live = viewmodel.RolePrimary
			}
		}
		if withSeries {
			if sw := s.sweeper(); sw != nil {
				row.Series = sparklines(sw.Series(g.sweepKey(), pod.Name))
			}
		}
		row.Health = instanceHealth(row)
		rows = append(rows, row)
	}
	for _, name := range g.cluster.Status.InstanceNames {
		if seen[name] {
			continue
		}
		rows = append(rows, viewmodel.InstanceRow{
			Name: name, Health: viewmodel.HealthCritical, Fenced: fenced(name),
			Role:       viewmodel.Roles{Operator: role(g.cluster, name), Label: viewmodel.RoleUnknown, Live: viewmodel.RoleUnknown},
			StatusRead: viewmodel.StatusRead{State: viewmodel.StatusReadNotRunning, Reason: "the operator reports this instance but no pod exists"},
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		pi, pj := rows[i].Role.Operator == viewmodel.RolePrimary, rows[j].Role.Operator == viewmodel.RolePrimary
		if pi != pj {
			return pi
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}

func labelRole(pod *corev1.Pod) viewmodel.InstanceRole {
	value := pod.Labels["cnpg.io/instanceRole"]
	if value == "" {
		value = pod.Labels["role"]
	}
	switch value {
	case "primary":
		return viewmodel.RolePrimary
	case "replica":
		return viewmodel.RoleReplica
	default:
		return viewmodel.RoleUnknown
	}
}

func (s *Service) podView(g *gathered, pod *corev1.Pod, now time.Time) *viewmodel.PodView {
	v := &viewmodel.PodView{
		Phase: string(pod.Status.Phase), Ready: podReady(pod), Node: pod.Spec.NodeName,
		Deleting:   pod.DeletionTimestamp != nil,
		Provenance: s.provenance(g.key.Namespace, cache.SourcePods, viewmodel.OriginKubernetes, "Pod", now),
	}
	if !pod.CreationTimestamp.IsZero() {
		t := pod.CreationTimestamp.UTC()
		v.CreatedAt = &t
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == "postgres" {
			v.Image = c.Image
		}
	}
	for _, cs := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		v.Restarts += cs.RestartCount
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && v.Waiting == "" {
			v.Waiting = cs.State.Waiting.Reason
		}
	}
	return v
}

func (s *Service) statusRead(g *gathered, name, primaryLSN string) (viewmodel.StatusRead, *viewmodel.LiveStatus) {
	if sw := s.sweeper(); sw == nil || sw.State().State == viewmodel.SourceDisabled {
		reason := "the status sweep is switched off"
		if sw != nil {
			reason = sw.State().Reason
		}
		return viewmodel.StatusRead{State: viewmodel.StatusReadDisabled, Reason: reason}, nil
	}
	inst, ok := g.swept.Instances[name]
	if !ok {
		return viewmodel.StatusRead{State: viewmodel.StatusReadPending, Reason: "not swept yet"}, nil
	}
	at, since := inst.At, inst.Since
	read := viewmodel.StatusRead{State: inst.State, Reason: inst.Reason, At: &at, Since: &since}
	if inst.Reading == nil {
		return read, nil
	}
	archiver := inst.Reading.IsPrimary
	if g.cluster != nil {
		archiver = archiver || name == g.cluster.Status.CurrentPrimary || name == g.cluster.Status.TargetPrimary
	}
	return read, liveStatus(inst, primaryLSN, archiver)
}

// liveStatus is an instance's own report. archiver says whether the
// instance is the one that archives: the primary, or a replica cluster's
// designated primary (status.currentPrimary / targetPrimary). A replica
// cluster's other standbys run with archive_mode always too
// (pkg/postgres/configuration.go v1.30.1:741-742), so CloudNativePG's
// isArchivingWAL reads true on them, but the instance manager's archiver
// returns without archiving there (pkg/management/postgres/archiver/
// archiver.go v1.30.1:132-142): it is
// not served for them (CNPG P3-2).
func liveStatus(inst sweep.Instance, primaryLSN string, archiver bool) *viewmodel.LiveStatus {
	r := inst.Reading
	observed := r.ObservedAt
	live := &viewmodel.LiveStatus{
		IsPrimary: r.IsPrimary, SystemID: r.SystemID, CurrentLSN: r.CurrentLSN, ReceivedLSN: r.ReceivedLSN, ReplayLSN: r.ReplayLSN,
		ReplayPaused: r.ReplayPaused, WALReceiverActive: r.IsWalReceiverActive, PendingRestart: r.PendingRestart,
		PendingRestartDecr: r.PendingRestartForDecrease, ArchivingWAL: r.IsArchivingWAL && archiver, LastFailedWAL: r.LastFailedWAL,
		ReadyWALFiles: r.ReadyWALFiles, MightBeUnavailable: r.MightBeUnavailable, PgRewindRunning: r.IsPgRewindRunning,
		ManagerVersion: r.InstanceManagerVersion, ManagerUpgrading: r.IsInstanceManagerUpgrading,
		Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", ObservedAt: &observed},
	}
	if r.TimelineID > 0 {
		tl := r.TimelineID
		live.TimelineID = &tl
	}
	if inst.State != viewmodel.StatusReadOK {
		live.Provenance.Stale, live.Provenance.StaleReason = true, "the last read failed: "+inst.Reason
	}
	if !r.IsPrimary && inst.State == viewmodel.StatusReadOK {
		if lag, ok := sweep.LagBytes(primaryLSN, r.ReplayLSN); ok {
			live.LagBytes = &lag
		}
	}
	for _, sb := range r.Standbys {
		live.Standbys = append(live.Standbys, viewmodel.Standby{Name: sb.ApplicationName, State: sb.State, SyncState: sb.SyncState, SentLSN: sb.SentLSN, ReplayLSN: sb.ReplayLSN, ReplayLag: sb.ReplayLag})
	}
	return live
}

// instanceHealth reads one row: a waiting container or an unready pod is
// a fault; an instance the console could not read is unknown, never ok.
func instanceHealth(r viewmodel.InstanceRow) viewmodel.Health {
	switch {
	case r.Fenced:
		return viewmodel.HealthInactive
	case r.Pod == nil:
		return viewmodel.HealthCritical
	case r.Pod.Deleting:
		return viewmodel.HealthProgressing
	case r.Pod.Waiting == "CrashLoopBackOff" || r.Pod.Waiting == "ImagePullBackOff" || r.Pod.Waiting == "ErrImagePull":
		return viewmodel.HealthCritical
	case !r.Pod.Ready:
		return viewmodel.HealthWarning
	}
	switch r.StatusRead.State {
	case viewmodel.StatusReadOK:
	case viewmodel.StatusReadNoTLS:
		return viewmodel.HealthWarning
	case viewmodel.StatusReadDisabled:
		return viewmodel.HealthOK
	default:
		return viewmodel.HealthUnknown
	}
	if r.Live != nil && (r.Live.MightBeUnavailable || r.Live.ReplayPaused) {
		return viewmodel.HealthWarning
	}
	if r.Live != nil && r.Live.PendingRestart {
		return viewmodel.HealthProgressing
	}
	return viewmodel.HealthOK
}

func (s *Service) backupSummary(g *gathered, now time.Time) viewmodel.BackupSummary {
	cfg := cnpgstatus.Backups(g.cluster)
	archiving := cnpgstatus.Archiving(g.cluster, primaryArchiver(g))
	summary := viewmodel.BackupSummary{
		BackupRecency: s.backupRecency(g),
		Archiving: viewmodel.Archiving{
			Health: health(archiving.Class), ConditionStatus: archiving.ConditionStatus,
			Reason: archiving.Reason, Message: bound(archiving.Message), Provenance: s.archivingProvenance(g, now),
		},
	}
	if live := primaryArchiverReading(g); live != nil {
		archivingWAL, ready := live.IsArchivingWAL, live.ReadyWALFiles
		summary.Archiving.LiveArchiving = &archivingWAL
		if live.IsPrimary {
			// A designated primary reports no readyWalFiles.
			summary.Archiving.ReadyWALFiles = &ready
		}
		summary.Archiving.LastFailedWAL, summary.Archiving.LastArchivedWAL = live.LastFailedWAL, live.LastArchivedWAL
		summary.Archiving.LastArchivedAt, summary.Archiving.LastFailedAt = utc(live.LastArchivedWALTime), utc(live.LastFailedWALTime)
	}
	if cfg.PluginBased {
		summary.NotReported = append([]string(nil), cfg.NotReported...)
		if cfg.PgBackRest != nil {
			summary.RecoveryWindow = s.pgbackrestWindow(g)
		} else {
			summary.RecoveryWindow = s.recoveryWindow(g, now)
		}
	} else if first := g.cluster.Status.FirstRecoverabilityPoint; first != "" { //nolint:staticcheck // the in-tree field is the source for in-tree backups
		window := &viewmodel.RecoveryWindow{Provenance: s.provenance(g.key.Namespace, cache.SourceClusters, viewmodel.OriginOperator, "Cluster.status.firstRecoverabilityPoint", now)}
		if t, err := time.Parse(time.RFC3339, first); err == nil {
			t = t.UTC()
			window.First = &t
		}
		summary.RecoveryWindow = window
	}
	for _, sb := range g.scheduled {
		if sb.Status.NextScheduleTime == nil || (sb.Spec.Suspend != nil && *sb.Spec.Suspend) {
			continue
		}
		next := sb.Status.NextScheduleTime.UTC()
		if summary.NextScheduledAt == nil || next.Before(*summary.NextScheduledAt) {
			summary.NextScheduledAt = &next
		}
	}
	return summary
}

// pgbackrestWindow is a pgBackRest Cluster's recovery window: neither
// plugin writes one to the Cluster, so it is the evidence
// agent's, from the oldest usable backup's stop to the receipt of the
// last contiguous WAL segment (the newest usable backup's stop when that
// receipt is unknown). Nil without an agent's window.
func (s *Service) pgbackrestWindow(g *gathered) *viewmodel.RecoveryWindow {
	reader := s.opts.Evidence
	if reader == nil {
		return nil
	}
	r := reader.ForCluster(g.key.Namespace, g.key.Name)
	if r.Snapshot == nil || r.Snapshot.Details.PGBackRest == nil || r.Snapshot.Details.PGBackRest.RecoveryWindow == nil {
		return nil
	}
	w := r.Snapshot.Details.PGBackRest.RecoveryWindow
	first, last := w.StartAt.UTC(), w.NewestStopAt.UTC()
	if w.EndReceiptAt != nil && w.EndReceiptAt.After(last) {
		last = w.EndReceiptAt.UTC()
	}
	out := &viewmodel.RecoveryWindow{First: &first, Last: &last, Provenance: viewmodel.Provenance{
		Origin: viewmodel.OriginEvidence, Source: "EvidenceAgent " + r.Agent + " " + evidencev1alpha1.SnapshotPath + " recovery_window",
		ObservedAt: r.ObservedAt,
	}}
	if r.State != viewmodel.EvidenceLive {
		out.Provenance.Stale, out.Provenance.StaleReason = true, bound(r.Reason)
	}
	return out
}

// recoveryWindow reads ObjectStore.status.serverRecoveryWindow[server],
// the only place plugin backups report recoverability.
func (s *Service) recoveryWindow(g *gathered, now time.Time) *viewmodel.RecoveryWindow {
	if g.store == nil {
		return nil
	}
	server := g.cluster.Name
	for _, plugin := range g.cluster.Spec.Plugins {
		if v := plugin.Parameters["serverName"]; v != "" && plugin.Parameters["barmanObjectName"] == g.store.GetName() {
			server = v
		}
	}
	window, found, err := unstructured.NestedMap(g.store.Object, "status", "serverRecoveryWindow", server)
	if err != nil || !found {
		return nil
	}
	out := &viewmodel.RecoveryWindow{Provenance: s.provenance(g.key.Namespace, cache.SourceObjectStores, viewmodel.OriginOperator, "ObjectStore.status.serverRecoveryWindow["+server+"]", now)}
	parse := func(key string) *time.Time {
		raw, ok := window[key].(string)
		if !ok {
			return nil
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil
		}
		t = t.UTC()
		return &t
	}
	out.First = parse("firstRecoverabilityPoint")
	out.Last = parse("lastSuccessfulBackupTime")
	return out
}

// Events is the events screen of one Cluster.
func (s *Service) Events(ctx context.Context, namespace, name string) (viewmodel.EventList, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, true)
	if err != nil {
		return viewmodel.EventList{}, err
	}
	list := viewmodel.EventList{
		Ref: ref(g.cluster), GeneratedAt: s.now().UTC(),
		Source: g.markFailed([]viewmodel.SourceState{s.cache.Source(namespace, cache.SourceEvents)})[0],
		Window: s.opts.EventsMaxAge.String(),
		Items:  []viewmodel.EventRow{},
	}
	for i, e := range g.events {
		if i == s.opts.EventsMaxPerCluster {
			list.Truncated = true
			break
		}
		list.Items = append(list.Items, eventRow(e))
	}
	return list, nil
}

func eventRow(e *corev1.Event) viewmodel.EventRow {
	reporter := e.ReportingController
	if reporter == "" {
		reporter = e.Source.Component
	}
	row := viewmodel.EventRow{
		Type: e.Type, Reason: bound(e.Reason), Message: bound(e.Message),
		Object: viewmodel.ObjectRef{Kind: e.InvolvedObject.Kind, Name: e.InvolvedObject.Name},
		Count:  e.Count, LastSeen: eventTime(e).UTC(), Reporter: bound(reporter),
		Origin: viewmodel.OriginKubernetes, Health: viewmodel.HealthInfo,
	}
	if row.Type != corev1.EventTypeWarning {
		row.Type = corev1.EventTypeNormal
	}
	if row.Count == 0 {
		row.Count = 1
	}
	if strings.Contains(reporter, "cloudnative-pg") || strings.Contains(reporter, "cnpg") {
		row.Origin = viewmodel.OriginOperator
	}
	if !e.FirstTimestamp.IsZero() {
		t := e.FirstTimestamp.UTC()
		row.FirstSeen = &t
	}
	if e.Type == corev1.EventTypeWarning {
		row.Health = viewmodel.HealthWarning
	}
	return row
}

// sparklines are the series an instance row draws: the primary's WAL
// position and last-archived rings are read by the recovery and backlog
// facts only.
func sparklines(series []viewmodel.Series) []viewmodel.Series {
	return slices.DeleteFunc(series, func(s viewmodel.Series) bool {
		return s.Key == sweep.SeriesCurrentLSN || s.Key == sweep.SeriesLastArchivedAt
	})
}
