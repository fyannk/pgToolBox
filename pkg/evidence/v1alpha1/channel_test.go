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
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syntheticToken is shaped like a ServiceAccount JWS; it is not a credential.
const syntheticToken = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQ6dGVhbTpjb25zb2xlIn0.c2lnbmF0dXJl"

// TestChannelConstantsArePinned freezes the values the console and the
// operator compile against: changing one is a contract change.
func TestChannelConstantsArePinned(t *testing.T) {
	pinned := map[string]any{
		"MediaType":                MediaType,
		"HealthPath":               HealthPath,
		"ReadinessPath":            ReadinessPath,
		"SnapshotPath":             SnapshotPath,
		"BackupsPath":              BackupsPath,
		"WALRangesPath":            WALRangesPath,
		"WALGapsPath":              WALGapsPath,
		"RecoveryPathsPath":        RecoveryPathsPath,
		"MaximumSnapshotBytes":     MaximumSnapshotBytes,
		"MaximumCollectionBytes":   MaximumCollectionBytes,
		"DefaultPageSize":          DefaultPageSize,
		"MaximumPageSize":          MaximumPageSize,
		"MaximumCursorBytes":       MaximumCursorBytes,
		"RequestTimeout":           RequestTimeout,
		"TokenAudiencePrefix":      TokenAudiencePrefix,
		"ClusterWideTokenAudience": ClusterWideTokenAudience,
		"DefaultTokenDirectory":    DefaultTokenDirectory,
		"TokenFileName":            TokenFileName,
		"AuthorizationGroup":       AuthorizationGroup,
		"AuthorizationResource":    AuthorizationResource,
		"AuthorizationVerb":        AuthorizationVerb,
		"AgentPort":                AgentPort,
		"AgentPortName":            AgentPortName,
		"APIVersion":               APIVersion,
	}
	want := map[string]any{
		"MediaType":                "application/vnd.pgtoolbox.evidence.v1alpha1+json",
		"HealthPath":               "/healthz",
		"ReadinessPath":            "/readyz",
		"SnapshotPath":             "/api/v1alpha1/snapshot",
		"BackupsPath":              "/api/v1alpha1/backups",
		"WALRangesPath":            "/api/v1alpha1/wal-ranges",
		"WALGapsPath":              "/api/v1alpha1/wal-gaps",
		"RecoveryPathsPath":        "/api/v1alpha1/recovery-paths",
		"MaximumSnapshotBytes":     262144,
		"MaximumCollectionBytes":   1048576,
		"DefaultPageSize":          100,
		"MaximumPageSize":          200,
		"MaximumCursorBytes":       4096,
		"RequestTimeout":           5 * time.Second,
		"TokenAudiencePrefix":      "pgtoolbox-evidence-agent/",
		"ClusterWideTokenAudience": "pgtoolbox-evidence-agent",
		"DefaultTokenDirectory":    "/var/run/secrets/pgtoolbox.fyannk.dev/evidence-agent",
		"TokenFileName":            "token",
		"AuthorizationGroup":       "pgtoolbox.fyannk.dev",
		"AuthorizationResource":    "evidenceagents",
		"AuthorizationVerb":        "get",
		"AgentPort":                8443,
		"AgentPortName":            "https",
		"APIVersion":               "evidence.pgtoolbox.fyannk.dev/v1alpha1",
	}
	for name, value := range want {
		if fmt.Sprint(pinned[name]) != fmt.Sprint(value) {
			t.Errorf("%s = %v, want %v", name, pinned[name], value)
		}
	}
	if MinimumTLSVersion != tls.VersionTLS13 {
		t.Error("the agent channel is TLS 1.3 only")
	}
	if RecommendedTokenExpirationSeconds < MinimumTokenExpirationSeconds {
		t.Error("recommended token lifetime is below the kubelet floor")
	}
}

func TestEndpointAndServerName(t *testing.T) {
	if got := Endpoint("orders-evidence", "team-a"); got != "https://orders-evidence.team-a.svc:8443" {
		t.Fatalf("Endpoint = %q", got)
	}
	if got := ServerName("orders-evidence", "team-a"); got != "orders-evidence.team-a.svc" {
		t.Fatalf("ServerName = %q", got)
	}
}

func TestParseProducerEpoch(t *testing.T) {
	for raw, want := range map[string]uint64{"1": 1, "42": 42, "18446744073709551615": ^uint64(0)} {
		got, err := ParseProducerEpoch(raw)
		if err != nil || got != want {
			t.Fatalf("ParseProducerEpoch(%q) = %d, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "0", "-1", "+1", " 1", "1 ", "0x1", "18446744073709551616", "1e3"} {
		if _, err := ParseProducerEpoch(raw); err == nil {
			t.Fatalf("ParseProducerEpoch(%q) accepted", raw)
		}
	}
}

func TestParseBearerToken(t *testing.T) {
	for _, content := range []string{syntheticToken, syntheticToken + "\n", "a.b.c"} {
		token, err := ParseBearerToken([]byte(content))
		if err != nil || !token.Valid() || token.Value() != strings.TrimSuffix(content, "\n") {
			t.Fatalf("ParseBearerToken(%q) = %v", content, err)
		}
	}
	for _, content := range []string{
		"", "\n", "a.b", "a.b.c.d", ".b.c", "a..c", "a.b.", "a.b.c\n\n", "a.b.c\r\n", " a.b.c", "a.b.c ",
		"a+b.c.d", "a/b.c.d", "a=.b.c", "a.b.c\x00", "é.b.c",
		strings.Repeat("a", MaximumBearerTokenBytes) + ".b.c",
	} {
		if _, err := ParseBearerToken([]byte(content)); err == nil {
			t.Fatalf("ParseBearerToken(%q) accepted", content)
		}
	}
}

func TestParseAuthorization(t *testing.T) {
	for _, header := range []string{"Bearer " + syntheticToken, "bearer " + syntheticToken, "BEARER a.b.c"} {
		token, err := ParseAuthorization(header)
		if err != nil || token.Value() != header[len("Bearer "):] {
			t.Fatalf("ParseAuthorization(%q) = %v", header, err)
		}
		if token.AuthorizationHeader() != "Bearer "+token.Value() {
			t.Fatal("AuthorizationHeader does not round-trip")
		}
	}
	for _, header := range []string{
		"", "Bearer", "Bearer ", "Bearer  a.b.c", "Bearer\ta.b.c", "Basic a.b.c", "Bearera.b.c",
		"Bearer a.b.c ", "Bearer a.b", "Bearer " + strings.Repeat("a", MaximumBearerTokenBytes),
		// an opaque 43-character random token is not a bound token
		"Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		if _, err := ParseAuthorization(header); err == nil {
			t.Fatalf("ParseAuthorization(%q) accepted", header)
		}
	}
}

func TestBearerTokenNeverFormatsItsValue(t *testing.T) {
	token, err := ParseBearerToken([]byte(syntheticToken))
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("token", slog.Any("token", token))
	encoded, err := json.Marshal(map[string]any{"token": token})
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{fmt.Sprint(token), fmt.Sprintf("%v %+v %#v %s", token, token, token, token), logged.String(), string(encoded)} {
		if strings.Contains(rendered, "eyJ") {
			t.Fatalf("token value leaked: %s", rendered)
		}
	}
	if (BearerToken{}).Valid() || (BearerToken{}).AuthorizationHeader() != "" {
		t.Fatal("the zero token must be invalid")
	}
}

func TestReadBearerTokenFile(t *testing.T) {
	directory := t.TempDir()
	// The kubelet's projected-volume layout: token -> ..data/token -> versioned file.
	versioned := filepath.Join(directory, "..2026_10_29_10_00_00.000000001")
	if err := os.Mkdir(versioned, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versioned, "token"), []byte(syntheticToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(versioned), filepath.Join(directory, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(directory, "token")); err != nil {
		t.Fatal(err)
	}
	token, err := ReadBearerTokenFile(filepath.Join(directory, "token"))
	if err != nil || token.Value() != syntheticToken {
		t.Fatalf("ReadBearerTokenFile = %v", err)
	}

	oversized := filepath.Join(directory, "oversized")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("a", MaximumBearerTokenBytes+8)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "relative/token", directory + "/./token", filepath.Join(directory, "missing"), directory, oversized} {
		_, err := ReadBearerTokenFile(path)
		if err == nil {
			t.Fatalf("ReadBearerTokenFile(%q) accepted", path)
		}
		if path != "" && strings.Contains(err.Error(), path) {
			t.Fatal("error embeds the path")
		}
	}
}

func TestValidProducerInstance(t *testing.T) {
	if !ValidProducerInstance("0123456789abcdef0123456789abcdef") {
		t.Fatal("valid instance rejected")
	}
	for _, value := range []string{"", "0123456789abcdef", "0123456789ABCDEF0123456789ABCDEF", "0123456789abcdef0123456789abcdeg"} {
		if ValidProducerInstance(value) {
			t.Fatalf("accepted %q", value)
		}
	}
}

// FuzzParseAuthorization asserts the parser's invariant, not just absence of
// panics: a header is accepted only when it is the bearer scheme, one space
// and a compact JWS within bounds, and the accepted token re-renders to the
// same header modulo the scheme's case.
func FuzzParseAuthorization(f *testing.F) {
	for _, seed := range []string{
		"Bearer " + syntheticToken, "bearer a.b.c", "Bearer a.b", "Basic dXNlcjpwYXNz", "Bearer  a.b.c",
		"Bearer a.b.c\n", "Bearer a..c", "", "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		token, err := ParseAuthorization(header)
		if err != nil {
			if token.Valid() {
				t.Fatal("rejected header produced a valid token")
			}
			return
		}
		value := token.Value()
		if len(value) == 0 || len(value) > MaximumBearerTokenBytes || strings.Count(value, ".") != 2 {
			t.Fatalf("accepted token out of shape: %q", value)
		}
		for _, segment := range strings.Split(value, ".") {
			if segment == "" || strings.Trim(segment, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
				t.Fatalf("accepted segment %q", segment)
			}
		}
		if !strings.EqualFold(token.AuthorizationHeader(), header) || token.AuthorizationHeader()[len("Bearer "):] != header[len("Bearer "):] {
			t.Fatalf("accepted header does not round-trip: %q", header)
		}
		reparsed, err := ParseBearerToken([]byte(value))
		if err != nil || reparsed.Value() != value {
			t.Fatal("header and file parsers disagree")
		}
	})
}

// sharedToken is 48 alphanumerics, what the pgtools chart generates.
const sharedToken = "Zx81QpL0aVt7Kc2mN4bR9sWe3Yh6Ju5Gd0Fo1Ti8Lk2Mn7Bv"

func TestParseSharedToken(t *testing.T) {
	for _, content := range []string{sharedToken, sharedToken + "\n", sharedToken + "\r\n", syntheticToken, strings.Repeat("a", 30) + "==", "abc-._~+/" + strings.Repeat("x", 23)} {
		token, err := ParseSharedToken([]byte(content))
		if err != nil || token.Value() != strings.TrimRight(content, "\r\n") {
			t.Fatalf("ParseSharedToken(%q) = %v", content, err)
		}
		header, err := ParseSharedAuthorization(token.AuthorizationHeader())
		if err != nil || header.Value() != token.Value() {
			t.Fatalf("the header of %q does not round-trip: %v", content, err)
		}
	}
	for _, content := range []string{
		"", "short", strings.Repeat("a", MinimumSharedTokenBytes-1), strings.Repeat("=", 40), sharedToken + " ", " " + sharedToken,
		sharedToken + "\n\n", sharedToken[:20] + "\n" + sharedToken[20:], sharedToken + "=a", sharedToken + "é",
		strings.Repeat("a", MaximumBearerTokenBytes+1),
	} {
		if token, err := ParseSharedToken([]byte(content)); err == nil || token.Valid() {
			t.Fatalf("ParseSharedToken(%q) accepted", content)
		}
	}
	for _, header := range []string{"", sharedToken, "Basic " + sharedToken, "Bearer  " + sharedToken, "Bearer " + sharedToken + "\n", "Bearer"} {
		if _, err := ParseSharedAuthorization(header); err == nil {
			t.Fatalf("ParseSharedAuthorization(%q) accepted", header)
		}
	}
	if _, err := ParseSharedAuthorization("bEaReR " + sharedToken); err != nil {
		t.Fatal("the scheme is case-insensitive")
	}
}

func TestReadSharedTokenFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "token")
	if err := os.WriteFile(path, []byte(sharedToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := ReadSharedTokenFile(path)
	if err != nil || token.Value() != sharedToken {
		t.Fatalf("ReadSharedTokenFile = %v", err)
	}
	if fmt.Sprint(token) != "[REDACTED]" {
		t.Fatal("a shared token formats its value")
	}
	empty := filepath.Join(directory, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"relative", empty, filepath.Join(directory, "missing"), directory} {
		if _, err := ReadSharedTokenFile(candidate); err == nil {
			t.Fatalf("ReadSharedTokenFile(%q) accepted", candidate)
		}
	}
}

// FuzzParseSharedAuthorization asserts the shared-token parser's invariant:
// an accepted header is the bearer scheme, one space and a token68 within
// bounds, and it agrees with the file parser.
func FuzzParseSharedAuthorization(f *testing.F) {
	for _, seed := range []string{"Bearer " + sharedToken, "bearer " + syntheticToken, "Bearer short", "Bearer " + sharedToken + "==", "Bearer " + sharedToken + "=x", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, header string) {
		token, err := ParseSharedAuthorization(header)
		if err != nil {
			if token.Valid() {
				t.Fatal("rejected header produced a valid token")
			}
			return
		}
		value := token.Value()
		if len(value) < MinimumSharedTokenBytes || len(value) > MaximumBearerTokenBytes {
			t.Fatalf("accepted token of %d bytes", len(value))
		}
		body := strings.TrimRight(value, "=")
		if body == "" || strings.Trim(body, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~+/") != "" {
			t.Fatalf("accepted token out of the token68 alphabet: %q", value)
		}
		if token.AuthorizationHeader()[len("Bearer "):] != header[len("Bearer "):] {
			t.Fatalf("accepted header does not round-trip: %q", header)
		}
		reparsed, err := ParseSharedToken([]byte(value))
		if err != nil || reparsed.Value() != value {
			t.Fatal("header and file parsers disagree")
		}
	})
}
