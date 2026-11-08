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
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Docs is the documentation of the console's own version, embedded in
// its image (internal/console/docsbundle). The endpoint
// registers search_docs, read_doc and the docs:// resources only when it
// is given one; the adapter over the bundle lives in the wiring, so this
// package does not depend on the bundle's types.
type Docs interface {
	// Search ranks the bundle's sections for query (BM25) and returns at
	// most limit hits.
	Search(query string, limit int) []DocHit
	// Page returns one page by its path, as Search names it.
	Page(path string) (DocPage, bool)
	// Version is the documentation version (the console's release).
	Version() string
}

// DocLister is a Docs that can list its pages: each is then also a
// concrete docs:// resource, so a client's resource list shows them.
type DocLister interface {
	Pages() []DocPage
}

// DocHit is one ranked section.
type DocHit struct {
	// ID is the section's chunk id: the page path, "#", its anchor.
	ID      string  `json:"id"`
	Path    string  `json:"path" jsonschema:"page path, for read_doc"`
	Title   string  `json:"title"`
	Section string  `json:"section,omitempty"`
	Excerpt string  `json:"excerpt"`
	Score   float64 `json:"score"`
}

// DocPage is one page's Markdown.
type DocPage struct {
	Path     string `json:"path"`
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
}

// Bounds of the docs tools.
const (
	defaultDocHits = 8
	maxDocHits     = 25
	maxDocQuery    = 256
	maxDocPath     = 256
)

type searchDocsArgs struct {
	Query string `json:"query" jsonschema:"words to look for, such as a setting, a condition reason or a symptom"`
	Limit int    `json:"limit,omitempty" jsonschema:"most hits returned (default 8, at most 25)"`
}

type readDocArgs struct {
	Path string `json:"path" jsonschema:"the page path search_docs returned"`
}

// addDocs registers the documentation tools and resource template.
func (e *Endpoint) addDocs(s *mcp.Server) {
	docs := e.o.Docs
	if docs == nil {
		return
	}
	addTool(e, s, toolSpec[searchDocsArgs]{
		name: "search_docs", title: "Search the console's documentation",
		desc: "Keyword search (BM25) over the documentation of this console's own version: settings, conditions, " +
			"reasons, runbooks. Returns ranked sections with the page path for read_doc.",
		freeText: true,
		call: func(_ context.Context, in searchDocsArgs) (any, error) {
			q := strings.TrimSpace(in.Query)
			if q == "" || len(q) > maxDocQuery {
				return nil, errInvalid("query must be 1 to 256 characters")
			}
			limit := in.Limit
			if limit <= 0 {
				limit = defaultDocHits
			}
			limit = min(limit, maxDocHits)
			hits := docs.Search(q, limit)
			if hits == nil {
				hits = []DocHit{}
			}
			return map[string]any{"version": docs.Version(), "hits": hits}, nil
		},
	})
	addTool(e, s, toolSpec[readDocArgs]{
		name: "read_doc", title: "Read a documentation page",
		desc: "One page of this console's documentation, as Markdown (the docs://<version>/<path> resource, " +
			"for clients without resource support).",
		call: func(_ context.Context, in readDocArgs) (any, error) {
			if in.Path == "" || len(in.Path) > maxDocPath {
				return nil, errInvalid("path must be 1 to 256 characters")
			}
			page, ok := docs.Page(in.Path)
			if !ok {
				return nil, errNotFound("no such documentation page: use search_docs for the paths")
			}
			return map[string]any{"version": docs.Version(), "page": page}, nil
		},
	})
	if l, ok := docs.(DocLister); ok {
		for _, p := range l.Pages() {
			uri := DocURI(docs.Version(), p.Path)
			s.AddResource(&mcp.Resource{
				URI: uri, Name: strings.Trim(p.Path, "/"), Title: p.Title, MIMEType: "text/markdown",
			}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				page, ok := docs.Page(p.Path)
				if !ok {
					return nil, mcp.ResourceNotFoundError(req.Params.URI)
				}
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: page.Markdown}}}, nil
			})
		}
	}
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		Name: "docs", Title: "Documentation page", MIMEType: "text/markdown",
		Description: "A page of this console's documentation, at its own version.",
		URITemplate: "docs://{version}/{+path}",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		rest, ok := strings.CutPrefix(uri, "docs://"+docs.Version()+"/")
		if !ok {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		page, ok := docs.Page(rest)
		if !ok {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: page.Markdown}}}, nil
	})
}

// DocURI is a page's resource URI: docs://<version>/<path without its
// surrounding slashes>.
func DocURI(version, path string) string {
	return "docs://" + version + "/" + strings.Trim(path, "/")
}

// errInvalid is a refusal of the arguments.
func errInvalid(msg string) error { return &toolError{status: 422, msg: msg} }

func errNotFound(msg string) error { return &toolError{status: 404, msg: msg} }

// toolError is an error the tool reports with a status, like the API.
type toolError struct {
	status int
	msg    string
}

func (e *toolError) Error() string    { return e.msg }
func (e *toolError) GetStatus() int   { return e.status }
func (e *toolError) SafeText() string { return e.msg }
