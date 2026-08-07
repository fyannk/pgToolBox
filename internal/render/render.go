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

// Package render contains deterministic, cluster-independent render
// utilities shared by the operator's controllers.
package render

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
)

// Checksum returns the lowercase SHA-256 digest of a versioned canonical
// concatenation of rendered files, keyed by name.
func Checksum(rendered map[string][]byte) string {
	digest := sha256.New()
	writeChecksumPart(digest, []byte("pgtoolbox-checksum-v1"))
	writeChecksumMap(digest, rendered)
	return hex.EncodeToString(digest.Sum(nil))
}

// writeChecksumMap hashes a map in sorted key order: map iteration order
// must never reach the digest, or identical configurations would checksum
// differently and trigger spurious rollouts.
func writeChecksumMap(digest hash.Hash, values map[string][]byte) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writeChecksumUint64(digest, uint64(len(keys)))
	for _, key := range keys {
		writeChecksumPart(digest, []byte(key))
		writeChecksumPart(digest, values[key])
	}
}

// writeChecksumPart length-prefixes each value so adjacent parts cannot
// collide by shifting bytes across their boundary.
func writeChecksumPart(digest hash.Hash, value []byte) {
	writeChecksumUint64(digest, uint64(len(value)))
	_, _ = digest.Write(value)
}

// writeChecksumUint64 encodes lengths as fixed-width big-endian so the
// checksum framing is unambiguous and platform-independent.
func writeChecksumUint64(digest hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = digest.Write(encoded[:])
}

// Revision returns the short status revision for a configuration checksum.
func Revision(checksum string) string {
	if len(checksum) > 8 {
		checksum = checksum[:8]
	}
	return "cfg-" + checksum
}
