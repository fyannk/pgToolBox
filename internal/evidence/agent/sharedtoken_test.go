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

package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidenceapi"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	firstSharedToken  = "Zx81QpL0aVt7Kc2mN4bR9sWe3Yh6Ju5Gd0Fo1Ti8Lk2Mn7Bv"
	secondSharedToken = "Qa5Ws1Ed9Rf3Tg7Yh2Uj6Ik0Ol4Pz8Xc5Vb1Nm9Mq3Wr7Et2"
)

func sharedHeaderToken(t *testing.T, value string) evidencev1alpha1.BearerToken {
	t.Helper()
	token, err := evidencev1alpha1.ParseSharedAuthorization("Bearer " + value)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// swapSecret mimics the kubelet's atomic update of a Secret volume: a new
// versioned directory, then the ..data symlink swapped to it.
func swapSecret(t *testing.T, directory, content string) {
	t.Helper()
	versioned, err := os.MkdirTemp(directory, "..version")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(versioned, "token"), []byte(content))
	next := filepath.Join(directory, "..data_tmp")
	if err := os.Symlink(filepath.Base(versioned), next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, filepath.Join(directory, "..data")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "token")); err != nil {
		if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(directory, "token")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSharedTokenAuthenticatorFollowsRotation(t *testing.T) {
	directory := t.TempDir()
	swapSecret(t, directory, firstSharedToken+"\n")
	var logs bytes.Buffer
	authenticator, err := NewSharedTokenAuthenticator(t.Context(), filepath.Join(directory, "token"), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if decision := authenticator.Authenticate(ctx, sharedHeaderToken(t, firstSharedToken)); decision.Outcome != evidenceapi.AuthAllowed || decision.User != SharedTokenUser {
		t.Fatalf("the current token = %+v", decision)
	}
	if decision := authenticator.Authenticate(ctx, sharedHeaderToken(t, secondSharedToken)); decision.Outcome != evidenceapi.AuthUnauthenticated {
		t.Fatalf("another token = %+v", decision)
	}

	// The Secret is rotated: the next request sees the new value, and the
	// old one is refused at once (a rotation is a revocation).
	time.Sleep(10 * time.Millisecond)
	swapSecret(t, directory, secondSharedToken)
	if decision := authenticator.Authenticate(ctx, sharedHeaderToken(t, secondSharedToken)); decision.Outcome != evidenceapi.AuthAllowed {
		t.Fatalf("the rotated token = %+v", decision)
	}
	if decision := authenticator.Authenticate(ctx, sharedHeaderToken(t, firstSharedToken)); decision.Outcome != evidenceapi.AuthUnauthenticated {
		t.Fatalf("the previous token after rotation = %+v", decision)
	}
	if !strings.Contains(logs.String(), "shared token rotated") || strings.Contains(logs.String(), firstSharedToken) || strings.Contains(logs.String(), secondSharedToken) {
		t.Fatalf("logs = %s", logs.String())
	}
}

func TestSharedTokenAuthenticatorRefusesEverythingWithoutAValidFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "token")
	authenticator, err := NewSharedTokenAuthenticator(t.Context(), path, nil)
	if err != nil {
		t.Fatalf("a missing file must not fail the start: %v", err)
	}
	ctx := context.Background()
	for name, content := range map[string]*string{"missing": nil, "empty": new(""), "too short": new("short"), "two lines": new(firstSharedToken + "\n" + firstSharedToken)} {
		if content != nil {
			_ = os.Remove(path)
			writeFile(t, path, []byte(*content))
		}
		if decision := authenticator.Authenticate(ctx, sharedHeaderToken(t, firstSharedToken)); decision.Outcome != evidenceapi.AuthUnavailable {
			t.Fatalf("%s file: decision = %+v", name, decision)
		}
	}
	// The file appears: requests are accepted without a restart.
	_ = os.Remove(path)
	writeFile(t, path, []byte(firstSharedToken))
	if decision := authenticator.Authenticate(ctx, sharedHeaderToken(t, firstSharedToken)); decision.Outcome != evidenceapi.AuthAllowed {
		t.Fatalf("after the file appeared: %+v", decision)
	}
	// The zero token is refused before any comparison.
	if decision := authenticator.Authenticate(ctx, evidencev1alpha1.BearerToken{}); decision.Outcome != evidenceapi.AuthUnauthenticated {
		t.Fatalf("zero token = %+v", decision)
	}
	for _, bad := range []string{"", "relative/token", directory + "/../token"} {
		if _, err := NewSharedTokenAuthenticator(t.Context(), bad, nil); err == nil {
			t.Fatalf("NewSharedTokenAuthenticator(%q) accepted", bad)
		}
	}
}

// TestSharedTokenComparisonIsDigestBased pins the constant-time shape: a
// token sharing a long prefix with the expected one is refused exactly like
// an unrelated one (both compare two fixed-size digests).
func TestSharedTokenComparisonIsDigestBased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	writeFile(t, path, []byte(firstSharedToken))
	authenticator, err := NewSharedTokenAuthenticator(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	prefix := firstSharedToken[:len(firstSharedToken)-1] + "X"
	for _, candidate := range []string{prefix, firstSharedToken + "A", secondSharedToken} {
		if decision := authenticator.Authenticate(context.Background(), sharedHeaderToken(t, candidate)); decision.Outcome != evidenceapi.AuthUnauthenticated {
			t.Fatalf("%q = %+v", candidate, decision)
		}
	}
}
