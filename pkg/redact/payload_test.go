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

package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJkYmEifQ.c2lnbmF0dXJl"

func TestPayload(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"plain values stay", `{"namespace":"team-a","cluster":"pg","limit":5}`, `{"cluster":"pg","limit":5,"namespace":"team-a"}`},
		{
			"sensitive keys at any depth", `{"a":{"accessToken":"x","Password":"y","list":[{"api_key":"z","name":"n"}]}}`,
			`{"a":{"Password":"[redacted]","accessToken":"[redacted]","list":[{"api_key":"[redacted]","name":"n"}]}}`,
		},
		{"a whole object under a sensitive key", `{"secret":{"inner":"v"}}`, `{"secret":"[redacted]"}`},
		{"a JWT inside text", `{"message":"login with ` + jwt + ` failed"}`, `{"message":"login with [redacted] failed"}`},
		{"an Authorization value", `["Bearer abcdefgh12345678"]`, `["[redacted]"]`},
		{"user information in a URL", `{"url":"https://user:pa55@host/x"}`, `{"url":"https://[redacted]@host/x"}`},
		{"numbers keep their text", `{"n":12345678901234567890}`, `{"n":12345678901234567890}`},
		{"not JSON is one string", `token=` + jwt, `"token=[redacted]"`},
		{"a SQL password clause", `["ALTER ROLE app PASSWORD 'pa''55' VALID UNTIL 'infinity'"]`, `["ALTER ROLE app [redacted] VALID UNTIL 'infinity'"]`},
		{"a SQL password escape string", `["ALTER ROLE app PASSWORD E'pa\\'5''5' LOGIN"]`, `["ALTER ROLE app [redacted] LOGIN"]`},
		{"a SQL password escape string with a backslash-newline", `["ALTER ROLE app PASSWORD E'ab\\\ncdSECRET' LOGIN"]`, `["ALTER ROLE app [redacted] LOGIN"]`},
		{"a libpq password with a backslash-newline", `["password='ab\\\ncdSECRET' host=db"]`, `["[redacted] host=db"]`},
		{"a lower-case SQL password escape string", `["alter role app password e'pa55' login"]`, `["alter role app [redacted] login"]`},
		{"a SQL password Unicode string", `["ALTER ROLE app PASSWORD U&'pa\\0035''5' LOGIN"]`, `["ALTER ROLE app [redacted] LOGIN"]`},
		{"a libpq password keyword", `["host=db password=pa55 user=app"]`, `["host=db [redacted] user=app"]`},
		{"a quoted libpq password keyword", `["host=db password = 'pa 5\\'5' user=app"]`, `["host=db [redacted] user=app"]`},
		{"a / in a URL password", `["postgresql://app:pa/ss@db/x"]`, `["postgresql://[redacted]@db/x"]`},
		{"an @ in a URL password", `["postgresql://app:pa@ss@db/x"]`, `["postgresql://[redacted]@db/x"]`},
		{"prose about passwords stays", `["password policy changed"]`, `["password policy changed"]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := string(Payload([]byte(c.in))); got != c.want {
				t.Errorf("Payload(%s) = %s, want %s", c.in, got, c.want)
			}
		})
	}
}

// TestPayloadRedactsCredentialKeys: a fixed list of keys, written apart
// from the implementation's pattern, whose values never survive.
func TestPayloadRedactsCredentialKeys(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"token", "access_token", "idToken", "secret", "client_secret", "password", "Password",
		"PGPASSWORD", "passwd", "passphrase", "repo1-cipher-pass", "authorization", "Cookie",
		"set-cookie", "credentials", "apiKey", "api_key", "private_key", "privateKey", "bearer",
	} {
		doc, err := json.Marshal(map[string]string{key: "c4n4ry"})
		if err != nil {
			t.Fatal(err)
		}
		if out := string(Payload(doc)); strings.Contains(out, "c4n4ry") {
			t.Errorf("the value of %q survived: %s", key, out)
		}
	}
}

// FuzzTextCredential: whatever the password, a SQL PASSWORD clause (a
// standard, escape or Unicode string), a libpq password keyword and a
// URL's user information redact to the same text, so no part of the
// password survives.
func FuzzTextCredential(f *testing.F) {
	f.Add(uint8(0), "pa55")     // ALTER ROLE app PASSWORD 'pa55'
	f.Add(uint8(1), "pa55")     // host=db password=pa55 user=app
	f.Add(uint8(2), "pa 5'5\\") // host=db password='pa 5\'5\\' user=app
	f.Add(uint8(3), "pa/ss")    // postgresql://app:pa/ss@db/x
	f.Add(uint8(3), "pa@ss")    // postgresql://app:pa@ss@db/x
	f.Add(uint8(4), "pa 5'5\\") // ALTER ROLE app PASSWORD E'pa 5\'5\\' LOGIN
	f.Add(uint8(5), "pa'55")    // ALTER ROLE app PASSWORD U&'pa''55' LOGIN
	forms := []func(string) (string, bool){
		func(pw string) (string, bool) {
			return "ALTER ROLE app PASSWORD '" + strings.ReplaceAll(pw, "'", "''") + "' LOGIN", true
		},
		func(pw string) (string, bool) {
			return "host=db password=" + pw + " user=app", pw != "" && pw[0] != '\'' && !strings.ContainsAny(pw, "\t\n\f\r ")
		},
		func(pw string) (string, bool) {
			quoted := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(pw)
			return "host=db password='" + quoted + "' user=app", true
		},
		func(pw string) (string, bool) {
			return "postgresql://app:" + pw + "@db/x", !strings.ContainsAny(pw, "\t\n\f\r \"")
		},
		func(pw string) (string, bool) {
			quoted := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(pw)
			return "ALTER ROLE app PASSWORD E'" + quoted + "' LOGIN", true
		},
		func(pw string) (string, bool) {
			return "ALTER ROLE app PASSWORD U&'" + strings.ReplaceAll(pw, "'", "''") + "' LOGIN", true
		},
	}
	f.Fuzz(func(t *testing.T, form uint8, pw string) {
		build := forms[int(form)%len(forms)]
		text, ok := build(pw)
		if !ok {
			t.Skip()
		}
		reference, _ := build("x")
		if got, want := Text(text), Text(reference); got != want {
			t.Fatalf("Text(%q) = %q, want %q", text, got, want)
		}
	})
}

// FuzzPayload: whatever surrounds it, a JWT-shaped credential or a value
// under a credential key never survives, and the output is JSON.
func FuzzPayload(f *testing.F) {
	f.Add(`{"k":"v"}`, "token")
	f.Add(`[1,"x"]`, "name")
	f.Add(`not json`, "password")
	f.Add(`{"a":[{"b":null}]}`, "Authorization")
	const canary = "c4n4ry-" + jwt
	f.Fuzz(func(t *testing.T, doc, key string) {
		var tree any
		if json.Unmarshal([]byte(doc), &tree) != nil {
			tree = doc
		}
		wrapped, err := json.Marshal(map[string]any{"doc": tree, key: canary, "text": "x " + jwt + " y"})
		if err != nil {
			t.Skip()
		}
		out := Payload(wrapped)
		if !json.Valid(out) {
			t.Fatalf("not JSON: %s", out)
		}
		if strings.Contains(string(out), jwt) {
			t.Fatalf("a JWT survived: %s", out)
		}
		if sensitiveKey.MatchString(key) && strings.Contains(string(out), "c4n4ry") {
			t.Fatalf("the value of %q survived: %s", key, out)
		}
	})
}
