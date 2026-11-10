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

package sharedstate

import (
	"encoding/json"
	"log/slog"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// KeyDrills holds the results of the restore drills that finished: what
// a restart without a history volume would otherwise lose. Only the
// outcome is kept (drill, source, backup, result, times, the console's
// own reason), never a check's detail or any credential: the drill's
// full record stays in the history journal when there is one.
const KeyDrills = "drills.json"

// Bounds of the drill results: per source and in total. A person starts
// each drill, so this is years of drills, and the document stays a small
// share of the budget (about 300 bytes a result).
const (
	maxDrillsPerSource = 50
	maxDrills          = 500
	maxDrillName       = 253
	maxDrillReason     = 512
)

// DrillResult is one finished restore drill as the shared state keeps
// it. Times are Unix seconds.
type DrillResult struct {
	Namespace       string `json:"ns"`
	Name            string `json:"name"`
	SourceNamespace string `json:"srcNs"`
	Source          string `json:"src"`
	SourceUID       string `json:"srcUid,omitempty"`
	Backup          string `json:"backup,omitempty"`
	BackupLabel     string `json:"label,omitempty"`
	Result          string `json:"result"`
	Reason          string `json:"reason,omitempty"`
	StartedAt       int64  `json:"started"`
	Deadline        int64  `json:"deadline,omitempty"`
	HealthyAt       int64  `json:"healthy,omitempty"`
	FinishedAt      int64  `json:"finished"`
}

type drillsDoc struct {
	Version int `json:"version"`
	// Since is when the document started keeping drill results (Unix
	// seconds); the earliest one any replica wrote wins.
	Since   int64                  `json:"since,omitempty"`
	Results map[string]DrillResult `json:"results"`
}

func drillKey(namespace, name string) string { return namespace + "/" + name }

// finishedResults are the results this console writes; a running drill
// is never kept here (its sealed Cluster carries it).
var finishedResults = map[string]bool{
	string(viewmodel.DrillPassed): true, string(viewmodel.DrillFailed): true,
	string(viewmodel.DrillTimedOut): true, string(viewmodel.DrillCancelled): true,
}

func validDrill(key string, r DrillResult) bool {
	name := func(s string) bool { return s != "" && len(s) <= maxDrillName }
	return name(r.Namespace) && name(r.Name) && name(r.SourceNamespace) && name(r.Source) &&
		key == drillKey(r.Namespace, r.Name) && finishedResults[r.Result] &&
		len(r.Backup) <= maxDrillName && len(r.SourceUID) <= maxDrillName && len(r.BackupLabel) <= maxDrillName &&
		len(r.Reason) <= maxDrillReason && utf8.ValidString(r.Reason) &&
		r.StartedAt > 0 && r.FinishedAt >= r.StartedAt
}

// newerDrill orders two results of one drill: the later finish wins,
// then the larger encoding, so every replica keeps the same one.
func newerDrill(a, b DrillResult) bool {
	if a.FinishedAt != b.FinishedAt {
		return a.FinishedAt > b.FinishedAt
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) > string(jb)
}

// KeepDrill shares the result of a finished drill. An unfinished or
// malformed result is ignored; the reason is cut to its bound.
func (s *Store) KeepDrill(r DrillResult) {
	if !s.Enabled() {
		return
	}
	if len(r.Reason) > maxDrillReason {
		cut := maxDrillReason
		for cut > 0 && !utf8.RuneStart(r.Reason[cut]) {
			cut--
		}
		r.Reason = r.Reason[:cut]
	}
	key := drillKey(r.Namespace, r.Name)
	if !validDrill(key, r) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.drills[key]; ok && !newerDrill(r, prev) {
		return
	}
	s.drills[key] = r
	if s.drillsSince == 0 || r.StartedAt < s.drillsSince {
		s.drillsSince = min(r.StartedAt, s.opts.Now().Unix())
	}
	s.boundDrillsLocked()
	// Urgent: the result is the evidence a restart would lose.
	s.changedLocked(true)
}

// StartKeepingDrills records now as the start of the drill results when
// none is known: drills that finish from then on are kept.
func (s *Store) StartKeepingDrills(now time.Time) {
	if !s.Enabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drillsSince == 0 {
		s.drillsSince = now.Unix()
		s.changedLocked(false)
	}
}

// KeptDrills returns the kept results, ordered by key.
func (s *Store) KeptDrills() []DrillResult {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DrillResult, 0, len(s.drills))
	for _, r := range s.drills {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return drillKey(out[i].Namespace, out[i].Name) < drillKey(out[j].Namespace, out[j].Name)
	})
	return out
}

// KeepsDrills reports whether drill results survive a restart here: a
// ConfigMap is configured and is neither missing nor forbidden. While it
// is pending or failing the results are held and written when it comes
// back.
func (s *Store) KeepsDrills() bool {
	if !s.Enabled() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode != modeMissing && s.mode != modeForbidden && !s.foreign[KeyDrills]
}

// DrillsKeptSince is when the drill results started being kept, zero
// when unknown.
func (s *Store) DrillsKeptSince() time.Time {
	if !s.Enabled() {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drillsSince == 0 {
		return time.Time{}
	}
	return time.Unix(s.drillsSince, 0).UTC()
}

// mergeDrillsLocked folds the ConfigMap's drill results in: union, the
// newer result of one drill wins, the earliest start of keeping wins.
func (s *Store) mergeDrillsLocked(raw string) {
	var doc drillsDoc
	switch {
	case foreignVersion(raw):
		s.foreign[KeyDrills] = true
	case json.Unmarshal([]byte(raw), &doc) != nil:
		s.opts.Logger.Warn("shared state: the drills document does not parse; it will be rewritten", slog.String("configmap", s.opts.Name))
	case len(doc.Results) <= maxEntries:
		for key, r := range doc.Results {
			if !validDrill(key, r) {
				continue
			}
			if prev, ok := s.drills[key]; !ok || newerDrill(r, prev) {
				s.drills[key] = r
			}
		}
		if doc.Since > 0 && (s.drillsSince == 0 || doc.Since < s.drillsSince) {
			s.drillsSince = doc.Since
		}
		s.boundDrillsLocked()
	}
}

// boundDrillsLocked drops the oldest results beyond the bounds, keeping
// each source's newest pass: dropping it would turn "passed 40 days ago"
// into "no pass recorded".
func (s *Store) boundDrillsLocked() {
	keys := make([]string, 0, len(s.drills))
	for key := range s.drills {
		keys = append(keys, key)
	}
	// Newest first.
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.drills[keys[i]], s.drills[keys[j]]
		if a.FinishedAt != b.FinishedAt {
			return a.FinishedAt > b.FinishedAt
		}
		return keys[i] < keys[j]
	})
	newestPass := map[string]string{}
	for _, key := range keys {
		r := s.drills[key]
		src := drillKey(r.SourceNamespace, r.Source)
		if _, ok := newestPass[src]; !ok && r.Result == string(viewmodel.DrillPassed) {
			newestPass[src] = key
		}
	}
	perSource := map[string]int{}
	total := 0
	for _, key := range keys {
		r := s.drills[key]
		src := drillKey(r.SourceNamespace, r.Source)
		perSource[src]++
		total++
		if (perSource[src] > maxDrillsPerSource || total > maxDrills) && newestPass[src] != key {
			delete(s.drills, key)
			perSource[src]--
			total--
		}
	}
}
