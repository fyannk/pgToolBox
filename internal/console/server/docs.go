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

package server

import (
	"github.com/fyannk/pgToolBox/internal/console/docsbundle"
	"github.com/fyannk/pgToolBox/internal/console/mcp"
)

// EmbeddedDocs is the documentation embedded in the console image,
// as the MCP endpoint's search_docs, read_doc and
// docs:// resources read it.
func EmbeddedDocs() (mcp.Docs, error) {
	b, err := docsbundle.Default()
	if err != nil {
		return nil, err
	}
	return BundleDocs(b), nil
}

// BundleDocs adapts a documentation bundle to the MCP endpoint.
func BundleDocs(b *docsbundle.Bundle) mcp.Docs { return bundleDocs{b} }

type bundleDocs struct{ b *docsbundle.Bundle }

func (d bundleDocs) Search(query string, limit int) []mcp.DocHit {
	hits := d.b.Search(query, limit)
	out := make([]mcp.DocHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, mcp.DocHit{
			ID: h.ChunkID, Path: h.Path, Title: h.PageTitle, Section: h.Section, Excerpt: h.Excerpt, Score: h.Score,
		})
	}
	return out
}

func (d bundleDocs) Page(path string) (mcp.DocPage, bool) {
	p, ok := d.b.Page(path)
	if !ok {
		return mcp.DocPage{}, false
	}
	return mcp.DocPage{Path: p.Path, Title: p.Title, Markdown: p.Markdown}, true
}

func (d bundleDocs) Pages() []mcp.DocPage {
	pages := d.b.Pages()
	out := make([]mcp.DocPage, 0, len(pages))
	for _, p := range pages {
		// The list names the pages; Markdown is read per page.
		out = append(out, mcp.DocPage{Path: p.Path, Title: p.Title})
	}
	return out
}

func (d bundleDocs) Version() string { return d.b.Version() }
