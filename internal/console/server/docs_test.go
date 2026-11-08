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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/docsbundle"
	"github.com/fyannk/pgToolBox/internal/console/mcp"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
)

type bearerRT struct{}

func (bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer reader")
	return http.DefaultTransport.RoundTrip(r)
}

// fixtureBundle is a small bundle built in the test, so the docs tools are
// tested on a fresh clone, where no generated bundle is embedded.
func fixtureBundle(t *testing.T) *docsbundle.Bundle {
	t.Helper()
	long := strings.Repeat("A session ends when it is revoked; the revocation list is kept until the token expires. ", 4)
	files, err := docsbundle.Build("9.9.9", []docsbundle.Source{
		{
			Path: "/docs/console/sessions/", File: "docs/console/sessions.md", Title: "Sessions",
			Markdown: "# Sessions\n\nIntro.\n\n## Revocation\n\n" + long + "\n",
		},
		{Path: "/docs/console/other/", File: "docs/console/other.md", Title: "Other", Markdown: "# Other\n\nNothing here.\n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := fstest.MapFS{}
	for name, data := range files {
		m[name] = &fstest.MapFile{Data: data}
	}
	b, err := docsbundle.Load(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDocsOverMCP(t *testing.T) {
	t.Parallel()
	docsOverMCP(t, fixtureBundle(t))
}

// Without a generated bundle the console reports it by name and serves the
// rest; with one, the docs tools read the documentation embedded in the image.
func TestEmbeddedDocsOverMCP(t *testing.T) {
	t.Parallel()
	if _, err := EmbeddedDocs(); errors.Is(err, docsbundle.ErrNotEmbedded) {
		if os.Getenv("DOCSBUNDLE_REQUIRED") != "" {
			t.Fatal(err)
		}
		t.Skip("no embedded bundle; run make docs-bundle")
	}
	b, err := docsbundle.Default()
	if err != nil {
		t.Fatal(err)
	}
	docsOverMCP(t, b)
}

// The MCP endpoint serves the documentation of a bundle: the docs tools find
// pages and read them back, at the bundle's version.
func docsOverMCP(t *testing.T, b *docsbundle.Bundle) {
	t.Helper()
	docs := BundleDocs(b)
	if docs.Version() == "" || docs.Version() != b.Version() {
		t.Fatalf("version %q, bundle %q", docs.Version(), b.Version())
	}
	_, _, reads := api.NewWithReads(api.Deps{Source: api.FixtureSource{}, Authz: api.FixtureAuthorizer{}})
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e, err := mcp.New(mcp.Options{
		Reads: reads, PublicURL: srv.URL, Docs: docs,
		Authenticator: mcpauth.AuthenticatorFunc(func(context.Context, string) (*auth.Principal, error) {
			return &auth.Principal{User: authz.User{Name: "reader"}}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Register(mux)
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "v0"}, nil).Connect(t.Context(),
		&sdk.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerRT{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "search_docs", Arguments: map[string]any{"query": "revocation list sessions", "limit": 3}})
	if err != nil || res.IsError {
		t.Fatalf("search_docs: %v %+v", err, res)
	}
	var found struct {
		Data struct {
			Version string       `json:"version"`
			Hits    []mcp.DocHit `json:"hits"`
		} `json:"data"`
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &found); err != nil || len(found.Data.Hits) == 0 || found.Data.Version != docs.Version() {
		t.Fatalf("search_docs = %s (%v)", raw, err)
	}
	hit := found.Data.Hits[0]
	if hit.ID == "" || hit.Path == "" || hit.Title == "" || len(found.Data.Hits) > 3 {
		t.Errorf("hit = %+v", hit)
	}

	res, err = cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "read_doc", Arguments: map[string]any{"path": hit.Path}})
	if err != nil || res.IsError {
		t.Fatalf("read_doc %s: %v %+v", hit.Path, err, res)
	}
	var page struct {
		Data struct {
			Page mcp.DocPage `json:"page"`
		} `json:"data"`
	}
	raw, _ = json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &page); err != nil || page.Data.Page.Title != hit.Title || len(page.Data.Page.Markdown) < 200 {
		t.Errorf("read_doc = %.300s (%v)", raw, err)
	}

	// Every page is a resource of its own, readable at the bundle's version.
	list, err := cs.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	pages := 0
	for _, r := range list.Resources {
		if strings.HasPrefix(r.URI, "docs://"+docs.Version()+"/") {
			pages++
		}
	}
	if want := len(b.Pages()); pages != want || pages == 0 {
		t.Errorf("%d docs:// resources, want %d", pages, want)
	}
	rr, err := cs.ReadResource(t.Context(), &sdk.ReadResourceParams{URI: mcp.DocURI(docs.Version(), hit.Path)})
	if err != nil || rr.Contents[0].Text != page.Data.Page.Markdown {
		t.Errorf("docs resource %s: %v", mcp.DocURI(docs.Version(), hit.Path), err)
	}
	if _, err := cs.ReadResource(t.Context(), &sdk.ReadResourceParams{URI: "docs://0.0/console/tour"}); err == nil {
		t.Error("a page of another version was served")
	}
}
