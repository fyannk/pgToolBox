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
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// RequestHeader is the custom header every unsafe JSON API call carries.
// A cross-site form or a <img> cannot set it, and a
// cross-origin fetch that sets it needs a CORS preflight this server never
// grants.
const RequestHeader = "X-PgToolBox-Request"

// CSRF errors; they are returned to the client as 403 with this text.
var (
	ErrMissingRequestHeader = errors.New("unsafe API request without the " + RequestHeader + ": 1 header")
	ErrCrossSite            = errors.New("cross-site request refused")
	ErrNoOrigin             = errors.New("unsafe request with neither Origin nor Sec-Fetch-Site")
)

// isSafeMethod reports the methods that must not change anything.
func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// CheckRequest applies the CSRF rules to one request. publicURL is the
// configured external origin ("" derives it from the request); api asks
// for the custom header on top.
//
// SameSite=Strict alone is not enough: on OpenShift every Route under
// *.apps.<domain> is the same site, so a neighbouring application can
// make same-site requests. Sec-Fetch-Site must therefore say same-origin
// (not same-site), and where a browser does not send it the Origin must
// be this console's.
func CheckRequest(r *http.Request, publicURL string, api bool) error {
	if isSafeMethod(r.Method) {
		return nil
	}
	if api && r.Header.Get(RequestHeader) != "1" {
		return ErrMissingRequestHeader
	}
	site := r.Header.Get("Sec-Fetch-Site")
	origin := r.Header.Get("Origin")
	if site == "" && origin == "" {
		return ErrNoOrigin
	}
	if site != "" && site != "same-origin" {
		return ErrCrossSite
	}
	if origin != "" && !sameOrigin(origin, ExpectedOrigin(r, publicURL)) {
		return ErrCrossSite
	}
	return nil
}

// CSRF wraps h with CheckRequest: the custom header is required under
// /api/, the origin rules everywhere.
func CSRF(publicURL string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := CheckRequest(r, publicURL, strings.HasPrefix(r.URL.Path, "/api/")); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ExpectedOrigin is the origin this console is reached on: the configured
// public URL, or the request's scheme and Host. X-Forwarded-Proto is
// honoured because TLS usually terminates at the Route or Ingress; the
// value only ever widens what a same-origin browser sends, never what a
// cross-site page can forge (it cannot choose the Origin header).
func ExpectedOrigin(r *http.Request, publicURL string) string {
	if publicURL != "" {
		return publicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		p, _, _ = strings.Cut(p, ",")
		if p = strings.ToLower(strings.TrimSpace(p)); p == "https" || p == "http" {
			scheme = p
		}
	}
	return scheme + "://" + r.Host
}

// SameOrigin compares two serialized origins, ignoring case and default
// ports. "null" (sandboxed frames, some redirects) never matches.
func SameOrigin(a, b string) bool { return sameOrigin(a, b) }

// sameOrigin is SameOrigin.
func sameOrigin(a, b string) bool {
	na, ok1 := normalizeOrigin(a)
	nb, ok2 := normalizeOrigin(b)
	return ok1 && ok2 && na == nb
}

func normalizeOrigin(o string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(o))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", false
	}
	if u.Path != "" && u.Path != "/" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = host + ":" + port
	}
	if strings.Contains(u.Hostname(), ":") { // IPv6 literal
		host = "[" + strings.ToLower(u.Hostname()) + "]"
		if port != "" {
			host += ":" + port
		}
	}
	return u.Scheme + "://" + host, true
}
