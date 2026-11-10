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
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/diag"
	"github.com/fyannk/pgToolBox/internal/console/timeline"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// TimelineQuery selects a window of a Cluster's timeline.
type TimelineQuery struct {
	// From and To bound the window; zero To is now, zero From is To
	// minus DefaultTimelineWindow.
	From, To time.Time
	// Kinds selects entry kinds (viewmodel.TimelineKinds); empty is all.
	Kinds []string
	// Limit bounds the entries; zero is DefaultTimelineLimit.
	Limit int
}

// The timeline's defaults and bounds.
const (
	DefaultTimelineWindow = 24 * time.Hour
	DefaultTimelineLimit  = 500
	MaxTimelineLimit      = 2000
)

// timelineSources are the per-namespace sources whose gaps the journal
// records: the ones the timeline's facts come from.
var timelineSources = []string{cache.SourceClusters, cache.SourcePods, cache.SourceBackups, cache.SourceEvents}

// ClusterFacts is what the timeline recorder compares between passes.
func (s *Service) ClusterFacts(ctx context.Context, key cache.ClusterKey) (timeline.ClusterFacts, bool) {
	g, err := s.gather(ctx, key, false)
	if err != nil {
		return timeline.ClusterFacts{}, false
	}
	c := g.cluster
	f := timeline.ClusterFacts{
		Namespace: key.Namespace, Name: key.Name, UID: string(c.UID),
		Phase: bound(c.Status.Phase), PhaseReason: bound(c.Status.PhaseReason), CurrentPrimary: c.Status.CurrentPrimary,
	}
	if ts := c.Status.CurrentPrimaryTimestamp; ts != "" && len(ts) < 64 {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			f.CurrentPrimaryTimestamp = t.UTC()
		}
	}
	for _, pod := range g.pods {
		p := timeline.PodFacts{Name: pod.Name, UID: string(pod.UID)}
		for i := range pod.Status.ContainerStatuses {
			cs := &pod.Status.ContainerStatuses[i]
			if cs.Name != "postgres" {
				continue
			}
			p.Restarts = cs.RestartCount
			if t := cs.LastTerminationState.Terminated; t != nil {
				p.LastReason, p.LastExitCode = bound(t.Reason), t.ExitCode
				if !t.FinishedAt.IsZero() {
					p.LastFinishedAt = t.FinishedAt.UTC()
				}
			}
		}
		f.Pods = append(f.Pods, p)
	}
	for _, b := range g.backups {
		bf := timeline.BackupFacts{Name: b.Name, UID: string(b.UID), Phase: string(b.Status.Phase), Error: bound(b.Status.Error)}
		if b.Status.StoppedAt != nil {
			bf.StoppedAt = b.Status.StoppedAt.UTC()
		}
		f.Backups = append(f.Backups, bf)
	}
	return f, true
}

// SourceFacts are the states of the sources the timeline rests on.
func (s *Service) SourceFacts() []timeline.SourceFacts {
	var out []timeline.SourceFacts
	for _, ns := range s.cache.Namespaces() {
		for _, src := range timelineSources {
			st := s.cache.Source(ns, src)
			out = append(out, timeline.SourceFacts{Namespace: ns, Source: src, State: string(st.State), Reason: bound(st.Reason)})
		}
	}
	if sw := s.sweeper(); sw != nil {
		st := sw.State()
		out = append(out, timeline.SourceFacts{Source: st.Source, State: string(st.State), Reason: bound(st.Reason)})
	}
	if d := s.diagnostics(); d != nil {
		st := d.State()
		out = append(out, timeline.SourceFacts{Source: st.Source, State: string(st.State), Reason: bound(st.Reason)})
	}
	return out
}

// RunFindings converts a diagnostics run for the recorder: its findings
// and the checks that could not run. A failed run returns ok false.
func RunFindings(run diag.Run) (findings []timeline.FindingFacts, unavailable map[string]bool, ok bool) {
	if run.Failed != "" {
		return nil, nil, false
	}
	unavailable = map[string]bool{}
	for _, c := range run.Result.Checks {
		if c.Outcome == diagnose.CheckUnavailable {
			unavailable[c.Name] = true
		}
	}
	for _, f := range run.Result.Findings {
		findings = append(findings, timeline.FindingFacts{
			ID: f.ID, Check: f.Check, SubjectKind: f.Subject.Kind, Subject: f.Subject.Name,
			Severity: string(severity(f.Severity)), Summary: bound(f.Summary),
		})
	}
	return findings, unavailable, true
}

// Timeline is the incident timeline of one Cluster.
func (s *Service) Timeline(ctx context.Context, namespace, name string, q TimelineQuery) (viewmodel.Timeline, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, true)
	if err != nil {
		return viewmodel.Timeline{}, err
	}
	now := s.now().UTC()
	if q.To.IsZero() {
		q.To = now
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-DefaultTimelineWindow)
	}
	q.From, q.To = q.From.UTC(), q.To.UTC()
	if q.Limit <= 0 {
		q.Limit = DefaultTimelineLimit
	}
	q.Limit = min(q.Limit, MaxTimelineLimit)
	out := viewmodel.Timeline{
		Ref: ref(g.cluster), GeneratedAt: now, From: q.From, To: q.To, Kinds: append([]string{}, q.Kinds...),
		EventsWindow: s.opts.EventsMaxAge.String(), Counts: []viewmodel.TimelineCount{}, Items: []viewmodel.TimelineEntry{},
	}
	j := s.opts.Timeline
	if j == nil {
		out.Source = viewmodel.SourceState{Source: "timeline", State: viewmodel.SourceDisabled, Reason: "HISTORY_ENABLED is false"}
	} else {
		out.Source = viewmodel.SourceState{Source: "timeline", State: viewmodel.SourceLive}
		out.Persistent = j.Persistent()
	}
	entries, oldest, evicted := s.timelineEntries(g, q.From, q.To)
	out.Evicted = evicted
	if !oldest.IsZero() {
		out.ObservedSince = &oldest
	}
	counts := map[string]int{}
	want := map[string]bool{}
	for _, k := range q.Kinds {
		want[k] = true
	}
	for _, e := range entries {
		counts[e.Kind]++
		if len(want) == 0 || want[e.Kind] {
			out.Items = append(out.Items, e)
		}
	}
	for _, k := range viewmodel.TimelineKinds {
		if counts[k] > 0 {
			out.Counts = append(out.Counts, viewmodel.TimelineCount{Kind: k, Count: counts[k]})
		}
	}
	if len(out.Items) > q.Limit {
		out.Items, out.Truncated = out.Items[:q.Limit], true
	}
	return out, nil
}

// digestKinds are the journal kinds the digest counts.
var digestKinds = map[timeline.Kind]bool{timeline.KindPrimary: true, timeline.KindRestart: true, timeline.KindBackup: true, timeline.KindFinding: true}

// DigestEntries is every Cluster's digest news in [from, to), newest
// first, bucketed by Cluster: the journal's primary changes, container
// restarts, backups and findings, and the Clusters' own spec revisions.
// One journal query and one history snapshot serve the whole fleet
// (the digest's budget at 1,600 Clusters); Events are not digest news.
func (s *Service) DigestEntries(_ context.Context, from, to time.Time) map[viewmodel.ClusterRef][]viewmodel.TimelineEntry {
	from, to = from.UTC(), to.UTC()
	out := map[viewmodel.ClusterRef][]viewmodel.TimelineEntry{}
	if j := s.opts.Timeline; j != nil {
		res := j.Query(timeline.Query{From: from, To: to, Kinds: digestKinds})
		for i := range res.Events {
			e := &res.Events[i]
			if e.Cluster == "" {
				continue
			}
			ref := viewmodel.ClusterRef{Namespace: e.Namespace, Name: e.Cluster}
			out[ref] = append(out[ref], journalEntry(e, ""))
		}
	}
	if h := s.historyReader(); h != nil {
		snap, _ := h.Snapshot()
		for _, e := range snap.Entries {
			if e.Kind != "Cluster" || e.Change != history.ChangeSpec || e.ObservedAt.Before(from) || !e.ObservedAt.Before(to) {
				continue
			}
			ref := viewmodel.ClusterRef{Namespace: e.Namespace, Name: e.Name}
			out[ref] = append(out[ref], revisionEntry(e))
		}
	}
	for ref, entries := range out {
		sort.SliceStable(entries, func(a, b int) bool { return entries[a].At.After(entries[b].At) })
		out[ref] = entries
	}
	return out
}

// timelineEntries merges the journal, the revisions and the Events in
// [from, to), newest first.
func (s *Service) timelineEntries(g *gathered, from, to time.Time) (entries []viewmodel.TimelineEntry, oldest time.Time, evicted bool) {
	in := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	if j := s.opts.Timeline; j != nil {
		res := j.Query(timeline.Query{Namespace: g.key.Namespace, Cluster: g.key.Name, From: from, To: to})
		oldest, evicted = res.Oldest, res.Evicted
		for i := range res.Events {
			entries = append(entries, journalEntry(&res.Events[i], g.phaseMinor))
		}
	}
	if h := s.historyReader(); h != nil {
		snap, _ := h.Snapshot()
		evicted = evicted || snap.Evicted
		for _, e := range snap.Entries {
			if e.Namespace != g.key.Namespace || !in(e.ObservedAt) || !timelineRevision(e) {
				continue
			}
			if s.belongs(h, e.UID, e.Kind, e.Namespace, e.Name, g.key) {
				entries = append(entries, revisionEntry(e))
			}
		}
	}
	for _, ev := range g.events {
		if t := eventTime(ev); in(t) {
			entries = append(entries, eventEntry(ev))
		}
	}
	sort.SliceStable(entries, func(a, b int) bool { return entries[a].At.After(entries[b].At) })
	return entries, oldest, evicted
}

// timelineRevision keeps the revisions that are events: definitions
// created, changed or deleted, and the first observation of the Cluster
// itself. Status transitions are the journal's (phase, primary,
// restarts), and the first observation of every child at each start is
// noise.
func timelineRevision(e history.Entry) bool {
	switch e.Change {
	case history.ChangeStatus:
		return false
	case history.ChangeFirstObserved:
		return e.Kind == "Cluster"
	case history.ChangeSpec:
		return e.Kind != "Pod"
	default:
		return true
	}
}

func revisionEntry(e history.Entry) viewmodel.TimelineEntry {
	seq := strconv.FormatUint(e.Seq, 10)
	out := viewmodel.TimelineEntry{
		ID: "r:" + seq, Kind: "revision", Action: string(e.Change), At: e.ObservedAt.UTC(), Timing: string(timeline.TimingObserved),
		Scope: "cluster", Subject: &viewmodel.ObjectRef{Kind: e.Kind, Name: e.Name}, Health: viewmodel.HealthInfo,
		Ref: seq, AfterGap: e.AfterGap, Link: "history/" + seq,
		Provenance: viewmodel.Provenance{Origin: viewmodel.OriginKubernetes, Source: "revision history (" + e.Kind + ")"},
	}
	if e.Actor.Manager != "" {
		out.Detail = bound(e.Actor.Manager)
	}
	if !e.PrevObservedAt.IsZero() && (e.AfterGap || e.Change == history.ChangeDeletedUnobserved) {
		t := e.PrevObservedAt.UTC()
		out.Since = &t
	}
	switch e.Change {
	case history.ChangeDeleted, history.ChangeDeletedUnobserved:
		out.Health = viewmodel.HealthWarning
	case history.ChangeSpec:
		out.Health = viewmodel.HealthProgressing
	case history.ChangeCreated, history.ChangeFirstObserved, history.ChangeStatus:
		// Informational: the default health stands.
	}
	at := out.At
	out.Provenance.ObservedAt = &at
	return out
}

func eventEntry(e *corev1.Event) viewmodel.TimelineEntry {
	row := eventRow(e)
	return viewmodel.TimelineEntry{
		ID: "e:" + string(e.UID), Kind: "event", Action: row.Type, At: row.LastSeen, Timing: string(timeline.TimingReported),
		Scope: "cluster", Subject: &viewmodel.ObjectRef{Kind: row.Object.Kind, Name: row.Object.Name}, Health: row.Health,
		Detail: row.Message, Ref: row.Reason, Count: row.Count, Link: "events",
		Provenance: viewmodel.Provenance{Origin: row.Origin, Source: "Event " + row.Reporter},
	}
}

// journalEntry draws one journal event.
func journalEntry(e *timeline.Event, minor cnpgstatus.Minor) viewmodel.TimelineEntry {
	out := viewmodel.TimelineEntry{
		ID: "j:" + strconv.FormatUint(e.Seq, 10), Kind: string(e.Kind), Action: e.Action, At: e.At, Timing: string(e.Timing),
		AfterGap: e.AfterGap, Uncertain: e.Uncertain, From: e.From, To: e.To, Detail: e.Detail, Ref: e.Ref, Health: viewmodel.HealthInfo,
	}
	switch {
	case e.Cluster != "":
		out.Scope = "cluster"
	case e.Namespace != "":
		out.Scope = "namespace"
	default:
		out.Scope = "console"
	}
	if !e.Since.IsZero() {
		t := e.Since
		out.Since = &t
	}
	if e.Subject != "" {
		out.Subject = &viewmodel.ObjectRef{Kind: e.SubjectKind, Name: e.Subject}
	}
	prov := func(origin viewmodel.Origin, source string) {
		out.Provenance = viewmodel.Provenance{Origin: origin, Source: source}
		if e.Timing == timeline.TimingObserved {
			at := e.At
			out.Provenance.ObservedAt = &at
		}
	}
	switch e.Kind {
	case timeline.KindPhase:
		out.Health = health(cnpgstatus.ClassifyPhase(minor, e.To).Class)
		prov(viewmodel.OriginOperator, "Cluster.status.phase")
	case timeline.KindPrimary:
		out.Health = map[string]viewmodel.Health{
			timeline.ActionFailover: viewmodel.HealthCritical, timeline.ActionSwitchover: viewmodel.HealthProgressing,
			timeline.ActionChanged: viewmodel.HealthWarning,
		}[e.Action]
		if out.Health == "" {
			out.Health = viewmodel.HealthInfo
		}
		prov(viewmodel.OriginOperator, "Cluster.status.currentPrimary")
		out.Link = "replication"
	case timeline.KindRestart:
		out.Health = viewmodel.HealthWarning
		prov(viewmodel.OriginKubernetes, "Pod.status.containerStatuses[postgres]")
		out.Link = "instances/" + e.Subject
	case timeline.KindBackup:
		out.Health = viewmodel.HealthOK
		if e.Action == timeline.ActionFailed {
			out.Health = viewmodel.HealthCritical
		}
		prov(viewmodel.OriginOperator, "Backup.status.phase")
		out.Link = "backups"
	case timeline.KindFinding:
		out.Severity = viewmodel.Severity(e.Severity)
		out.Health = severityHealth(out.Severity)
		if e.Action == timeline.ActionCleared {
			out.Health = viewmodel.HealthOK
		}
		prov(viewmodel.OriginConsole, "diagnostics")
		out.Link = "diagnostics"
	case timeline.KindGap:
		out.Health = viewmodel.HealthUnknown
		if e.Action == timeline.ActionEnded {
			out.Health = viewmodel.HealthOK
		}
		prov(viewmodel.OriginConsole, "source "+e.Ref)
	case timeline.KindConsole:
		prov(viewmodel.OriginConsole, "console")
	}
	return out
}

func severityHealth(s viewmodel.Severity) viewmodel.Health {
	switch s {
	case viewmodel.SeverityCritical, viewmodel.SeverityHigh:
		return viewmodel.HealthCritical
	case viewmodel.SeverityWarning:
		return viewmodel.HealthWarning
	default:
		return viewmodel.HealthInfo
	}
}
