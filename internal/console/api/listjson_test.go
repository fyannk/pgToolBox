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

package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// sealed is the list with every row's encoding attached, as the read
// model's memo serves it.
func sealed(l viewmodel.ClusterList) viewmodel.ClusterList {
	items := make([]viewmodel.ClusterRow, len(l.Items))
	for i, row := range l.Items {
		items[i] = row.WithJSON()
	}
	l.Items = items
	return l
}

// hostile is a row whose untrusted strings exercise every escaping rule:
// HTML characters (left alone with HTML escaping off), U+2028/U+2029
// (always escaped), control characters, invalid UTF-8 and quotes.
func hostile() viewmodel.ClusterRow {
	row := fixtures.HealthyRow()
	row.Ref.Name = "hostile"
	row.Labels = map[string]string{"<script>": "</script>&amp;", "z": "\u2028\u2029", "q": "\"\\\x01\x7f", "bad": "\xff\xfe"}
	row.Phase.Text = "<b>Failing</b> & \"over\""
	row.Phase.Reason = "line\nbreak\ttab"
	return row
}

func encodeWith(t *testing.T, f huma.Format, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := f.Marshal(&buf, v); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return buf.Bytes()
}

// TestClusterListJSONMatchesTheDefaultFormat: the list writer's bytes
// are the default format's bytes, whichever rows carry a memoized
// encoding.
func TestClusterListJSONMatchesTheDefaultFormat(t *testing.T) {
	t.Parallel()
	mixed := fleetRows(40)
	for i := range mixed.Items {
		if i%3 == 0 {
			mixed.Items[i] = mixed.Items[i].WithJSON()
		}
	}
	withHostile := fixtures.ClusterList()
	withHostile.Items = append(withHostile.Items, hostile())
	cases := map[string]ClusterListView{
		"fixture, unsealed":           {ClusterList: fixtures.ClusterList()},
		"fixture, sealed":             {ClusterList: sealed(fixtures.ClusterList())},
		"fleet, sealed, unchecked":    {ClusterList: sealed(fleetRows(200)), Unchecked: 7},
		"mixed":                       {ClusterList: mixed, Unchecked: 1},
		"hostile, unsealed":           {ClusterList: withHostile},
		"hostile, sealed":             {ClusterList: sealed(withHostile)},
		"empty items":                 {ClusterList: viewmodel.ClusterList{GeneratedAt: fixtures.Now, Items: []viewmodel.ClusterRow{}, Namespaces: []viewmodel.NamespaceState{}}},
		"nil items and namespaces":    {ClusterList: viewmodel.ClusterList{GeneratedAt: fixtures.Now}},
		"zero time, one sealed row":   {ClusterList: viewmodel.ClusterList{Items: []viewmodel.ClusterRow{fixtures.HealthyRow().WithJSON()}}},
		"one unsealed row, unchecked": {ClusterList: viewmodel.ClusterList{Items: []viewmodel.ClusterRow{fixtures.FailingOverRow()}}, Unchecked: 3},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := encodeWith(t, huma.DefaultJSONFormat, v)
			if got := encodeWith(t, jsonFormat, v); !bytes.Equal(got, want) {
				t.Fatalf("list writer differs from the default format:\n got %s\nwant %s", got, want)
			}
			if got := encodeWith(t, jsonFormat, &v); !bytes.Equal(got, want) {
				t.Fatalf("list writer (pointer) differs from the default format:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestJSONFormatLeavesOtherBodiesToTheDefault: every other body is the
// default format's.
func TestJSONFormatLeavesOtherBodiesToTheDefault(t *testing.T) {
	t.Parallel()
	for _, v := range []any{fixtures.HealthyOverview(), map[string]string{"a": "<&>"}, (*ClusterListView)(nil), nil} {
		if got, want := encodeWith(t, jsonFormat, v), encodeWith(t, huma.DefaultJSONFormat, v); !bytes.Equal(got, want) {
			t.Errorf("%T: got %s, want %s", v, got, want)
		}
	}
}

// TestClusterListViewFields pins the document writeJSON spells out by
// hand: a field added to ClusterListView or ClusterList must be added
// to the writer, or this fails.
func TestClusterListViewFields(t *testing.T) {
	t.Parallel()
	var names []string
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		for f := range rt.Fields() {
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			if f.Anonymous && tag == "" {
				walk(f.Type)
				continue
			}
			names = append(names, tag)
		}
	}
	walk(reflect.TypeFor[ClusterListView]())
	want := []string{"generatedAt", "namespaces", "items", "unchecked,omitempty"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("ClusterListView JSON fields = %q, want %q: update writeJSON (listjson.go) and this test", names, want)
	}
}

// TestClusterListJSONFailureWritesNothing: a value the encoder refuses
// (a year past 9999) fails before the first byte, as with the default.
func TestClusterListJSONFailureWritesNothing(t *testing.T) {
	t.Parallel()
	bad := fixtures.FailingOverRow()
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	bad.Phase.Since = &far
	for name, v := range map[string]ClusterListView{
		"unsealed row": {ClusterList: viewmodel.ClusterList{Items: []viewmodel.ClusterRow{fixtures.HealthyRow().WithJSON(), bad}}},
		// WithJSON could not encode it, so it carries no encoding.
		"row WithJSON refused": {ClusterList: viewmodel.ClusterList{Items: []viewmodel.ClusterRow{bad.WithJSON()}}},
		"generatedAt":          {ClusterList: viewmodel.ClusterList{GeneratedAt: far}},
	} {
		var buf bytes.Buffer
		if err := jsonFormat.Marshal(&buf, v); err == nil || buf.Len() != 0 {
			t.Errorf("%s: err = %v, %d bytes written; want an error and nothing written", name, err, buf.Len())
		}
		if err := huma.DefaultJSONFormat.Marshal(&bytes.Buffer{}, v); err == nil {
			t.Errorf("%s: the default format accepts it; the case tests nothing", name)
		}
	}
}

type failingWriter struct{ n int }

func (w *failingWriter) Write(p []byte) (int, error) {
	w.n++
	return 0, errors.New("connection reset")
}

// TestClusterListJSONReportsAWriteError: the connection's error is the
// writer's error.
func TestClusterListJSONReportsAWriteError(t *testing.T) {
	t.Parallel()
	w := &failingWriter{}
	if err := jsonFormat.Marshal(w, ClusterListView{ClusterList: sealed(fleetRows(100))}); err == nil {
		t.Fatal("a failed write was not reported")
	}
}

// TestMemoizedRowsAreFilteredPerCaller: rows the caller may not get are
// left out though their encoding is memoized, and the response is the
// default format's encoding of what the caller may see. The encoding is
// row content; authorization is decided per request, after it.
func TestMemoizedRowsAreFilteredPerCaller(t *testing.T) {
	t.Parallel()
	list := sealed(fleetRows(64))
	hidden := map[authz.ObjectRef]bool{}
	unchecked := map[authz.ObjectRef]bool{}
	for i, row := range list.Items {
		ref := authz.ObjectRef{Namespace: row.Ref.Namespace, Name: row.Ref.Name}
		switch i % 5 {
		case 1:
			hidden[ref] = true
		case 3:
			unchecked[ref] = true
		}
	}
	h, _ := New(Deps{Source: fleetSource{list: list}, Authz: FixtureAuthorizer{Denied: hidden, Unchecked: unchecked}})
	w := call(t, h, FixturePrincipal(), "/api/v1/clusters")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /clusters = %d %s", w.Code, w.Body)
	}
	body := w.Body.Bytes()
	for ref := range hidden {
		if bytes.Contains(body, []byte(`"`+ref.Name+`"`)) {
			t.Errorf("denied %s is in the response", ref)
		}
	}
	for ref := range unchecked {
		if bytes.Contains(body, []byte(`"`+ref.Name+`"`)) {
			t.Errorf("unchecked %s is in the response", ref)
		}
	}
	// The same request answered with the default format.
	view, err := (&Reads{d: Deps{Source: fleetSource{list: list}, Authz: FixtureAuthorizer{Denied: hidden, Unchecked: unchecked}}}).
		ClusterList(auth.WithPrincipal(context.Background(), FixturePrincipal()))
	if err != nil {
		t.Fatal(err)
	}
	if want := encodeWith(t, huma.DefaultJSONFormat, view); !bytes.Equal(body, want) {
		t.Fatalf("response differs from the default format's encoding of the filtered view")
	}
	if !strings.Contains(string(body), `"unchecked":`) {
		t.Error("the unchecked count is missing")
	}
}

// FuzzClusterListJSON: whatever a Cluster's untrusted strings hold, the
// memoized row's bytes in the list are the default format's.
func FuzzClusterListJSON(f *testing.F) {
	f.Add("app", "orders", "Cluster in healthy state", "")
	f.Add("<script>", "</script>&", "\u2028", "\xff")
	f.Add("\"", "\\", "\x00\x1f", "line\nbreak")
	f.Fuzz(func(t *testing.T, key, value, phase, reason string) {
		row := fixtures.HealthyRow()
		row.Labels = map[string]string{key: value}
		row.Phase.Text, row.Phase.Reason = phase, reason
		v := ClusterListView{ClusterList: viewmodel.ClusterList{GeneratedAt: fixtures.Now, Items: []viewmodel.ClusterRow{row.WithJSON(), row}}}
		var want, got bytes.Buffer
		if err := huma.DefaultJSONFormat.Marshal(&want, v); err != nil {
			t.Skip(err)
		}
		if err := jsonFormat.Marshal(&got, v); err != nil {
			t.Fatalf("list writer: %v", err)
		}
		if !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatalf("list writer differs:\n got %s\nwant %s", got.Bytes(), want.Bytes())
		}
	})
}

// TestMemoizedRowsAreSharedReadOnly: concurrent requests copy the same
// memoized bytes; under -race this proves nobody writes them.
func TestMemoizedRowsAreSharedReadOnly(t *testing.T) {
	t.Parallel()
	list := sealed(fleetRows(64))
	h, _ := New(Deps{Source: fleetSource{list: list}, Authz: FixtureAuthorizer{}})
	want := encodeWith(t, huma.DefaultJSONFormat, ClusterListView{ClusterList: list})
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			for range 20 {
				w := call(t, h, FixturePrincipal(), "/api/v1/clusters")
				// generatedAt is the fixture's, so every body is the same.
				if !bytes.Equal(w.Body.Bytes(), want) {
					errs <- "a concurrent response differs"
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}
