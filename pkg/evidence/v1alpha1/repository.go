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
	"net/url"
	"strings"
	"unicode"
)

// The repository providers of the identity. S3 is the initial profile;
// Azure Blob Storage and Google Cloud Storage use the same domain-separated
// hash with their own component list, so no two providers can collide.
const (
	ProviderS3    = "s3"
	ProviderAzure = "azure"
	ProviderGCS   = "gcs"
)

// validProvider reports whether provider is one of the three.
func validProvider(provider string) bool {
	return provider == ProviderS3 || provider == ProviderAzure || provider == ProviderGCS
}

// ErrInvalidAzureDestination is the only error ParseAzureDestination
// returns. It never embeds the rejected value, which may carry a SAS token.
var ErrInvalidAzureDestination = errors.New("the Azure destination must be a credential-free https://<account>.blob.core.windows.net/<container>/<prefix> URL, or an emulator http(s)://<host>/<account>/<container>/<prefix> URL")

// ErrInvalidGCSDestination is the only error ParseGCSDestination returns.
var ErrInvalidGCSDestination = errors.New("GCS destination must be a credential-free gs://bucket/prefix URI")

// azurePublicBlobSuffix is the public cloud's Blob Storage domain. Barman
// reads the account from the host under it and the container from the first
// path segment; any other host is an emulator (Azurite), whose path starts
// with the account (barman-cloud's AzureCloudInterface makes the same
// distinction).
const azurePublicBlobSuffix = ".blob.core.windows.net"

// azureSovereignBlobSuffixes are the national clouds' Blob Storage domains
// (Azure China, Azure Government, Azure Germany): addressed like the public
// cloud, the account first in the host.
var azureSovereignBlobSuffixes = []string{".blob.core.chinacloudapi.cn", ".blob.core.usgovcloudapi.net", ".blob.core.cloudapi.de"}

// AzureDestination is a parsed Barman Azure destinationPath.
type AzureDestination struct {
	// Endpoint is empty for the public cloud, the account's https origin in
	// a national cloud (https://<account>.blob.core.chinacloudapi.cn), and
	// the canonical emulator origin (scheme://host[:port]) otherwise.
	// AzureServiceURL turns it back into the account's Blob service URL.
	Endpoint  string
	Account   string
	Container string
	// Prefix is percent-decoded once and slash-trimmed.
	Prefix string
}

// ParseAzureDestination parses a Barman Azure destinationPath:
// https://<account>.blob.core.windows.net/<container>/<prefix>, or for an
// emulator http(s)://<host>[:port]/<account>/<container>/<prefix>.
func ParseAzureDestination(raw string) (AzureDestination, error) {
	parsed, segments, ok := parseObjectURL(raw, "http", "https")
	if !ok {
		return AzureDestination{}, ErrInvalidAzureDestination
	}
	host := strings.ToLower(parsed.Hostname())
	destination := AzureDestination{}
	if account, suffix, cloud := azureCloudHost(host); cloud {
		if parsed.Port() != "" || strings.Contains(account, ".") {
			return AzureDestination{}, ErrInvalidAzureDestination
		}
		destination.Account = account
		if suffix != azurePublicBlobSuffix {
			destination.Endpoint = "https://" + host
		}
	} else {
		endpoint, err := canonicalEndpoint(parsed.Scheme + "://" + parsed.Host)
		if err != nil || len(segments) == 0 {
			return AzureDestination{}, ErrInvalidAzureDestination
		}
		destination.Endpoint, destination.Account, segments = endpoint, segments[0], segments[1:]
	}
	if len(segments) == 0 || !validAzureAccount(destination.Account) || !validAzureContainer(segments[0]) {
		return AzureDestination{}, ErrInvalidAzureDestination
	}
	destination.Container = segments[0]
	destination.Prefix = strings.Join(segments[1:], "/")
	if !validPrefix(destination.Prefix) {
		return AzureDestination{}, ErrInvalidAzureDestination
	}
	return destination, nil
}

// azureCloudHost splits a public or national cloud Blob host into its
// account and domain suffix.
func azureCloudHost(host string) (account, suffix string, ok bool) {
	for _, suffix := range append([]string{azurePublicBlobSuffix}, azureSovereignBlobSuffixes...) {
		if account, found := strings.CutSuffix(host, suffix); found {
			return account, suffix, true
		}
	}
	return "", "", false
}

// Emulator reports a path-style destination (Azurite and the like), which
// Barman reaches only through a connection string.
func (d AzureDestination) Emulator() bool {
	return d.Endpoint != "" && AzureServiceURL(d.Endpoint, d.Account) != strings.TrimSuffix(d.Endpoint, "/")+"/"
}

// AzureServiceURL is the Blob service URL of account behind an
// AzureDestination's Endpoint: https://<account>.blob.core.windows.net/
// for the public cloud, the national cloud's origin, or the emulator's
// path-style <endpoint>/<account>/.
func AzureServiceURL(endpoint, account string) string {
	if endpoint == "" {
		return "https://" + account + azurePublicBlobSuffix + "/"
	}
	endpoint = strings.TrimSuffix(endpoint, "/")
	if parsed, err := url.Parse(endpoint); err == nil {
		if host, _, cloud := azureCloudHost(strings.ToLower(parsed.Hostname())); cloud && host == account {
			return endpoint + "/"
		}
	}
	return endpoint + "/" + account + "/"
}

// ParseGCSDestination parses a Barman Google Cloud Storage destinationPath,
// gs://bucket/prefix.
func ParseGCSDestination(raw string) (bucket, prefix string, err error) {
	parsed, segments, ok := parseObjectURL("https://"+strings.TrimPrefix(raw, "gs://"), "https")
	if !ok || !strings.HasPrefix(raw, "gs://") || parsed.Port() != "" {
		return "", "", ErrInvalidGCSDestination
	}
	bucket = parsed.Host
	prefix = strings.Join(segments, "/")
	if !validGCSBucket(bucket) || !validPrefix(prefix) {
		return "", "", ErrInvalidGCSDestination
	}
	return bucket, prefix, nil
}

// parseObjectURL is the common grammar of a credential-free object URL: one
// of schemes, a host that is not percent-encoded, no userinfo, query or
// fragment, and a path without empty segments (raw or decoded). It returns
// the decoded, non-empty path segments.
func parseObjectURL(raw string, schemes ...string) (*url.URL, []string, bool) {
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "?#") || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return nil, nil, false
	}
	_, rest, _ := strings.Cut(raw, "://")
	if authority, _, _ := strings.Cut(rest, "/"); strings.Contains(authority, "%") {
		return nil, nil, false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.Hostname() == "" {
		return nil, nil, false
	}
	schemeAllowed := false
	for _, scheme := range schemes {
		schemeAllowed = schemeAllowed || parsed.Scheme == scheme
	}
	if !schemeAllowed || strings.Contains(parsed.EscapedPath(), "//") || strings.Contains(parsed.Path, "//") {
		return nil, nil, false
	}
	trimmed := strings.Trim(parsed.Path, "/")
	if trimmed == "" {
		return parsed, nil, true
	}
	return parsed, strings.Split(trimmed, "/"), true
}

func validPrefix(prefix string) bool {
	return len(prefix) <= maximumS3PrefixBytes && strings.ToValidUTF8(prefix, "") == prefix && strings.IndexFunc(prefix, unicode.IsControl) < 0
}

// validAzureAccount: 3 to 24 lower-case letters and digits.
func validAzureAccount(value string) bool {
	if len(value) < 3 || len(value) > 24 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

// validAzureContainer: 3 to 63 lower-case letters, digits and single
// hyphens, starting and ending with a letter or digit.
func validAzureContainer(value string) bool {
	if len(value) < 3 || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' || strings.Contains(value, "--") {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

// validGCSBucket: 3 to 222 lower-case letters, digits, '-', '_' and '.',
// starting and ending with a letter or digit.
func validGCSBucket(value string) bool {
	if len(value) < 3 || len(value) > 222 {
		return false
	}
	alphanumeric := func(character byte) bool {
		return (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9')
	}
	if !alphanumeric(value[0]) || !alphanumeric(value[len(value)-1]) {
		return false
	}
	for i := range len(value) {
		if character := value[i]; !alphanumeric(character) && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

// RepositoryFingerprintInput is the provider-neutral identity input. Bucket
// is the S3 or GCS bucket or the Azure container; Account is the Azure
// storage account (empty for the other providers); Endpoint is the S3
// compatible origin, the Azure emulator origin, or empty (always for GCS).
type RepositoryFingerprintInput struct {
	Provider  string
	Endpoint  string
	Account   string
	Bucket    string
	Prefix    string
	Format    string
	ScopeKind string
	ScopeName string
}

// CanonicalRepositoryFingerprintInput validates and canonicalizes an input
// exactly as FingerprintRepository hashes it. For S3 it is
// CanonicalS3FingerprintInput.
func CanonicalRepositoryFingerprintInput(input RepositoryFingerprintInput) (RepositoryFingerprintInput, error) {
	switch input.Provider {
	case ProviderS3:
		if input.Account != "" {
			return RepositoryFingerprintInput{}, errors.New("S3 fingerprint input has an account")
		}
		canonical, err := CanonicalS3FingerprintInput(S3FingerprintInput{
			Endpoint: input.Endpoint, Bucket: input.Bucket, Prefix: input.Prefix,
			Format: input.Format, ScopeKind: input.ScopeKind, ScopeName: input.ScopeName,
		})
		if err != nil {
			return RepositoryFingerprintInput{}, err
		}
		return RepositoryFingerprintInput{
			Provider: ProviderS3, Endpoint: canonical.Endpoint, Bucket: canonical.Bucket, Prefix: canonical.Prefix,
			Format: canonical.Format, ScopeKind: canonical.ScopeKind, ScopeName: canonical.ScopeName,
		}, nil
	case ProviderAzure, ProviderGCS:
	default:
		return RepositoryFingerprintInput{}, errors.New("repository provider is invalid")
	}
	endpoint, err := canonicalEndpoint(input.Endpoint)
	if err != nil {
		return RepositoryFingerprintInput{}, err
	}
	input.Endpoint = endpoint
	input.Prefix = strings.Trim(input.Prefix, "/")
	if !validScopeProfile(input.Format, input.ScopeKind, input.ScopeName) {
		return RepositoryFingerprintInput{}, errors.New("repository fingerprint profile is invalid")
	}
	if input.Provider == ProviderGCS && (input.Endpoint != "" || input.Account != "") {
		return RepositoryFingerprintInput{}, errors.New("GCS fingerprint input has an endpoint or an account")
	}
	if input.Provider == ProviderAzure && invalidFingerprintText(input.Account, 255, true) {
		return RepositoryFingerprintInput{}, errors.New("no valid account in the Azure fingerprint input")
	}
	if invalidFingerprintText(input.Bucket, 255, true) || invalidFingerprintText(input.Prefix, 1024, false) || invalidFingerprintText(input.ScopeName, 256, true) {
		return RepositoryFingerprintInput{}, errors.New("repository fingerprint input is invalid")
	}
	return input, nil
}

func invalidFingerprintText(value string, maximum int, required bool) bool {
	return (required && value == "") || len(value) > maximum || strings.ToValidUTF8(value, "") != value || strings.IndexFunc(value, unicode.IsControl) >= 0
}

// FingerprintRepository returns the sha256: destination identity of one
// Barman server in one repository of any provider. An S3 input yields
// exactly FingerprintS3's value. Azure hashes
// ("azure", endpoint, account, container, prefix, format, scope kind,
// scope name) and GCS ("gcs", bucket, prefix, format, scope kind, scope
// name), each length-prefixed after FingerprintDomain.
func FingerprintRepository(input RepositoryFingerprintInput) (string, error) {
	canonical, err := CanonicalRepositoryFingerprintInput(input)
	if err != nil {
		return "", err
	}
	var components []string
	switch canonical.Provider {
	case ProviderS3:
		return FingerprintS3(S3FingerprintInput{
			Endpoint: canonical.Endpoint, Bucket: canonical.Bucket, Prefix: canonical.Prefix,
			Format: canonical.Format, ScopeKind: canonical.ScopeKind, ScopeName: canonical.ScopeName,
		})
	case ProviderAzure:
		components = []string{ProviderAzure, canonical.Endpoint, canonical.Account, canonical.Bucket, canonical.Prefix, canonical.Format, canonical.ScopeKind, canonical.ScopeName}
	default:
		components = []string{ProviderGCS, canonical.Bucket, canonical.Prefix, canonical.Format, canonical.ScopeKind, canonical.ScopeName}
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(FingerprintDomain))
	for _, value := range components {
		var length [4]byte
		// #nosec G115 -- every canonical component is bounded above.
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}
