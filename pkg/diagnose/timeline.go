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

package diagnose

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
)

// The conditions here read the object timeline, which is the one input
// that carries the past rather than the present. Everything else the
// engine reads is a current snapshot, so a state that is wrong only
// because it keeps happening — a pod replaced over and over, a
// definition rewritten by two controllers in turn — is invisible to
// every other check. Nothing here needs state of its own: the store
// already retains the timeline, and a run just counts what is in it.
//
// Two properties of that store shape every claim made from it. It
// coalesces consecutive status transitions of one object inside a
// window, and it evicts old revisions to stay inside its bounds. Both
// mean a count taken from it is a floor: the timeline can under-report
// what happened and never over-report it. So a match here is sound, and
// an absence rules nothing out — the same footing as the log checks,
// and every rule built on these conditions says so.

// objectKey identifies one object across its incarnations: everything
// that makes it that object except the identity a replacement changes.
// Grouping by name alone would merge objects that merely share a name —
// a Pooler and the Service the operator names after it — into one
// inflated count, so the whole coordinate is the key and the UID
// deliberately is not part of it.
type objectKey struct {
	Group, Version, Kind string
	Namespace, Name      string
}

// String renders the key for ordering.
func (k objectKey) String() string {
	return k.Group + "/" + k.Version + "/" + k.Kind + "/" + k.Namespace + "/" + k.Name
}

// windowed is the entries of one bounded kind observed inside the
// trailing window, grouped by object in a stable order.
func windowed(in Input, kind string, span time.Duration) ([]objectKey, map[objectKey][]history.Entry) {
	cutoff := in.Now.Add(-span)
	byObject := map[objectKey][]history.Entry{}
	for _, entry := range in.History.Entries {
		if kind != "" && entry.Kind != kind {
			continue
		}
		if entry.ObservedAt.Before(cutoff) {
			continue
		}
		key := objectKey{
			Group: entry.Group, Version: entry.Version, Kind: entry.Kind,
			Namespace: entry.Namespace, Name: entry.Name,
		}
		byObject[key] = append(byObject[key], entry)
	}
	keys := make([]objectKey, 0, len(byObject))
	for key := range byObject {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(a, b int) bool { return keys[a].String() < keys[b].String() })
	return keys, byObject
}

// gapNote reports how many of the entries were discovered only after a
// contact gap, whose timing is therefore bounded rather than exact.
func gapNote(entries []history.Entry) string {
	gapped := 0
	for _, entry := range entries {
		if entry.AfterGap {
			gapped++
		}
	}
	if gapped == 0 {
		return ""
	}
	return fmt.Sprintf(", %d of them discovered after a contact gap, so their timing is bounded rather than exact", gapped)
}

// newest is the most recent observation time among the entries.
func newest(entries []history.Entry) time.Time {
	var at time.Time
	for _, entry := range entries {
		if entry.ObservedAt.After(at) {
			at = entry.ObservedAt
		}
	}
	return at
}

// HistoryIncarnations matches an object name the timeline has seen
// under several distinct identities inside the window — the same name
// created, destroyed and created again. Counting identities rather than
// change records is what separates a replacement from an edit: a pod
// that is modified stays one object, while a pod that is replaced is a
// new one wearing the old one's name.
type HistoryIncarnations struct {
	// Kind bounds the objects considered; empty considers every kind
	// the timeline records.
	Kind string
	// Identities is the number of distinct identities that matches. It
	// is one more than the replacements it implies: going from one
	// identity to the next is one replacement, so three identities
	// inside the window prove two replacements happened inside it.
	Identities int
	// Within is the trailing window.
	Within time.Duration
}

func (c HistoryIncarnations) describe() string {
	subject := "an object"
	if c.Kind != "" {
		subject = "a " + c.Kind
	}
	return fmt.Sprintf("%s seen under at least %d distinct identities within %s",
		subject, c.Identities, c.Within)
}

func (c HistoryIncarnations) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := historyUnavailable(in); reason != "" {
		return nil, reason
	}
	objects, byObject := windowed(in, c.Kind, c.Within)
	var matches []conditionMatch
	for _, object := range objects {
		entries := byObject[object]
		seen := map[string]bool{}
		for _, entry := range entries {
			if entry.UID != "" {
				seen[entry.UID] = true
			}
		}
		if len(seen) < c.Identities {
			continue
		}
		// Each step from one identity to the next is one replacement,
		// and the first identity may itself have replaced one from
		// before the window — so this is a floor, and says so.
		replacements := len(seen) - 1
		matches = append(matches, conditionMatch{
			idSuffix: "/" + strings.ToLower(object.Kind) + "/" + object.Name,
			subject:  EntityRef{Kind: object.Kind, Name: object.Name},
			at:       newest(entries),
			summary: fmt.Sprintf("%s %s has been replaced at least %d times in the last %s.",
				object.Kind, object.Name, replacements, c.Within),
			evidence: []Evidence{{
				Origin: "console-observed timeline",
				Object: object.Kind + "/" + object.Name,
				Detail: fmt.Sprintf("%d distinct object identities under this name since %s%s",
					len(seen), in.Now.Add(-c.Within).UTC().Format("15:04:05Z"), gapNote(entries)),
			}},
			link:      "/history",
			linkLabel: "History",
		})
	}
	return matches, ""
}

// HistoryChanges matches an object the timeline records changing too
// often inside the window. The changes that count are named by the
// rule, because the kinds mean different things: a definition rewritten
// again and again is two writers disagreeing, while a status moving
// again and again is the operator reconciling.
type HistoryChanges struct {
	// Kind bounds the objects considered; empty considers every kind.
	Kind string
	// Changes are the change classifications that count.
	Changes []history.Change
	// Count is the number of matching records that matches.
	Count int
	// Within is the trailing window.
	Within time.Duration
}

func (c HistoryChanges) describe() string {
	subject := "an object"
	if c.Kind != "" {
		subject = "a " + c.Kind
	}
	kinds := make([]string, len(c.Changes))
	for i, change := range c.Changes {
		kinds[i] = string(change)
	}
	return fmt.Sprintf("%s with at least %d %s records within %s",
		subject, c.Count, strings.Join(kinds, " or "), c.Within)
}

func (c HistoryChanges) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := historyUnavailable(in); reason != "" {
		return nil, reason
	}
	objects, byObject := windowed(in, c.Kind, c.Within)
	var matches []conditionMatch
	for _, object := range objects {
		var counted []history.Entry
		for _, entry := range byObject[object] {
			for _, change := range c.Changes {
				if entry.Change == change {
					counted = append(counted, entry)
					break
				}
			}
		}
		if len(counted) < c.Count {
			continue
		}
		detail := fmt.Sprintf("%d records since %s%s", len(counted),
			in.Now.Add(-c.Within).UTC().Format("15:04:05Z"), gapNote(counted))
		evidence := []Evidence{{
			Origin: "console-observed timeline",
			Object: object.Kind + "/" + object.Name,
			Detail: detail,
		}}
		if actors := actorsOf(counted); actors != "" {
			// The field manager is the API server's own attribution of
			// who wrote, which is the whole answer when the question is
			// what keeps rewriting this.
			evidence = append(evidence, Evidence{
				Origin: "Kubernetes-reported",
				Object: object.Kind + "/" + object.Name,
				Detail: "last-touching field managers: " + actors,
			})
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + strings.ToLower(object.Kind) + "/" + object.Name,
			subject:  EntityRef{Kind: object.Kind, Name: object.Name},
			at:       newest(counted),
			summary: fmt.Sprintf("%s %s changed %d times in the last %s.",
				object.Kind, object.Name, len(counted), c.Within),
			evidence:  evidence,
			link:      "/history",
			linkLabel: "History",
		})
	}
	return matches, ""
}

// actorsOf lists the distinct field managers that touched the entries,
// most frequent first, empty when none was attributed.
func actorsOf(entries []history.Entry) string {
	counts := map[string]int{}
	for _, entry := range entries {
		if entry.Actor.Manager != "" {
			counts[entry.Actor.Manager]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	managers := make([]string, 0, len(counts))
	for manager := range counts {
		managers = append(managers, manager)
	}
	sort.Slice(managers, func(a, b int) bool {
		if counts[managers[a]] != counts[managers[b]] {
			return counts[managers[a]] > counts[managers[b]]
		}
		return managers[a] < managers[b]
	})
	parts := make([]string, len(managers))
	for i, manager := range managers {
		parts[i] = fmt.Sprintf("%s (%d)", manager, counts[manager])
	}
	return strings.Join(parts, ", ")
}
