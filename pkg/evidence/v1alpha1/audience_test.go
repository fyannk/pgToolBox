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

package v1alpha1

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// tokenWithClaims builds a syntactically valid compact JWS whose payload is
// claims; the signature is a placeholder (no verification happens here).
func tokenWithClaims(t testing.TB, claims any) BearerToken {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	token, err := ParseBearerToken([]byte("eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".c2lnbmF0dXJl"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestTokenAudience(t *testing.T) {
	for _, tc := range []struct {
		namespace, want string
		ok              bool
	}{
		{"team-a", "pgtoolbox-evidence-agent/team-a", true},
		{"a", "pgtoolbox-evidence-agent/a", true},
		{strings.Repeat("a", 63), "pgtoolbox-evidence-agent/" + strings.Repeat("a", 63), true},
		{"", "", false},
		{strings.Repeat("a", 64), "", false},
		{"Team", "", false},
		{"-team", "", false},
		{"team-", "", false},
		{"team/b", "", false},
		{"team.b", "", false},
	} {
		got, err := TokenAudience(tc.namespace)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("TokenAudience(%q) = %q, %v; want %q, ok=%v", tc.namespace, got, err, tc.want, tc.ok)
		}
	}
}

func TestParseTokenAudience(t *testing.T) {
	for _, tc := range []struct {
		audience, namespace string
		ok                  bool
	}{
		{"pgtoolbox-evidence-agent/team-a", "team-a", true},
		{"pgtoolbox-evidence-agent", "", false},
		{"pgtoolbox-evidence-agent/", "", false},
		{"pgtoolbox-evidence-agent/team-a/b", "", false},
		{"pgtoolbox-evidence-agent/Team", "", false},
		{"PGTOOLBOX-evidence-agent/team", "", false},
		{"https://kubernetes.default.svc", "", false},
		{"", "", false},
	} {
		namespace, ok := ParseTokenAudience(tc.audience)
		if ok != tc.ok || namespace != tc.namespace {
			t.Errorf("ParseTokenAudience(%q) = %q, %v; want %q, %v", tc.audience, namespace, ok, tc.namespace, tc.ok)
		}
	}
}

func TestTokenPath(t *testing.T) {
	got, err := TokenPath(DefaultTokenDirectory, "team-a")
	if err != nil || got != "/var/run/secrets/pgtoolbox.fyannk.dev/evidence-agent/team-a/token" {
		t.Fatalf("TokenPath = %q, %v", got, err)
	}
	for _, tc := range []struct{ directory, namespace string }{
		{"relative", "team-a"}, {"/a/../b", "team-a"}, {"/a/", "team-a"}, {"/a", ".."}, {"/a", "team/x"}, {"/a", ""},
	} {
		if got, err := TokenPath(tc.directory, tc.namespace); err == nil {
			t.Errorf("TokenPath(%q, %q) = %q, want an error", tc.directory, tc.namespace, got)
		}
	}
}

func TestClassifyAudienceClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		claims any
		want   AudienceClaim
	}{
		{"own single", map[string]any{"aud": "pgtoolbox-evidence-agent/team-a"}, AudienceOwn},
		{"own in list", map[string]any{"aud": []string{"x", "pgtoolbox-evidence-agent/team-b", "pgtoolbox-evidence-agent/team-a"}}, AudienceOwn},
		{"other namespace", map[string]any{"aud": []string{"pgtoolbox-evidence-agent/team-b"}}, AudienceOtherNamespace},
		{"other beats cluster-wide", map[string]any{"aud": []string{"pgtoolbox-evidence-agent", "pgtoolbox-evidence-agent/team-b"}}, AudienceOtherNamespace},
		{"cluster-wide", map[string]any{"aud": "pgtoolbox-evidence-agent"}, AudienceClusterWide},
		{"api server", map[string]any{"aud": []string{"https://kubernetes.default.svc"}}, AudienceUnknown},
		{"no aud", map[string]any{"sub": "x"}, AudienceUnknown},
		{"aud number", map[string]any{"aud": 7}, AudienceUnknown},
		{"too many", map[string]any{"aud": make([]string, maximumAudienceClaims+1)}, AudienceUnknown},
		{"not an object", []string{"pgtoolbox-evidence-agent/team-a"}, AudienceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAudienceClaim(tokenWithClaims(t, tc.claims), "team-a"); got != tc.want {
				t.Fatalf("ClassifyAudienceClaim = %v, want %v", got, tc.want)
			}
		})
	}
	if ClassifyAudienceClaim(BearerToken{}, "team-a") != AudienceUnknown {
		t.Fatal("an invalid token was classified")
	}
	if ClassifyAudienceClaim(tokenWithClaims(t, map[string]any{"aud": "pgtoolbox-evidence-agent/team-a"}), "Bad") != AudienceUnknown {
		t.Fatal("an invalid namespace was classified")
	}
	// The payload segment is not base64url JSON.
	garbage, err := ParseBearerToken([]byte("a.b.c"))
	if err != nil || ClassifyAudienceClaim(garbage, "team-a") != AudienceUnknown {
		t.Fatal("an undecodable payload was classified")
	}
}

// FuzzParseTokenAudience asserts the audience parser's invariant: an
// accepted audience is exactly the prefix plus a DNS-1123 label, it
// round-trips through TokenAudience, and it yields a TokenPath confined to
// one directory level below the token directory.
func FuzzParseTokenAudience(f *testing.F) {
	for _, seed := range []string{
		"pgtoolbox-evidence-agent/team-a", "pgtoolbox-evidence-agent", "pgtoolbox-evidence-agent/",
		"pgtoolbox-evidence-agent/..", "pgtoolbox-evidence-agent/a/b", "pgtoolbox-evidence-agent/" + strings.Repeat("z", 64), "",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, audience string) {
		namespace, ok := ParseTokenAudience(audience)
		if !ok {
			if namespace != "" {
				t.Fatal("a refused audience returned a namespace")
			}
			return
		}
		if namespace == "" || len(namespace) > 63 || strings.Trim(namespace, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" ||
			strings.HasPrefix(namespace, "-") || strings.HasSuffix(namespace, "-") {
			t.Fatalf("accepted namespace out of shape: %q", namespace)
		}
		again, err := TokenAudience(namespace)
		if err != nil || again != audience {
			t.Fatalf("audience does not round-trip: %q -> %q (%v)", audience, again, err)
		}
		path, err := TokenPath(DefaultTokenDirectory, namespace)
		if err != nil || path != DefaultTokenDirectory+"/"+namespace+"/"+TokenFileName {
			t.Fatalf("token path escapes its directory: %q (%v)", path, err)
		}
	})
}

// FuzzClassifyAudienceClaim asserts that the unverified claim reader is
// total and consistent: whatever the payload, a token is classified as the
// namespace's own only when one of its audiences parses to that namespace.
func FuzzClassifyAudienceClaim(f *testing.F) {
	for _, seed := range []string{
		`{"aud":"pgtoolbox-evidence-agent/team-a"}`, `{"aud":["pgtoolbox-evidence-agent/team-b"]}`,
		`{"aud":"pgtoolbox-evidence-agent"}`, `{"aud":null}`, `{"aud":[1,2]}`, `[]`, `{`, ``,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		token, err := ParseBearerToken([]byte("eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"))
		if err != nil {
			return
		}
		got := ClassifyAudienceClaim(token, "team-a")
		var claims struct {
			Audience any `json:"aud"`
		}
		own := false
		if json.Unmarshal(payload, &claims) == nil {
			switch value := claims.Audience.(type) {
			case string:
				own = value == "pgtoolbox-evidence-agent/team-a"
			case []any:
				allStrings := true
				for _, item := range value {
					_, isString := item.(string)
					allStrings = allStrings && isString
					own = own || item == "pgtoolbox-evidence-agent/team-a"
				}
				own = own && allStrings && len(value) <= maximumAudienceClaims
			}
		}
		if (got == AudienceOwn) != own {
			t.Fatalf("classified %v for payload %q (own=%v)", got, payload, own)
		}
	})
}
