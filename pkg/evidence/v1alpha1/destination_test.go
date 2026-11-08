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

package v1alpha1

import (
	"errors"
	"strings"
	"testing"
)

func TestParseS3Destination(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		bucket string
		prefix string
	}{
		{name: "bare bucket", raw: "s3://bucket", bucket: "bucket"},
		{name: "scheme is case-insensitive", raw: "S3://bucket/p", bucket: "bucket", prefix: "p"},
		{name: "bucket with trailing slash", raw: "s3://bucket/", bucket: "bucket"},
		{name: "nested prefix trimmed", raw: "s3://bucket/a/b/", bucket: "bucket", prefix: "a/b"},
		{name: "legacy upper-case bucket kept verbatim", raw: "s3://Legacy_Bucket.1/p", bucket: "Legacy_Bucket.1", prefix: "p"},
		{name: "percent-encoded space decoded once", raw: "s3://bucket/a%20b", bucket: "bucket", prefix: "a b"},
		{name: "encoded slash decoded once", raw: "s3://bucket/a%2Fb", bucket: "bucket", prefix: "a/b"},
		{name: "one encoded trailing slash is insignificant", raw: "s3://bucket/a%2F", bucket: "bucket", prefix: "a"},
		{name: "double encoding stays encoded once", raw: "s3://bucket/a%2520b", bucket: "bucket", prefix: "a%20b"},
		{name: "encoded question mark is a key character", raw: "s3://bucket/a%3Fb", bucket: "bucket", prefix: "a?b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bucket, prefix, err := ParseS3Destination(test.raw)
			if err != nil || bucket != test.bucket || prefix != test.prefix {
				t.Fatalf("ParseS3Destination(%q) = %q, %q, %v; want %q, %q", test.raw, bucket, prefix, err, test.bucket, test.prefix)
			}
		})
	}
}

func TestParseS3DestinationRejectsAmbiguousOrSecretBearingValues(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":                 "",
		"other scheme":          "gs://bucket/prefix",
		"no scheme":             "bucket/prefix",
		"opaque":                "s3:bucket/prefix",
		"empty bucket":          "s3:///prefix",
		"userinfo":              "s3://key:secret@bucket/prefix",
		"query":                 "s3://bucket/prefix?versionId=1",
		"empty query":           "s3://bucket/prefix?",
		"fragment":              "s3://bucket/prefix#part",
		"port":                  "s3://bucket:9000/prefix",
		"ipv6 host":             "s3://[::1]/prefix",
		"encoded bucket":        "s3://b%61cket/prefix",
		"bucket with space":     "s3://my bucket/prefix",
		"empty segment":         "s3://bucket/a//b",
		"leading empty segment": "s3://bucket//a",
		"encoded empty segment": "s3://bucket/a%2F%2Fb",
		"encoded leading slash": "s3://bucket/%2Fa",
		"encoded trailing pair": "s3://bucket/a%2F%2F",
		"mixed empty segment":   "s3://bucket/a/%2Fb",
		"control character":     "s3://bucket/a\nb",
		"encoded control":       "s3://bucket/a%0Ab",
		"encoded NUL":           "s3://bucket/a%00b",
		"invalid UTF-8":         "s3://bucket/a%FFb",
		"invalid escape":        "s3://bucket/a%zzb",
		"oversized bucket":      "s3://" + strings.Repeat("b", 256),
		"oversized prefix":      "s3://bucket/" + strings.Repeat("p", 1025),
	} {
		t.Run(name, func(t *testing.T) {
			bucket, prefix, err := ParseS3Destination(raw)
			if !errors.Is(err, ErrInvalidS3Destination) || bucket != "" || prefix != "" {
				t.Fatalf("ParseS3Destination(%q) = %q, %q, %v; want rejection", raw, bucket, prefix, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("error disclosed the rejected value")
			}
		})
	}
}

func FuzzParseS3Destination(f *testing.F) {
	for _, seed := range []string{"s3://bucket", "s3://bucket/a%20b/", "s3://a:b@c/d?e#f", "s3://bucket/a%2F%2Fb"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		bucket, prefix, err := ParseS3Destination(raw)
		if err != nil {
			if bucket != "" || prefix != "" {
				t.Fatal("rejection returned values")
			}
			return
		}
		if !validS3Bucket(bucket) || strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") || strings.Contains(prefix, "//") {
			t.Fatalf("accepted non-canonical result %q, %q", bucket, prefix)
		}
		// Every accepted destination must be fingerprintable.
		if _, err := FingerprintS3(S3FingerprintInput{Bucket: bucket, Prefix: prefix, Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders"}); err != nil {
			t.Fatalf("accepted destination %q is not fingerprintable: %v", raw, err)
		}
	})
}
