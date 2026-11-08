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

// Command docsbundle writes internal/console/docsbundle/data from
// the documentation site's build: the pages of the current version, as the
// site's llms plugin wrote them (docs/site/build/docs-manifest.json and the
// Markdown files it lists), and the BM25 index over them.
//
//	go run ./hack/docsbundle -site docs/site/build -version 1.3.0
//
// The output is not committed (the directory is ignored but for .keep):
// make docs-bundle and the console image's Dockerfile stages run this. Run from
// the module root. After writing, the command loads what it wrote and searches
// it, so a bundle the console could not use fails here, not at start.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fyannk/pgToolBox/internal/console/docsbundle"
)

// maxBytes is the budget of the whole data directory.
const maxBytes = 1 << 20

type manifest struct {
	Versions []struct {
		Version string `json:"version"`
		Path    string `json:"path"`
		Pages   []struct {
			Title string `json:"title"`
			Path  string `json:"path"`
			File  string `json:"file"`
		} `json:"pages"`
	} `json:"versions"`
}

func main() {
	site := flag.String("site", "docs/site/build", "the built documentation site")
	out := flag.String("out", "internal/console/docsbundle/data", "the bundle directory")
	version := flag.String("version", "", "the documentation version of the bundle, the release the console is built for")
	flag.Parse()
	if err := run(*site, *out, *version); err != nil {
		fmt.Fprintln(os.Stderr, "docsbundle:", err)
		os.Exit(1)
	}
}

func run(site, out, version string) error {
	if version == "" {
		return errors.New("-version is required")
	}
	raw, err := os.ReadFile(filepath.Join(site, "docs-manifest.json")) //nolint:gosec // a build tool: paths are its flags
	if err != nil {
		return fmt.Errorf("%w (build the site first: make docs-build)", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("docs-manifest.json: %w", err)
	}
	var sources []docsbundle.Source
	found := false
	currentPath := ""
	for _, v := range m.Versions {
		// "current" is main's documentation: the console's own version.
		if v.Version != "current" {
			continue
		}
		found = true
		currentPath = v.Path
		for _, p := range v.Pages {
			md, err := os.ReadFile(filepath.Join(site, filepath.FromSlash(p.File))) //nolint:gosec // a build tool: paths come from the site build
			if err != nil {
				return err
			}
			sources = append(sources, docsbundle.Source{Path: p.Path, File: p.File, Title: p.Title, Markdown: string(md)})
		}
	}
	if !found {
		return errors.New("docs-manifest.json has no current version")
	}
	// Once the site is versioned, main is served under /docs/next/, but the
	// console this bundle ships in is a release whose pages live under its
	// minor's snapshot (/docs/1.3/), cut before the tag: address them there.
	if len(m.Versions) > 1 {
		parent := currentPath[:strings.LastIndex(strings.TrimSuffix(currentPath, "/"), "/")+1]
		sources = rebase(sources, currentPath, parent+minor(version)+"/")
	}
	files, err := docsbundle.Build(version, sources)
	if err != nil {
		return err
	}
	total := 0
	for _, b := range files {
		total += len(b)
	}
	if total >= maxBytes {
		return fmt.Errorf("the bundle is %d bytes, over the 1 MiB budget: shorten or split pages", total)
	}
	// Replace the generated files only: the directory also holds .keep, the
	// committed placeholder.
	for _, name := range []string{"index.bin", "pages"} {
		if err := os.RemoveAll(filepath.Join(out, name)); err != nil {
			return err
		}
	}
	for name, b := range files {
		target := filepath.Join(out, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		// Embedded by go:embed, so world-readable like any source file.
		if err := os.WriteFile(target, b, 0o644); err != nil { //nolint:gosec // see above
			return err
		}
	}
	if err := verify(out, version, sources); err != nil {
		return err
	}
	fmt.Printf("%s: %d pages, %d bytes (version %s)\n", out, len(sources), total, version)
	return nil
}

// verify loads the bundle as the console will, reads every page back and searches
// every page title: the gate that replaces the old committed-copy comparison.
func verify(dir, version string, sources []docsbundle.Source) error {
	b, err := docsbundle.Load(os.DirFS(dir))
	if err != nil {
		return fmt.Errorf("the bundle written to %s does not load: %w", dir, err)
	}
	if b.Version() != version || len(b.Pages()) != len(sources) {
		return fmt.Errorf("the bundle written to %s reads back as version %q with %d pages, want %q and %d", dir, b.Version(), len(b.Pages()), version, len(sources))
	}
	found := false
	for _, s := range sources {
		if _, ok := b.Page(s.Path); !ok {
			return fmt.Errorf("the bundle written to %s cannot read back the page %s", dir, s.Path)
		}
		// A title can be all stop words ("A"), so one hit anywhere is the proof.
		found = found || len(b.Search(s.Title, 5)) > 0
	}
	if !found {
		return fmt.Errorf("the bundle written to %s finds nothing for any page title", dir)
	}
	return nil
}

// minor is "1.3" for "1.3.0" and "1.3.0-rc.1".
func minor(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

// rebase moves pages from one URL prefix to another: their paths, their files
// and the links inside them.
func rebase(sources []docsbundle.Source, from, to string) []docsbundle.Source {
	out := make([]docsbundle.Source, len(sources))
	fromFile, toFile := strings.TrimPrefix(from, "/"), strings.TrimPrefix(to, "/")
	for i, s := range sources {
		s.Path = to + strings.TrimPrefix(s.Path, from)
		s.File = toFile + strings.TrimPrefix(s.File, fromFile)
		s.Markdown = strings.ReplaceAll(s.Markdown, "]("+from, "]("+to)
		out[i] = s
	}
	return out
}
