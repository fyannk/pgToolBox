/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package session

import (
	"strings"
	"testing"
	"time"
)

func testCodec(t *testing.T, secrets ...string) *Codec {
	t.Helper()
	c, err := NewCodec(secrets)
	if err != nil {
		t.Fatalf("NewCodec: %v", err)
	}
	return c
}

func TestSealOpenRoundtrip(t *testing.T) {
	c := testCodec(t, "secret-one-abcdefghij")
	in := c.NewData("jane@corp.example", "dba", "oidc", time.Hour)
	sealed, err := c.Seal(in)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	var out Data
	if err := c.Open(sealed, &out); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if out != in {
		t.Fatalf("roundtrip mismatch: got %+v want %+v", out, in)
	}
	if strings.Contains(sealed, "jane") {
		t.Fatal("sealed cookie leaks plaintext subject")
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	c := testCodec(t, "secret-one-abcdefghij")
	sealed, err := c.Seal(c.NewData("jane@corp.example", "view", "oidc", time.Hour))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Flip a character in the middle of the payload.
	b := []byte(sealed)
	mid := len(b) / 2
	if b[mid] == 'A' {
		b[mid] = 'B'
	} else {
		b[mid] = 'A'
	}
	var out Data
	if err := c.Open(string(b), &out); err == nil {
		t.Fatal("Open succeeded on tampered cookie")
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	sealer := testCodec(t, "secret-one-abcdefghij")
	opener := testCodec(t, "secret-two-klmnopqrst")
	sealed, err := sealer.Seal(sealer.NewData("jane@corp.example", "view", "oidc", time.Hour))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	var out Data
	if err := opener.Open(sealed, &out); err == nil {
		t.Fatal("Open succeeded with the wrong key")
	}
}

func TestKeyRotation(t *testing.T) {
	old := testCodec(t, "old-secret-abcdefghij")
	sealed, err := old.Seal(old.NewData("jane@corp.example", "poweruser", "local", time.Hour))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Rotate: new secret prepended, old one still accepted.
	rotated := testCodec(t, "new-secret-abcdefghij", "old-secret-abcdefghij")
	var out Data
	if err := rotated.Open(sealed, &out); err != nil {
		t.Fatalf("Open with rotated keys: %v", err)
	}
	if out.Level != "poweruser" {
		t.Fatalf("unexpected level %q", out.Level)
	}
	// New cookies are sealed with the new (first) key only.
	fresh, err := rotated.Seal(rotated.NewData("x", "view", "local", time.Hour))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := old.Open(fresh, &out); err == nil {
		t.Fatal("old-only codec opened a cookie sealed with the new key")
	}
}

func TestExpiryEnforced(t *testing.T) {
	now := time.Now()
	c := testCodec(t, "secret-one-abcdefghij")
	c.now = func() time.Time { return now }
	d := c.NewData("jane@corp.example", "view", "oidc", time.Hour)
	sealed, err := c.Seal(d)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// Advance the clock past expiry: the payload opens (it is well
	// formed) but ReadCookie-style validity must fail.
	c.now = func() time.Time { return now.Add(2 * time.Hour) }
	var out Data
	if err := c.Open(sealed, &out); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if c.Valid(out) {
		t.Fatal("expired session reported valid")
	}
	if c.Valid(Data{}) {
		t.Fatal("empty session reported valid")
	}
}

func TestCSRFToken(t *testing.T) {
	c := testCodec(t, "secret-one-abcdefghij")
	d := c.NewData("jane@corp.example", "", "oidc", time.Hour)
	token := c.CSRFToken(d)
	if !c.ValidCSRF(d, token) {
		t.Fatal("valid CSRF token rejected")
	}
	if c.ValidCSRF(d, "deadbeef") {
		t.Fatal("garbage CSRF token accepted")
	}
	other := c.NewData("mallory@corp.example", "", "oidc", time.Hour)
	if c.ValidCSRF(other, token) {
		t.Fatal("CSRF token of another subject accepted")
	}
	differentExpiry := c.NewData("jane@corp.example", "", "oidc", 2*time.Hour)
	if c.ValidCSRF(differentExpiry, token) {
		t.Fatal("CSRF token accepted for a different session expiry")
	}
}

func TestNewCodecRejectsEmpty(t *testing.T) {
	if _, err := NewCodec(nil); err == nil {
		t.Fatal("NewCodec accepted no secrets")
	}
	if _, err := NewCodec([]string{""}); err == nil {
		t.Fatal("NewCodec accepted an empty secret")
	}
}
