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

package timeline

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/cache"
)

// ClusterFacts is what the recorder compares between two observations
// of one Cluster, read from the informer cache by the read model.
type ClusterFacts struct {
	Namespace, Name, UID string
	// Phase and PhaseReason are Cluster.status.phase and phaseReason.
	Phase, PhaseReason string
	// CurrentPrimary is status.currentPrimary; CurrentPrimaryTimestamp is
	// the operator's status.currentPrimaryTimestamp (zero when absent).
	CurrentPrimary          string
	CurrentPrimaryTimestamp time.Time
	Pods                    []PodFacts
	Backups                 []BackupFacts
}

// PodFacts is one member pod's postgres container.
type PodFacts struct {
	Name, UID string
	// Restarts is the container's restartCount.
	Restarts int32
	// LastReason and LastExitCode describe the previous termination;
	// LastFinishedAt is the kubelet's finishedAt of it (zero if none).
	LastReason     string
	LastExitCode   int32
	LastFinishedAt time.Time
}

// BackupFacts is one Backup object.
type BackupFacts struct {
	Name, UID, Phase string
	// StoppedAt is the Backup's plausible status.stoppedAt, else
	// CloudNativePG's completion stamp (status.reconciliationTerminatedAt);
	// zero when neither (a 1970 or future stop is not plausible).
	StoppedAt time.Time
	// Error is status.error, bounded by the caller.
	Error string
}

// FindingFacts is one finding of a diagnostics run.
type FindingFacts struct {
	ID, Check, SubjectKind, Subject, Severity, Summary string
}

// SourceFacts is the state of one source the console reads.
type SourceFacts struct {
	// Namespace is empty for a console-wide source.
	Namespace, Source, State, Reason string
}

// severityRank orders the finding severities of the view model.
var severityRank = map[string]int{"note": 0, "warning": 1, "high": 2, "critical": 3}

// failoverWindow is how far back a phase counts as the cause of a
// primary change: the operator sets "Failing over" or "Switchover in
// progress" before it moves currentPrimary.
const failoverWindow = 15 * time.Minute

type seenPhase struct {
	phase string
	at    time.Time
}

type clusterState struct {
	uid     string
	phase   string
	primary string
	// recent are the phases seen since the last primary change, within
	// failoverWindow: the candidates for its cause.
	recent   []seenPhase
	restarts map[string]int32
	// findings are the open findings; known reports that a run (or the
	// journal) has said what is open.
	findings map[string]FindingFacts
	known    bool
	// primed reports state reloaded from a previous console life: the
	// first observation is compared with it, and a difference happened
	// while the console was not looking.
	primed       bool
	primedFinds  bool
	observedOnce bool
}

type sourceState struct {
	state string
	since time.Time
}

// Recorder turns successive observations into journal events.
type Recorder struct {
	j *Journal
	// previousAlive is when the previous console life was last alive;
	// zero when unknown (a new file, or no persistence).
	previousAlive time.Time

	mu       sync.Mutex
	clusters map[cache.ClusterKey]*clusterState
	sources  map[string]sourceState
}

// NewRecorder primes a recorder from what the journal retains and
// records the console's start.
func NewRecorder(j *Journal, previousAlive time.Time) *Recorder {
	return NewRecorderWithStart(j, previousAlive, "")
}

// NewRecorderWithStart is NewRecorder whose start event carries detail
// (a fixed text, never a value read from a cluster).
func NewRecorderWithStart(j *Journal, previousAlive time.Time, detail string) *Recorder {
	r := &Recorder{j: j, previousAlive: previousAlive.UTC(), clusters: map[cache.ClusterKey]*clusterState{}, sources: map[string]sourceState{}}
	r.prime(j.All())
	start := Event{Kind: KindConsole, Action: ActionStarted, At: j.Now(), Timing: TimingObserved, Detail: detail}
	if !previousAlive.IsZero() {
		start.Since = previousAlive
	}
	j.Record(start)
	return r
}

func (r *Recorder) state(key cache.ClusterKey) *clusterState {
	st := r.clusters[key]
	if st == nil {
		st = &clusterState{restarts: map[string]int32{}, findings: map[string]FindingFacts{}}
		r.clusters[key] = st
	}
	return st
}

// prime rebuilds the last known phase, primary, open findings and open
// gaps from the retained events, oldest first.
func (r *Recorder) prime(events []Event) {
	for _, e := range events {
		switch e.Kind {
		case KindPhase, KindPrimary, KindFinding:
			if e.Cluster == "" {
				continue
			}
			st := r.state(cache.ClusterKey{Namespace: e.Namespace, Name: e.Cluster})
			if e.ClusterUID != "" && st.uid != "" && e.ClusterUID != st.uid {
				// A recreated Cluster: what the old one had says nothing.
				*st = clusterState{restarts: map[string]int32{}, findings: map[string]FindingFacts{}}
			}
			if e.ClusterUID != "" {
				st.uid = e.ClusterUID
			}
			st.primed = true
			switch e.Kind {
			case KindPhase:
				st.phase = e.To
			case KindPrimary:
				st.primary = e.To
			case KindFinding:
				st.primedFinds = true
				// After an eviction the journal may have lost when a still
				// open finding appeared: a finding not primed is then
				// first observed, not "appeared while the console was gone".
				st.known = !r.j.ScopeEvicted(e.Namespace, e.Cluster)
				if e.Action == ActionCleared {
					delete(st.findings, e.Ref)
				} else {
					st.findings[e.Ref] = FindingFacts{ID: e.Ref, Check: e.Check, SubjectKind: e.SubjectKind, Subject: e.Subject, Severity: e.Severity, Summary: e.Detail}
				}
			case KindRestart, KindBackup, KindGap, KindConsole:
				// Excluded by the enclosing case.
			}
		case KindGap:
			key := e.Namespace + "/" + e.Ref
			if e.Action == ActionBegan {
				r.sources[key] = sourceState{state: e.To, since: e.At}
			} else {
				delete(r.sources, key)
			}
		case KindRestart, KindBackup, KindConsole:
			// Point events: no state to prime.
		}
	}
}

// gapSince is the start of the unobserved window for a change found on
// the first observation after a restart.
func (r *Recorder) gapSince() time.Time { return r.previousAlive }

// Observe compares one Cluster's facts with the previous observation.
func (r *Recorder) Observe(f ClusterFacts) {
	now := r.j.Now()
	key := cache.ClusterKey{Namespace: f.Namespace, Name: f.Name}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(key)
	if st.uid != "" && st.uid != f.UID {
		// A recreated Cluster: a new incarnation starts from nothing.
		*st = clusterState{restarts: map[string]int32{}, findings: map[string]FindingFacts{}}
	}
	first := !st.observedOnce
	st.uid, st.observedOnce = f.UID, true
	base := Event{Namespace: f.Namespace, Cluster: f.Name, ClusterUID: f.UID}

	r.phase(st, f, base, now, first)
	r.primary(st, f, base, now, first)
	r.restarts(st, f, base, now)
	r.backups(f, base, now)
	st.primed = false
}

func (r *Recorder) phase(st *clusterState, f ClusterFacts, base Event, now time.Time, first bool) {
	if f.Phase != "" {
		st.recent = append(st.recent, seenPhase{phase: f.Phase, at: now})
	}
	kept := st.recent[:0]
	for _, p := range st.recent {
		if now.Sub(p.at) <= failoverWindow {
			kept = append(kept, p)
		}
	}
	st.recent = kept
	if f.Phase == st.phase {
		return
	}
	e := base
	e.Kind, e.At, e.Timing, e.From, e.To, e.Detail = KindPhase, now, TimingObserved, st.phase, f.Phase, f.PhaseReason
	switch {
	case st.phase == "":
		// Nothing known before (new, recreated, or evicted from the
		// journal): no change can be claimed.
		e.Action = ActionFirstObserved
	case first && st.primed:
		e.Action, e.AfterGap, e.Since = ActionChanged, true, r.gapSince()
	default:
		e.Action = ActionChanged
	}
	st.phase = f.Phase
	if f.Phase != "" || e.From != "" {
		r.j.Record(e)
	}
}

func (r *Recorder) primary(st *clusterState, f ClusterFacts, base Event, now time.Time, first bool) {
	if f.CurrentPrimary == "" || f.CurrentPrimary == st.primary {
		return
	}
	e := base
	e.Kind, e.From, e.To, e.SubjectKind, e.Subject = KindPrimary, st.primary, f.CurrentPrimary, "Pod", f.CurrentPrimary
	e.At, e.Timing = now, TimingObserved
	if !f.CurrentPrimaryTimestamp.IsZero() {
		e.At, e.Timing = f.CurrentPrimaryTimestamp, TimingReported
		e.Key = fmt.Sprintf("primary/%s/%s/%d", f.UID, f.CurrentPrimary, f.CurrentPrimaryTimestamp.Unix())
	}
	if st.primary == "" {
		e.Action = ActionFirstObserved
	} else {
		e.Action, e.Uncertain = classifyPrimaryChange(st.recent)
		if first && st.primed {
			// Changed while the console was gone: its cause was not seen.
			e.Action, e.Uncertain = ActionChanged, true
			e.AfterGap, e.Since = true, r.gapSince()
		}
	}
	st.primary = f.CurrentPrimary
	// The next primary change is caused by what is seen from now on.
	st.recent = st.recent[:0]
	r.j.Record(e)
}

// classifyPrimaryChange names the cause of a primary change from the
// phases seen since the previous one: the most recent of "Failing over"
// and "Switchover in progress". It is uncertain when neither was seen
// (the change fell between two observations) or when both were (several
// changes may have been coalesced into one observation).
func classifyPrimaryChange(recent []seenPhase) (action string, uncertain bool) {
	failover, switchover := false, false
	action = ""
	for i := len(recent) - 1; i >= 0; i-- {
		switch recent[i].phase {
		case "Failing over":
			failover = true
			if action == "" {
				action = ActionFailover
			}
		case "Switchover in progress":
			switchover = true
			if action == "" {
				action = ActionSwitchover
			}
		}
	}
	if action == "" {
		return ActionChanged, true
	}
	return action, failover && switchover
}

func (r *Recorder) restarts(st *clusterState, f ClusterFacts, base Event, now time.Time) {
	live := map[string]bool{}
	for _, p := range f.Pods {
		live[p.UID] = true
		known, seen := st.restarts[p.UID]
		st.restarts[p.UID] = p.Restarts
		if p.Restarts == 0 || (seen && p.Restarts <= known) {
			continue
		}
		if !seen && p.LastFinishedAt.IsZero() {
			// Restarted before the console looked, at an unknown time.
			continue
		}
		e := base
		e.Kind, e.Action, e.SubjectKind, e.Subject = KindRestart, ActionRestarted, "Pod", p.Name
		e.Key = "restart/" + p.UID + "/" + strconv.Itoa(int(p.Restarts))
		e.From, e.To = strconv.Itoa(int(known)), strconv.Itoa(int(p.Restarts))
		if !seen {
			e.From = ""
		}
		e.At, e.Timing = now, TimingObserved
		if !p.LastFinishedAt.IsZero() {
			e.At, e.Timing = p.LastFinishedAt, TimingReported
		}
		if p.LastReason != "" {
			e.Detail = p.LastReason + " (exit code " + strconv.Itoa(int(p.LastExitCode)) + ")"
		}
		r.j.Record(e)
	}
	for uid := range st.restarts {
		if !live[uid] {
			delete(st.restarts, uid)
		}
	}
}

func (r *Recorder) backups(f ClusterFacts, base Event, now time.Time) {
	done := make([]BackupFacts, 0, len(f.Backups))
	for _, b := range f.Backups {
		if b.Phase == "completed" || b.Phase == "failed" {
			done = append(done, b)
		}
	}
	// Oldest first, so a bounded journal keeps the newest.
	sort.SliceStable(done, func(i, j int) bool { return done[i].StoppedAt.Before(done[j].StoppedAt) })
	for _, b := range done {
		e := base
		e.Kind, e.SubjectKind, e.Subject, e.Key = KindBackup, "Backup", b.Name, "backup/"+b.UID+"/"+b.Phase
		e.Action = ActionCompleted
		if b.Phase == "failed" {
			e.Action, e.Detail = ActionFailed, b.Error
		}
		e.At, e.Timing = now, TimingObserved
		if !b.StoppedAt.IsZero() {
			e.At, e.Timing = b.StoppedAt, TimingReported
		}
		r.j.Record(e)
	}
}

// Findings compares one diagnostics run with the previous one. A failed
// run says nothing; a finding whose check could not run (unavailable)
// is not cleared, because its absence is the observer's, not the
// Cluster's.
func (r *Recorder) Findings(key cache.ClusterKey, at time.Time, findings []FindingFacts, unavailable map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(key)
	now := map[string]FindingFacts{}
	for _, f := range findings {
		now[f.ID] = f
	}
	base := Event{Namespace: key.Namespace, Cluster: key.Name, ClusterUID: st.uid, Kind: KindFinding, At: at.UTC(), Timing: TimingObserved}
	firstAfterRestart := st.primedFinds
	ids := make([]string, 0, len(now))
	for id := range now {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		f := now[id]
		prev, open := st.findings[id]
		e := base
		e.Ref, e.Check, e.SubjectKind, e.Subject, e.Severity, e.Detail = id, f.Check, f.SubjectKind, f.Subject, f.Severity, f.Summary
		switch {
		case !open && !st.known:
			e.Action = ActionFirstObserved
		case !open:
			e.Action = ActionAppeared
			if firstAfterRestart {
				e.AfterGap, e.Since = true, r.gapSince()
			}
		case severityRank[f.Severity] > severityRank[prev.Severity]:
			e.Action, e.From, e.To = ActionWorsened, prev.Severity, f.Severity
		case severityRank[f.Severity] < severityRank[prev.Severity]:
			e.Action, e.From, e.To = ActionImproved, prev.Severity, f.Severity
		default:
			continue
		}
		r.j.Record(e)
	}
	gone := make([]string, 0)
	for id, prev := range st.findings {
		if _, still := now[id]; !still && !unavailable[prev.Check] {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, id := range gone {
		prev := st.findings[id]
		e := base
		e.Action, e.Ref, e.Check, e.SubjectKind, e.Subject, e.Severity, e.Detail = ActionCleared, id, prev.Check, prev.SubjectKind, prev.Subject, prev.Severity, prev.Summary
		if firstAfterRestart {
			e.AfterGap, e.Since = true, r.gapSince()
		}
		r.j.Record(e)
		delete(st.findings, id)
	}
	for id, f := range now {
		st.findings[id] = f
	}
	st.known, st.primedFinds = true, false
}

// Sources compares the sources' states with the previous pass. Pending
// is a source still delivering its first observation and is not a gap;
// disabled and notInstalled are decisions, not gaps.
func (r *Recorder) Sources(states []SourceFacts) {
	now := r.j.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range states {
		key := s.Namespace + "/" + s.Source
		prev, open := r.sources[key]
		gap := s.State == "stale" || s.State == "unavailable" || s.State == "forbidden"
		e := Event{Namespace: s.Namespace, Kind: KindGap, Ref: s.Source, At: now, Timing: TimingObserved}
		switch {
		case gap && !open:
			e.Action, e.To, e.Detail = ActionBegan, s.State, s.Reason
			if s.Namespace == "" {
				// A console-wide source's reason can be any error text (a
				// failed run, a transport error): the journal and the
				// digest keep a fixed sentence; the log has the rest.
				e.Detail = consoleGapDetail[s.State]
			}
			r.sources[key] = sourceState{state: s.State, since: now}
			r.j.Record(e)
		case !gap && open && s.State == "live":
			e.Action, e.From, e.To, e.Since = ActionEnded, prev.state, s.State, prev.since
			delete(r.sources, key)
			r.j.Record(e)
		}
	}
}

// consoleGapDetail is the fixed text of a console-wide gap, by state.
var consoleGapDetail = map[string]string{
	"stale":       "not refreshed in time",
	"unavailable": "could not be read or run",
	"forbidden":   "not permitted",
}

// FactSource is the read model as the recorder reads it.
type FactSource interface {
	Keys(ctx context.Context) []cache.ClusterKey
	ClusterFacts(ctx context.Context, key cache.ClusterKey) (ClusterFacts, bool)
	SourceFacts() []SourceFacts
}

// Changes is the informer cache's change feed.
type Changes interface {
	Subscribe() *cache.Subscription
	Unsubscribe(*cache.Subscription)
}

// passInterval is the full pass that catches what the change feed did
// not deliver and polls the source states.
const passInterval = 30 * time.Second

// Run observes every Cluster on start, on each change and on every
// pass, until ctx ends.
func (r *Recorder) Run(ctx context.Context, src FactSource, changes Changes) error {
	sub := changes.Subscribe()
	defer changes.Unsubscribe(sub)
	ticker := time.NewTicker(passInterval)
	defer ticker.Stop()
	r.pass(ctx, src, nil)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.pass(ctx, src, nil)
		case <-sub.Ready():
			r.pass(ctx, src, sub.Drain())
		}
	}
}

// pass observes the given keys (namespace-wide keys expand), or every
// Cluster when keys is nil, then the sources.
func (r *Recorder) pass(ctx context.Context, src FactSource, keys []cache.ClusterKey) {
	all := src.Keys(ctx)
	targets := all
	if keys != nil {
		want := map[cache.ClusterKey]bool{}
		nsWide := map[string]bool{}
		for _, k := range keys {
			if k.Name == "" {
				nsWide[k.Namespace] = true
			} else {
				want[k] = true
			}
		}
		targets = targets[:0:0]
		for _, k := range all {
			if want[k] || nsWide[k.Namespace] {
				targets = append(targets, k)
			}
		}
	} else {
		present := map[cache.ClusterKey]bool{}
		for _, k := range all {
			present[k] = true
		}
		r.mu.Lock()
		for k := range r.clusters {
			if !present[k] && r.clusters[k].observedOnce {
				delete(r.clusters, k)
			}
		}
		r.mu.Unlock()
	}
	for _, k := range targets {
		if ctx.Err() != nil {
			return
		}
		if f, ok := src.ClusterFacts(ctx, k); ok {
			r.Observe(f)
		}
	}
	r.Sources(src.SourceFacts())
}
