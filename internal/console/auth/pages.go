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

package auth

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"html/template"
	"io"
	"net/http"
)

// The five server-rendered pages (sign-in, denied, error, signed-out,
// JavaScript disabled):
// no script, no external resource, one inline stylesheet allowed by its
// hash, a data: logo.

//go:embed web/style.css
var pageCSS string

//go:embed web/pages.html
var pageTemplates string

var pagesTmpl = template.Must(template.New("pages").Parse(pageTemplates))

// PageCSP is the Content-Security-Policy of every page this package
// renders. Forms may only post back here, nothing may frame a page (the
// sign-in form must not be clickjackable).
var PageCSP = "default-src 'none'; style-src '" + styleHash() + "'; img-src data:; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func styleHash() string {
	sum := sha256.Sum256([]byte(pageCSS))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

type pageBase struct {
	Title   string
	CSS     template.CSS
	Content any
}

type loginButton struct {
	Label string
	Path  string
}

type loginPage struct {
	Notice     string
	RedirectTo string
	Buttons    []loginButton
	Token      bool
}

type deniedPage struct {
	Subject string
	Message string
}

type errorPage struct {
	Title   string
	Message string
}

type signedOutPage struct {
	Confirm bool
	Subject string
	// Disconnect offers "sign out and disconnect my assistants".
	Disconnect bool
}

const pageFallback = `<!doctype html><meta charset="utf-8"><title>Error</title><p>This page could not be rendered.</p>`

// renderPage executes into memory first so a template failure never sends
// a truncated page under a success status.
func renderPage(w http.ResponseWriter, status int, name, title string, content any) {
	renderPageCSP(w, status, name, title, content, PageCSP)
}

// renderPageCSP is renderPage with another policy (the consent page's
// form posts to a redirect that leaves the origin).
func renderPageCSP(w http.ResponseWriter, status int, name, title string, content any, csp string) {
	var buf bytes.Buffer
	data := pageBase{Title: title, CSS: template.CSS(pageCSS), Content: content} // #nosec G203 -- embedded constant, not input
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", "text/html; charset=utf-8")
	if err := pagesTmpl.ExecuteTemplate(&buf, name, data); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, pageFallback)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// ErrorPage renders the generic error page.
func ErrorPage(w http.ResponseWriter, status int, message string) {
	renderPage(w, status, "error", http.StatusText(status), errorPage{Title: http.StatusText(status), Message: message})
}

// NoScriptPage renders the "JavaScript disabled" page.
func NoScriptPage(w http.ResponseWriter) {
	renderPage(w, http.StatusOK, "noscript", "JavaScript required", struct{}{})
}
