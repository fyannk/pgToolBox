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

// Package uiassets embeds the console SPA (ui/dist) and
// serves it per the contract in ui/README.md "Embedding": hashed assets are
// immutable and served from their .br/.gz siblings when the client accepts
// them; index.html is never cached and answers every other path (client
// routing). The console image build copies ui/dist into dist/ before
// `go build`; without it (unit tests, a plain `go build`) only the committed.
// gitkeep is embedded and every page says the SPA is not part of the build.
//
// The Content-Security-Policy is not set here: the server's default policy
// is the one ui/README.md prescribes for index.html.
package uiassets

import (
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed all:dist
var embedded embed.FS

const (
	indexFile    = "index.html"
	assetsPrefix = "/assets/"

	cacheImmutable = "public, max-age=31536000, immutable"
	cacheNever     = "no-store"
)

// FS is the embedded build, rooted at what ui/dist holds.
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		// "dist" is a valid, embedded directory name: unreachable.
		panic(err)
	}
	return sub
}

// Embedded reports whether the binary carries a SPA build.
func Embedded() bool { return hasIndex(FS()) }

// Handler serves the embedded build.
func Handler() http.Handler { return New(FS()) }

// New serves the SPA build in fsys (tests pass an fstest.MapFS).
func New(fsys fs.FS) http.Handler {
	return &handler{fsys: fsys, built: hasIndex(fsys)}
}

type handler struct {
	fsys  fs.FS
	built bool
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	p := path.Clean("/" + r.URL.Path)
	switch {
	case p == "/assets" || strings.HasPrefix(p, assetsPrefix):
		// A missing asset is a 404, never index.html: a stale tab asking
		// for an old chunk must fail loudly, not parse HTML as JavaScript.
		name := strings.TrimPrefix(p, "/")
		if !isFile(h.fsys, name) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", cacheImmutable)
		h.serve(w, r, name)
	case reserved(p):
		http.NotFound(w, r)
	case !h.built:
		notEmbedded(w)
	default:
		w.Header().Set("Cache-Control", cacheNever)
		h.serve(w, r, indexFile)
	}
}

// reserved paths never fall back to index.html (ui/README.md): the API and
// the probe paths, whichever listener they reach.
func reserved(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/") ||
		p == "/healthz" || p == "/readyz" || p == "/metrics"
}

// builtinTypes covers what the SPA ships that Go's own table lacks: the
// distroless image has no /etc/mime.types to fill the gap.
var builtinTypes = map[string]string{".woff2": "font/woff2"}

// serve writes name, or its pre-compressed sibling when the client accepts
// one, with the uncompressed file's Content-Type.
func (h *handler) serve(w http.ResponseWriter, r *http.Request, name string) {
	hd := w.Header()
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = builtinTypes[path.Ext(name)]
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	hd.Set("Content-Type", ctype)
	file := name
	variants := false
	for _, enc := range []struct{ token, ext string }{{"br", ".br"}, {"gzip", ".gz"}} {
		if !isFile(h.fsys, name+enc.ext) {
			continue
		}
		variants = true
		if file == name && accepts(r.Header.Get("Accept-Encoding"), enc.token) {
			file = name + enc.ext
			hd.Set("Content-Encoding", enc.token)
		}
	}
	if variants {
		hd.Add("Vary", "Accept-Encoding")
	}
	f, err := h.fsys.Open(file)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	// The embedded files carry no modification time, so ServeContent sets
	// no Last-Modified; it answers HEAD and Range.
	http.ServeContent(w, r, name, time.Time{}, rs)
}

// accepts reports whether an Accept-Encoding value allows token (RFC 9110
// §12.5.3): listed with a non-zero weight.
func accepts(header, token string) bool {
	for part := range strings.SplitSeq(header, ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), token) {
			continue
		}
		for param := range strings.SplitSeq(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				return err == nil && q > 0
			}
		}
		return true
	}
	return false
}

func hasIndex(fsys fs.FS) bool { return isFile(fsys, indexFile) }

func isFile(fsys fs.FS, name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	st, err := fs.Stat(fsys, name)
	return err == nil && st.Mode().IsRegular()
}

// notEmbedded answers in place of the SPA in a build without it.
func notEmbedded(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", cacheNever)
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>pgToolBox console</title>`+
		`<p>SPA not embedded: this console binary was built without the user interface (ui/dist). `+
		`Build the console image, or run make ui-build and copy ui/dist into internal/console/uiassets/dist.</p>`)
}
