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

// Package cursor confines provider continuation tokens to one store and prefix.
package cursor

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
)

const maxEncodedBytes = 16 * 1024

// Codec signs provider continuation tokens with a random per-process key, so a
// cursor is only accepted by the store that issued it and for the same prefix.
type Codec struct{ key []byte }

// New returns a codec with a fresh random key.
func New() (Codec, error) {
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return Codec{}, err
	}
	return Codec{key: key}, nil
}

// Encode wraps the provider token and the prefix it belongs to into a signed,
// URL-safe cursor.
func (c Codec) Encode(prefix, token string) (string, error) {
	if len(c.key) != sha256.Size || len(prefix) > maxEncodedBytes || len(token) > maxEncodedBytes {
		return "", errors.New("invalid cursor")
	}
	body := make([]byte, 4+len(prefix)+len(token), 4+len(prefix)+len(token)+sha256.Size)
	// #nosec G115 -- the guard above bounds len(prefix) to maxEncodedBytes.
	binary.BigEndian.PutUint32(body, uint32(len(prefix)))
	copy(body[4:], prefix)
	copy(body[4+len(prefix):], token)
	mac := hmac.New(sha256.New, c.key)
	_, _ = mac.Write(body)
	encoded := base64.RawURLEncoding.EncodeToString(append(body, mac.Sum(nil)...))
	if len(encoded) > maxEncodedBytes {
		return "", errors.New("invalid cursor")
	}
	return encoded, nil
}

// Decode returns the provider token inside a cursor from Encode, or an error
// if the cursor is malformed, not signed by this codec or issued for another
// prefix. An empty cursor decodes to an empty token.
func (c Codec) Decode(prefix, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) > maxEncodedBytes {
		return "", errors.New("invalid cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) < 36 {
		return "", errors.New("invalid cursor")
	}
	// The decoder accepts more than one spelling of the same bytes: it ignores
	// carriage returns and newlines, and outside Strict mode it also ignores
	// non-zero unused bits in the final character. Either way the signature
	// still verifies, so a caller could hold several distinct strings for one
	// cursor. Requiring the value to be exactly what Encode would produce
	// rejects all of them without depending on which quirks the decoder has.
	if base64.RawURLEncoding.EncodeToString(raw) != value {
		return "", errors.New("invalid cursor")
	}
	body, sig := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, c.key)
	_, _ = mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", errors.New("invalid cursor")
	}
	n := int(binary.BigEndian.Uint32(body[:4]))
	if n > len(body)-4 || string(body[4:4+n]) != prefix {
		return "", errors.New("invalid cursor")
	}
	return string(body[4+n:]), nil
}
