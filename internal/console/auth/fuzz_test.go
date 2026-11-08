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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// FuzzOpenSession: a session cookie opens only if this codec sealed it.
// Any other value, including a mutation of a real cookie, is refused, and
// the parser never panics on attacker bytes.
func FuzzOpenSession(f *testing.F) {
	// Lifetimes long enough that the fixed seed below stays valid.
	forever := 100 * 365 * 24 * time.Hour
	c, err := NewCodec([][]byte{secretA}, true, Limits{MaxAge: forever, IdleTimeout: forever})
	if err != nil {
		f.Fatal(err)
	}
	// The genuine cookie is sealed with a fixed nonce and a fixed payload
	// so every fuzz worker process derives the same value: a worker that
	// sealed its own would see the coordinator's seed as foreign.
	issued := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	plain, err := json.Marshal(Session{ID: strings.Repeat("ab", 16), Provider: ProviderToken, Token: "tok", IssuedAt: issued, LastSeen: issued})
	if err != nil {
		f.Fatal(err)
	}
	aead := c.sealer.session[0]
	nonce := make([]byte, aead.NonceSize())
	genuine := base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(c.names.session)))
	f.Add(genuine, "")
	f.Add(genuine[:len(genuine)-1]+"A", "")
	f.Add("", genuine)
	f.Add("AAAA", "BBBB")
	f.Add(strings.Repeat("A", 4000), strings.Repeat("A", 4000))
	f.Fuzz(func(t *testing.T, c0, c1 string) {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		r.Header.Set("Cookie", "__Host-pgtoolbox-console-0="+c0+"; __Host-pgtoolbox-console-1="+c1)
		var seen strings.Builder
		for i := range maxChunks {
			ck, err := r.Cookie("__Host-pgtoolbox-console-" + string(rune('0'+i)))
			if err != nil {
				break
			}
			seen.WriteString(ck.Value)
		}
		s, err := c.ReadSession(r)
		if err != nil {
			if seen.String() == genuine {
				t.Fatalf("the genuine cookie did not open: %v", err)
			}
			return
		}
		if seen.String() != genuine {
			t.Fatalf("a value this codec did not seal opened: %+v (seen %d bytes, genuine %d)", s, seen.Len(), len(genuine))
		}
	})
}

func browserClean(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool { return r <= 0x20 })
	s = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(s)
	return strings.ReplaceAll(s, `\`, "/")
}

// FuzzSafeRedirect: the result is "/" or the input, and
// resolved the way a browser would against https://h/, it stays on h.
func FuzzSafeRedirect(f *testing.F) {
	for _, seed := range []string{
		"/", "/clusters/", "/a?b=c#d", "//evil.example", `/\evil.example`, "/\t/evil.example",
		"https://evil.example", "javascript:alert(1)", "/%2f%2fevil.example", "/.//evil",
		"\x00/x", "/x\x7f", " /x", "/@evil.example", "/:80", "", "/auth/oidc/callback",
	} {
		f.Add(seed)
	}
	base, err := url.Parse("https://h/")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out := SafeRedirect(in)
		if out != "/" && out != in {
			t.Fatalf("SafeRedirect(%q) = %q: neither the input nor /", in, out)
		}
		ref, err := url.Parse(browserClean(out))
		if err != nil {
			t.Fatalf("SafeRedirect(%q) = %q, which a browser-cleaned parse rejects: %v", in, out, err)
		}
		got := base.ResolveReference(ref)
		if got.Scheme != "https" || got.Host != "h" {
			t.Fatalf("SafeRedirect(%q) = %q resolves to %q, off origin", in, out, got)
		}
		if strings.HasPrefix(got.Path, "/auth/") || got.Path == "/logout" {
			t.Fatalf("SafeRedirect(%q) = %q targets a sign-in endpoint", in, out)
		}
	})
}

// FuzzParseRevocationList: whatever the admin types, the parser either
// refuses the whole list or returns one whose entries are well-formed;
// it never panics and never accepts more than the bound.
func FuzzParseRevocationList(f *testing.F) {
	f.Add("2026-10-29T10:00:00Z", "alice 2026-10-29T12:00:00Z", strings.Repeat("ab", 16))
	f.Add("", "# c\n\n", "")
	f.Add("x", "a b c", "zz")
	f.Fuzz(func(t *testing.T, nb, users, sessions string) {
		l, err := ParseRevocationList(map[string]string{"notBefore": nb, "users": users, "sessions": sessions})
		if err != nil {
			return
		}
		if len(l.Users)+len(l.Sessions) > maxRevocationEntries {
			t.Fatal("bound exceeded")
		}
		for s := range l.Sessions {
			if len(s) != 32 {
				t.Fatalf("session entry %q accepted", s)
			}
		}
	})
}

// FuzzNormalizePastedToken: an accepted token is non-empty, bounded and
// printable ASCII without whitespace, so it cannot split a header.
func FuzzNormalizePastedToken(f *testing.F) {
	for _, s := range []string{"Bearer abc", "  eyJ.x.y  ", "a\r\nX-Evil: 1", "", "bearer ", "sha256~x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, ok := normalizePastedToken(in)
		if !ok {
			return
		}
		if out == "" || len(out) > maxPastedToken || strings.ContainsFunc(out, func(r rune) bool { return r <= 0x20 || r >= 0x7f }) {
			t.Fatalf("normalizePastedToken(%q) = %q", in, out)
		}
	})
}

// FuzzCheckRequest: an unsafe request passes the CSRF rules only with a
// same-origin Sec-Fetch-Site or an Origin equal to the console's, and
// under /api/ only with the custom header.
func FuzzCheckRequest(f *testing.F) {
	f.Add("POST", "same-origin", "https://c.example", "1", true)
	f.Add("POST", "same-site", "https://evil.c.example", "1", true)
	f.Add("DELETE", "", "https://C.EXAMPLE:443", "", false)
	f.Add("PUT", "", "null", "1", true)
	f.Fuzz(func(t *testing.T, method, site, origin, header string, api bool) {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://c.example/api/v1/x", nil)
		r.Method = method
		r.Host = "c.example"
		r.Header.Set("Sec-Fetch-Site", site)
		r.Header.Set("Origin", origin)
		r.Header.Set(RequestHeader, header)
		if CheckRequest(r, "", api) != nil || isSafeMethod(method) {
			return
		}
		if api && header != "1" {
			t.Fatal("API request passed without the header")
		}
		if site != "" && site != "same-origin" {
			t.Fatalf("Sec-Fetch-Site %q passed", site)
		}
		if site == "" && origin == "" {
			t.Fatal("passed without origin information")
		}
		if origin != "" && !sameOrigin(origin, "https://c.example") {
			t.Fatalf("Origin %q passed", origin)
		}
	})
}
