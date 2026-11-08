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

package cursor

import (
	"strings"
	"testing"
)

func TestCodecConfinesTokensToPrefixAndStore(t *testing.T) {
	t.Parallel()
	first, err := New()
	if err != nil {
		t.Fatal(err)
	}
	second, err := New()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := first.Encode("alpha/", "provider-token-canary")
	if err != nil {
		t.Fatal(err)
	}
	if encoded == "" || encoded == "provider-token-canary" {
		t.Fatalf("opaque cursor = %q", encoded)
	}
	if got, err := first.Decode("alpha/", encoded); err != nil || got != "provider-token-canary" {
		t.Fatalf("Decode() = %q, %v", got, err)
	}
	for name, codec := range map[string]Codec{"other store": second, "tampered": first} {
		value := encoded
		if name == "tampered" {
			value = encoded[:len(encoded)-1] + "A"
		}
		if _, err := codec.Decode("beta/", value); err == nil {
			t.Fatalf("%s cursor was accepted", name)
		}
	}
}

func TestCodecRejectsOversizedCursor(t *testing.T) {
	t.Parallel()
	codec, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Encode("prefix", strings.Repeat("x", maxEncodedBytes)); err == nil {
		t.Fatal("oversized native token was accepted")
	}
	if _, err := codec.Decode("prefix", strings.Repeat("x", maxEncodedBytes+1)); err == nil {
		t.Fatal("oversized encoded cursor was accepted")
	}
}

func TestCodecRejectsNonCanonicalEncoding(t *testing.T) {
	t.Parallel()
	codec, err := New()
	if err != nil {
		t.Fatal(err)
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	// A body length that is not a multiple of three leaves unused bits in the
	// final base64 character, which is where a second spelling of one cursor
	// would otherwise appear.
	for _, token := range []string{"a", "ab", "abcd", "abcde"} {
		value, err := codec.Encode("alpha/", token)
		if err != nil {
			t.Fatal(err)
		}
		for i := range len(alphabet) {
			variant := value[:len(value)-1] + string(alphabet[i])
			if variant == value {
				continue
			}
			if _, err := codec.Decode("alpha/", variant); err == nil {
				t.Fatalf("token %q: cursor %q was accepted as well as %q", token, variant, value)
			}
		}
		// The decoder also skips carriage returns and newlines, which would
		// otherwise let the same cursor be spelled at any length.
		for _, filler := range []string{"\r", "\n", "\r\n"} {
			variant := value[:4] + filler + value[4:]
			if _, err := codec.Decode("alpha/", variant); err == nil {
				t.Fatalf("token %q: cursor carrying %q was accepted", token, filler)
			}
		}
	}
}
