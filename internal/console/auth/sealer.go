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

package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// HKDF labels, one key per purpose and per configured secret: a login-flow
// cookie can never be opened as a session and the other way round. The
// labels are versioned, so a cookie sealed under other labels never opens
// here even when an admin reuses the secret.
const (
	labelSession = "pgtoolbox-console/v1/session-aead"
	labelLogin   = "pgtoolbox-console/v1/login-flow-aead"
)

// Errors returned by Open. They name the failure class only; the input is
// attacker-controlled and is never echoed.
var (
	ErrMalformed     = errors.New("sealed value is malformed")
	ErrUndecryptable = errors.New("sealed value does not open under any configured key")
)

// maxSealedBytes bounds what Open decodes: the session cookie is at most
// maxChunks chunks of maxChunkBytes.
const maxSealedBytes = maxChunks * maxChunkBytes

// sealer encrypts JSON payloads with AES-256-GCM under HKDF-SHA256 keys.
// Index 0 of each key list seals; every key opens, which is the rotation
// story: prepend a new secret, keep the old one for one session lifetime.
type sealer struct {
	session []cipher.AEAD
	login   []cipher.AEAD
	// secrets derive the keys of the purposes outside sign-in (SealPurpose,
	// KeyedHashes), each under a label of its own, on first use.
	secrets  [][]byte
	purposes sync.Map // HKDF label -> []cipher.AEAD
}

func newSealer(secrets [][]byte) (*sealer, error) {
	if len(secrets) == 0 {
		return nil, errors.New("at least one session secret is required")
	}
	s := &sealer{secrets: secrets}
	for i, secret := range secrets {
		if len(secret) == 0 {
			return nil, fmt.Errorf("session secret %d is empty", i)
		}
		for _, d := range []struct {
			label string
			into  *[]cipher.AEAD
		}{{labelSession, &s.session}, {labelLogin, &s.login}} {
			key, err := hkdf.Key(sha256.New, secret, nil, d.label, 32)
			if err != nil {
				return nil, fmt.Errorf("deriving key %d: %w", i, err)
			}
			block, err := aes.NewCipher(key)
			if err != nil {
				return nil, err
			}
			aead, err := cipher.NewGCM(block)
			if err != nil {
				return nil, err
			}
			*d.into = append(*d.into, aead)
		}
	}
	return s, nil
}

// purposeAEADs are the AEADs of one sealing purpose: HKDF keys of their
// own (PurposeKeys), so that a key or a mistake of one purpose never
// touches another's or the session cookie's.
func (s *sealer) purposeAEADs(label string) ([]cipher.AEAD, error) {
	if v, ok := s.purposes.Load(label); ok {
		aeads, _ := v.([]cipher.AEAD)
		return aeads, nil
	}
	keys, err := PurposeKeys(s.secrets, label)
	if err != nil {
		return nil, err
	}
	aeads := make([]cipher.AEAD, 0, len(keys))
	for _, key := range keys {
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		aeads = append(aeads, aead)
	}
	v, _ := s.purposes.LoadOrStore(label, aeads)
	out, _ := v.([]cipher.AEAD)
	return out, nil
}

// seal encrypts v; aad binds the value to its cookie so one cookie's value
// pasted into another does not open.
func seal(aeads []cipher.AEAD, aad string, v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encoding sealed payload: %w", err)
	}
	aead := aeads[0]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(aad))), nil
}

// open decrypts s into v, trying every key.
func open(aeads []cipher.AEAD, aad, s string, v any) error {
	if s == "" || len(s) > base64.RawURLEncoding.EncodedLen(maxSealedBytes) {
		return ErrMalformed
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s) // canonical only: one value per ciphertext
	if err != nil {
		return ErrMalformed
	}
	return openRaw(aeads, aad, raw, v)
}

// openRaw decrypts decoded raw into v, trying every key.
func openRaw(aeads []cipher.AEAD, aad string, raw []byte, v any) error {
	for _, aead := range aeads {
		if len(raw) < aead.NonceSize()+aead.Overhead() {
			return ErrMalformed
		}
		nonce, ct := raw[:aead.NonceSize()], raw[aead.NonceSize():]
		plain, err := aead.Open(nil, nonce, ct, []byte(aad))
		if err != nil {
			continue
		}
		if err := json.Unmarshal(plain, v); err != nil {
			return ErrMalformed
		}
		return nil
	}
	return ErrUndecryptable
}

// PurposeKeys derives one 32-byte key per configured session secret for a
// purpose outside sign-in (HKDF-SHA256 with the purpose's own label, so a
// key never equals a cookie key). Index 0 signs; every key verifies, the
// same rotation story as the cookies. The restore drills use it to seal
// the Clusters the console creates (internal/console/ops).
func PurposeKeys(secrets [][]byte, label string) ([][]byte, error) {
	if len(secrets) == 0 {
		return nil, errors.New("at least one session secret is required")
	}
	if label == labelSession || label == labelLogin || label == "" {
		return nil, errors.New("purpose label must be distinct from the sign-in labels")
	}
	out := make([][]byte, 0, len(secrets))
	for i, secret := range secrets {
		if len(secret) == 0 {
			return nil, fmt.Errorf("session secret %d is empty", i)
		}
		key, err := hkdf.Key(sha256.New, secret, nil, label, 32)
		if err != nil {
			return nil, fmt.Errorf("deriving key %d: %w", i, err)
		}
		out = append(out, key)
	}
	return out, nil
}
