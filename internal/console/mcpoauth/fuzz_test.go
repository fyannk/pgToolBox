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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzRegistration feeds arbitrary registration metadata (what any caller
// may POST to /oauth/register, or a client ID metadata document says) to
// the checks. Whatever is accepted names only loopback http redirect URIs
// on this computer, a printable bounded name, and every kept URI matches
// itself; nothing panics.
func FuzzRegistration(f *testing.F) {
	for _, seed := range []string{
		`{"redirect_uris":["http://127.0.0.1:33418/callback"],"client_name":"Claude Code","token_endpoint_auth_method":"none"}`,
		`{"redirect_uris":["http://localhost/cb","https://vscode.dev/redirect"],"grant_types":["authorization_code","refresh_token"]}`,
		`{"redirect_uris":["http://[::1]:8080/x?y=1"],"response_types":["code"],"application_type":"native"}`,
		`{"redirect_uris":["http://127.0.0.1@evil.example/"],"client_name":"\u202eevil\u0000"}`,
		`{"redirect_uris":["http://localhost:99999/"],"token_endpoint_auth_method":"client_secret_basic"}`,
		`{"redirect_uris":["myapp:/cb","http://127.0.0.1#frag"]}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var req registerRequest
		if json.Unmarshal(raw, &req) != nil {
			return
		}
		name, uris, err := checkMetadata(req)
		if err != nil {
			if me := (*metaError)(nil); !errors.As(err, &me) {
				t.Fatalf("a refusal is not an RFC 7591 error: %T %v", err, err)
			}
			return
		}
		if name == "" || !utf8.ValidString(name) || len(name) > 256 {
			t.Fatalf("accepted name %q", name)
		}
		for _, r := range name {
			if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
				t.Fatalf("accepted name %q holds %U", name, r)
			}
		}
		if len(uris) == 0 || len(uris) > maxRedirectURIs {
			t.Fatalf("accepted %d redirect URIs", len(uris))
		}
		for _, u := range uris {
			p, err := parseLoopback(u)
			if err != nil || p.Scheme != "http" || p.User != nil || strings.Contains(u, "#") {
				t.Fatalf("accepted redirect URI %q (%v)", u, err)
			}
			switch strings.ToLower(p.Hostname()) {
			case "127.0.0.1", "localhost", "::1":
			default:
				t.Fatalf("accepted redirect URI %q points at %q", u, p.Hostname())
			}
			if got, ok := matchRedirect(uris, u); !ok || got != u {
				t.Fatalf("a kept URI %q does not match itself", u)
			}
		}
	})
}
