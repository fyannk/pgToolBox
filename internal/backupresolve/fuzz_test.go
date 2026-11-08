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

package backupresolve

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// FuzzParseDestination drives the barman destination parser with
// arbitrary paths. The value comes from a CRD spec, so it is whatever a
// user with edit on the resource typed — and the parsed bucket and
// prefix are fed into a fingerprint that identifies a backup repository.
// A parse that accepts a malformed path silently points the operator at
// the wrong place, and one that disagrees with the producer's parser makes
// the console reject a viewer that is reading the right repository.
func FuzzParseDestination(f *testing.F) {
	f.Add("s3://bucket/prefix")
	f.Add("s3://bucket")
	f.Add("s3://")
	f.Add("s3:///prefix-with-no-bucket")
	f.Add("gs://bucket/prefix")
	f.Add("s3://bucket/prefix/with/many/segments")
	f.Add("s3://bucket//double//slash")
	f.Add("s3://bucket/a%20b/%2F")
	f.Add("s3://user:pass@bucket/p")

	f.Fuzz(func(t *testing.T, raw string) {
		destination, err := ParseDestination(raw)
		bucket, prefix, contractErr := evidencev1alpha1.ParseS3Destination(raw)
		if (err == nil) != (contractErr == nil) {
			t.Fatalf("ParseDestination(%s) disagrees with the contract parser on acceptance", brief(raw))
		}
		if err != nil {
			// A rejected path yields nothing a caller could act on, and the
			// error never echoes the (possibly credential-bearing) input.
			if destination != (Destination{}) {
				t.Fatalf("ParseDestination(%s) failed but returned %+v", brief(raw), destination)
			}
			if err.Error() != ErrInvalidDestination.Error() {
				t.Fatalf("ParseDestination(%s) error is not the fixed, value-free message", brief(raw))
			}
			return
		}

		// Accepting means the path was an s3:// URL naming a bucket, split
		// exactly as the producer splits it.
		if !strings.HasPrefix(strings.ToLower(raw), "s3://") {
			t.Fatalf("ParseDestination(%s) accepted a path that is not an s3:// URL", brief(raw))
		}
		if destination.Bucket == "" || strings.Contains(destination.Bucket, "/") {
			t.Fatalf("ParseDestination(%s) produced bucket %s", brief(raw), brief(destination.Bucket))
		}
		if destination.Bucket != bucket || destination.Prefix != prefix {
			t.Fatalf("ParseDestination(%s) = %+v, contract parser %q/%q", brief(raw), destination, bucket, prefix)
		}
		if strings.HasPrefix(destination.Prefix, "/") || strings.HasSuffix(destination.Prefix, "/") {
			t.Fatalf("ParseDestination(%s) left an untrimmed prefix %s", brief(raw), brief(destination.Prefix))
		}

		if again, againErr := ParseDestination(raw); againErr != nil || again != destination {
			t.Fatalf("ParseDestination(%s) is not deterministic", brief(raw))
		}
	})
}

// FuzzRegionFromEndpoint checks the region derivation never invents a
// region for a host outside the AWS partitions and never panics.
func FuzzRegionFromEndpoint(f *testing.F) {
	f.Add("https://s3.eu-west-1.amazonaws.com")
	f.Add("https://s3.amazonaws.com")
	f.Add("http://minio:9000")
	f.Add("https://s3.eu-west-1.amazonaws.com.evil.test")
	f.Add("https://eu-west-1.s3.amazonaws.com")
	f.Add("https://s3.eusc-de-east-1.amazonaws.eu")
	f.Fuzz(func(t *testing.T, endpoint string) {
		region := RegionFromEndpoint(endpoint)
		if region == "" {
			return
		}
		if !awsRegionLabel.MatchString(region) {
			t.Fatalf("RegionFromEndpoint(%s) = %s, not a region code", brief(endpoint), brief(region))
		}
		parsed, err := url.Parse(endpoint)
		if err != nil {
			t.Fatalf("RegionFromEndpoint(%s) derived a region from an unparseable endpoint", brief(endpoint))
		}
		host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
		if !awsPartitionHost(host) {
			t.Fatalf("RegionFromEndpoint(%s) derived a region from a non-AWS host %s", brief(endpoint), brief(host))
		}
	})
}

// FuzzPgBackRestEndpoint checks the plugin endpoint mapping (a Stanza's or
// an Archive's endpoint, written by whoever may edit it): it yields an
// empty origin or a credential-free http(s) origin with no path, query or
// fragment, which the evidence contract canonicalizes, and an http origin
// only when the input asked for it.
func FuzzPgBackRestEndpoint(f *testing.F) {
	f.Add("minio:9000")
	f.Add("https://172.18.0.2:9000")
	f.Add("s3.minio.svc.cluster.local")
	f.Add("http://minio:9000")
	f.Add("https://user:secret@minio")
	f.Add("https://minio/path?x=1")
	f.Add("[::1]:9000")
	f.Fuzz(func(t *testing.T, raw string) {
		origin, ok := pgBackRestEndpoint(raw)
		if !ok || origin == "" {
			if ok && raw != "" {
				t.Fatalf("pgBackRestEndpoint(%s) accepted an empty origin", brief(raw))
			}
			return
		}
		parsed, err := url.Parse(origin)
		if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Hostname() == "" ||
			(parsed.Scheme != "https" && parsed.Scheme != "http") {
			t.Fatalf("pgBackRestEndpoint(%s) = %s, not a bare origin", brief(raw), brief(origin))
		}
		if parsed.Scheme == "http" && !strings.HasPrefix(strings.ToLower(raw), "http://") {
			t.Fatalf("pgBackRestEndpoint(%s) = %s downgraded to plain http", brief(raw), brief(origin))
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
