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

package evidence

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func wireSeed(f *testing.F, name string) []byte {
	f.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "evidence", "v1alpha1", "testdata", "wire", name))
	if err != nil {
		f.Fatal(err)
	}
	return body
}

// FuzzParseEndpoint asserts that an accepted endpoint is always an https
// origin whose host is exactly <label>.<namespace>.svc in the agent's own
// namespace, and that the server name is that host.
func FuzzParseEndpoint(f *testing.F) {
	f.Add(evidencev1alpha1.Endpoint("orders-evidence", "team-a"), "team-a")
	f.Add(evidencev1alpha1.Endpoint("orders-evidence", "team-b"), "team-a")
	f.Add("https://svc.team-a.svc.cluster.local:8443", "team-a")
	f.Add("https://user:pw@svc.team-a.svc:8443/x?y#z", "team-a")
	f.Add("https://svc.team-a.svc", "team-a")
	f.Add("https://[::1]:8443", "team-a")
	f.Fuzz(func(t *testing.T, raw, namespace string) {
		origin, serverName, err := parseEndpoint(raw, namespace)
		if err != nil {
			if origin != "" || serverName != "" {
				t.Fatal("a refusal returned values")
			}
			return
		}
		labels := strings.Split(serverName, ".")
		if len(labels) != 3 || labels[1] != namespace || labels[2] != "svc" || labels[0] == "" {
			t.Fatalf("accepted %q for namespace %q as %q", raw, namespace, serverName)
		}
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() != serverName || parsed.User != nil ||
			parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			t.Fatalf("origin %q is not a bare https origin of %q", origin, serverName)
		}
		if again, name, err := parseEndpoint(origin, namespace); err != nil || again != origin || name != serverName {
			t.Fatalf("origin %q does not round-trip", origin)
		}
	})
}

// FuzzDecodeSnapshot asserts that a decoded snapshot always passes the
// contract's Validate, and that refusals carry a closed failure kind.
func FuzzDecodeSnapshot(f *testing.F) {
	f.Add(wireSeed(f, "snapshot.json"))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"api_version":"evidence.pgtoolbox.fyannk.dev/v1alpha1"} {}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		snapshot, err := decodeSnapshot(body)
		if err != nil {
			if snapshot != nil {
				t.Fatal("a refusal returned a snapshot")
			}
			if kind := kindOf(err); kind != failureContract && kind != failureTooLarge {
				t.Fatalf("refusal kind %q", kind)
			}
			return
		}
		if err := snapshot.Validate(); err != nil {
			t.Fatalf("accepted a snapshot that fails Validate: %v", err)
		}
	})
}

// FuzzDecodeBackupPage asserts that a decoded page always passes the
// contract's Validate and carries a bounded, non-empty cursor.
func FuzzDecodeBackupPage(f *testing.F) {
	f.Add(wireSeed(f, "backups.json"))
	f.Add([]byte(`{"api_version":"evidence.pgtoolbox.fyannk.dev/v1alpha1","kind":"BarmanBackupPage","revision":1,"evidence_generation":1,"total_items":0,"next_cursor":"","items":[]}`))
	f.Add([]byte(`{"items":null}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		page, err := decodeBackupPage(body)
		if err != nil {
			if page != nil {
				t.Fatal("a refusal returned a page")
			}
			return
		}
		if err := page.Validate(); err != nil {
			t.Fatalf("accepted a page that fails Validate: %v", err)
		}
		if page.NextCursor != nil && (*page.NextCursor == "" || len(*page.NextCursor) > evidencev1alpha1.MaximumCursorBytes) {
			t.Fatal("accepted an out-of-bounds cursor")
		}
		if len(page.Items) > evidencev1alpha1.MaximumPageSize {
			t.Fatal("accepted an oversize page")
		}
	})
}

// FuzzDecodeAPIError asserts that an accepted error body always names a
// code of the closed vocabulary.
func FuzzDecodeAPIError(f *testing.F) {
	f.Add(wireSeed(f, "error.json"))
	seed, _ := json.Marshal(evidencev1alpha1.EvidenceAPIError{APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ErrorKind, Code: evidencev1alpha1.ErrorAuthenticationUnavailable, Message: "m"})
	f.Add(seed)
	f.Add([]byte(`{"code":"anything"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		code, ok := decodeAPIError(body)
		if !ok {
			if code != "" {
				t.Fatal("a refusal returned a code")
			}
			return
		}
		if !slices.Contains(evidencev1alpha1.ErrorCodes(), code) {
			t.Fatalf("accepted code %q outside the vocabulary", code)
		}
	})
}

// FuzzDecodeRecoveryPage asserts that a decoded recovery-path page always
// passes the contract's Validate and carries a bounded cursor.
func FuzzDecodeRecoveryPage(f *testing.F) {
	f.Add(wireSeed(f, "recovery-paths.json"))
	f.Add([]byte(`{"api_version":"evidence.pgtoolbox.fyannk.dev/v1alpha1","kind":"BarmanRecoveryPathPage","revision":1,"evidence_generation":1,"total_items":0,"next_cursor":"","items":[]}`))
	f.Add([]byte(`{"items":null}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		page, err := decodeRecoveryPage(body)
		if err != nil {
			if page != nil {
				t.Fatal("a refusal returned a page")
			}
			return
		}
		if err := page.Validate(); err != nil {
			t.Fatalf("accepted a page that fails Validate: %v", err)
		}
		if page.NextCursor != nil && (*page.NextCursor == "" || len(*page.NextCursor) > evidencev1alpha1.MaximumCursorBytes) {
			t.Fatal("accepted an out-of-bounds cursor")
		}
		if len(page.Items) > evidencev1alpha1.MaximumPageSize {
			t.Fatal("accepted an oversize page")
		}
	})
}

// FuzzDecodePGBackRestBackupPage asserts that a decoded pgBackRest backup
// page always passes the contract's Validate and carries a bounded cursor.
func FuzzDecodePGBackRestBackupPage(f *testing.F) {
	f.Add(wireSeed(f, "pgbackrest-backups.json"))
	f.Add([]byte(`{"api_version":"evidence.pgtoolbox.fyannk.dev/v1alpha1","kind":"PGBackRestBackupPage","revision":1,"evidence_generation":1,"total_items":0,"next_cursor":"","items":[]}`))
	f.Add([]byte(`{"items":null}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		page, err := decodePGBackRestBackupPage(body)
		if err != nil {
			if page != nil {
				t.Fatal("a refusal returned a page")
			}
			return
		}
		if err := page.Validate(); err != nil {
			t.Fatalf("accepted a page that fails Validate: %v", err)
		}
		if page.NextCursor != nil && (*page.NextCursor == "" || len(*page.NextCursor) > evidencev1alpha1.MaximumCursorBytes) {
			t.Fatal("accepted an out-of-bounds cursor")
		}
		if len(page.Items) > evidencev1alpha1.MaximumPageSize {
			t.Fatal("accepted an oversize page")
		}
	})
}

// FuzzDecodePGBackRestRecoveryPage asserts the same of a pgBackRest
// recovery-path page.
func FuzzDecodePGBackRestRecoveryPage(f *testing.F) {
	f.Add(wireSeed(f, "pgbackrest-recovery-paths.json"))
	f.Add([]byte(`{"api_version":"evidence.pgtoolbox.fyannk.dev/v1alpha1","kind":"PGBackRestRecoveryPathPage","revision":1,"evidence_generation":1,"total_items":0,"next_cursor":"","items":[]}`))
	f.Add([]byte(`{"items":null}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		page, err := decodePGBackRestRecoveryPage(body)
		if err != nil {
			if page != nil {
				t.Fatal("a refusal returned a page")
			}
			return
		}
		if err := page.Validate(); err != nil {
			t.Fatalf("accepted a page that fails Validate: %v", err)
		}
		if page.NextCursor != nil && (*page.NextCursor == "" || len(*page.NextCursor) > evidencev1alpha1.MaximumCursorBytes) {
			t.Fatal("accepted an out-of-bounds cursor")
		}
		if len(page.Items) > evidencev1alpha1.MaximumPageSize {
			t.Fatal("accepted an oversize page")
		}
	})
}
