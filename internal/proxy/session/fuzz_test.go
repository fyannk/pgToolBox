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
	"fmt"
	"strings"
	"testing"
)

// FuzzOpen drives the session codec with arbitrary cookie values. The
// sealed cookie is the most untrusted input the proxy takes: it arrives
// from a browser, and anyone can put anything in it. Invariant 5 makes
// the proxy the only authorization boundary, so a forged cookie that
// opens is a complete authentication bypass. Nothing that was not sealed
// by a configured key may open, at any length or encoding.
func FuzzOpen(f *testing.F) {
	f.Add("")
	f.Add("not-base64!!")
	f.Add("aGVsbG8")                    // valid base64, far too short
	f.Add(strings.Repeat("A", 64))      // valid base64 alphabet, wrong content
	f.Add(strings.Repeat("A", 100_000)) // long enough to matter

	codec, err := NewCodec([]string{"fuzz-secret-one"})
	if err != nil {
		f.Fatalf("NewCodec: %v", err)
	}

	f.Fuzz(func(t *testing.T, cookie string) {
		var data Data
		err := codec.Open(cookie, &data)
		if err == nil {
			// The only way this is legitimate is if the fuzzer happened to
			// produce a genuine sealing, which it cannot: it does not have
			// the key. Anything else is forgery that succeeded.
			t.Fatalf("Open accepted an unsealed cookie (%s) as %+v", brief(cookie), data)
		}
		if data != (Data{}) {
			t.Fatalf("Open failed but wrote into the destination: %+v", data)
		}

		// A rejected cookie must never yield a usable session even if a
		// caller ignores the error, which is the mistake this guards.
		if codec.Valid(data) {
			t.Fatalf("a session from a rejected cookie reported valid: %+v", data)
		}
	})
}

// FuzzValidCSRF asserts the CSRF check cannot be satisfied by a token the
// codec did not mint. Nothing here is secret from the fuzzer except the
// key, which is the whole point.
func FuzzValidCSRF(f *testing.F) {
	f.Add("alice", int64(1), "")
	f.Add("alice", int64(1), "deadbeef")
	f.Add("", int64(0), strings.Repeat("0", 64))
	f.Add("bob", int64(1<<40), "not-hex")

	codec, err := NewCodec([]string{"fuzz-secret-one"})
	if err != nil {
		f.Fatalf("NewCodec: %v", err)
	}

	f.Fuzz(func(t *testing.T, subject string, expiry int64, token string) {
		data := Data{Subject: subject, Expiry: expiry}

		// EqualFold, not ==: ValidCSRF compares the decoded bytes, and
		// hex.DecodeString accepts either case, so an uppercase spelling
		// of the genuine token verifies correctly. Comparing the strings
		// raw would flag that as a forgery the moment the fuzzer found
		// one.
		if codec.ValidCSRF(data, token) && !strings.EqualFold(token, codec.CSRFToken(data)) {
			t.Fatalf("ValidCSRF accepted a token that is not its own for subject %s", brief(subject))
		}

		// The genuine token always verifies, and it is deterministic:
		// a CSRF check that drifted per call would lock every user out.
		genuine := codec.CSRFToken(data)
		if !codec.ValidCSRF(data, genuine) {
			t.Fatalf("ValidCSRF rejected the token it just minted for subject %s", brief(subject))
		}
		if again := codec.CSRFToken(data); again != genuine {
			t.Fatalf("CSRFToken is not deterministic for subject %s", brief(subject))
		}

		// A token for a different expiry must not verify: the expiry is
		// inside the MAC precisely so a captured token dies with it.
		other := Data{Subject: subject, Expiry: expiry ^ 1}
		if codec.ValidCSRF(other, genuine) {
			t.Fatalf("a token minted for expiry %d verified for %d", expiry, other.Expiry)
		}
	})
}

// brief renders a fuzzed input for a failure message without pasting the
// whole thing into the log. The exact input is already persisted under
// testdata/fuzz/ by the fuzzing engine, so the message only has to say
// enough to recognise it.
func brief(s string) string {
	const max = 48
	if len(s) <= max {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q… (%d bytes)", s[:max], len(s))
}
