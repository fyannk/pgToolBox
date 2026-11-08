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

package uiassets

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const index = `<!doctype html><script type="module" crossorigin src="/assets/index-abc123.js"></script>`

func build() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                   {Data: []byte(index)},
		"assets/index-abc123.js":       {Data: []byte("console.log(1)")},
		"assets/index-abc123.js.br":    {Data: []byte("BR")},
		"assets/index-abc123.js.gz":    {Data: []byte("GZ")},
		"assets/index-def456.css":      {Data: []byte("body{}")},
		"assets/fonts/inter-789.woff2": {Data: []byte("WOFF")},
	}
}

func TestHandler(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, method, path, acceptEncoding string
		fsys                               fstest.MapFS
		wantStatus                         int
		wantBody, wantType, wantCache      string
		wantEncoding, wantVary             string
	}{
		{name: "root is index.html", path: "/", wantStatus: 200, wantBody: index, wantType: "text/html; charset=utf-8", wantCache: "no-store"},
		{name: "client route falls back", path: "/c/payments/orders/instances/orders-1", wantStatus: 200, wantBody: index, wantCache: "no-store"},
		{name: "dotted client route falls back", path: "/c/payments/orders.v2", wantStatus: 200, wantBody: index},
		{name: "index.html by name", path: "/index.html", wantStatus: 200, wantBody: index, wantCache: "no-store"},
		{
			name: "asset, identity", path: "/assets/index-abc123.js", wantStatus: 200, wantBody: "console.log(1)",
			wantType: "text/javascript; charset=utf-8", wantCache: "public, max-age=31536000, immutable", wantVary: "Accept-Encoding",
		},
		{
			name: "asset, br preferred", path: "/assets/index-abc123.js", acceptEncoding: "gzip, deflate, br, zstd",
			wantStatus: 200, wantBody: "BR", wantType: "text/javascript; charset=utf-8", wantEncoding: "br", wantVary: "Accept-Encoding",
		},
		{name: "asset, gzip only", path: "/assets/index-abc123.js", acceptEncoding: "gzip", wantStatus: 200, wantBody: "GZ", wantEncoding: "gzip"},
		{name: "asset, br refused by weight", path: "/assets/index-abc123.js", acceptEncoding: "br;q=0, gzip;q=0.5", wantStatus: 200, wantBody: "GZ", wantEncoding: "gzip"},
		{name: "asset, nothing acceptable", path: "/assets/index-abc123.js", acceptEncoding: "br;q=0.0, gzip;q=0", wantStatus: 200, wantBody: "console.log(1)"},
		{
			name: "asset without siblings", path: "/assets/index-def456.css", acceptEncoding: "br", wantStatus: 200, wantBody: "body{}",
			wantType: "text/css; charset=utf-8", wantCache: "public, max-age=31536000, immutable",
		},
		{name: "nested asset", path: "/assets/fonts/inter-789.woff2", wantStatus: 200, wantBody: "WOFF", wantType: "font/woff2"},
		{name: "missing asset never falls back", path: "/assets/index-old.js", wantStatus: 404},
		{name: "assets directory", path: "/assets/", wantStatus: 404},
		{name: "traversal stays in the build", path: "/assets/../../uiassets.go", wantStatus: 200, wantBody: index},
		{name: "api never falls back", path: "/api/v1/nope", wantStatus: 404},
		{name: "probe never falls back", path: "/healthz", wantStatus: 404},
		{name: "metrics never falls back", path: "/metrics", wantStatus: 404},
		{name: "HEAD", method: http.MethodHead, path: "/", wantStatus: 200, wantCache: "no-store"},
		{name: "POST refused", method: http.MethodPost, path: "/", wantStatus: 405},
		{
			name: "no build: page says so", fsys: fstest.MapFS{".gitkeep": {}}, path: "/c/payments/orders",
			wantStatus: 503, wantBody: "SPA not embedded", wantCache: "no-store",
		},
		{name: "no build: assets 404", fsys: fstest.MapFS{".gitkeep": {}}, path: "/assets/index-abc123.js", wantStatus: 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := tc.fsys
			if fsys == nil {
				fsys = build()
			}
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequestWithContext(t.Context(), method, tc.path, nil)
			if tc.acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tc.acceptEncoding)
			}
			rec := httptest.NewRecorder()
			New(fsys).ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
			if method == http.MethodHead && rec.Body.Len() != 0 {
				t.Errorf("HEAD wrote a body")
			}
			for _, h := range []struct{ key, want string }{
				{"Content-Type", tc.wantType}, {"Cache-Control", tc.wantCache}, {"Vary", tc.wantVary},
			} {
				if h.want != "" && rec.Header().Get(h.key) != h.want {
					t.Errorf("%s %q, want %q", h.key, rec.Header().Get(h.key), h.want)
				}
			}
			if got := rec.Header().Get("Content-Encoding"); got != tc.wantEncoding {
				t.Errorf("Content-Encoding %q, want %q", got, tc.wantEncoding)
			}
		})
	}
}

func TestAccepts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		header, token string
		want          bool
	}{
		{"", "br", false},
		{"br", "br", true},
		{"BR", "br", true},
		{"gzip, br", "br", true},
		{"br;q=0", "br", false},
		{"br; q=0.001", "br", true},
		{"br;q=0.000", "br", false},
		{"br;q=bogus", "br", false},
		{"brotli", "br", false},
		{"*", "br", false},
	}
	for _, tc := range cases {
		if got := accepts(tc.header, tc.token); got != tc.want {
			t.Errorf("accepts(%q, %q) = %v, want %v", tc.header, tc.token, got, tc.want)
		}
	}
}

// The repository carries only the placeholder: a plain `go build` must say
// the SPA is missing rather than serve something that looks like it.
func TestEmbeddedPlaceholder(t *testing.T) {
	t.Parallel()
	if Embedded() {
		t.Skip("a SPA build is present in dist/ (image build or local copy)")
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "SPA not embedded") {
		t.Fatalf("placeholder build answered %d %q", rec.Code, rec.Body.String())
	}
}
