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
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Source is one documentation page as the site build wrote it: the clean
// Markdown file (with the plugin's front matter) and where it is served.
type Source struct {
	// Path is the path the page is served at, e.g. /docs/console/tour/.
	Path string
	// File is where the page is stored under pages/, e.g. docs/console/tour.md.
	File string
	// Title is the page title.
	Title string
	// Markdown is the file content written by the llms plugin.
	Markdown string
}

// Build turns the pages of one documentation version into the files of a
// bundle, keyed by their path under the bundle directory. There is one,
// bundle.gz: a gzip archive of pages/<File> for each page and index.bin. The
// output is a pure function of its input: the same pages and version are
// always the same bytes. A bundle over MaxEmbeddedBytes compressed or
// MaxBundleBytes decompressed is refused, the error naming the limit.
func Build(version string, sources []Source) (map[string][]byte, error) {
	files, err := buildFiles(version, sources)
	if err != nil {
		return nil, err
	}
	archive, decompressed, err := pack(files)
	if err != nil {
		return nil, err
	}
	if err := checkBudget(len(archive), decompressed); err != nil {
		return nil, err
	}
	return map[string][]byte{archiveName: archive}, nil
}

// buildFiles is Build before the archive: the files the archive holds.
func buildFiles(version string, sources []Source) (map[string][]byte, error) {
	if version == "" {
		return nil, errors.New("docsbundle: version is empty")
	}
	if len(sources) == 0 {
		return nil, errors.New("docsbundle: no pages")
	}
	sorted := append([]Source(nil), sources...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	files := map[string][]byte{}
	ix := &index{Version: version, Terms: map[string][]posting{}}
	for pi, src := range sorted {
		if src.Path == "" || src.File == "" || src.Title == "" {
			return nil, fmt.Errorf("docsbundle: page %q needs a path, file and title", src.File)
		}
		if _, dup := files["pages/"+src.File]; dup {
			return nil, fmt.Errorf("docsbundle: two pages stored as %s", src.File)
		}
		body := stripFrontMatter(src.Markdown)
		// The stored file names the version the console carries and no host:
		// the site's canonical URL depends on where the site is served.
		file := fmt.Sprintf("---\ntitle: %s\nversion: %s\npath: %s\n---\n\n", quote(src.Title), quote(version), src.Path)
		offset := len(file)
		file += body
		files["pages/"+src.File] = []byte(file)
		ix.Pages = append(ix.Pages, pageMeta{Path: src.Path, Title: src.Title, File: src.File})
		for _, c := range chunkBody(body) {
			counts, length := termCounts(src.Title, c.heading, c.text)
			id := src.Path
			if c.anchor != "" {
				id += "#" + c.anchor
			}
			ix.addChunk(chunkMeta{
				Page:    pi,
				ID:      id,
				Heading: c.heading,
				Start:   offset + c.start,
				End:     offset + c.end,
				Length:  length,
			}, counts)
		}
	}
	files["index.bin"] = ix.encode()
	return files, nil
}

func quote(s string) string {
	if strings.ContainsAny(s, ":#\"'\n[]{}&*!|>%@`,") || s != strings.TrimSpace(s) {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// stripFrontMatter returns the body of a page without its front matter.
func stripFrontMatter(md string) string {
	if !strings.HasPrefix(md, "---\n") {
		return md
	}
	end := strings.Index(md[4:], "\n---\n")
	if end < 0 {
		return md
	}
	return strings.TrimLeft(md[4+end+5:], "\n")
}

type rawChunk struct {
	anchor  string
	heading string
	text    string // the section's text, heading line included
	start   int    // byte offsets in the body
	end     int
}

var headingRe = regexp.MustCompile(`^(#{1,3})[ \t]+(.+?)[ \t]*#*[ \t]*$`)

// chunkBody splits a page at its "##" and "###" headings (never inside a code
// fence). Whatever comes before the first of them, the title and the
// introduction, is a chunk of its own, headed by the page's "#" title.
func chunkBody(body string) []rawChunk {
	type cut struct {
		at      int
		heading string
	}
	var cuts []cut
	title := ""
	fence := ""
	for pos := 0; pos < len(body); {
		nl := strings.IndexByte(body[pos:], '\n')
		lineEnd := len(body)
		if nl >= 0 {
			lineEnd = pos + nl
		}
		line := body[pos:lineEnd]
		trim := strings.TrimLeft(line, " \t")
		switch {
		case fence != "":
			if strings.HasPrefix(trim, fence) && strings.Trim(trim, fence[:1]) == "" {
				fence = ""
			}
		case strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~"):
			fence = trim[:3]
		default:
			if m := headingRe.FindStringSubmatch(line); m != nil {
				text := plainHeading(m[2])
				switch len(m[1]) {
				case 1:
					if title == "" {
						title = text
					}
				default:
					cuts = append(cuts, cut{at: pos, heading: text})
				}
			}
		}
		pos = lineEnd + 1
	}

	slugs := map[string]int{}
	anchor := func(h string) string {
		base := slug(h)
		n := slugs[base]
		slugs[base]++
		if n == 0 {
			return base
		}
		return fmt.Sprintf("%s-%d", base, n)
	}

	var out []rawChunk
	first := len(body)
	if len(cuts) > 0 {
		first = cuts[0].at
	}
	if intro := strings.TrimSpace(body[:first]); intro != "" {
		out = append(out, rawChunk{anchor: "", heading: title, text: intro, start: 0, end: first})
	}
	for i, c := range cuts {
		end := len(body)
		if i+1 < len(cuts) {
			end = cuts[i+1].at
		}
		out = append(out, rawChunk{
			anchor: anchor(c.heading), heading: c.heading,
			text: strings.TrimSpace(body[c.at:end]), start: c.at, end: end,
		})
	}
	return out
}

var (
	linkRe   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	idSuffix = regexp.MustCompile(`\s*\{#[^}]*\}\s*$`)
)

// plainHeading removes the Markdown from a heading's text: code ticks, link
// targets, emphasis and an explicit {#id}.
func plainHeading(h string) string {
	h = idSuffix.ReplaceAllString(h, "")
	h = linkRe.ReplaceAllString(h, "$1")
	h = strings.NewReplacer("`", "", "**", "", "__", "").Replace(h)
	return strings.TrimSpace(h)
}

// slug is Docusaurus's heading anchor (github-slugger): lower case, anything
// but letters, digits, spaces, hyphens and underscores dropped, spaces to
// hyphens.
func slug(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || isWordRune(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}
