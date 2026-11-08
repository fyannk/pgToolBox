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

package history

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxDiffEntries bounds the changed paths one diff reports. The bound is
// the rendered contract: a diff larger than this carries a truncation
// flag rather than an unbounded list.
const MaxDiffEntries = 256

// maxDiffValueLen bounds one rendered before/after value in bytes; the
// full definition is always available through the revision itself.
const maxDiffValueLen = 256

// DiffOp classifies one diff entry.
type DiffOp string

// The diff operations.
const (
	// DiffAdded is a path present only in the newer revision.
	DiffAdded DiffOp = "added"
	// DiffRemoved is a path present only in the older revision.
	DiffRemoved DiffOp = "removed"
	// DiffChanged is a path whose value differs between the two.
	DiffChanged DiffOp = "changed"
)

// DiffEntry is one changed path between two revisions of an object.
type DiffEntry struct {
	// Path is the field path, in the shared field-path encoding.
	Path string
	// Op classifies the entry.
	Op DiffOp
	// Before and After are the bounded rendered JSON values; Before is
	// empty for an addition, After for a removal.
	Before, After string
	// Manager is the field manager that owned the path at the newer
	// revision — Kubernetes-reported, self-declared attribution. Empty
	// when no manager or more than one plausibly owns the path: an
	// ambiguous attribution is no attribution.
	Manager string
}

// Diff is the structural comparison of one revision against the previous
// retained definition of the same object. It is computed on demand and
// never stored.
type Diff struct {
	// Seq is the compared revision.
	Seq uint64
	// HasBase reports that a baseline existed. False for a first
	// observation, a deletion, or a baseline retention already evicted —
	// in each case there is honestly nothing to compare against.
	HasBase bool
	// BaseSeq is the baseline revision, valid when HasBase.
	BaseSeq uint64
	// BaseObservedAt is when the baseline was observed, valid when
	// HasBase.
	BaseObservedAt time.Time
	// Entries are the changed paths, ordered by path.
	Entries []DiffEntry
	// Truncated reports that the entry bound cut the list.
	Truncated bool
}

// Diff compares the revision at seq against the previous retained
// definition of the same object. The second return is false only when
// the sequence is unknown; a revision with nothing to compare against
// returns a Diff with HasBase false.
func (s *Store) Diff(seq uint64) (Diff, bool) {
	s.mu.Lock()
	i := sort.Search(len(s.entries), func(i int) bool { return s.entries[i].Seq >= seq })
	if i >= len(s.entries) || s.entries[i].Seq != seq {
		s.mu.Unlock()
		return Diff{}, false
	}
	rev := s.entries[i]
	diff := Diff{Seq: seq}
	if rev.Manifest == nil {
		s.mu.Unlock()
		return diff, true
	}
	var base *Revision
	for k := i - 1; k >= 0; k-- {
		if s.entries[k].UID == rev.UID && s.entries[k].Manifest != nil {
			base = s.entries[k]
			break
		}
	}
	if base == nil {
		s.mu.Unlock()
		return diff, true
	}
	diff.HasBase = true
	diff.BaseSeq = base.Seq
	diff.BaseObservedAt = base.ObservedAt
	// Manifests and owners are replaced wholesale, never mutated in
	// place, so the references stay valid outside the lock and the
	// parse-and-walk does not serialize the capture path.
	beforeManifest, afterManifest, owners := base.Manifest, rev.Manifest, rev.Owners
	s.mu.Unlock()

	var before, after any
	if err := json.Unmarshal(beforeManifest, &before); err != nil {
		return diff, true
	}
	if err := json.Unmarshal(afterManifest, &after); err != nil {
		return diff, true
	}
	d := &differ{}
	d.walk("", before, after)
	sort.Slice(d.entries, func(a, b int) bool { return d.entries[a].Path < d.entries[b].Path })
	for i := range d.entries {
		d.entries[i].Manager = attribute(d.entries[i].Path, owners)
	}
	diff.Entries = d.entries
	diff.Truncated = d.truncated
	return diff, true
}

// differ accumulates bounded diff entries.
type differ struct {
	entries   []DiffEntry
	truncated bool
}

// add appends one entry under the bound.
func (d *differ) add(entry DiffEntry) {
	if len(d.entries) >= MaxDiffEntries {
		d.truncated = true
		return
	}
	d.entries = append(d.entries, entry)
}

// walk compares two nodes structurally and emits entries at the
// mismatches.
func (d *differ) walk(path string, before, after any) {
	if beforeMap, ok := before.(map[string]any); ok {
		if afterMap, ok := after.(map[string]any); ok {
			d.walkMaps(path, beforeMap, afterMap)
			return
		}
	}
	if beforeList, ok := before.([]any); ok {
		if afterList, ok := after.([]any); ok {
			d.walkLists(path, beforeList, afterList)
			return
		}
	}
	if !jsonEqual(before, after) {
		d.add(DiffEntry{Path: path, Op: DiffChanged, Before: renderValue(before), After: renderValue(after)})
	}
}

// walkMaps compares the union of both key sets in sorted order.
func (d *differ) walkMaps(path string, before, after map[string]any) {
	keys := make([]string, 0, len(before)+len(after))
	for key := range before {
		keys = append(keys, key)
	}
	for key := range after {
		if _, shared := before[key]; !shared {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := FieldPathKey(path, key)
		beforeValue, inBefore := before[key]
		afterValue, inAfter := after[key]
		switch {
		case !inBefore:
			d.add(DiffEntry{Path: child, Op: DiffAdded, After: renderValue(afterValue)})
		case !inAfter:
			d.add(DiffEntry{Path: child, Op: DiffRemoved, Before: renderValue(beforeValue)})
		default:
			d.walk(child, beforeValue, afterValue)
		}
	}
}

// walkLists compares two lists. When both sides are associative on the
// Kubernetes "name" convention the match is by name, so a reordered
// container list is no change at all; otherwise the comparison is
// positional.
func (d *differ) walkLists(path string, before, after []any) {
	beforeNames, beforeNamed := namedElements(before)
	afterNames, afterNamed := namedElements(after)
	if beforeNamed && afterNamed {
		names := make([]string, 0, len(beforeNames)+len(afterNames))
		for name := range beforeNames {
			names = append(names, name)
		}
		for name := range afterNames {
			if _, shared := beforeNames[name]; !shared {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			child := FieldPathNamed(path, name)
			beforeValue, inBefore := beforeNames[name]
			afterValue, inAfter := afterNames[name]
			switch {
			case !inBefore:
				d.add(DiffEntry{Path: child, Op: DiffAdded, After: renderValue(afterValue)})
			case !inAfter:
				d.add(DiffEntry{Path: child, Op: DiffRemoved, Before: renderValue(beforeValue)})
			default:
				d.walk(child, beforeValue, afterValue)
			}
		}
		return
	}

	shared := len(before)
	if len(after) < shared {
		shared = len(after)
	}
	for i := 0; i < shared; i++ {
		d.walk(FieldPathIndex(path, i), before[i], after[i])
	}
	for i := shared; i < len(after); i++ {
		d.add(DiffEntry{Path: FieldPathIndex(path, i), Op: DiffAdded, After: renderValue(after[i])})
	}
	for i := shared; i < len(before); i++ {
		d.add(DiffEntry{Path: FieldPathIndex(path, i), Op: DiffRemoved, Before: renderValue(before[i])})
	}
}

// namedElements indexes a list by the Kubernetes "name" convention. It
// reports false unless every element is an object carrying a unique
// string name.
func namedElements(list []any) (map[string]any, bool) {
	if len(list) == 0 {
		return nil, false
	}
	named := make(map[string]any, len(list))
	for _, element := range list {
		object, ok := element.(map[string]any)
		if !ok {
			return nil, false
		}
		name, ok := object["name"].(string)
		if !ok || name == "" {
			return nil, false
		}
		if _, dup := named[name]; dup {
			return nil, false
		}
		named[name] = object
	}
	return named, true
}

// jsonEqual reports deep equality of two decoded JSON values.
func jsonEqual(a, b any) bool {
	switch aValue := a.(type) {
	case map[string]any:
		bValue, ok := b.(map[string]any)
		if !ok || len(aValue) != len(bValue) {
			return false
		}
		for key, av := range aValue {
			bv, ok := bValue[key]
			if !ok || !jsonEqual(av, bv) {
				return false
			}
		}
		return true
	case []any:
		bValue, ok := b.([]any)
		if !ok || len(aValue) != len(bValue) {
			return false
		}
		for i := range aValue {
			if !jsonEqual(aValue[i], bValue[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// renderValue renders one JSON value bounded for display. Truncation
// keeps the string valid UTF-8 and marks itself with an ellipsis.
func renderValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	rendered := string(encoded)
	if len(rendered) <= maxDiffValueLen {
		return rendered
	}
	rendered = rendered[:maxDiffValueLen]
	for len(rendered) > 0 && !utf8.ValidString(rendered) {
		rendered = rendered[:len(rendered)-1]
	}
	return rendered + "…"
}

// attribute names the manager owning one changed path: the one whose
// owned paths equal, contain, or refine it. More than one distinct
// candidate — or none — attributes to nobody, because a guessed
// attribution is worse than an absent one.
func attribute(path string, owners []FieldOwner) string {
	manager := ""
	for i := range owners {
		for _, owned := range owners[i].Paths {
			if !pathsRelate(path, owned) {
				continue
			}
			if manager != "" && manager != owners[i].Manager {
				return ""
			}
			manager = owners[i].Manager
			break
		}
	}
	return manager
}

// pathsRelate reports that one path equals the other or extends it at a
// segment boundary, in either direction: an owner of a subtree owns the
// change inside it, and an owner of a leaf owns the change of the
// subtree that carried it away.
func pathsRelate(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return extendsAt(a, b) || extendsAt(b, a)
}

// extendsAt reports that long extends short at a segment boundary.
func extendsAt(long, short string) bool {
	if !strings.HasPrefix(long, short) || len(long) <= len(short) {
		return false
	}
	next := long[len(short)]
	return next == '.' || next == '['
}

// simpleFieldKey matches a key that reads unambiguously in dotted form.
var simpleFieldKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// FieldPathKey appends one object key to a field path. It is shared
// with the managed-fields parser at the kube boundary, so attribution
// compares identical encodings.
func FieldPathKey(prefix, key string) string {
	if simpleFieldKey.MatchString(key) {
		return prefix + "." + key
	}
	return prefix + `["` + key + `"]`
}

// FieldPathNamed appends one name-keyed list element to a field path.
func FieldPathNamed(prefix, name string) string {
	return prefix + "[name=" + name + "]"
}

// FieldPathIndex appends one positional list element to a field path.
func FieldPathIndex(prefix string, i int) string {
	return prefix + "[" + strconv.Itoa(i) + "]"
}
