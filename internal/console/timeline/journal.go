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

// Package timeline keeps what the console saw happen and nothing else
// would remember: a Cluster's phase changes, primary
// changes, instance restarts, backup results, diagnostics findings
// appearing and clearing, and the windows in which the console itself
// was not looking. The revision history (internal/console/history)
// keeps the definitions; Events stay in the API server; this journal
// keeps the transitions between them, so "what happened at 03:12" has
// one answer after the Events have expired.
//
// The journal is bounded (a global cap and a per-Cluster cap, oldest
// first) and in memory, mirrored into a bbolt file beside the history
// journal when history is persistent, so it survives restarts exactly
// when the history does. Every event says how its time is known: the
// console's own clock when it observed the change, or a timestamp the
// operator or the kubelet reported.
package timeline

import (
	"sort"
	"sync"
	"time"
)

// Kind is the closed set of journal events.
type Kind string

// The event kinds.
const (
	// KindPhase is a change of Cluster.status.phase.
	KindPhase Kind = "phase"
	// KindPrimary is a change of Cluster.status.currentPrimary.
	KindPrimary Kind = "primary"
	// KindRestart is a restart of an instance's postgres container.
	KindRestart Kind = "restart"
	// KindBackup is a Backup reaching completed or failed.
	KindBackup Kind = "backup"
	// KindFinding is a diagnostics finding appearing, clearing, or
	// changing severity.
	KindFinding Kind = "finding"
	// KindGap is a source of the console leaving or regaining live.
	KindGap Kind = "gap"
	// KindConsole is the console process starting.
	KindConsole Kind = "console"
)

// Kinds lists every kind, in the order the API documents them.
var Kinds = []Kind{KindPhase, KindPrimary, KindRestart, KindBackup, KindFinding, KindGap, KindConsole}

// The actions, per kind.
const (
	ActionChanged       = "changed"       // phase, primary (cause not observed)
	ActionFirstObserved = "firstObserved" // phase, primary: seen, not seen change
	ActionFailover      = "failover"      // primary
	ActionSwitchover    = "switchover"    // primary
	ActionRestarted     = "restarted"     // restart
	ActionCompleted     = "completed"     // backup
	ActionFailed        = "failed"        // backup
	ActionAppeared      = "appeared"      // finding
	ActionCleared       = "cleared"       // finding
	ActionWorsened      = "worsened"      // finding
	ActionImproved      = "improved"      // finding
	ActionBegan         = "began"         // gap
	ActionEnded         = "ended"         // gap
	ActionStarted       = "started"       // console
)

// Timing says whose clock At is.
type Timing string

// The timings.
const (
	// TimingObserved is the console's clock when it observed the change:
	// the change happened at or shortly before At.
	TimingObserved Timing = "observed"
	// TimingReported is a timestamp the operator or the kubelet wrote
	// (status.currentPrimaryTimestamp, Backup.status.stoppedAt, a
	// container's finishedAt).
	TimingReported Timing = "reported"
)

// Event is one journal entry. Namespace and Cluster scope it: an event
// with no Cluster concerns every Cluster of its namespace (a source
// gap), one with no Namespace concerns the whole console.
type Event struct {
	Seq uint64 `json:"seq"`
	// Key deduplicates: an event whose key the journal holds is not
	// recorded again (a Backup result seen on every pass, a restart
	// already journaled before a console restart).
	Key    string    `json:"key,omitempty"`
	Kind   Kind      `json:"kind"`
	Action string    `json:"action"`
	At     time.Time `json:"at"`
	Timing Timing    `json:"timing"`
	// Since is the start of the window the change happened in when the
	// console was not looking (AfterGap), or when a gap began (on its
	// end), or when the previous console life was last alive (on a
	// start).
	Since     time.Time `json:"since,omitzero"`
	AfterGap  bool      `json:"afterGap,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
	Cluster   string    `json:"cluster,omitempty"`
	// ClusterUID is the Cluster's UID when the event concerns one, so a
	// recreated Cluster of the same name is not compared with the old.
	ClusterUID string `json:"clusterUid,omitempty"`
	// Uncertain marks a classification the console could not make for
	// sure (a primary change whose cause was not, or not only, seen).
	Uncertain bool `json:"uncertain,omitempty"`
	// SubjectKind and Subject name the object (Pod, Backup, the
	// finding's subject); empty for the Cluster itself.
	SubjectKind string `json:"subjectKind,omitempty"`
	Subject     string `json:"subject,omitempty"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	// Severity is a finding's severity after the change (before it, for
	// a cleared finding).
	Severity string `json:"severity,omitempty"`
	// Ref is the finding ID, or the source name of a gap.
	Ref string `json:"ref,omitempty"`
	// Check is a finding's check, so a later run whose check could not
	// run does not clear it.
	Check string `json:"check,omitempty"`
	// Detail is a bounded, secret-free sentence from the source (the
	// operator's phase reason, a finding's summary, an exit reason).
	Detail string `json:"detail,omitempty"`
}

// maxField bounds every string of an event.
const maxField = 512

func (e *Event) bound() {
	for _, s := range []*string{&e.Key, &e.Action, &e.Namespace, &e.Cluster, &e.ClusterUID, &e.SubjectKind, &e.Subject, &e.From, &e.To, &e.Severity, &e.Ref, &e.Check, &e.Detail} {
		if len(*s) > maxField {
			*s = (*s)[:maxField]
		}
	}
	e.At = e.At.UTC()
	if !e.Since.IsZero() {
		e.Since = e.Since.UTC()
	}
}

// Clock supplies time (observe.RealClock satisfies it).
type Clock interface {
	Now() time.Time
}

// Limits bound the journal. Zero is unbounded (tests).
type Limits struct {
	// MaxEvents bounds the retained events across all Clusters.
	MaxEvents int
	// PerCluster bounds the events of one Cluster (or of one namespace's
	// namespace-wide events), so a flapping Cluster cannot crowd the
	// quiet ones out.
	PerCluster int
}

// Persister mirrors the journal; mutation methods only enqueue (called
// under the journal's lock).
type Persister interface {
	LoadEvents() (events []Event, seq uint64, evicted bool, err error)
	PutEvent(e Event)
	DeleteEvent(seq uint64)
	MarkEvicted()
}

// Journal is the bounded event store.
type Journal struct {
	mu      sync.Mutex
	limits  Limits
	clock   Clock
	persist Persister

	seq     uint64
	events  []*Event // oldest first by Seq
	keys    map[string]bool
	scopes  map[string]int // events per scope key
	evicted bool
	// horizon is, per scope, the latest At of an event evicted from it:
	// nothing at or before it can be shown honestly any more.
	horizon map[string]time.Time
	// reloadedEvicted reports a journal loaded after evictions, whose
	// horizons were not persisted: every scope is treated as evicted up
	// to its oldest retained event.
	reloadedEvicted bool
}

// New returns an in-memory journal.
func New(limits Limits, clock Clock) *Journal {
	return &Journal{limits: limits, clock: clock, keys: map[string]bool{}, scopes: map[string]int{}, horizon: map[string]time.Time{}}
}

// NewPersisted returns a journal primed from p and mirrored into it. A
// load failure is returned: a deployment that kept history expects the
// timeline to survive too.
func NewPersisted(limits Limits, clock Clock, p Persister) (*Journal, error) {
	events, seq, evicted, err := p.LoadEvents()
	if err != nil {
		return nil, err
	}
	j := New(limits, clock)
	j.persist = p
	j.seq, j.evicted, j.reloadedEvicted = seq, evicted, evicted
	sort.Slice(events, func(a, b int) bool { return events[a].Seq < events[b].Seq })
	for i := range events {
		e := events[i]
		j.events = append(j.events, &e)
		j.index(&e, 1)
		if e.Seq > j.seq {
			j.seq = e.Seq
		}
	}
	j.enforce()
	return j, nil
}

func scopeOf(e *Event) string { return e.Namespace + "/" + e.Cluster }

func (j *Journal) index(e *Event, delta int) {
	j.scopes[scopeOf(e)] += delta
	if j.scopes[scopeOf(e)] <= 0 {
		delete(j.scopes, scopeOf(e))
	}
	if e.Key == "" {
		return
	}
	if delta > 0 {
		j.keys[e.Key] = true
	} else {
		delete(j.keys, e.Key)
	}
}

// Has reports whether an event with this key is retained.
func (j *Journal) Has(key string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.keys[key]
}

// Record appends e (its Seq is assigned) and reports whether it was
// kept: an event whose key is retained is a duplicate, and a keyed event
// older than everything its scope still retains after an eviction is
// outside the window the journal can honestly show, so it is dropped
// rather than re-added and evicted again on every pass.
func (j *Journal) Record(e Event) bool {
	e.bound()
	if e.Timing == "" {
		e.Timing = TimingObserved
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if e.Key != "" && j.keys[e.Key] {
		return false
	}
	if e.Key != "" && j.belowHorizon(&e) {
		return false
	}
	j.seq++
	e.Seq = j.seq
	j.events = append(j.events, &e)
	j.index(&e, 1)
	if j.persist != nil {
		j.persist.PutEvent(e)
	}
	j.enforce()
	return true
}

// belowHorizon reports a keyed event at or before its scope's
// horizon: the latest time of an event evicted from that scope. It is
// decided by time, not by sequence, because events dated by the
// operator or the kubelet arrive out of order.
func (j *Journal) belowHorizon(e *Event) bool {
	scope := scopeOf(e)
	if h, ok := j.horizon[scope]; ok {
		return !e.At.After(h)
	}
	if !j.reloadedEvicted {
		return false
	}
	// Loaded after evictions whose horizon was not kept: the oldest
	// retained event of the scope stands in for it.
	var oldest time.Time
	for _, kept := range j.events {
		if scopeOf(kept) == scope && (oldest.IsZero() || kept.At.Before(oldest)) {
			oldest = kept.At
		}
	}
	return !oldest.IsZero() && e.At.Before(oldest)
}

// ScopeEvicted reports whether events of one Cluster (or of a
// namespace's namespace-wide events) may have been evicted, so what the
// journal retains about it is not its whole story.
func (j *Journal) ScopeEvicted(namespace, cluster string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.horizon[namespace+"/"+cluster]
	return ok || j.reloadedEvicted
}

// enforce evicts oldest-first until both bounds hold.
func (j *Journal) enforce() {
	if j.limits.PerCluster > 0 {
		for scope, n := range j.scopes {
			for ; n > j.limits.PerCluster; n-- {
				for i, e := range j.events {
					if scopeOf(e) == scope {
						j.evict(i)
						break
					}
				}
			}
		}
	}
	for j.limits.MaxEvents > 0 && len(j.events) > j.limits.MaxEvents {
		j.evict(0)
	}
}

func (j *Journal) evict(i int) {
	e := j.events[i]
	j.events = append(j.events[:i], j.events[i+1:]...)
	j.index(e, -1)
	if h, ok := j.horizon[scopeOf(e)]; !ok || e.At.After(h) {
		j.horizon[scopeOf(e)] = e.At
	}
	if j.persist != nil {
		j.persist.DeleteEvent(e.Seq)
		if !j.evicted {
			j.persist.MarkEvicted()
		}
	}
	j.evicted = true
}

// Query selects events.
type Query struct {
	// Namespace and Cluster select one Cluster: its own events, its
	// namespace's namespace-wide events and the console-wide ones.
	// Both empty select everything (the digest).
	Namespace, Cluster string
	// From and To bound At (inclusive From, exclusive To); zero is open.
	From, To time.Time
	// Kinds selects kinds; empty is every kind.
	Kinds map[Kind]bool
	// Limit bounds the answer (newest first); zero is unbounded.
	Limit int
}

func (q *Query) matches(e *Event) bool {
	if q.Namespace != "" || q.Cluster != "" {
		switch {
		case e.Namespace == "":
		case e.Namespace != q.Namespace:
			return false
		case e.Cluster != "" && e.Cluster != q.Cluster:
			return false
		}
	}
	if !q.From.IsZero() && e.At.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && !e.At.Before(q.To) {
		return false
	}
	return len(q.Kinds) == 0 || q.Kinds[e.Kind]
}

// Result is a query's answer.
type Result struct {
	// Events are newest first by At (then Seq).
	Events []Event
	// Truncated reports that Limit cut the answer.
	Truncated bool
	// Evicted reports that bounds have dropped older events: the oldest
	// retained one is a budget, not the beginning.
	Evicted bool
	// Oldest is the oldest retained event's time across the journal;
	// zero when empty.
	Oldest time.Time
}

// Query answers q from a copy; the journal's state is never shared.
func (j *Journal) Query(q Query) Result {
	j.mu.Lock()
	out := Result{Evicted: j.evicted}
	for _, e := range j.events {
		if out.Oldest.IsZero() || e.At.Before(out.Oldest) {
			out.Oldest = e.At
		}
		if q.matches(e) {
			out.Events = append(out.Events, *e)
		}
	}
	j.mu.Unlock()
	sort.SliceStable(out.Events, func(a, b int) bool {
		if !out.Events[a].At.Equal(out.Events[b].At) {
			return out.Events[a].At.After(out.Events[b].At)
		}
		return out.Events[a].Seq > out.Events[b].Seq
	})
	if q.Limit > 0 && len(out.Events) > q.Limit {
		out.Events, out.Truncated = out.Events[:q.Limit], true
	}
	return out
}

// All returns every retained event, oldest first by Seq (priming).
func (j *Journal) All() []Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Event, len(j.events))
	for i, e := range j.events {
		out[i] = *e
	}
	return out
}

// Persistent reports whether the journal survives restarts.
func (j *Journal) Persistent() bool { return j != nil && j.persist != nil }

// Now is the journal's clock.
func (j *Journal) Now() time.Time { return j.clock.Now().UTC() }
