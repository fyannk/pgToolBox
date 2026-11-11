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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/docsbundle"
)

func TestMaintainerPagesAreLeftOut(t *testing.T) {
	for path, want := range map[string]bool{
		"/docs/release/":                         false,
		"/docs/development/":                     true,
		"/docs/development/ci/":                  true,
		"/docs/development/repository-settings/": true,
		"/docs/security/":                        false,
		"/docs/console/mcp/":                     false,
		"/docs/reference/api/":                   false,
		"/docs/guides/release-notes/":            false,
	} {
		if got := maintainerPage(path, "/docs/"); got != want {
			t.Errorf("maintainerPage(%q) = %v, want %v", path, got, want)
		}
	}
	if !maintainerPage("/docs/next/development/ci/", "/docs/next/") {
		t.Error("a versioned development page is a maintainer page")
	}
}

func TestMinor(t *testing.T) {
	for in, want := range map[string]string{"1.3.0": "1.3", "1.3.0-rc.1": "1.3", "2.10.4": "2.10", "weird": "weird"} {
		if got := minor(in); got != want {
			t.Errorf("minor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRebaseMovesPathsFilesAndLinks(t *testing.T) {
	got := rebase([]docsbundle.Source{{
		Path: "/docs/next/console/tour/", File: "docs/next/console/tour.md", Title: "T",
		Markdown: "See [a](/docs/next/operator/x.md#y) and [b](https://example.org/docs/next/z).\n",
	}}, "/docs/next/", "/docs/1.3/")
	want := docsbundle.Source{
		Path: "/docs/1.3/console/tour/", File: "docs/1.3/console/tour.md", Title: "T",
		Markdown: "See [a](/docs/1.3/operator/x.md#y) and [b](https://example.org/docs/next/z).\n",
	}
	if got[0] != want {
		t.Errorf("rebase = %+v, want %+v", got[0], want)
	}
}

func site(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docs-manifest.json", manifest)
	write("docs/next/a.md", "---\ntitle: A\nversion: next (main)\nurl: https://h/docs/next/a/\n---\n\n# A\n\n## Sec\n\nSee [b](/docs/next/b.md).\n")
	write("docs/1.2/a.md", "# old\n")
	return dir
}

const versionedManifest = `{"versions":[
 {"version":"current","path":"/docs/next/","pages":[{"title":"Alpha","path":"/docs/next/a/","file":"docs/next/a.md"}]},
 {"version":"1.2","path":"/docs/","pages":[{"title":"Alpha","path":"/docs/a/","file":"docs/1.2/a.md"}]}]}`

func TestWriteThenRewrite(t *testing.T) {
	dir := site(t, versionedManifest)
	out := filepath.Join(t.TempDir(), "data")
	if err := run(dir, out, "1.3.0"); err != nil {
		t.Fatal(err)
	}
	// One file, the archive, is written.
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "bundle.gz" {
		t.Fatalf("the bundle directory holds %v, want bundle.gz alone", entries)
	}
	// Only current is bundled, addressed under the release's own snapshot.
	b, err := docsbundle.Load(os.DirFS(out))
	if err != nil {
		t.Fatal(err)
	}
	page, ok := b.Page("/docs/1.3/a/")
	if !ok || page.Path != "/docs/1.3/a/" || !strings.Contains(page.Markdown, "](/docs/1.3/b.md)") || strings.Contains(page.Markdown, "https://h") {
		t.Errorf("page = %+v, %v", page, ok)
	}
	// A rerun replaces the generated files, the layout before the archive
	// (index.bin and pages/) included, and leaves the placeholder.
	if err := os.WriteFile(filepath.Join(out, ".keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "pages"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"index.bin", filepath.Join("pages", "stale.md")} {
		if err := os.WriteFile(filepath.Join(out, stale), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(dir, out, "1.3.1"); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"index.bin", "pages"} {
		if _, err := os.Stat(filepath.Join(out, stale)); err == nil {
			t.Errorf("%s survived the rerun", stale)
		}
	}
	if _, err := os.Stat(filepath.Join(out, ".keep")); err != nil {
		t.Errorf("the placeholder must survive: %v", err)
	}
	b, err = docsbundle.Load(os.DirFS(out))
	if err != nil || b.Version() != "1.3.1" {
		t.Errorf("the rerun's bundle = %v, %v", b, err)
	}
}

func TestUnversionedSiteKeepsItsPaths(t *testing.T) {
	dir := site(t, `{"versions":[{"version":"current","path":"/docs/","pages":[{"title":"Alpha","path":"/docs/a/","file":"docs/next/a.md"}]}]}`)
	out := filepath.Join(t.TempDir(), "data")
	if err := run(dir, out, "1.2.0"); err != nil {
		t.Fatal(err)
	}
	b, err := docsbundle.Load(os.DirFS(out))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Page("/docs/a/"); !ok {
		t.Error("the page must keep its path")
	}
}

func TestRunRefusesMissingInput(t *testing.T) {
	if err := run(t.TempDir(), t.TempDir(), "1.0.0"); err == nil {
		t.Error("a directory without a manifest must be refused")
	}
	if err := run(t.TempDir(), t.TempDir(), ""); err == nil {
		t.Error("an empty version must be refused")
	}
	dir := site(t, `{"versions":[{"version":"1.2","path":"/docs/","pages":[]}]}`)
	if err := run(dir, t.TempDir(), "1.0.0"); err == nil {
		t.Error("a manifest without a current version must be refused")
	}
}

// A site whose pages break a budget fails the generator, the error naming the
// limit, and nothing is written.
func TestRunRefusesAnOversizedBundle(t *testing.T) {
	dir := site(t, `{"versions":[{"version":"current","path":"/docs/","pages":[{"title":"Alpha","path":"/docs/a/","file":"docs/next/a.md"}]}]}`)
	if err := os.WriteFile(filepath.Join(dir, "docs", "next", "a.md"), []byte(strings.Repeat("word ", docsbundle.MaxBundleBytes/5+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "data")
	err := run(dir, out, "1.2.0")
	if err == nil || !strings.Contains(err.Error(), "MaxBundleBytes") || !strings.Contains(err.Error(), " over the ") {
		t.Errorf("run = %v, want the MaxBundleBytes error", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("an oversized bundle must not be written")
	}
}
