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
	"bytes"
	"crypto/sha256"
	"testing"
)

func FuzzCodec(f *testing.F) {
	codec := Codec{key: bytes.Repeat([]byte{0xa5}, sha256.Size)}
	seed, err := codec.Encode("alpha/", "provider-token")
	if err != nil {
		f.Fatal(err)
	}
	f.Add("alpha/", "provider-token", seed)
	f.Add("", "", "")
	f.Add("beta/", "token", "not-a-cursor")

	f.Fuzz(func(t *testing.T, prefix, token, candidate string) {
		encoded, err := codec.Encode(prefix, token)
		if err == nil {
			decoded, decodeErr := codec.Decode(prefix, encoded)
			if decodeErr != nil || decoded != token {
				t.Fatal("encoded cursor did not round-trip within its prefix")
			}
			if _, decodeErr := codec.Decode(prefix+"\x00", encoded); decodeErr == nil {
				t.Fatal("encoded cursor escaped its prefix")
			}
		}

		decoded, err := codec.Decode(prefix, candidate)
		if err != nil {
			return
		}
		if candidate == "" {
			if decoded != "" {
				t.Fatal("empty cursor decoded to a provider token")
			}
			return
		}
		canonical, err := codec.Encode(prefix, decoded)
		if err != nil || canonical != candidate {
			t.Fatal("accepted cursor was not a canonical confined token")
		}
	})
}
