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

package evidence

import (
	"fmt"
	"strings"
	"testing"
)

// FuzzParseDestination drives the barman destination parser with
// arbitrary paths. The value comes from a CRD spec, so it is whatever a
// user with edit on the resource typed — and the parsed bucket and
// prefix are fed into a fingerprint that identifies a backup repository.
// A parse that accepts a malformed path silently points the operator at
// the wrong place.
func FuzzParseDestination(f *testing.F) {
	f.Add("s3://bucket/prefix")
	f.Add("s3://bucket")
	f.Add("s3://")
	f.Add("s3:///prefix-with-no-bucket")
	f.Add("gs://bucket/prefix")
	f.Add("s3://bucket/prefix/with/many/segments")
	f.Add("s3://bucket//double//slash")

	f.Fuzz(func(t *testing.T, raw string) {
		destination, err := ParseDestination(raw)
		if err != nil {
			// A rejected path yields nothing a caller could act on.
			if destination != (Destination{}) {
				t.Fatalf("ParseDestination(%s) failed but returned %+v", brief(raw), destination)
			}
			return
		}

		// Accepting means the path was an s3:// URL naming a bucket.
		if !strings.HasPrefix(raw, "s3://") {
			t.Fatalf("ParseDestination(%s) accepted a path that is not an s3:// URL", brief(raw))
		}
		if destination.Bucket == "" {
			t.Fatalf("ParseDestination(%s) accepted a path naming no bucket", brief(raw))
		}
		if strings.Contains(destination.Bucket, "/") {
			t.Fatalf("ParseDestination(%s) put a separator inside the bucket: %s", brief(raw), brief(destination.Bucket))
		}

		// The parse is a pure split, so it has to round-trip: anything
		// else means a byte was dropped or invented on the way through,
		// and the fingerprint downstream would identify the wrong
		// repository.
		rebuilt := "s3://" + destination.Bucket
		if destination.Prefix != "" || strings.Contains(strings.TrimPrefix(raw, "s3://"), "/") {
			rebuilt += "/" + destination.Prefix
		}
		if rebuilt != raw {
			t.Fatalf("ParseDestination(%s) does not round-trip: rebuilt %s", brief(raw), brief(rebuilt))
		}

		if again, againErr := ParseDestination(raw); againErr != nil || again != destination {
			t.Fatalf("ParseDestination(%s) is not deterministic", brief(raw))
		}
	})
}

// brief renders a fuzzed input for a failure message without pasting the
// whole thing into the log. The exact input is already persisted under
// testdata/fuzz/ by the fuzzing engine.
func brief(s string) string {
	const max = 48
	if len(s) <= max {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q… (%d bytes)", s[:max], len(s))
}
