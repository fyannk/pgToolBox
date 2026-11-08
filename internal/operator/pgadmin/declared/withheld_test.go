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

package declared

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWithheldRoundTrip: the encoding is stable (sorted, an empty list is
// [] not null) and reads back.
func TestWithheldRoundTrip(t *testing.T) {
	w := Withheld{Servers: []WithheldServer{
		{Name: "b ro (application)", Reason: "no replica: the Cluster has one instance"},
		{Name: "a r (application)", Reason: "the Cluster disabled its r Service"},
	}}
	raw, err := w.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"servers":[{"name":"a r (application)","reason":"the Cluster disabled its r Service"},` +
		`{"name":"b ro (application)","reason":"no replica: the Cluster has one instance"}]}` + "\n"
	if string(raw) != want {
		t.Fatalf("encoded %s", raw)
	}
	back, err := ParseWithheld(raw)
	if err != nil {
		t.Fatal(err)
	}
	if names := back.Names(); len(names) != 2 || !names["a r (application)"] || !names["b ro (application)"] {
		t.Fatalf("names %v", names)
	}
	empty, err := Withheld{}.Encode()
	if err != nil || string(empty) != `{"version":1,"servers":[]}`+"\n" {
		t.Fatalf("empty %s %v", empty, err)
	}
}

func TestParseWithheldRefuses(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"another version", `{"version":2,"servers":[]}`, "version 2"},
		{"unknown field", `{"version":1,"servers":[],"extra":1}`, "unknown field"},
		{"trailing data", `{"version":1,"servers":[]} {}`, "trailing data"},
		{"too large", strings.Repeat(" ", MaxWithheldBytes+1), "exceed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseWithheld([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadWithheldAbsent(t *testing.T) {
	if _, err := LoadWithheld(filepath.Join(t.TempDir(), WithheldFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v", err)
	}
}
