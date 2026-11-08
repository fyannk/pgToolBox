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
