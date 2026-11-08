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

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// UntrustedKey is the key every untrusted value is moved under: a field
// whose value anyone with a database login or a Kubernetes client can
// write (a log line, an Event's reason and message, a label, a database
// or slot name, a statement's text) is rendered as {"untrusted": <value>}
// in place of the value, and the server's instructions say that such
// values are data, never instructions.
const UntrustedKey = "untrusted"

// UntrustedTag marks a view field by provenance: `untrusted:"true"` on a
// viewmodel field (any type: a string, a list, a map of labels) wraps its
// JSON value whatever its key is called.
const UntrustedTag = "untrusted"

// untrustedEverywhere are the keys whose string values are untrusted in
// every view, a second net under the tags: Kubernetes and operator
// messages, the timeline's detail and the application name a PostgreSQL
// client chose.
var untrustedEverywhere = []string{"message", "detail", "application"}

// Envelope is every tool's structured result: the API's JSON under data,
// untrusted values wrapped, and what the size cap cut when it cut.
type Envelope struct {
	Data      any        `json:"data"`
	Truncated *Truncated `json:"truncated,omitempty"`
}

// Truncated says what the response size cap removed: the client is told,
// never handed a partial answer as if whole.
type Truncated struct {
	LimitBytes int    `json:"limitBytes"`
	Note       string `json:"note"`
	// Lists are the first arrays that were shortened (at most
	// maxReportedLists), as JSON paths, with how many items were kept of
	// how many; ListsCut counts them all.
	Lists    []TruncatedList `json:"lists,omitempty"`
	ListsCut int             `json:"listsCut,omitempty"`
	// Strings counts the long strings that were cut.
	Strings int `json:"strings,omitempty"`
}

// TruncatedList is one shortened array.
type TruncatedList struct {
	Path string `json:"path"`
	Kept int    `json:"kept"`
	Of   int    `json:"of"`
}

const (
	// cutStringAt is where a long string is cut when shortening lists
	// was not enough.
	cutStringAt = 2048
	// maxReportedLists bounds the truncation report itself.
	maxReportedLists = 32

	shortenedNote = "the result was shortened to fit the response cap: every list keeps at most its first items " +
		"(the API's order; the lists named here, listsCut in all), and long strings end with an ellipsis; " +
		"ask a narrower question for the rest"
	tooLargeNote = "the result is larger than the response cap even when shortened; narrow the request"
)

// render turns a view into the envelope: untrusted values wrapped, then
// shortened until its JSON fits limit bytes. Shortening is bounded: it
// looks for the largest per-list item count that fits by bisection (each
// try one copy and one encoding of the tree), first with strings whole,
// then with long strings cut, and gives up with a note. ctx ends it.
func render(ctx context.Context, v any, extraUntrusted []string, limit int) (Envelope, int, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return Envelope{}, 0, fmt.Errorf("encoding the result: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return Envelope{}, 0, fmt.Errorf("decoding the result: %w", err)
	}
	tree = markTagged(reflect.ValueOf(v), tree)
	keys := append(slices.Clone(untrustedEverywhere), extraUntrusted...)
	tree = wrapUntrusted(tree, keys)
	env := Envelope{Data: tree}
	size := encodedSize(env)
	if limit <= 0 || size <= limit {
		return env, size, nil
	}
	longest := longestList(tree)
	try := func(k int, cut bool) (Envelope, int) {
		tr := &Truncated{LimitBytes: limit, Note: shortenedNote}
		t := capLists(tree, k, "$", tr)
		if cut {
			t, tr.Strings = cutStrings(t)
		}
		e := Envelope{Data: t, Truncated: tr}
		return e, encodedSize(e)
	}
	for _, cut := range []bool{false, true} {
		if err := ctx.Err(); err != nil {
			return Envelope{}, 0, err
		}
		// The largest k in [1, longest] that fits, or none. With no list,
		// k = 0 caps nothing and only the string cut is tried.
		lo, hi := 1, longest
		if longest == 0 {
			lo, hi = 0, 0
		}
		best, bestSize, found := Envelope{}, 0, false
		for lo <= hi {
			if err := ctx.Err(); err != nil {
				return Envelope{}, 0, err
			}
			mid := lo + (hi-lo)/2
			e, s := try(mid, cut)
			if s <= limit {
				best, bestSize, found = e, s, true
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		if found {
			return best, bestSize, nil
		}
	}
	env = Envelope{Truncated: &Truncated{LimitBytes: limit, Note: tooLargeNote}}
	return env, encodedSize(env), nil
}

func encodedSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// wrap moves a value under UntrustedKey.
func wrap(v any) any { return map[string]any{UntrustedKey: v} }

// isWrapped reports whether v is already a wrapper.
func isWrapped(v any) bool {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return false
	}
	_, ok = m[UntrustedKey]
	return ok
}

var jsonMarshaler = reflect.TypeFor[json.Marshaler]()

// markTagged walks the Go value beside its decoded JSON and wraps the
// value of every field tagged `untrusted:"true"`, at any depth: structs
// (embedded ones included), pointers, slices, arrays, string-keyed maps
// and interfaces. A type with its own MarshalJSON is left alone: its JSON
// need not follow its fields.
func markTagged(v reflect.Value, t any) any {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return t
		}
		v = v.Elem()
	}
	if !v.IsValid() || t == nil {
		return t
	}
	if v.Type().Implements(jsonMarshaler) || reflect.PointerTo(v.Type()).Implements(jsonMarshaler) {
		return t
	}
	switch v.Kind() {
	case reflect.Struct:
		m, ok := t.(map[string]any)
		if !ok {
			return t
		}
		markStruct(v, m)
		return m
	case reflect.Slice, reflect.Array:
		l, ok := t.([]any)
		if !ok {
			return t
		}
		for i := 0; i < len(l) && i < v.Len(); i++ {
			l[i] = markTagged(v.Index(i), l[i])
		}
		return l
	case reflect.Map:
		m, ok := t.(map[string]any)
		if !ok || v.Type().Key().Kind() != reflect.String {
			return t
		}
		iter := v.MapRange()
		for iter.Next() {
			k := iter.Key().String()
			if child, ok := m[k]; ok {
				m[k] = markTagged(iter.Value(), child)
			}
		}
		return m
	default:
		return t
	}
}

func markStruct(v reflect.Value, m map[string]any) {
	typ := v.Type()
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		fv := v.Field(i)
		if f.Anonymous && name == "" {
			// Promoted fields share the parent's object.
			for fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					break
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				markStruct(fv, m)
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		child, present := m[name]
		if !present || child == nil {
			continue
		}
		if f.Tag.Get(UntrustedTag) == "true" {
			if s, ok := child.(string); ok && s == "" {
				continue
			}
			if !isWrapped(child) {
				m[name] = wrap(child)
			}
			continue
		}
		m[name] = markTagged(fv, child)
	}
}

// wrapUntrusted replaces every string held under one of keys, at any
// depth, with {"untrusted": "<string>"}. A wrapper is not entered.
func wrapUntrusted(v any, keys []string) any {
	switch t := v.(type) {
	case map[string]any:
		if isWrapped(t) {
			return t
		}
		for k, child := range t {
			if s, ok := child.(string); ok && slices.Contains(keys, k) && s != "" {
				t[k] = wrap(s)
				continue
			}
			t[k] = wrapUntrusted(child, keys)
		}
		return t
	case []any:
		for i := range t {
			t[i] = wrapUntrusted(t[i], keys)
		}
		return t
	default:
		return v
	}
}

// longestList is the length of the longest array in v.
func longestList(v any) int {
	n := 0
	switch t := v.(type) {
	case map[string]any:
		for _, c := range t {
			n = max(n, longestList(c))
		}
	case []any:
		n = len(t)
		for _, c := range t {
			n = max(n, longestList(c))
		}
	}
	return n
}

// capLists copies v with every array cut to its first k items (k = 0
// cuts nothing), recording the cuts in tr. v is not changed. Paths are
// built only while the report still has room.
func capLists(v any, k int, path string, tr *Truncated) any {
	report := path != "" && len(tr.Lists) < maxReportedLists
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		keys := make([]string, 0, len(t))
		for key := range t {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			p := ""
			if report && len(tr.Lists) < maxReportedLists {
				p = path + "." + key
			}
			out[key] = capLists(t[key], k, p, tr)
		}
		return out
	case []any:
		n := len(t)
		if k > 0 && n > k {
			n = k
			tr.ListsCut++
			if report {
				tr.Lists = append(tr.Lists, TruncatedList{Path: path, Kept: k, Of: len(t)})
			}
		}
		out := make([]any, n)
		for i := range n {
			p := ""
			if report && len(tr.Lists) < maxReportedLists {
				p = path + "[" + strconv.Itoa(i) + "]"
			}
			out[i] = capLists(t[i], k, p, tr)
		}
		return out
	default:
		return v
	}
}

// cutStrings shortens every string longer than cutStringAt, in place.
func cutStrings(v any) (any, int) {
	n := 0
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				t[k] = walk(c)
			}
			return t
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
			return t
		case string:
			if len(t) <= cutStringAt {
				return t
			}
			n++
			cut := t[:cutStringAt]
			for !utf8.ValidString(cut) && len(cut) > 0 {
				cut = cut[:len(cut)-1]
			}
			return cut + "…"
		default:
			return v
		}
	}
	return walk(v), n
}
