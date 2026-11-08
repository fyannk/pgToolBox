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

// Package digest makes the console's daily summary: at a
// configured UTC hour, what changed in the last 24 hours across the
// team's Clusters — revisions, findings that appeared, worsened or
// cleared, failovers, failed backups, restarts — and which Clusters need
// attention now. Digests are kept for a retention (30 days by default)
// beside the history, so they survive restarts when it does, and are
// optionally delivered to a chat webhook, a generic webhook or by mail:
// a summary and a link only, never the details, because a channel's
// members are not the people RBAC lets see the Clusters.
//
// The digest reads only what the console already holds (the timeline
// and the cluster list); delivery runs on its own goroutines with
// bounded timeouts and retries, and a failure is reported on the admin
// page, never blocking anything else.
package digest

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/timeline"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// IDLayout is the digest ID: its end time, UTC, minute precision.
const IDLayout = "20060102T1504Z"

// Window is the span a digest covers.
const Window = 24 * time.Hour

// maxEntriesPerCluster bounds one Cluster's entries in a digest, and
// maxEntries the whole digest's: entries are listed for the Clusters
// that need attention only, worst first; every Cluster keeps its counts.
const (
	maxEntriesPerCluster = 50
	maxEntries           = 1000
)

// The reasons a digest is partial (viewmodel.DigestSummary.PartialReasons).
const (
	// PartialNotObserved: the console's memory begins after the window did.
	PartialNotObserved = "notObserved"
	// PartialConsoleRestarted: the console was down during the window.
	PartialConsoleRestarted = "consoleRestarted"
	// PartialSourceGap: a source the timeline rests on was not live
	// during the window.
	PartialSourceGap = "sourceGap"
	// PartialClustersUnreadable: a namespace's Clusters cannot be read
	// now, so the digest cannot list them.
	PartialClustersUnreadable = "clustersUnreadable"
	// PartialNoTimeline: there is no timeline to read.
	PartialNoTimeline = "noTimeline"
)

// maxStored bounds the digests kept whatever the retention says.
const maxStored = 400

// Source is the read model as the digest reads it.
type Source interface {
	ClusterList(ctx context.Context) (viewmodel.ClusterList, error)
	// DigestEntries is every Cluster's digest news in [from, to), newest
	// first, bucketed by Cluster: one journal query and one history
	// snapshot for the whole fleet, never one per Cluster.
	DigestEntries(ctx context.Context, from, to time.Time) map[viewmodel.ClusterRef][]viewmodel.TimelineEntry
	// SourceFacts are the states of the sources the timeline rests on.
	SourceFacts() []timeline.SourceFacts
}

// Journal is the timeline journal's query.
type Journal interface {
	Query(q timeline.Query) timeline.Result
}

// Clock is time for the scheduler (observe.RealClock).
type Clock interface {
	Now() time.Time
	Wait(ctx context.Context, d time.Duration) error
}

// Persister mirrors the digests (timeline.File).
type Persister interface {
	LoadDigests() ([][]byte, error)
	PutDigest(id string, raw []byte)
	DeleteDigest(id string)
}

// Generate makes the digest of the window ending at `to`, at now.
func Generate(ctx context.Context, src Source, j Journal, to time.Time, now time.Time) (viewmodel.Digest, error) {
	to, now = to.UTC(), now.UTC()
	from := to.Add(-Window)
	d := viewmodel.Digest{
		DigestSummary: viewmodel.DigestSummary{ID: to.Format(IDLayout), From: from, To: to, GeneratedAt: now, Attention: []viewmodel.ClusterRef{}},
		ConsoleGaps:   []viewmodel.TimelineEntry{},
		Clusters:      []viewmodel.DigestCluster{},
	}
	list, err := src.ClusterList(ctx)
	if err != nil {
		return viewmodel.Digest{}, err
	}
	reasons := map[string]bool{}
	for _, f := range src.SourceFacts() {
		if f.Namespace != "" && f.Source == clustersSource && f.State != string(viewmodel.SourceLive) {
			reasons[PartialClustersUnreadable] = true
		}
	}
	// The list itself says where its Clusters could not be read, even
	// on a live informer: a failed list or a Cluster read that failed.
	for _, ns := range list.Namespaces {
		for _, st := range ns.Sources {
			if st.Source == clustersSource && st.State != viewmodel.SourceLive {
				reasons[PartialClustersUnreadable] = true
			}
		}
	}
	if j == nil {
		reasons[PartialNoTimeline] = true
	} else {
		coverage(j, from, to, now, &d, reasons)
	}
	byCluster := src.DigestEntries(ctx, from, to)
	for _, row := range list.Items {
		d.Clusters = append(d.Clusters, cluster(row, byCluster[row.Ref]))
	}
	sort.SliceStable(d.Clusters, func(a, b int) bool {
		if d.Clusters[a].Attention != d.Clusters[b].Attention {
			return d.Clusters[a].Attention
		}
		ra, rb := healthRank[d.Clusters[a].Health], healthRank[d.Clusters[b].Health]
		if ra != rb {
			return ra > rb
		}
		return refLess(d.Clusters[a].Ref, d.Clusters[b].Ref)
	})
	budget := maxEntries
	for i := range d.Clusters {
		c := &d.Clusters[i]
		if len(c.Entries) > budget {
			c.Entries, c.Truncated = c.Entries[:budget], true
		}
		budget -= len(c.Entries)
	}
	for _, r := range []string{PartialNotObserved, PartialConsoleRestarted, PartialSourceGap, PartialClustersUnreadable, PartialNoTimeline} {
		if reasons[r] {
			d.PartialReasons = append(d.PartialReasons, r)
		}
	}
	d.Partial = len(d.PartialReasons) > 0
	Summarize(&d)
	return d, nil
}

// clustersSource is cache.SourceClusters (the digest does not import the
// cache).
const clustersSource = "clusters"

// coverage says what of [from, to) the console did not observe: its
// memory beginning late, a start whose previous life ended before `to`,
// and a source gap overlapping the window — including one that opened
// before `from` and one still open. It lists the console-wide entries
// inside the window for the page.
func coverage(j Journal, from, to, now time.Time, d *viewmodel.Digest, reasons map[string]bool) {
	if all := j.Query(timeline.Query{Limit: 1}); !all.Oldest.IsZero() {
		oldest := all.Oldest
		d.ObservedSince = &oldest
		if oldest.After(from) {
			reasons[PartialNotObserved] = true
		}
	} else {
		reasons[PartialNotObserved] = true
	}
	// Starts up to now: a restart after `to` whose previous life ended
	// before it still cost the window its end.
	for _, e := range j.Query(timeline.Query{From: from, Kinds: map[timeline.Kind]bool{timeline.KindConsole: true}}).Events {
		if e.Action == timeline.ActionStarted && e.At.After(from) && e.Since.Before(to) {
			reasons[PartialConsoleRestarted] = true
		}
	}
	// Every gap event up to now, oldest first, paired per source.
	gaps := j.Query(timeline.Query{To: now.Add(time.Nanosecond), Kinds: map[timeline.Kind]bool{timeline.KindGap: true}}).Events
	open := map[string]time.Time{}
	overlaps := func(began, ended time.Time) bool { return began.Before(to) && ended.After(from) }
	for i := len(gaps) - 1; i >= 0; i-- {
		e := gaps[i]
		key := e.Namespace + "/" + e.Ref
		switch e.Action {
		case timeline.ActionBegan:
			if _, already := open[key]; !already {
				open[key] = e.At
			}
		case timeline.ActionEnded:
			began, known := open[key]
			if !known {
				began = e.Since // the began event was evicted; the end says when
			}
			delete(open, key)
			if overlaps(began, e.At) {
				reasons[PartialSourceGap] = true
			}
		}
	}
	for _, began := range open {
		if began.Before(to) {
			reasons[PartialSourceGap] = true
		}
	}
	for _, e := range j.Query(timeline.Query{From: from, To: to, Kinds: map[timeline.Kind]bool{timeline.KindConsole: true, timeline.KindGap: true}}).Events {
		if e.Cluster == "" {
			d.ConsoleGaps = append(d.ConsoleGaps, gapEntry(e))
		}
	}
}

func refLess(a, b viewmodel.ClusterRef) bool {
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	return a.Name < b.Name
}

// healthRank orders verdicts for the attention list, worst first.
var healthRank = map[viewmodel.Health]int{
	viewmodel.HealthCritical: 5, viewmodel.HealthWarning: 4, viewmodel.HealthUnknown: 3,
	viewmodel.HealthProgressing: 2, viewmodel.HealthInactive: 1,
}

// needsAttention: a verdict a person should look at. Unknown is one:
// the console cannot tell, which is not the same as fine.
func needsAttention(h viewmodel.Health) bool {
	return h == viewmodel.HealthCritical || h == viewmodel.HealthWarning || h == viewmodel.HealthUnknown
}

func cluster(row viewmodel.ClusterRow, entries []viewmodel.TimelineEntry) viewmodel.DigestCluster {
	c := viewmodel.DigestCluster{
		Ref: row.Ref, Health: row.Verdict.Health, Attention: needsAttention(row.Verdict.Health),
		Reasons: []string{}, Entries: []viewmodel.TimelineEntry{},
	}
	for _, contributor := range row.Verdict.Contributors {
		if needsAttention(contributor.Health) && len(c.Reasons) < 3 {
			c.Reasons = append(c.Reasons, contributor.Label)
		}
	}
	for _, e := range entries {
		if !count(&c.Counts, e) || !c.Attention {
			continue // a Cluster that needs no attention carries its counts only
		}
		if len(c.Entries) == maxEntriesPerCluster {
			c.Truncated = true
			continue
		}
		c.Entries = append(c.Entries, e)
	}
	return c
}

// count adds e to the counts and reports whether the digest lists it.
func count(n *viewmodel.DigestCounts, e viewmodel.TimelineEntry) bool {
	switch e.Kind {
	case "revision":
		// The Cluster's own spec changes; its other objects' revisions
		// are on its history page, not in the digest.
		if e.Action != "spec-changed" || e.Subject == nil || e.Subject.Kind != "Cluster" {
			return false
		}
		n.Revisions++
	case "finding":
		switch e.Action {
		case timeline.ActionAppeared:
			n.NewFindings++
		case timeline.ActionWorsened:
			n.WorseFindings++
		case timeline.ActionCleared:
			n.ClearedFindings++
		default:
			return false
		}
	case "primary":
		switch e.Action {
		case timeline.ActionFailover:
			n.Failovers++
		case timeline.ActionSwitchover:
			n.Switchovers++
		case timeline.ActionChanged:
		default:
			return false
		}
	case "backup":
		if e.Action != timeline.ActionFailed {
			return false
		}
		n.FailedBackups++
	case "restart":
		n.Restarts++
	default:
		return false
	}
	return true
}

func gapEntry(e timeline.Event) viewmodel.TimelineEntry {
	out := viewmodel.TimelineEntry{
		ID: "j:" + strconv.FormatUint(e.Seq, 10), Kind: string(e.Kind), Action: e.Action, At: e.At, Timing: string(e.Timing),
		From: e.From, To: e.To, Ref: e.Ref, Detail: e.Detail, Health: viewmodel.HealthInfo, Scope: "console",
		Provenance: viewmodel.Provenance{Origin: viewmodel.OriginConsole, Source: "console"},
	}
	if e.Namespace != "" {
		out.Scope = "namespace"
		out.Subject = &viewmodel.ObjectRef{Kind: "Namespace", Name: e.Namespace}
	}
	if !e.Since.IsZero() {
		t := e.Since
		out.Since = &t
	}
	switch {
	case e.Kind == timeline.KindGap && e.Action == timeline.ActionBegan:
		out.Health = viewmodel.HealthUnknown
	case e.Kind == timeline.KindGap:
		out.Health = viewmodel.HealthOK
	}
	return out
}

// Summarize recomputes the headline (viewmodel.Digest.Recount).
func Summarize(d *viewmodel.Digest) { d.Recount() }

// record is a stored digest with its delivery marks: which channels
// delivered it, and when. They are persisted with the digest, so a
// restart neither sends it again nor forgets a channel that had not.
type record struct {
	viewmodel.Digest
	Sent map[string]time.Time `json:"sent,omitempty"`
}

// Store keeps the digests within the retention, newest last.
type Store struct {
	mu        sync.RWMutex
	items     []record
	retention time.Duration
	persist   Persister
	logger    *slog.Logger
}

// NewStore returns a store, loaded from p when set. A digest the file
// holds but that does not decode is skipped and logged: it is a summary,
// and losing one must not stop the console.
func NewStore(retention time.Duration, p Persister, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &Store{retention: retention, persist: p, logger: logger}
	if p == nil {
		return s, nil
	}
	raws, err := p.LoadDigests()
	if err != nil {
		return nil, err
	}
	for _, raw := range raws {
		var r record
		if err := json.Unmarshal(raw, &r); err != nil || r.ID == "" {
			logger.Warn("a stored digest could not be read and is skipped")
			continue
		}
		s.items = append(s.items, r)
	}
	sort.Slice(s.items, func(a, b int) bool { return s.items[a].To.Before(s.items[b].To) })
	return s, nil
}

// Add stores d (replacing a digest with the same ID, whose delivery
// marks it keeps) and drops what left the retention by now.
func (s *Store) Add(d viewmodel.Digest, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := sort.Search(len(s.items), func(i int) bool { return !s.items[i].To.Before(d.To) })
	if i < len(s.items) && s.items[i].ID == d.ID {
		s.items[i].Digest = d
	} else {
		s.items = append(s.items, record{})
		copy(s.items[i+1:], s.items[i:])
		s.items[i] = record{Digest: d}
	}
	s.save(&s.items[i])
	s.prune(now)
}

// MarkSent records that channel delivered the digest id at `at`.
func (s *Store) MarkSent(id, channel string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			if s.items[i].Sent == nil {
				s.items[i].Sent = map[string]time.Time{}
			}
			s.items[i].Sent[channel] = at.UTC()
			s.save(&s.items[i])
			return
		}
	}
}

// Sent returns when channel delivered the digest id.
func (s *Store) Sent(id, channel string) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.items {
		if s.items[i].ID == id {
			at, ok := s.items[i].Sent[channel]
			return at, ok
		}
	}
	return time.Time{}, false
}

func (s *Store) save(r *record) {
	if s.persist == nil {
		return
	}
	if raw, err := json.Marshal(r); err == nil {
		s.persist.PutDigest(r.ID, raw)
	}
}

func (s *Store) prune(now time.Time) {
	cut := 0
	for cut < len(s.items) && (now.Sub(s.items[cut].To) > s.retention || len(s.items)-cut > maxStored) {
		if s.persist != nil {
			s.persist.DeleteDigest(s.items[cut].ID)
		}
		cut++
	}
	s.items = s.items[cut:]
}

// All returns the digests, newest first (copies).
func (s *Store) All() []viewmodel.Digest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]viewmodel.Digest, 0, len(s.items))
	for i := len(s.items) - 1; i >= 0; i-- {
		out = append(out, s.items[i].Digest)
	}
	return out
}

// Get returns one digest.
func (s *Store) Get(id string) (viewmodel.Digest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.items {
		if s.items[i].ID == id {
			return s.items[i].Digest, true
		}
	}
	return viewmodel.Digest{}, false
}

// Has reports whether a digest ends at to.
func (s *Store) Has(to time.Time) bool {
	_, ok := s.Get(to.UTC().Format(IDLayout))
	return ok
}

// ErrNotFound is returned for an unknown digest ID.
var ErrNotFound = errors.New("no such digest")
