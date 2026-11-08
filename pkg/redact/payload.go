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
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Redacted replaces a value Payload removed.
const Redacted = "[redacted]"

// sensitiveKey matches object keys whose values are credentials.
var sensitiveKey = regexp.MustCompile(`(?i)(token|secret|passw(or)?d|authorization|cookie|credential|api[-_]?key|private[-_]?key|bearer)`)

// credentialText matches credentials that can sit inside any string: a
// JWT (an OIDC id_token, a ServiceAccount token), an Authorization value,
// and a URL's user information.
var credentialText = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]*` +
	`|(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}` +
	`|://[^/\s:@]+:[^/\s@]+@`)

// Payload returns raw, a JSON document, with every credential removed: the
// values of keys that name one (token, secret, password, authorization,
// cookie, credential, api key, private key) and credential-shaped text
// anywhere in a string (JWTs, Authorization values, user information in
// URLs). A document that is not JSON is redacted as one string. It is the
// boundary a payload crosses before it leaves the process in a trace.
func Payload(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil || dec.More() {
		out, _ := json.Marshal(Text(string(raw)))
		return out
	}
	out, err := json.Marshal(scrub(tree))
	if err != nil {
		return []byte(`"` + Redacted + `"`)
	}
	return out
}

// Text removes credential-shaped substrings from s.
func Text(s string) string {
	return credentialText.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "://") {
			return "://" + Redacted + "@"
		}
		return Redacted
	})
}

func scrub(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if sensitiveKey.MatchString(k) {
				t[k] = Redacted
				continue
			}
			t[k] = scrub(child)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = scrub(child)
		}
		return t
	case string:
		return Text(t)
	default:
		return v
	}
}
