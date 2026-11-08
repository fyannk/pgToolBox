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
	"net/url"
	"strings"
	"unicode"
)

// ErrInvalidS3Destination is the only error ParseS3Destination returns. It
// never embeds the rejected value, which may carry credentials.
var ErrInvalidS3Destination = errors.New("S3 destination must be a credential-free s3://bucket/prefix URI")

const (
	maximumS3BucketBytes = 255
	maximumS3PrefixBytes = 1024
)

// ParseS3Destination splits a Barman-style S3 destination ("s3://bucket" or
// "s3://bucket/prefix") into the bucket and the decoded, slash-trimmed key
// prefix used by the evidence agent and by the destination fingerprint.
//
// The prefix is percent-decoded exactly once ("s3://b/a%20b" names the key
// prefix "a b", "%2F" is a "/"), so every party hashing the same destination
// obtains the same fingerprint. After decoding, one leading and one trailing
// "/" are insignificant ("s3://b/a%2F" equals "s3://b/a/" and "s3://b/a"),
// but an empty segment anywhere is rejected, encoded or not. The parser rejects values whose meaning is ambiguous or that
// could carry secrets: a query or fragment (any raw '?' or '#'), userinfo, a
// port, an empty, percent-encoded, or non-portable bucket, an empty path
// segment ("//", also after decoding), control characters, and invalid UTF-8
// after decoding. The scheme is case-insensitive, as in every URI.
func ParseS3Destination(raw string) (bucket, prefix string, err error) {
	if raw == "" || strings.ContainsAny(raw, "?#") || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "", "", ErrInvalidS3Destination
	}
	// url.Parse percent-decodes the host, so an encoded bucket would hash
	// differently from its literal spelling; reject it before parsing.
	_, rest, _ := strings.Cut(raw, "://")
	if authority, _, _ := strings.Cut(rest, "/"); strings.Contains(authority, "%") {
		return "", "", ErrInvalidS3Destination
	}
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil || parsed.Scheme != "s3" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Port() != "" {
		return "", "", ErrInvalidS3Destination
	}
	// The empty-segment check runs on the raw and on the decoded path before
	// trimming, so "%2Fa" and "a%2F%2F" are rejected exactly like "//a".
	if !validS3Bucket(parsed.Host) || strings.Contains(parsed.EscapedPath(), "//") || strings.Contains(parsed.Path, "//") {
		return "", "", ErrInvalidS3Destination
	}
	prefix = strings.Trim(parsed.Path, "/")
	if len(prefix) > maximumS3PrefixBytes || strings.ToValidUTF8(prefix, "") != prefix ||
		strings.IndexFunc(prefix, unicode.IsControl) >= 0 {
		return "", "", ErrInvalidS3Destination
	}
	return parsed.Host, prefix, nil
}

// validS3Bucket accepts the portable bucket alphabet used by AWS (including
// legacy upper-case names) and S3-compatible stores. Anything else, such as a
// percent-encoded or IPv6 host, cannot be a bucket name.
func validS3Bucket(value string) bool {
	if value == "" || len(value) > maximumS3BucketBytes {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case character == '-', character == '.', character == '_':
		default:
			return false
		}
	}
	return true
}
