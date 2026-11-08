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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"
)

// FingerprintDomain separates the S3 repository identity hash from every other
// SHA-256 use; its suffix versions the hashed input. The region is not part of
// that input: an S3 bucket name is unique within its partition, so the region
// adds no identity while making producer and deployment tooling disagree
// whenever only one side knows it.
const FingerprintDomain = "pgtoolbox-evidence-repository-v1"

// S3FingerprintInput contains only credential-free values accepted by the S3
// adapter. Endpoint may be empty when the provider default is used. The
// signing region is deliberately absent: it selects how requests are signed,
// not which repository is read.
type S3FingerprintInput struct {
	Endpoint  string
	Bucket    string
	Prefix    string
	Format    string
	ScopeKind string
	ScopeName string
}

// CanonicalS3FingerprintInput validates the initial S3 profile and returns the
// exact values used in the repository destination fingerprint.
func CanonicalS3FingerprintInput(input S3FingerprintInput) (S3FingerprintInput, error) {
	endpoint, err := canonicalEndpoint(input.Endpoint)
	if err != nil {
		return S3FingerprintInput{}, err
	}
	input.Endpoint = endpoint
	input.Prefix = strings.Trim(input.Prefix, "/")
	if !validScopeProfile(input.Format, input.ScopeKind, input.ScopeName) {
		return S3FingerprintInput{}, errors.New("S3 fingerprint profile is invalid")
	}
	for _, field := range []struct {
		value    string
		maximum  int
		required bool
	}{
		{input.Bucket, 255, true},
		{input.Prefix, 1024, false},
		{input.ScopeName, 256, true},
	} {
		if (field.required && field.value == "") || len(field.value) > field.maximum || strings.ToValidUTF8(field.value, "") != field.value || strings.IndexFunc(field.value, unicode.IsControl) >= 0 {
			return S3FingerprintInput{}, errors.New("S3 fingerprint input is invalid")
		}
	}
	if !validBarmanServer(input.ScopeName) {
		return S3FingerprintInput{}, errors.New("S3 fingerprint scope is invalid")
	}
	return input, nil
}

// FingerprintS3 returns the domain-separated, length-prefixed SHA-256 identity
// shared by the evidence agent, the console, and pgtoolbox.
func FingerprintS3(input S3FingerprintInput) (string, error) {
	canonical, err := CanonicalS3FingerprintInput(input)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(FingerprintDomain))
	for _, value := range []string{
		"s3",
		canonical.Endpoint,
		canonical.Bucket,
		canonical.Prefix,
		canonical.Format,
		canonical.ScopeKind,
		canonical.ScopeName,
	} {
		var length [4]byte
		// #nosec G115 -- every canonical component is bounded by the validators in
		// this package before it reaches the domain-separated fingerprint.
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

func canonicalEndpoint(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || (parsed.Path != "" && parsed.Path != "/") || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return "", errors.New("S3 endpoint must be a credential-free HTTP origin")
	}
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	hostname := strings.ToLower(parsed.Hostname())
	host := hostname
	if strings.Contains(hostname, ":") || port != "" {
		host = net.JoinHostPort(hostname, port)
		if port == "" {
			host = "[" + hostname + "]"
		}
	}
	return strings.ToLower(parsed.Scheme) + "://" + host, nil
}
