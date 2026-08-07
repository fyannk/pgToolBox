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

// Package session implements the proxy's stdlib-only sealed session
// cookies: a JSON blob encrypted with AES-256-GCM under keys derived from
// the configured secrets. The first configured secret encrypts; all are
// tried on decrypt so operators rotate keys by prepending a new secret.
package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Data is the sealed session payload.
type Data struct {
	// Subject is the authenticated identity, lowercased.
	Subject string `json:"sub"`
	// Level is the user's authorization level; empty for authenticated
	// identities that are unknown to the console.
	Level string `json:"lvl"`
	// Expiry is the session expiry as a Unix timestamp.
	Expiry int64 `json:"exp"`
	// ProviderMode records which provider minted the session.
	ProviderMode string `json:"prv"`
}

// Codec seals and opens session payloads.
type Codec struct {
	// keys holds one AES-256 key per configured secret. keys[0]
	// encrypts; every key is tried on decrypt.
	keys [][]byte
	// now is the clock, replaceable in tests.
	now func() time.Time
}

// NewCodec derives keys from the configured secrets. Secrets are
// arbitrary strings; each is hashed with SHA-256 into an AES-256 key.
// Secret values are never logged.
func NewCodec(secrets []string) (*Codec, error) {
	if len(secrets) == 0 {
		return nil, errors.New("at least one session secret is required")
	}
	keys := make([][]byte, len(secrets))
	for i, s := range secrets {
		if s == "" {
			return nil, fmt.Errorf("session secret %d is empty", i)
		}
		sum := sha256.Sum256([]byte(s))
		keys[i] = sum[:]
	}
	return &Codec{keys: keys, now: time.Now}, nil
}

// KeyCount reports how many keys the codec holds.
func (c *Codec) KeyCount() int { return len(c.keys) }

// Seal encrypts and encodes v into a cookie-safe string.
func (c *Codec) Seal(v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshaling session: %w", err)
	}
	block, err := aes.NewCipher(c.keys[0])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open decodes and decrypts s into v, trying each key in turn so cookies
// sealed under rotated-out keys keep working.
func (c *Codec) Open(s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return errors.New("session cookie is not valid base64")
	}
	for _, key := range c.keys {
		block, err := aes.NewCipher(key)
		if err != nil {
			return err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return err
		}
		if len(raw) < gcm.NonceSize() {
			return errors.New("session cookie is too short")
		}
		nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
		plain, err := gcm.Open(nil, nonce, ct, nil)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(plain, v); err != nil {
			return errors.New("session cookie payload is not valid JSON")
		}
		return nil
	}
	return errors.New("session cookie cannot be decrypted with any configured key")
}

// NewData builds a session payload expiring maxAge from now.
func (c *Codec) NewData(subject, level, providerMode string, maxAge time.Duration) Data {
	return Data{
		Subject:      subject,
		Level:        level,
		Expiry:       c.now().Add(maxAge).Unix(),
		ProviderMode: providerMode,
	}
}

// Valid reports whether d is a usable session: non-empty subject and not
// expired.
func (c *Codec) Valid(d Data) bool {
	return d.Subject != "" && c.now().Unix() < d.Expiry
}

// SetCookie seals d and writes it as the session cookie.
func (c *Codec) SetCookie(w http.ResponseWriter, name string, secure bool, maxAge time.Duration, d Data) error {
	v, err := c.Seal(d)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    v,
		Path:     "/",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// ReadCookie extracts and opens the session cookie of r.
func (c *Codec) ReadCookie(r *http.Request, name string) (Data, error) {
	cookie, err := r.Cookie(name)
	if err != nil {
		return Data{}, errors.New("no session cookie")
	}
	var d Data
	if err := c.Open(cookie.Value, &d); err != nil {
		return Data{}, err
	}
	if !c.Valid(d) {
		return Data{}, errors.New("session is expired or empty")
	}
	return d, nil
}

// ClearCookie expires the named cookie.
func ClearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// CSRFToken computes the access-request form token for a session: an
// HMAC-SHA256 over the session identity, keyed by the primary session
// key. It binds the form to this exact session.
func (c *Codec) CSRFToken(d Data) string {
	mac := hmac.New(sha256.New, c.keys[0])
	_, _ = fmt.Fprintf(mac, "%s|%d", d.Subject, d.Expiry)
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidCSRF reports whether token is the CSRF token of session d, in
// constant time.
func (c *Codec) ValidCSRF(d Data, token string) bool {
	want, err := hex.DecodeString(c.CSRFToken(d))
	if err != nil {
		return false
	}
	got, err := hex.DecodeString(token)
	if err != nil {
		return false
	}
	return hmac.Equal(want, got)
}
