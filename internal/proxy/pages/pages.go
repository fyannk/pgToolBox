/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

// Package pages renders the proxy's styled HTML pages server-side. All
// assets are embedded; pages reference no external content and need no
// JavaScript.
package pages

import (
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed style.css
var css string

//go:embed layout.html
var layout string

var tmpl = template.Must(template.New("pages").Parse(layout))

type base struct {
	Title   string
	CSS     template.CSS
	Content any
}

// LoginData feeds the local-mode login form.
type LoginData struct {
	RedirectTo string
	Error      string
}

// DeniedUnknownData feeds the 403 page of an authenticated identity that
// is unknown to the console.
type DeniedUnknownData struct {
	Subject     string
	CSRFToken   string
	ShowForm    bool
	ConsoleName string
}

// DeniedKnownData feeds the 403 page of a known user whose level is too
// low for the route.
type DeniedKnownData struct {
	Subject  string
	Level    string
	MinLevel string
	Path     string
}

// ConfirmationData feeds the access-request confirmation page.
type ConfirmationData struct {
	Subject string
}

// ErrorData feeds the generic error page.
type ErrorData struct {
	Title   string
	Message string
}

func render(w http.ResponseWriter, status int, templateName, title string, content any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// The CSS is a compile-time constant, never user input; marking it safe
	// is what allows embedding it inline.
	_ = tmpl.ExecuteTemplate(w, templateName, base{Title: title, CSS: template.CSS(css), Content: content}) // #nosec G203 -- constant CSS, not user input
}

// Login renders the local-mode login form.
func Login(w http.ResponseWriter, status int, d LoginData) {
	render(w, status, "login", "Sign in", d)
}

// DeniedUnknown renders the access-denied page with the request form.
func DeniedUnknown(w http.ResponseWriter, d DeniedUnknownData) {
	render(w, http.StatusForbidden, "denied-unknown", "Access required", d)
}

// DeniedKnown renders the insufficient-level page.
func DeniedKnown(w http.ResponseWriter, d DeniedKnownData) {
	render(w, http.StatusForbidden, "denied-known", "Insufficient privileges", d)
}

// Confirmation renders the access-request confirmation.
func Confirmation(w http.ResponseWriter, d ConfirmationData) {
	render(w, http.StatusOK, "confirmation", "Request sent", d)
}

// Error renders a generic error page with the given status.
func Error(w http.ResponseWriter, status int, message string) {
	render(w, status, "error", http.StatusText(status), ErrorData{
		Title:   http.StatusText(status),
		Message: message,
	})
}
