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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every case is a payload shape that reaches another origin through a
// naive redirect check.
func TestSafeRedirect(t *testing.T) {
	t.Parallel()
	stripped := strings.NewReplacer("\t", "", "\r", "", "\n", "")
	for _, p := range []string{
		"/\t/evil.example", "/\t\t/evil.example", "/\v/evil.example", "/\x00/evil.example",
		"/\x7f/evil.example", "/\r/evil.example", "/\n/evil.example", "//evil.example",
		"///evil.example", "/\\evil.example", "\\\\evil.example", "https://evil.example",
		"http:/evil.example", "", "evil.example", "/path\r\nX-Evil: 1",
		// The sign-in endpoints are never a destination.
		"/logout", "/auth/oidc/callback?code=x", "/auth/login",
	} {
		got := SafeRedirect(p)
		if got != "/" {
			t.Errorf("SafeRedirect(%q) = %q, want /", p, got)
			continue
		}
		if strings.HasPrefix(stripped.Replace(got), "//") {
			t.Errorf("SafeRedirect(%q) = %q resolves off origin", p, got)
		}
	}
	for _, p := range []string{"/", "/clusters", "/c/payments/orders/", "/x?y=1&z=2", "/a#b", "/authz"} {
		if got := SafeRedirect(p); got != p {
			t.Errorf("SafeRedirect(%q) = %q, want it unchanged", p, got)
		}
	}
}

func TestCheckRequest(t *testing.T) {
	t.Parallel()
	type hdr map[string]string
	cases := []struct {
		name      string
		method    string
		host      string
		publicURL string
		api       bool
		headers   hdr
		want      error
	}{
		{"GET never checked", http.MethodGet, "c.example", "", true, nil, nil},
		{"HEAD never checked", http.MethodHead, "c.example", "", true, nil, nil},
		{
			"API POST same-origin with header", http.MethodPost, "c.example", "", true,
			hdr{RequestHeader: "1", "Sec-Fetch-Site": "same-origin", "Origin": "https://c.example"},
			nil,
		},
		{
			"API POST without header", http.MethodPost, "c.example", "", true,
			hdr{"Sec-Fetch-Site": "same-origin", "Origin": "https://c.example"},
			ErrMissingRequestHeader,
		},
		{
			"API POST header value must be 1", http.MethodPost, "c.example", "", true,
			hdr{RequestHeader: "true", "Sec-Fetch-Site": "same-origin"},
			ErrMissingRequestHeader,
		},
		{
			"same-site is not same-origin (OpenShift *.apps)", http.MethodPost, "c.apps.example", "", true,
			hdr{RequestHeader: "1", "Sec-Fetch-Site": "same-site", "Origin": "https://evil.apps.example"},
			ErrCrossSite,
		},
		{
			"cross-site", http.MethodDelete, "c.example", "", true,
			hdr{RequestHeader: "1", "Sec-Fetch-Site": "cross-site"},
			ErrCrossSite,
		},
		{
			"origin mismatch without fetch metadata", http.MethodPost, "c.example", "", true,
			hdr{RequestHeader: "1", "Origin": "https://evil.example"},
			ErrCrossSite,
		},
		{
			"origin match without fetch metadata", http.MethodPost, "c.example", "", true,
			hdr{RequestHeader: "1", "Origin": "https://C.example:443"},
			nil,
		},
		{
			"null origin", http.MethodPost, "c.example", "", true,
			hdr{RequestHeader: "1", "Origin": "null"},
			ErrCrossSite,
		},
		{
			"neither origin nor fetch metadata", http.MethodPost, "c.example", "", true,
			hdr{RequestHeader: "1"},
			ErrNoOrigin,
		},
		{
			"form post same origin, no header needed", http.MethodPost, "c.example", "", false,
			hdr{"Origin": "https://c.example"},
			nil,
		},
		{
			"public URL wins over Host", http.MethodPost, "internal:3000", "https://c.example", false,
			hdr{"Origin": "https://c.example"},
			nil,
		},
		{
			"public URL rejects the Host origin", http.MethodPost, "c.example", "https://other.example", false,
			hdr{"Origin": "https://c.example"},
			ErrCrossSite,
		},
		{
			"origin with path is not an origin", http.MethodPost, "c.example", "", false,
			hdr{"Origin": "https://c.example/x"},
			ErrCrossSite,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(context.Background(), tc.method, "https://"+tc.host+"/api/v1/x", nil)
			r.Host = tc.host
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := CheckRequest(r, tc.publicURL, tc.api); got != tc.want { //nolint:errorlint // sentinel identity is the contract
				t.Fatalf("CheckRequest = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExpectedOriginHonoursForwardedProto(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://c.example/", nil)
	r.Header.Set("X-Forwarded-Proto", "https, http")
	if got := ExpectedOrigin(r, ""); got != "https://c.example" {
		t.Fatalf("ExpectedOrigin = %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "gopher")
	if got := ExpectedOrigin(r, ""); got != "http://c.example" {
		t.Fatalf("ExpectedOrigin with a bogus proto = %q", got)
	}
}

func TestCSRFMiddlewareScopesHeaderToAPI(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := CSRF("", ok)
	for path, want := range map[string]int{"/api/v1/x": http.StatusForbidden, "/logout": http.StatusNoContent} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "https://c.example"+path, nil)
		r.Header.Set("Origin", "https://c.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("POST %s = %d, want %d", path, w.Code, want)
		}
	}
}
