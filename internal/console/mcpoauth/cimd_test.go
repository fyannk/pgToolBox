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

package mcpoauth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// docServer serves client ID metadata documents over TLS on loopback; the
// fetcher reaches it as https://localhost:<port>/..., with the test
// certificate checked against its example.com name.
type docServer struct {
	srv  *httptest.Server
	hits atomic.Int32
	// docs maps a path to a handler.
	docs map[string]http.HandlerFunc
}

func newDocServer(t *testing.T) *docServer {
	t.Helper()
	d := &docServer{docs: map[string]http.HandlerFunc{}}
	d.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.hits.Add(1)
		if h, ok := d.docs[r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *docServer) url(path string) string {
	u, _ := url.Parse(d.srv.URL)
	return "https://localhost:" + u.Port() + path
}

func (d *docServer) tls() *tls.Config {
	cfg := d.srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.ServerName = "example.com"
	return cfg
}

func (d *docServer) serveJSON(path, body string) {
	d.docs[path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}
}

func document(id string, extra string) string {
	return `{"client_id":"` + id + `","client_name":"Codex","redirect_uris":["http://127.0.0.1/callback"],"token_endpoint_auth_method":"none"` + extra + `}`
}

func loopbackFetcher(d *docServer, c *clock) *Fetcher {
	return NewFetcher(FetcherOptions{AllowAddress: func(ip netip.Addr) bool { return ip.IsLoopback() }, TLS: d.tls(), Now: c.Now})
}

func TestCheckDocumentURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://client.example/metadata.json":                    true,
		"http://client.example/metadata.json":                     false,
		"https://client.example":                                  false,
		"https://client.example/":                                 false,
		"https://user@client.example/m.json":                      false,
		"https://client.example/m.json#x":                         false,
		"https://10.0.0.1/m.json":                                 false,
		"https://127.0.0.1/m.json":                                false,
		"https://[::1]/m.json":                                    false,
		"https://169.254.169.254/latest/meta-data/":               false,
		"https://[fd00::1]/m.json":                                false,
		"https://100.64.0.1/m.json":                               false,
		"https://[::ffff:10.0.0.1]/m.json":                        false,
		"https://93.184.215.14/m.json":                            true,
		"https://[2606:2800:21f:cb07:6820:80da:af6b:8b2c]/m.json": true,
		"https://client.example:8443/m.json?v=1":                  true,
		"https://client.example/m json":                           false,
		"https://client.example\\@10.0.0.1/m.json":                false,
		"https://client.example/m.json\n":                         false,
		" https://client.example/m.json":                          false,
	} {
		err := checkDocumentURL(raw)
		if !documentURLShape.MatchString(raw) {
			err = errDocumentURL
		}
		if (err == nil) != ok {
			t.Errorf("checkDocumentURL(%q) = %v", raw, err)
		}
	}
}

func TestFetcherRefusesNonPublicAddressesAfterDNS(t *testing.T) {
	d := newDocServer(t)
	id := d.url("/client.json")
	d.serveJSON("/client.json", document(id, ""))
	c := &clock{now: t0}
	// The default fetcher: localhost resolves to loopback, refused at dial.
	f := NewFetcher(FetcherOptions{TLS: d.tls(), Now: c.Now})
	if _, err := f.Fetch(context.Background(), id); !errors.Is(err, errForbiddenAddress) {
		t.Fatalf("loopback by name: %v", err)
	}
	if d.hits.Load() != 0 {
		t.Error("the server was reached")
	}
}

func TestFetcher(t *testing.T) {
	d := newDocServer(t)
	c := &clock{now: t0}
	ok := d.url("/ok.json")
	d.serveJSON("/ok.json", document(ok, ""))
	d.docs["/redirect.json"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusFound)
	}
	d.serveJSON("/big.json", document(d.url("/big.json"), `,"x":"`+strings.Repeat("a", maxDocument)+`"`))
	d.serveJSON("/other.json", document(ok, ""))
	d.serveJSON("/secret.json", document(d.url("/secret.json"), `,"client_secret":"s"`))
	d.serveJSON("/confidential.json", `{"client_id":"`+d.url("/confidential.json")+`","redirect_uris":["http://127.0.0.1/cb"],"token_endpoint_auth_method":"private_key_jwt"}`)
	d.serveJSON("/web.json", `{"client_id":"`+d.url("/web.json")+`","redirect_uris":["https://app.example/cb"]}`)
	d.docs["/html.json"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, document(d.url("/html.json"), ""))
	}
	d.docs["/slow.json"] = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(fetchTimeout + 2*time.Second):
		}
	}

	f := loopbackFetcher(d, c)
	doc, err := f.Fetch(context.Background(), ok)
	if err != nil || doc.Name != "Codex" || len(doc.RedirectURIs) != 1 {
		t.Fatalf("Fetch(ok) = %+v, %v", doc, err)
	}
	for _, p := range []string{"/redirect.json", "/big.json", "/other.json", "/secret.json", "/confidential.json", "/web.json", "/html.json", "/missing.json"} {
		if _, err := f.Fetch(context.Background(), d.url(p)); err == nil {
			t.Errorf("%s was admitted", p)
		}
	}
	if testing.Short() {
		return
	}
	start := time.Now()
	if _, err := f.Fetch(context.Background(), d.url("/slow.json")); err == nil || time.Since(start) > fetchTimeout+time.Second {
		t.Errorf("slow document: %v after %v", err, time.Since(start))
	}
}

func TestFetcherCaches(t *testing.T) {
	d := newDocServer(t)
	c := &clock{now: t0}
	ok := d.url("/ok.json")
	d.serveJSON("/ok.json", document(ok, ""))
	f := loopbackFetcher(d, c)
	for range 3 {
		if _, err := f.Fetch(context.Background(), ok); err != nil {
			t.Fatal(err)
		}
	}
	if d.hits.Load() != 1 {
		t.Errorf("%d fetches, want 1", d.hits.Load())
	}
	c.Add(defaultCacheTTL)
	if _, err := f.Fetch(context.Background(), ok); err != nil || d.hits.Load() != 2 {
		t.Errorf("after the TTL: %v, %d fetches", err, d.hits.Load())
	}
	// Failures are cached a minute.
	missing := d.url("/missing.json")
	_, _ = f.Fetch(context.Background(), missing)
	_, _ = f.Fetch(context.Background(), missing)
	if d.hits.Load() != 3 {
		t.Errorf("failure not cached: %d fetches", d.hits.Load())
	}
	for cc, want := range map[string]time.Duration{
		"": defaultCacheTTL, "max-age=60": minCacheTTL, "max-age=7200": 2 * time.Hour,
		"max-age=999999": maxCacheTTL, "no-store": minCacheTTL, "public, max-age=600": 10 * time.Minute,
	} {
		if got := cacheTTL(cc); got != want {
			t.Errorf("cacheTTL(%q) = %v, want %v", cc, got, want)
		}
	}
}

// With metadata documents on, an HTTPS client ID is resolved by the
// fetcher and its document's redirect URIs bind the request.
func TestAuthorizeWithAMetadataDocument(t *testing.T) {
	d := newDocServer(t)
	c := &clock{now: t0}
	id := d.url("/client.json")
	d.serveJSON("/client.json", document(id, ""))
	f := newSessions(t, c, secretA)
	r := newReplica(t, f, memoryStore(c), c, func(o *Options) {
		o.ClientMetadataDocuments = true
		o.Fetcher = loopbackFetcher(d, c)
	})
	w := r.do(httptest.NewRequestWithContext(context.Background(), http.MethodGet, console+PathMetadata, nil))
	if !strings.Contains(w.Body.String(), `"client_id_metadata_document_supported":true`) {
		t.Errorf("metadata: %s", w.Body)
	}
	q := authorizeQuery(id, "http://127.0.0.1:4000/callback")
	page := r.authorize(q)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Codex") || !strings.Contains(page.Body.String(), id) {
		t.Fatalf("consent page: %d %s", page.Code, page.Body)
	}
	q.Set("redirect_uri", "http://127.0.0.1:4000/elsewhere")
	if w := r.authorize(q); w.Code != http.StatusBadRequest {
		t.Errorf("redirect outside the document: %d", w.Code)
	}
}
