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

// Package logstream follows the member pods' container logs continuously
// and offers each line to sinks that decide what, if anything, to keep.
//
// It exists because two things the console needs are only in the logs:
// the operator writes some refusals nowhere else, and a container that
// has died takes its explanation with it.
//
// The design point is that the two consumers keep very different amounts:
//
//   - The matcher analyses each line once and keeps only what matched —
//     a bounded set of observations. Its memory does not grow with log
//     volume, so it is on whenever streaming is.
//   - The buffer keeps recent lines verbatim so a screen can show them.
//     That is a standing corpus of log text, which for PostgreSQL can
//     include statements and their literal values, so it is off by
//     default and bounded in bytes rather than lines.
//
// Everything here is best effort and says so. A follow stream drops
// lines across reconnects, rotation, and container restarts, and
// Kubernetes offers no way to detect what was missed. A gap is therefore
// recorded as an explicit marker rather than papered over, and nothing
// in this package reports a count as though it were complete.
package logstream

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// Line is one observed log line.
type Line struct {
	// Pod and Container name where it came from.
	Pod       string
	Container string
	// Text is the line without its trailing newline, bounded by the
	// reader before it reaches any sink.
	Text string
	// At is when the console observed the line, not when it was written.
	// The console's clock is the only one it can vouch for; a timestamp
	// inside the text is the database's claim and stays part of Text.
	At time.Time
}

// Sink consumes lines as they arrive. Implementations must be safe for
// concurrent use: one goroutine per container feeds them.
type Sink interface {
	// Observe takes one line. It must not block for long: the caller is
	// the reader of a live stream, and a slow sink loses lines rather
	// than delaying them.
	Observe(Line)
	// Gap records that lines were missed between two observations, with
	// the reason as far as it is known. Sinks that present content to a
	// reader must surface this rather than joining across it.
	Gap(pod, container string, at time.Time, reason string)
	// Attached, Detached and Dropped bracket the windows in which a
	// container's stream is actually being read. Gap says the record has
	// a hole in it; these say whether one is open right now, which is a
	// different question and the one a sink must answer before it lets a
	// check report that it looked and found nothing.
	//
	// Attached means a stream is open and lines are arriving. Detached
	// means none is, with the reason as far as it is known — including
	// before the first attach, when nothing from the container has been
	// read at all. Dropped means the container is no longer followed and
	// its coverage should be forgotten rather than left standing open.
	Attached(pod, container string, at time.Time)
	Detached(pod, container string, at time.Time, reason string)
	Dropped(pod, container string)
}

// Unread is one container whose stream is not being read at this
// moment, with when the blind window opened and the reason as far as
// the follower knows it.
//
// It is the log record's answer to the question every other source
// answers with a staleness flag: not "was something missed once" —
// following is best effort and every reconnect misses something — but
// "is the console reading this container now". A check that would
// report nothing wrong has to ask, because a rule looking for a line
// cannot tell a container that never said it from one the console
// stopped listening to.
type Unread struct {
	Pod, Container string
	// Since is when the current blind window opened. It survives
	// reconnect attempts that fail, so a container whose stream will not
	// open reports how long that has been true rather than restarting
	// its clock on every retry.
	Since time.Time
	// Reason is the follower's latest account of why nothing is being
	// read. The latest rather than the first, because after a failed
	// reconnect the newer one describes why the window is still open.
	Reason string
}

// FieldTest is one test against a named field of a structured log line.
// The operator writes JSON, so a rule that knows which field carries the
// string it looks for can say so instead of searching the whole line:
// matching "no free disk space" anywhere in a line also matches a line
// quoting that phrase back inside some other field, while matching it in
// the field that carries it does not.
//
// Exactly one of Equals and Contains is set. Equals is for a message the
// component writes whole; Contains is for one it formats around a value,
// where only the fixed part can be matched.
type FieldTest struct {
	// Path is the field, dotted from the root: "msg", or
	// "record.error_severity". No wildcards and no indexing — the same
	// reasoning that keeps matching to substrings keeps paths literal.
	Path string
	// Equals matches the field's exact value.
	Equals string
	// Contains matches a substring of the field's value.
	Contains string
}

// Valid reports whether the test states exactly one question about one
// named field. A test that states none, or two, is malformed rather
// than lenient: guessing which of them was meant is how a rule ends up
// matching something nobody declared.
func (t FieldTest) Valid() bool {
	if t.Path == "" {
		return false
	}
	return (t.Equals == "") != (t.Contains == "")
}

// holds reports whether the test passes against a decoded line. A
// malformed test never holds, so a mis-specified rule fires on nothing
// rather than on the half of itself that happens to be set. A field
// that is absent, or whose value is not a string, does not match
// either: this asks about text the component wrote, and anything else
// is a different question that would need a different test.
func (t FieldTest) holds(doc map[string]any) bool {
	if !t.Valid() {
		return false
	}
	value, ok := fieldValue(doc, t.Path)
	if !ok {
		return false
	}
	if t.Equals != "" {
		return value == t.Equals
	}
	return strings.Contains(value, t.Contains)
}

// fieldValue walks a dotted path to a string leaf.
func fieldValue(doc map[string]any, path string) (string, bool) {
	if doc == nil || path == "" {
		return "", false
	}
	var current any = doc
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = object[segment]
		if !ok {
			return "", false
		}
	}
	text, ok := current.(string)
	return text, ok
}

// Rule is one thing worth noticing in a log line.
//
// Matching is by substring or by the value of a named field,
// deliberately, and never by regular expression. The lines this looks
// for are fixed strings the operator emits, neither test can be made
// pathological by a hostile log line, and a rule that cannot express
// something clever is a rule that cannot quietly match the wrong thing.
type Rule struct {
	// ID is the stable identifier a finding is reported under.
	ID string
	// Contains are the substrings that must all appear in a line for it
	// to match. Several are allowed so a rule can be specific without a
	// pattern language.
	Contains []string
	// Except are substrings any one of which withdraws the match. They
	// exist for a rule that must stay broad — a severity marker matches
	// routine lifecycle chatter along with real faults — and they carry
	// the same guarantee as Contains: a substring cannot be made
	// pathological by a hostile line, and cannot quietly match more than
	// it says.
	Except []string
	// Fields are tests against named fields, all of which must hold. A
	// rule declaring any needs the line to decode as a JSON object; one
	// that does not decode matches no field rule, which is correct — a
	// line the component did not write in its structured format is not
	// a line whose fields can be read.
	Fields []FieldTest
	// Summary states what the match means, in plain language.
	Summary string
}

// matches reports whether the line satisfies every substring and every
// field test. The decoded line is passed in because one line is decoded
// once for the whole rule set, not once per rule.
func (r Rule) matches(text string, doc map[string]any) bool {
	if len(r.Contains) == 0 && len(r.Fields) == 0 {
		return false
	}
	for _, needle := range r.Contains {
		if !strings.Contains(text, needle) {
			return false
		}
	}
	for _, test := range r.Fields {
		if !test.holds(doc) {
			return false
		}
	}
	for _, benign := range r.Except {
		if strings.Contains(text, benign) {
			return false
		}
	}
	return true
}

// structured reports whether any rule reads fields, so a line is
// decoded only for a rule set that asks for it.
func structured(rules []Rule) bool {
	for _, rule := range rules {
		if len(rule.Fields) > 0 {
			return true
		}
	}
	return false
}

// Observation is one rule's account of what it has seen on one
// container. It is what "analyse a line once" leaves behind: the line
// that matched, kept once, plus how often the rule has fired since.
type Observation struct {
	// RuleID identifies the rule that matched.
	RuleID string
	// Summary is the rule's plain-language statement.
	Summary string
	// Pod and Container locate it.
	Pod       string
	Container string
	// Line is the first matching line, bounded. The first rather than
	// the most recent: the earliest occurrence is the one nearest the
	// cause, and a later one is usually the same failure repeating.
	Line string
	// FirstSeen and LastSeen bracket the occurrences.
	FirstSeen time.Time
	LastSeen  time.Time
	// Count is how many lines matched. It is a floor, never a total:
	// the stream is best effort, so lines were possibly missed.
	Count int
}

// maxObservedLine bounds a retained matching line. Long enough for the
// operator's messages, short enough that a hostile line cannot make one
// observation large.
const maxObservedLine = 2048

// Matcher analyses each line once against a closed rule set and retains
// only what matched.
//
// Its memory is bounded by the number of rules times the number of
// containers, independent of how much is logged, which is what makes it
// safe to run continuously.
type Matcher struct {
	rules  []Rule
	decode bool
	maxAge time.Duration
	now    func() time.Time

	mu       sync.RWMutex
	observed map[observationKey]*Observation
	// unread is the containers no stream is currently open for. It is
	// kept beside the observations because the two answer one question
	// together: what matched, and what the console was not in a position
	// to see.
	unread map[Member]Unread
}

type observationKey struct{ rule, pod, container string }

// NewMatcher builds a matcher over a closed rule set. Observations
// expire maxAge after their last matching line: a finding is a claim
// about what the logs say now, and a line nothing has repeated since
// yesterday stops being that. A maxAge of zero, or a nil clock, retains
// observations until the container is forgotten.
func NewMatcher(rules []Rule, maxAge time.Duration, now func() time.Time) *Matcher {
	return &Matcher{
		rules: rules, decode: structured(rules), maxAge: maxAge, now: now,
		observed: map[observationKey]*Observation{}, unread: map[Member]Unread{},
	}
}

// Observe analyses one line and keeps only a match.
func (m *Matcher) Observe(line Line) {
	// One decode for the whole rule set, and only when some rule reads
	// fields. A line that is not a JSON object decodes to nothing,
	// which every field test then declines.
	var doc map[string]any
	if m.decode {
		if err := json.Unmarshal([]byte(line.Text), &doc); err != nil {
			doc = nil
		}
	}
	for _, rule := range m.rules {
		if !rule.matches(line.Text, doc) {
			continue
		}
		key := observationKey{rule.ID, line.Pod, line.Container}
		m.mu.Lock()
		if existing, ok := m.observed[key]; ok {
			existing.Count++
			existing.LastSeen = line.At
		} else {
			text := line.Text
			if len(text) > maxObservedLine {
				text = text[:maxObservedLine]
			}
			m.observed[key] = &Observation{
				RuleID: rule.ID, Summary: rule.Summary,
				Pod: line.Pod, Container: line.Container, Line: text,
				FirstSeen: line.At, LastSeen: line.At, Count: 1,
			}
		}
		m.mu.Unlock()
	}
}

// Gap is a no-op for the matcher. A missed line cannot be analysed, and
// recording that some unknown line went unanalysed would add noise to
// every observation without making any of them more true. What a hole
// in the record does mean for the matcher is carried by Detached
// instead, which is about the window rather than the line.
func (m *Matcher) Gap(string, string, time.Time, string) {}

// Attached closes a container's blind window: lines are arriving again,
// so what the matcher does not hold from here is what was not said.
func (m *Matcher) Attached(pod, container string, _ time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.unread, Member{Pod: pod, Container: container})
}

// Detached opens one, or extends the one already open. The window's
// start is kept from the first detachment rather than moved by each
// failed reconnect: a stream that will not open has been unread since it
// first went, and restarting the clock every two seconds would report a
// half-hour outage as a new one.
func (m *Matcher) Detached(pod, container string, at time.Time, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	member := Member{Pod: pod, Container: container}
	window := Unread{Pod: pod, Container: container, Since: at, Reason: reason}
	if open, already := m.unread[member]; already {
		window.Since = open.Since
	}
	m.unread[member] = window
}

// Dropped forgets a container's coverage. A container that is no longer
// followed is not one the console is blind to: it is gone, and holding
// its window open would make every log check unavailable for as long as
// the cluster remembered a pod that had been replaced.
func (m *Matcher) Dropped(pod, container string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.unread, Member{Pod: pod, Container: container})
}

// Unread returns the containers no stream is currently open for, in a
// stable order.
func (m *Matcher) Unread() []Unread {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Unread, 0, len(m.unread))
	for _, window := range m.unread {
		out = append(out, window)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pod != out[j].Pod {
			return out[i].Pod < out[j].Pod
		}
		return out[i].Container < out[j].Container
	})
	return out
}

// Observations returns a stable copy, ordered by rule then pod then
// container so a refresh that changes nothing does not reorder them.
func (m *Matcher) Observations() []Observation {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Expiry happens at read time, under the write lock, so the map
	// never accumulates dead entries past the next read. An expired
	// observation is dropped entirely: a stale finding that cannot
	// clear teaches a reader to ignore the screen.
	if m.maxAge > 0 && m.now != nil {
		horizon := m.now().Add(-m.maxAge)
		for key, observation := range m.observed {
			if observation.LastSeen.Before(horizon) {
				delete(m.observed, key)
			}
		}
	}
	out := make([]Observation, 0, len(m.observed))
	for _, observation := range m.observed {
		out = append(out, *observation)
	}
	sortObservations(out)
	return out
}

// Export copies the observations out for a snapshot, expired ones
// excluded, in the stable order of Observations. The blind windows are
// not exported: whether a stream is open is a fact about this process.
func (m *Matcher) Export() []Observation { return m.Observations() }

// maxImported bounds what Import takes: every rule on 32 pods of a few
// containers each, with room to spare.
const maxImported = 4096

// Import restores observations from a snapshot, at startup and before
// any line is observed. An observation of a rule this matcher does not
// run, with no rule ID, pod or container, with LastSeen before
// FirstSeen, after now or older than the matcher's maxAge at now is
// dropped: the snapshot is a file on a volume, not a trusted source,
// and an observation past its age would be expired on the next read
// anyway. Lines are bounded again, and the rule's current summary
// replaces the stored one.
func (m *Matcher) Import(observations []Observation, now time.Time) int {
	summaries := make(map[string]string, len(m.rules))
	for _, rule := range m.rules {
		summaries[rule.ID] = rule.Summary
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	imported := 0
	for _, o := range observations {
		summary, known := summaries[o.RuleID]
		switch {
		case imported >= maxImported, !known, o.Pod == "", o.Container == "", o.Count < 1,
			o.LastSeen.Before(o.FirstSeen), o.LastSeen.After(now),
			m.maxAge > 0 && o.LastSeen.Before(now.Add(-m.maxAge)):
			continue
		}
		key := observationKey{o.RuleID, o.Pod, o.Container}
		if _, dup := m.observed[key]; dup {
			continue
		}
		if len(o.Line) > maxObservedLine {
			o.Line = o.Line[:maxObservedLine]
		}
		o.Summary = summary
		copied := o
		m.observed[key] = &copied
		imported++
	}
	return imported
}

// Forget drops observations for a container that no longer exists, so a
// replaced pod does not leave its findings behind forever.
func (m *Matcher) Forget(pod string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.observed {
		if key.pod == pod {
			delete(m.observed, key)
		}
	}
	// Its coverage goes with it: a pod that no longer exists is not one
	// the console is failing to read.
	for member := range m.unread {
		if member.Pod == pod {
			delete(m.unread, member)
		}
	}
}
