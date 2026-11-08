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

package insight

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// History cadence and retention. They are fixed rather than configured:
// each is the coarsest that still answers its question, which is what
// keeps a Cluster's history to a few tens of kilobytes.
const (
	// xidEvery and xidKeep: one transaction id sample a minute for two
	// hours (the rate is read over the last hour).
	xidEvery = time.Minute
	xidKeep  = 2 * time.Hour
	// RateWindow is the window the consumption rates average over, and
	// minRateWindow the shortest the console will extrapolate from.
	RateWindow    = time.Hour
	minRateWindow = 5 * time.Minute
	// tableEvery and tableKeep: dead tuples per table every five
	// minutes for an hour.
	tableEvery = 5 * time.Minute
	tableKeep  = time.Hour
	// minTrendSpan is the shortest window a keep-up verdict rests on.
	minTrendSpan = 15 * time.Minute
	// statementEvery and statementKeep: one statement snapshot an hour,
	// 26 of them: enough for "this hour" against "the same hour
	// yesterday".
	statementEvery = time.Hour
	statementKeep  = 26
	// RegressionNeeds is the history a regression comparison needs: a
	// one-hour window that ended at least 24 hours ago.
	RegressionNeeds = 25 * time.Hour
	// RegressionRatio is the mean-time ratio reported as a regression.
	RegressionRatio = 2.0
	// regressionMinCalls is the calls a window needs before its mean is
	// compared: a mean over two calls is an anecdote.
	regressionMinCalls = 10
	// maxTracked bounds the tables and databases with a history.
	maxTracked = 256
	// maxRegressions bounds the regressions listed.
	maxRegressions = 20
)

// InstanceReading is the latest Reading of one instance.
type InstanceReading struct {
	Instance string
	// At is when the console read it.
	At time.Time
	// DataAt is when the exporter ran the pack's queries
	// (cnpg_last_update_timestamp; it answers from a cache for
	// metricsQueriesTTL), never after At; At itself when the exporter
	// does not say (CloudNativePG before 1.28). Ages and samples use it.
	DataAt time.Time
	// Primary is set when the instance was the primary at that read.
	Primary bool
	Reading Reading
}

type xidSample struct {
	at   time.Time
	next float64
}

type mxidSample struct {
	at       time.Time
	age, min float64
}

type tableSample struct {
	at                    time.Time
	dead, threshold, runs *float64
}

type statementCounters struct{ calls, total float64 }

type statementSnapshot struct {
	at       time.Time
	instance string
	reset    float64
	// dealloc is pg_stat_statements_info.dealloc; -1 when not reported.
	dealloc float64
	stmts   map[string]statementCounters
}

// Tracker keeps one Cluster's latest readings and histories. It is safe
// for concurrent use; every method is in-memory and bounded.
type Tracker struct {
	mu        sync.Mutex
	instances map[string]InstanceReading
	xid       []xidSample
	mxid      map[string][]mxidSample
	tables    map[string][]tableSample
	stmts     []statementSnapshot
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{instances: map[string]InstanceReading{}, mxid: map[string][]mxidSample{}, tables: map[string][]tableSample{}}
}

// Observe records one instance's reading, read at at. Histories are fed
// by the primary only: the transaction id counter, table statistics and
// pg_stat_statements the pack reads are the primary's. Samples carry the
// exporter's run time (DataAt), so a rate is measured between two runs,
// not two reads; reading a cached run again adds nothing.
func (t *Tracker) Observe(instance string, at time.Time, r Reading, primary bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	dataAt := DataTime(r, at)
	prev, seen := t.instances[instance]
	t.instances[instance] = InstanceReading{Instance: instance, At: at, DataAt: dataAt, Primary: primary, Reading: r}
	if !primary || !r.Present() {
		return
	}
	if seen && prev.Primary && r.ExporterUpdatedAt != nil && !dataAt.After(prev.DataAt) {
		return
	}
	t.observeXID(dataAt, r)
	t.observeTables(dataAt, r)
	t.observeStatements(instance, dataAt, r)
}

// DataTime is when the reading's data was measured: the exporter's last
// run, never after the read; the read itself when the exporter does not
// say.
func DataTime(r Reading, readAt time.Time) time.Time {
	if r.ExporterUpdatedAt == nil || r.ExporterUpdatedAt.After(readAt) {
		return readAt
	}
	return *r.ExporterUpdatedAt
}

// Retain forgets the instances not named (pods that left the roster).
func (t *Tracker) Retain(names map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name := range t.instances {
		if !names[name] {
			delete(t.instances, name)
		}
	}
}

func (t *Tracker) observeXID(at time.Time, r Reading) {
	if r.NextXID != nil {
		n := len(t.xid)
		switch {
		case n > 0 && *r.NextXID < t.xid[n-1].next:
			// The counter went back: a restore or another cluster
			// behind the same name. The old samples describe nothing.
			t.xid = t.xid[:0]
			t.xid = append(t.xid, xidSample{at, *r.NextXID})
		case n == 0 || at.Sub(t.xid[n-1].at) >= xidEvery:
			t.xid = append(t.xid, xidSample{at, *r.NextXID})
		}
		t.xid = trimTimes(t.xid, at.Add(-xidKeep), func(s xidSample) time.Time { return s.at })
	}
	seen := map[string]bool{}
	for _, db := range r.Databases {
		if db.MXIDAge == nil || db.MinMXID == nil {
			continue
		}
		seen[db.Name] = true
		samples, tracked := t.mxid[db.Name]
		if !tracked && len(t.mxid) >= maxTracked {
			continue
		}
		if n := len(samples); n == 0 || at.Sub(samples[n-1].at) >= xidEvery {
			samples = append(samples, mxidSample{at, *db.MXIDAge, *db.MinMXID})
		}
		t.mxid[db.Name] = trimTimes(samples, at.Add(-xidKeep), func(s mxidSample) time.Time { return s.at })
	}
	for name := range t.mxid {
		if !seen[name] {
			delete(t.mxid, name)
		}
	}
}

func (t *Tracker) observeTables(at time.Time, r Reading) {
	seen := map[string]bool{}
	for _, tb := range r.Tables {
		k := tb.Key()
		seen[k] = true
		samples, tracked := t.tables[k]
		if !tracked && len(t.tables) >= maxTracked {
			continue
		}
		if n := len(samples); n == 0 || at.Sub(samples[n-1].at) >= tableEvery {
			samples = append(samples, tableSample{at: at, dead: tb.DeadTuples, threshold: tb.AutovacuumThreshold, runs: tb.AutovacuumCount})
		}
		t.tables[k] = trimTimes(samples, at.Add(-tableKeep), func(s tableSample) time.Time { return s.at })
	}
	// A table that left the top-N has a history with a hole in it; it
	// starts again if it comes back.
	for k := range t.tables {
		if !seen[k] {
			delete(t.tables, k)
		}
	}
}

func (t *Tracker) observeStatements(instance string, at time.Time, r Reading) {
	if len(r.Statements) == 0 {
		return
	}
	if n := len(t.stmts); n > 0 && at.Sub(t.stmts[n-1].at) < statementEvery {
		return
	}
	snap := statementSnapshot{at: at, instance: instance, dealloc: -1, stmts: make(map[string]statementCounters, len(r.Statements))}
	if r.StatementsInfo != nil && r.StatementsInfo.StatsResetUnix != nil {
		snap.reset = *r.StatementsInfo.StatsResetUnix
	}
	if r.StatementsInfo != nil && r.StatementsInfo.Dealloc != nil {
		snap.dealloc = *r.StatementsInfo.Dealloc
	}
	for _, s := range r.Statements {
		if s.Calls != nil && s.TotalMillis != nil {
			snap.stmts[s.Key()] = statementCounters{*s.Calls, *s.TotalMillis}
		}
	}
	t.stmts = append(t.stmts, snap)
	if len(t.stmts) > statementKeep {
		t.stmts = append(t.stmts[:0], t.stmts[len(t.stmts)-statementKeep:]...)
	}
}

func trimTimes[T any](s []T, cutoff time.Time, at func(T) time.Time) []T {
	i := 0
	for i < len(s) && at(s[i]).Before(cutoff) {
		i++
	}
	if i == 0 {
		return s
	}
	return append(s[:0], s[i:]...)
}

// Snapshot is a Cluster's insight state at one instant: the readings and
// what the console derived from its own retained samples.
type Snapshot struct {
	// Instances are the latest readings, by instance.
	Instances map[string]InstanceReading
	// Primary names the instance whose reading was the primary's at its
	// read; empty when none has been read.
	Primary string
	// XIDRate and MXIDRate are the consumption rates; nil when the
	// console has not kept samples over minRateWindow.
	XIDRate, MXIDRate *Rate
	// Trends are the autovacuum keep-up verdicts per table of the
	// primary's latest reading.
	Trends map[string]TableTrend
	// Statements is the regression comparison.
	Statements StatementComparison
}

// PrimaryReading returns the primary's reading.
func (s Snapshot) PrimaryReading() (InstanceReading, bool) {
	r, ok := s.Instances[s.Primary]
	return r, ok && s.Primary != ""
}

// Present reports whether any instance reports the pack.
func (s Snapshot) Present() bool {
	for _, r := range s.Instances {
		if r.Reading.Present() {
			return true
		}
	}
	return false
}

// Snapshot derives the Cluster's state at now.
func (t *Tracker) Snapshot(now time.Time) Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := Snapshot{Instances: make(map[string]InstanceReading, len(t.instances)), Trends: map[string]TableTrend{}}
	var newest time.Time
	for name, r := range t.instances {
		out.Instances[name] = r
		if r.Primary && r.Reading.Present() && r.At.After(newest) {
			out.Primary, newest = name, r.At
		}
	}
	out.XIDRate = t.xidRate()
	out.MXIDRate = t.mxidRate()
	if p, ok := out.Instances[out.Primary]; ok {
		for _, tb := range p.Reading.Tables {
			out.Trends[tb.Key()] = trend(t.tables[tb.Key()], tb)
		}
		out.Statements = t.compare(now, out.Primary, p.Reading, p.DataAt)
	} else {
		out.Statements = t.compare(now, "", Reading{}, time.Time{})
	}
	return out
}

// windowStart picks the oldest sample within RateWindow of the newest.
func windowStart[T any](s []T, at func(T) time.Time) (T, bool) {
	var zero T
	if len(s) < 2 {
		return zero, false
	}
	last := at(s[len(s)-1])
	for _, x := range s {
		if last.Sub(at(x)) <= RateWindow {
			if last.Sub(at(x)) < minRateWindow {
				return zero, false
			}
			return x, true
		}
	}
	return zero, false
}

func (t *Tracker) xidRate() *Rate {
	first, ok := windowStart(t.xid, func(s xidSample) time.Time { return s.at })
	if !ok {
		return nil
	}
	last := t.xid[len(t.xid)-1]
	w := last.at.Sub(first.at)
	return &Rate{PerSecond: (last.next - first.next) / w.Seconds(), Window: w, From: first.at, To: last.at}
}

// mxidRate is the multixact consumption: the growth of a database's
// mxid_age while its datminmxid did not move. The longest such window
// across databases wins; with none, the rate is unknown.
func (t *Tracker) mxidRate() *Rate {
	var best *Rate
	names := make([]string, 0, len(t.mxid))
	for name := range t.mxid {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := t.mxid[name]
		first, ok := windowStart(s, func(x mxidSample) time.Time { return x.at })
		if !ok {
			continue
		}
		last := s[len(s)-1]
		if first.min != last.min || last.age < first.age {
			continue
		}
		w := last.at.Sub(first.at)
		if best == nil || w > best.Window {
			best = &Rate{PerSecond: (last.age - first.age) / w.Seconds(), Window: w, From: first.at, To: last.at}
		}
	}
	return best
}

// KeepUp is the autovacuum verdict on one table.
type KeepUp string

const (
	// KeepUpUnknown means fewer than three samples over minTrendSpan.
	KeepUpUnknown KeepUp = "unknown"
	// KeepUpOK means dead tuples are not growing, or the table is under its
	// autovacuum threshold.
	KeepUpOK KeepUp = "keepingUp"
	// KeepUpNotStarted means past the threshold, dead tuples growing, and no
	// autovacuum completed on it in the window (workers busy elsewhere,
	// or each attempt cancelled by a conflicting lock).
	KeepUpNotStarted KeepUp = "notStarted"
	// KeepUpFallingBehind means autovacuum completed in the window and dead
	// tuples are still higher than at its start.
	KeepUpFallingBehind KeepUp = "fallingBehind"
	// KeepUpDisabled means the table sets autovacuum_enabled=false and is past
	// its threshold (only anti-wraparound autovacuum will visit it).
	KeepUpDisabled KeepUp = "disabled"
	// KeepUpNever means a temporary table; autovacuum never processes those.
	KeepUpNever KeepUp = "never"
)

// TableTrend is what the retained samples say about one table.
type TableTrend struct {
	Verdict KeepUp
	// Samples and Span describe the window the verdict rests on.
	Samples int
	Span    time.Duration
	// DeadFirst and DeadLast are the dead tuples at its ends.
	DeadFirst, DeadLast *float64
	// AutovacuumRuns are the autovacuum runs completed in the window;
	// nil when not reported.
	AutovacuumRuns *float64
}

func trend(s []tableSample, latest Table) TableTrend {
	tr := TableTrend{Verdict: KeepUpUnknown, Samples: len(s)}
	if latest.Kind == "temporary" {
		tr.Verdict = KeepUpNever
		return tr
	}
	past := latest.DeadTuples != nil && latest.AutovacuumThreshold != nil && *latest.DeadTuples > *latest.AutovacuumThreshold
	if latest.AutovacuumEnabled != nil && *latest.AutovacuumEnabled == 0 && past {
		tr.Verdict = KeepUpDisabled
	}
	if len(s) == 0 {
		return tr
	}
	first, last := s[0], s[len(s)-1]
	tr.Span = last.at.Sub(first.at)
	tr.DeadFirst, tr.DeadLast = first.dead, last.dead
	if first.runs != nil && last.runs != nil && *last.runs >= *first.runs {
		runs := *last.runs - *first.runs
		tr.AutovacuumRuns = &runs
	}
	if tr.Verdict == KeepUpDisabled {
		return tr
	}
	if len(s) < 3 || tr.Span < minTrendSpan || first.dead == nil || last.dead == nil {
		return tr
	}
	// Growing: up overall and up in at least two thirds of the steps,
	// so one burst between two vacuums does not read as a trend.
	rises, steps := 0, 0
	for i := 1; i < len(s); i++ {
		if s[i].dead == nil || s[i-1].dead == nil {
			continue
		}
		steps++
		if *s[i].dead > *s[i-1].dead {
			rises++
		}
	}
	growing := *last.dead > *first.dead && steps > 0 && rises*3 >= steps*2
	switch {
	case !growing || !past:
		tr.Verdict = KeepUpOK
	case tr.AutovacuumRuns != nil && *tr.AutovacuumRuns > 0:
		tr.Verdict = KeepUpFallingBehind
	default:
		tr.Verdict = KeepUpNotStarted
	}
	return tr
}

// Regression is one statement whose mean time grew.
type Regression struct {
	QueryID, Database, Role string
	// RecentMean and BaselineMean are milliseconds per call over the two
	// windows; RecentCalls and BaselineCalls their calls.
	RecentMean, BaselineMean   float64
	RecentCalls, BaselineCalls float64
	// RecentFrom/To and BaselineFrom/To bound the windows.
	RecentFrom, RecentTo, BaselineFrom, BaselineTo time.Time
	// Uncertain is set when pg_stat_statements evicted entries during
	// one of the windows (dealloc grew): an entry evicted and added back
	// restarts its counters, so its window can read short.
	Uncertain bool
}

// Ratio is RecentMean over BaselineMean.
func (r Regression) Ratio() float64 {
	if r.BaselineMean <= 0 {
		return 0
	}
	return r.RecentMean / r.BaselineMean
}

// StatementComparison is the regression check's account of itself.
type StatementComparison struct {
	// Covered is how far back the retained snapshots reach.
	Covered time.Duration
	// Comparable is set when a baseline window exists (RegressionNeeds
	// of history on the same instance without a statistics reset).
	Comparable bool
	// Reason says why not, when not.
	Reason string
	// Compared counts the statements with both windows measurable.
	Compared int
	// NoBaseline counts the statements of the current top N that were
	// not in the top N of any retained window a day ago: the console
	// keeps the top N only, so they have nothing to compare with.
	NoBaseline int
	// Restarted counts the statements whose counters went back within a
	// window (pg_stat_statements_reset for that entry, or eviction and
	// re-entry): not compared.
	Restarted int
	// Evicted is the entries pg_stat_statements evicted across the
	// windows compared (dealloc growth); nil when not reported.
	Evicted *float64
	// Regressions are the statements at or above RegressionRatio, the
	// costliest first (added milliseconds per hour).
	Regressions []Regression
}

// compare measures each statement's mean over the last hour against
// the same hour a day earlier. Snapshots are the primary's; a failover,
// a statistics reset or an eviction makes a window unmeasurable rather
// than wrong. Each statement's windows are searched in the history: one
// that entered the top N lately is compared from the snapshots that
// have it.
func (t *Tracker) compare(now time.Time, primary string, latest Reading, latestAt time.Time) StatementComparison {
	out := StatementComparison{}
	if len(t.stmts) == 0 {
		out.Reason = "the console has not read pg_stat_statements on this Cluster's primary yet"
		return out
	}
	out.Covered = now.Sub(t.stmts[0].at)
	if primary == "" || len(latest.Statements) == 0 {
		out.Reason = "the primary does not report pg_stat_statements now"
		return out
	}
	w := newStatementWindows(t.stmts, now, primary, latest, latestAt)
	if reason := w.unavailable(out.Covered); reason != "" {
		out.Reason = reason
		return out
	}
	out.Comparable = true
	var ev evictions
	for _, s := range latest.Statements {
		if s.Calls == nil || s.TotalMillis == nil {
			continue
		}
		recent, bFrom, bTo := w.find(s.Key())
		if recent == nil || bFrom == nil {
			out.NoBaseline++
			continue
		}
		reg, verdict := w.measure(s, recent, bFrom, bTo, &ev)
		switch verdict {
		case measureRestarted:
			out.Restarted++
			continue
		case measureTooFew:
			continue
		case measureCompared:
		}
		out.Compared++
		if reg.BaselineMean > 0 && reg.Ratio() >= RegressionRatio {
			out.Regressions = append(out.Regressions, reg)
		}
	}
	if ev.known {
		out.Evicted = &ev.total
	}
	sortRegressions(out.Regressions)
	if len(out.Regressions) > maxRegressions {
		out.Regressions = out.Regressions[:maxRegressions]
	}
	return out
}

// statementWindows is what decides which retained snapshots a statement
// can be measured between: a recent window ends at the latest reading, a
// baseline window lies a day back, and only snapshots of the current
// primary since its last statistics reset are usable.
type statementWindows struct {
	snaps         []statementSnapshot
	now, latestAt time.Time
	primary       string
	reset         float64
	// latestDealloc is the latest dealloc counter; -1 when not reported.
	latestDealloc float64
}

func newStatementWindows(snaps []statementSnapshot, now time.Time, primary string, latest Reading, latestAt time.Time) statementWindows {
	w := statementWindows{snaps: snaps, now: now, latestAt: latestAt, primary: primary, latestDealloc: -1}
	if latest.StatementsInfo != nil && latest.StatementsInfo.StatsResetUnix != nil {
		w.reset = *latest.StatementsInfo.StatsResetUnix
	}
	if latest.StatementsInfo != nil && latest.StatementsInfo.Dealloc != nil {
		w.latestDealloc = *latest.StatementsInfo.Dealloc
	}
	return w
}

func (w statementWindows) isRecent(s statementSnapshot) bool {
	return w.latestAt.Sub(s.at) >= 45*time.Minute
}

func (w statementWindows) isBaseline(s statementSnapshot) bool {
	return w.now.Sub(s.at) >= 24*time.Hour
}

func (w statementWindows) usable(s statementSnapshot) bool {
	return s.instance == w.primary && s.reset == w.reset
}

// unavailable is the reason no statement can have a window at all, or ""
// when some can: not enough history, a primary change, a reset.
func (w statementWindows) unavailable(covered time.Duration) string {
	var anyRecent, anyBaseline, sameInstance, sameReset bool
	for i, s := range w.snaps {
		anyRecent = anyRecent || w.isRecent(s)
		if i > 0 && w.isBaseline(s) {
			anyBaseline = true
			prev := w.snaps[i-1]
			if s.instance == w.primary && prev.instance == w.primary {
				sameInstance = true
				if w.usable(s) && w.usable(prev) {
					sameReset = true
				}
			}
		}
	}
	switch {
	case !anyRecent || !anyBaseline:
		return fmt.Sprintf("needs %d hours of retained samples on the same primary; the console has kept %s "+
			"(samples live in memory and restart empty)", int(RegressionNeeds/time.Hour), hoursMinutes(covered))
	case !sameInstance:
		return "the primary changed since the baseline window: pg_stat_statements is per instance"
	case !sameReset:
		return "pg_stat_statements was reset since the baseline window"
	}
	return ""
}

// find returns, for one statement, the newest usable recent snapshot that
// holds it and the newest usable baseline pair that both hold it; nil
// when the history has none.
func (w statementWindows) find(k string) (recent, bFrom, bTo *statementSnapshot) {
	for i := len(w.snaps) - 1; i >= 0; i-- {
		if _, ok := w.snaps[i].stmts[k]; ok && w.isRecent(w.snaps[i]) && w.usable(w.snaps[i]) {
			recent = &w.snaps[i]
			break
		}
	}
	for i := len(w.snaps) - 1; i >= 1; i-- {
		from, to := &w.snaps[i-1], &w.snaps[i]
		_, ok1 := from.stmts[k]
		_, ok2 := to.stmts[k]
		if ok1 && ok2 && w.isBaseline(*to) && w.usable(*from) && w.usable(*to) {
			return recent, from, to
		}
	}
	return recent, nil, nil
}

// measureVerdict is what measure made of one statement's two windows.
type measureVerdict int

const (
	// measureCompared: both windows measured.
	measureCompared measureVerdict = iota
	// measureRestarted: a counter went back within a window.
	measureRestarted
	// measureTooFew: too few calls in a window for a mean to mean much.
	measureTooFew
)

// measure computes one statement's recent and baseline means. Only a
// compared statement counts dealloc growth, which marks its regression
// uncertain: an evicted and re-entered entry restarts its counters.
func (w statementWindows) measure(s Statement, recent, bFrom, bTo *statementSnapshot, ev *evictions) (Regression, measureVerdict) {
	k := s.Key()
	r0, b0, b1 := recent.stmts[k], bFrom.stmts[k], bTo.stmts[k]
	rc, bc := *s.Calls-r0.calls, b1.calls-b0.calls
	rt, bt := *s.TotalMillis-r0.total, b1.total-b0.total
	if rc < 0 || bc < 0 || rt < 0 || bt < 0 {
		return Regression{}, measureRestarted
	}
	if rc < regressionMinCalls || bc < regressionMinCalls {
		return Regression{}, measureTooFew
	}
	uncertain := ev.grew(recent.dealloc, w.latestDealloc)
	uncertain = ev.grew(bFrom.dealloc, bTo.dealloc) || uncertain
	return Regression{
		QueryID: s.QueryID, Database: s.Database, Role: s.Role,
		RecentMean: rt / rc, BaselineMean: bt / bc, RecentCalls: rc, BaselineCalls: bc,
		RecentFrom: recent.at, RecentTo: w.latestAt, BaselineFrom: bFrom.at, BaselineTo: bTo.at,
		Uncertain: uncertain,
	}, measureCompared
}

// evictions sums the dealloc growth across the windows compared; known
// is false until one window had both ends reported.
type evictions struct {
	total float64
	known bool
}

// grew records one window's dealloc growth and says whether it grew.
func (e *evictions) grew(from, to float64) bool {
	if from < 0 || to < 0 {
		return false
	}
	e.known = true
	if to > from {
		e.total += to - from
		return true
	}
	return false
}

// sortRegressions puts the costliest first: the added milliseconds per
// hour, then the query id for a stable order.
func sortRegressions(regs []Regression) {
	sort.Slice(regs, func(i, j int) bool {
		a, b := regs[i], regs[j]
		ai := (a.RecentMean - a.BaselineMean) * a.RecentCalls / a.RecentTo.Sub(a.RecentFrom).Hours()
		bi := (b.RecentMean - b.BaselineMean) * b.RecentCalls / b.RecentTo.Sub(b.RecentFrom).Hours()
		if ai != bi {
			return ai > bi
		}
		return a.QueryID < b.QueryID
	})
}

// hoursMinutes renders a duration as "3 h 05 min", "12 min" or "none".
func hoursMinutes(d time.Duration) string {
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h > 0:
		return fmt.Sprintf("%d h %02d min", h, m)
	case m > 0:
		return fmt.Sprintf("%d min", m)
	}
	return "none"
}
