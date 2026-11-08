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
	"bytes"
	"testing"
)

func TestPurposeKeys(t *testing.T) {
	t.Parallel()
	secrets := [][]byte{bytes.Repeat([]byte("a"), 32), bytes.Repeat([]byte("b"), 32)}
	keys, err := PurposeKeys(secrets, "pgtoolbox-console/v1/test-purpose")
	if err != nil || len(keys) != 2 || len(keys[0]) != 32 || bytes.Equal(keys[0], keys[1]) {
		t.Fatalf("keys = %d, err = %v", len(keys), err)
	}
	other, _ := PurposeKeys(secrets, "pgtoolbox-console/v1/other-purpose")
	if bytes.Equal(keys[0], other[0]) {
		t.Error("two purposes derived the same key")
	}
	again, _ := PurposeKeys(secrets, "pgtoolbox-console/v1/test-purpose")
	if !bytes.Equal(keys[0], again[0]) {
		t.Error("derivation is not deterministic: a second replica could not verify")
	}
	for _, label := range []string{labelSession, labelLogin, ""} {
		if _, err := PurposeKeys(secrets, label); err == nil {
			t.Errorf("label %q accepted", label)
		}
	}
	if _, err := PurposeKeys(nil, "x"); err == nil {
		t.Error("no secrets accepted")
	}
}
