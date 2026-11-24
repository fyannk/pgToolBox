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

package offer

import (
	"strings"
	"testing"
)

// TestValidateCAEntries: Document.Validate itself (not only Parse) refuses a
// CA key that is not a namespace.name label pair, one over the 317-byte
// bound, and an empty certificate; a well-formed entry passes. The key
// becomes a file name under <dir>/ca/, so a refusal here is the path-safety
// check.
func TestValidateCAEntries(t *testing.T) {
	longest := strings.Repeat("a", 317)
	cases := []struct {
		name    string
		cas     map[string]string
		invalid bool
	}{
		{"name only", map[string]string{"c1": "PEM"}, false},
		{"namespace.name", map[string]string{"team-a.c1": "PEM"}, false},
		{"path traversal", map[string]string{"../evil": "PEM"}, true},
		{"slash", map[string]string{"team/c1": "PEM"}, true},
		{"upper case", map[string]string{"Team.c1": "PEM"}, true},
		{"empty key", map[string]string{"": "PEM"}, true},
		{"three labels", map[string]string{"a.b.c": "PEM"}, true},
		{"trailing dash", map[string]string{"c1-": "PEM"}, true},
		{"empty value", map[string]string{"c1": ""}, true},
		{"one bad entry among good ones", map[string]string{"c1": "PEM", "c2": ""}, true},
		{"longest accepted key", map[string]string{longest: "PEM"}, false},
		{"over the key bound", map[string]string{longest + "a": "PEM"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := validDoc()
			doc.CAs = tc.cas
			err := doc.Validate()
			switch {
			case tc.invalid && (err == nil || !strings.Contains(err.Error(), "invalid CA entry")):
				t.Fatalf("Validate = %v, want an invalid CA entry", err)
			case !tc.invalid && err != nil:
				t.Fatalf("Validate = %v, want nil", err)
			}
		})
	}
}
