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

package docsbundle

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"persistent history", []string{"persistent", "history"}},
		{"history.persistent", []string{"history.persistent", "history", "persistent"}},
		{"InstanceCABundleComplete", []string{"instancecabundlecomplete", "instance", "ca", "bundle", "complete"}},
		{"cluster-ref", []string{"cluster-ref", "cluster", "ref"}},
		{"RESTORE_DRILL_CPU_REQUEST", []string{"restore_drill_cpu_request", "restore", "drill", "cpu", "request"}},
		{"The conditions of the Pods.", []string{"condition", "pod"}},
		{"status access policies classes", []string{"status", "access", "policy", "class"}},
		{"`pgtoolbox.fyannk.dev/v1alpha1`", []string{"pgtoolbox.fyannk.dev/v1alpha1", "pgtoolbox", "fyannk", "dev", "v1alpha1"}},
		{"trailing. dash- and  ", []string{"trailing", "dash"}},
		{"", []string{}},
	}
	for _, tc := range tests {
		got := tokenize(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("tokenize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestChunkBodySplitsOnH2AndH3OutsideFences(t *testing.T) {
	body := "# Title\n\nIntro.\n\n## One\n\ntext\n\n```bash\n## not a heading\n```\n\n### Sub `x`\n\nmore\n\n## One\n\nagain\n"
	chunks := chunkBody(body)
	got := make([]string, 0, len(chunks))
	for _, c := range chunks {
		got = append(got, c.heading+"|"+c.anchor)
	}
	want := []string{"Title|", "One|one", "Sub x|sub-x", "One|one-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chunks = %q, want %q", got, want)
	}
	if !strings.Contains(chunks[1].text, "## not a heading") {
		t.Errorf("a fenced line must stay in its section: %q", chunks[1].text)
	}
	// The chunks tile the body: no byte is lost between them.
	if chunks[0].start != 0 || chunks[len(chunks)-1].end != len(body) {
		t.Errorf("chunks do not cover the body: %d..%d of %d", chunks[0].start, chunks[len(chunks)-1].end, len(body))
	}
}

func fixture(t *testing.T) *Bundle {
	t.Helper()
	files, err := Build("9.9.9", []Source{
		{Path: "/docs/b/", File: "docs/b/index.md", Title: "Beta", Markdown: "---\ntitle: Beta\n---\n\n# Beta\n\nIntro of beta.\n\n## Persistence\n\nSet `history.persistent` to keep the history across restarts.\n"},
		{Path: "/docs/a/x/", File: "docs/a/x.md", Title: "Alpha: x", Markdown: "# Alpha\n\nThe condition InstanceCABundleComplete is advisory.\n\n## Other\n\nNothing about it.\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := fstest.MapFS{}
	for name, b := range files {
		m[name] = &fstest.MapFile{Data: b}
	}
	b, err := Load(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFixtureSearchPageAndVersion(t *testing.T) {
	b := fixture(t)
	if b.Version() != "9.9.9" {
		t.Errorf("Version = %q", b.Version())
	}
	hits := b.Search("history.persistent", 5)
	if len(hits) == 0 || hits[0].Section != "Persistence" || hits[0].Path != "/docs/b/" || hits[0].ChunkID != "/docs/b/#persistence" {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Version != "9.9.9" || hits[0].PageTitle != "Beta" || !strings.Contains(hits[0].Excerpt, "history.persistent") {
		t.Errorf("hit = %+v", hits[0])
	}
	if got := b.Search("instancecabundlecomplete", 5); len(got) == 0 || got[0].Path != "/docs/a/x/" || got[0].ChunkID != "/docs/a/x/" {
		t.Errorf("the introduction chunk is the page path: %+v", got)
	}
	if b.Search("", 5) != nil || b.Search("zzzzqqq", 5) != nil || b.Search("the of", 5) != nil {
		t.Error("an empty, stop-word or unknown query must return nil")
	}
	if n := len(b.Search("history condition beta alpha", 1)); n != 1 {
		t.Errorf("limit 1 returned %d hits", n)
	}

	for _, p := range []string{"/docs/a/x/", "/docs/a/x", "docs/a/x.md", "/docs/a/x.md#other", "https://example.org/docs/a/x/?q=1", " /docs/a/x/ "} {
		d, ok := b.Page(p)
		if !ok || d.Title != "Alpha: x" || d.Version != "9.9.9" || d.Path != "/docs/a/x/" || !strings.HasPrefix(d.Markdown, "# Alpha") {
			t.Errorf("Page(%q) = %+v, %v", p, d, ok)
		}
	}
	for _, p := range []string{"/docs/b/", "/docs/b", "/docs/b/index.md"} {
		if d, ok := b.Page(p); !ok || d.Title != "Beta" {
			t.Errorf("Page(%q) = %+v, %v", p, d, ok)
		}
	}
	if _, ok := b.Page("/docs/nope/"); ok {
		t.Error("an unknown page must not be found")
	}
	pages := b.Pages()
	if len(pages) != 2 || pages[0].Path != "/docs/a/x/" || pages[1].Path != "/docs/b/" {
		t.Errorf("Pages = %+v", pages)
	}
}

func TestBuildIsDeterministicWhateverTheOrder(t *testing.T) {
	src := []Source{
		{Path: "/p/1/", File: "p/1.md", Title: "One", Markdown: "# One\n\n## A\n\nalpha beta\n\n## B\n\ngamma beta\n"},
		{Path: "/p/2/", File: "p/2.md", Title: "Two", Markdown: "# Two\n\n## C\n\nbeta delta\n"},
		{Path: "/p/3/", File: "p/3.md", Title: "Three", Markdown: "# Three\n\ntext\n"},
	}
	first, err := Build("1.0.0", src)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		shuffled := slices.Clone(src)
		slices.Reverse(shuffled)
		if i%2 == 1 {
			shuffled[0], shuffled[1] = shuffled[1], shuffled[0]
		}
		again, err := Build("1.0.0", shuffled)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatal("the same pages built to different bytes")
		}
	}
	other, _ := Build("1.0.1", src)
	if bytes.Equal(first["index.bin"], other["index.bin"]) {
		t.Error("the version must be part of the index")
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	if _, err := Build("", []Source{{Path: "/", File: "a.md", Title: "A"}}); err == nil {
		t.Error("an empty version must be refused")
	}
	if _, err := Build("1", nil); err == nil {
		t.Error("no pages must be refused")
	}
	if _, err := Build("1", []Source{{Path: "/a/", File: "a.md", Title: "A"}, {Path: "/b/", File: "a.md", Title: "B"}}); err == nil {
		t.Error("two pages in one file must be refused")
	}
}

func TestDecodeRefusesCorruptIndexes(t *testing.T) {
	good := fixture(t).ix.encode()
	if _, err := decodeIndex(good); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"empty":     nil,
		"magic":     []byte("nope"),
		"truncated": good[:len(good)/2],
		"trailing":  append(slices.Clone(good), 0),
	} {
		if _, err := decodeIndex(data); err == nil {
			t.Errorf("%s: a corrupt index was accepted", name)
		}
	}
}

// Without a bundle (a fresh clone: only data/.keep is committed) the package
// answers as for an empty one instead of failing.
func TestAbsentBundleIsAStateNotAFailure(t *testing.T) {
	if _, err := Load(fstest.MapFS{}); !errors.Is(err, ErrNotEmbedded) {
		t.Errorf("Load of an empty tree = %v, want ErrNotEmbedded", err)
	}
	if _, err := Load(fstest.MapFS{"index.bin": &fstest.MapFile{Data: []byte("nope")}}); err == nil || errors.Is(err, ErrNotEmbedded) {
		t.Errorf("a corrupt index must be an error that is not ErrNotEmbedded: %v", err)
	}
	if _, err := fs.Stat(embedded, "data/index.bin"); err == nil {
		t.Skip("a bundle is embedded (make docs-bundle was run): the absent case cannot be observed")
	}
	if _, err := Default(); !errors.Is(err, ErrNotEmbedded) {
		t.Fatalf("Default = %v, want ErrNotEmbedded", err)
	}
	if Search("history", 5) != nil || Version() != "" || Pages() != nil {
		t.Error("Search, Version and Pages must answer as for an empty bundle")
	}
	if _, ok := Page("/docs/"); ok {
		t.Error("Page must find nothing")
	}
}

// The tests below run on the real documentation, built by `make docs-bundle`
// (or the image build). They skip when no bundle is embedded, so a plain
// `go test` works on a fresh clone; `make docs` runs them with
// DOCSBUNDLE_REQUIRED=1, which turns the skip into a failure.
func requireEmbedded(t *testing.T) {
	t.Helper()
	_, err := Default()
	if err == nil {
		return
	}
	if errors.Is(err, ErrNotEmbedded) && os.Getenv("DOCSBUNDLE_REQUIRED") == "" {
		t.Skip("no embedded bundle; run make docs-bundle")
	}
	t.Fatal(err)
}

func TestEmbeddedBundleLoadsAndFitsTheBudget(t *testing.T) {
	requireEmbedded(t)
	if Version() == "" {
		t.Error("Version is empty")
	}
	if n := len(Pages()); n < 30 {
		t.Errorf("only %d pages embedded", n)
	}
	total := 0
	err := fs.WalkDir(embedded, "data", func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		total += int(info.Size())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if total >= 1<<20 {
		t.Errorf("the bundle is %d bytes, the budget is 1 MiB", total)
	}
}

// unversioned drops the minor's snapshot prefix a bundle built after the
// documentation was versioned carries (/docs/1.3/x/ -> /docs/x/), so the
// expectations below name a page, not the release that cut it.
func unversioned(path string) string {
	parts := strings.SplitN(Version(), ".", 3)
	if len(parts) < 2 {
		return path
	}
	return strings.Replace(path, "/docs/"+parts[0]+"."+parts[1]+"/", "/docs/", 1)
}

func hasPath(hits []Hit, path string) bool {
	return slices.ContainsFunc(hits, func(h Hit) bool { return unversioned(h.Path) == path })
}

func TestRankingOnRealDocs(t *testing.T) {
	requireEmbedded(t)
	tests := []struct {
		query string
		// where the answer must be: top n hits contain a hit with this path, or section when set
		n       int
		path    string
		section string
	}{
		{"persistent history", 1, "/docs/operator/console/", "Persistent history"},
		{"history.persistent", 3, "/docs/operator/console/", "Persistent history"},
		{"InstanceCABundleComplete", 3, "/docs/operator/conditions/", ""},
		{"instance ca bundle", 3, "/docs/operator/console/", "The instance CA bundle"},
		{"NetworkPolicy", 3, "/docs/console/networkpolicy/", ""},
		{"restore drill", 2, "/docs/console/restore-drills/", ""},
		{"cosign verify image signature", 3, "/docs/release/", ""},
		{"access requests", 4, "/docs/console/access-requests/", ""},
		{"how do I restart a cluster", 5, "/docs/operations/", ""},
	}
	for _, tc := range tests {
		hits := Search(tc.query, 10)
		if len(hits) == 0 {
			t.Errorf("%q: no hits", tc.query)
			continue
		}
		top := hits[:min(tc.n, len(hits))]
		ok := hasPath(top, tc.path)
		if tc.section != "" {
			ok = slices.ContainsFunc(top, func(h Hit) bool { return unversioned(h.Path) == tc.path && h.Section == tc.section })
		}
		if !ok {
			var got []string
			for _, h := range top {
				got = append(got, h.ChunkID)
			}
			t.Errorf("%q: want %s %s in the top %d, got %q", tc.query, tc.path, tc.section, tc.n, got)
		}
		for i, h := range hits {
			if h.Score <= 0 || h.Excerpt == "" || h.Version != Version() || h.PageTitle == "" {
				t.Errorf("%q: incomplete hit %+v", tc.query, h)
			}
			if i > 0 && h.Score > hits[i-1].Score {
				t.Errorf("%q: hits are not in score order", tc.query)
			}
			if _, ok := Page(h.Path); !ok {
				t.Errorf("%q: hit path %s is not a page", tc.query, h.Path)
			}
		}
	}
}

func TestEmbeddedExcerptCentresOnTheMatch(t *testing.T) {
	requireEmbedded(t)
	hits := Search("InstanceCABundleComplete", 3)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	for _, h := range hits {
		if !strings.Contains(strings.ToLower(h.Excerpt), "instancecabundlecomplete") {
			t.Errorf("excerpt of %s does not show the match: %q", h.ChunkID, h.Excerpt)
		}
		if n := len([]rune(h.Excerpt)); n > excerptRunes+2 {
			t.Errorf("excerpt is %d runes", n)
		}
	}
}

func TestEmbeddedSearchLimits(t *testing.T) {
	requireEmbedded(t)
	if n := len(Search("console", 0)); n != 10 {
		t.Errorf("default limit gave %d hits", n)
	}
	if n := len(Search("console", 1000)); n != 50 {
		t.Errorf("capped limit gave %d hits", n)
	}
	if Search("   ", 5) != nil {
		t.Error("a blank query must return nil")
	}
}
