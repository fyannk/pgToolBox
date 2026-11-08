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
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

type repositoryVector struct {
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Destination string `json:"destination"`
	Endpoint    string `json:"endpoint"`
	Account     string `json:"account"`
	Bucket      string `json:"bucket"`
	Prefix      string `json:"prefix"`
	Format      string `json:"format"`
	ScopeKind   string `json:"scope_kind"`
	ScopeName   string `json:"scope_name"`
	Fingerprint string `json:"fingerprint"`
}

func (v repositoryVector) input() RepositoryFingerprintInput {
	return RepositoryFingerprintInput{
		Provider: v.Provider, Endpoint: v.Endpoint, Account: v.Account, Bucket: v.Bucket, Prefix: v.Prefix,
		Format: v.Format, ScopeKind: v.ScopeKind, ScopeName: v.ScopeName,
	}
}

// TestFingerprintRepositoryGoldenVectors pins the cross-component identity of
// every provider: the parsers' output and the hash.
func TestFingerprintRepositoryGoldenVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/repository-fingerprints.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []repositoryVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			switch vector.Provider {
			case ProviderS3:
				bucket, prefix, err := ParseS3Destination(vector.Destination)
				if err != nil || bucket != vector.Bucket || prefix != vector.Prefix {
					t.Fatalf("ParseS3Destination = %q %q %v", bucket, prefix, err)
				}
				s3, err := FingerprintS3(S3FingerprintInput{Endpoint: vector.Endpoint, Bucket: vector.Bucket, Prefix: vector.Prefix, Format: vector.Format, ScopeKind: vector.ScopeKind, ScopeName: vector.ScopeName})
				if err != nil || s3 != vector.Fingerprint {
					t.Fatalf("FingerprintS3 = %q, %v: the S3 identity changed", s3, err)
				}
			case ProviderAzure:
				destination, err := ParseAzureDestination(vector.Destination)
				if err != nil || destination != (AzureDestination{Endpoint: vector.Endpoint, Account: vector.Account, Container: vector.Bucket, Prefix: vector.Prefix}) {
					t.Fatalf("ParseAzureDestination = %+v, %v", destination, err)
				}
			case ProviderGCS:
				bucket, prefix, err := ParseGCSDestination(vector.Destination)
				if err != nil || bucket != vector.Bucket || prefix != vector.Prefix {
					t.Fatalf("ParseGCSDestination = %q %q %v", bucket, prefix, err)
				}
			}
			got, err := FingerprintRepository(vector.input())
			if err != nil || got != vector.Fingerprint {
				t.Fatalf("FingerprintRepository = %q, %v; want %q", got, err, vector.Fingerprint)
			}
		})
	}
}

// TestFingerprintRepositorySeparatesProviders: the same bucket, prefix and
// server under two providers are two repositories.
func TestFingerprintRepositorySeparatesProviders(t *testing.T) {
	seen := map[string]string{}
	for _, input := range []RepositoryFingerprintInput{
		{Provider: ProviderS3, Bucket: "backups", Prefix: "a"},
		{Provider: ProviderGCS, Bucket: "backups", Prefix: "a"},
		{Provider: ProviderAzure, Account: "backups", Bucket: "backups", Prefix: "a"},
		{Provider: ProviderAzure, Endpoint: "http://azurite:10000", Account: "backups", Bucket: "backups", Prefix: "a"},
	} {
		input.Format, input.ScopeKind, input.ScopeName = "barman-cloud", "barman-server", "orders"
		fingerprint, err := FingerprintRepository(input)
		if err != nil {
			t.Fatal(err)
		}
		if previous, found := seen[fingerprint]; found {
			t.Fatalf("%+v collides with %s", input, previous)
		}
		seen[fingerprint] = input.Provider + input.Endpoint
	}
}

func TestCanonicalRepositoryFingerprintInputRejects(t *testing.T) {
	valid := RepositoryFingerprintInput{Provider: ProviderAzure, Account: "orders", Bucket: "backups", Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders"}
	for name, mutate := range map[string]func(*RepositoryFingerprintInput){
		"unknown provider":      func(i *RepositoryFingerprintInput) { i.Provider = "ftp" },
		"azure without account": func(i *RepositoryFingerprintInput) { i.Account = "" },
		"s3 with an account":    func(i *RepositoryFingerprintInput) { i.Provider = ProviderS3 },
		"gcs with an account":   func(i *RepositoryFingerprintInput) { i.Provider = ProviderGCS },
		"gcs with an endpoint": func(i *RepositoryFingerprintInput) {
			i.Provider, i.Account, i.Endpoint = ProviderGCS, "", "https://storage.example.test"
		},
		"endpoint with a path": func(i *RepositoryFingerprintInput) { i.Endpoint = "https://azurite/x" },
		"no bucket":            func(i *RepositoryFingerprintInput) { i.Bucket = "" },
		"pgbackrest":           func(i *RepositoryFingerprintInput) { i.Format = "pgbackrest" },
		"scope with a slash":   func(i *RepositoryFingerprintInput) { i.ScopeName = "a/b" },
	} {
		t.Run(name, func(t *testing.T) {
			input := valid
			mutate(&input)
			if _, err := FingerprintRepository(input); err == nil {
				t.Fatalf("%+v was fingerprinted", input)
			}
		})
	}
	if _, err := FingerprintRepository(valid); err != nil {
		t.Fatal(err)
	}
}

func TestParseAzureDestination(t *testing.T) {
	for _, raw := range []string{
		"",
		"azure://backups/a",
		"https://orders.blob.core.windows.net",
		"https://orders.blob.core.windows.net/",
		"https://orders.blob.core.windows.net/Backups/a",
		"https://orders.blob.core.windows.net/backups//a",
		"https://orders.blob.core.windows.net/backups/%2Fa",
		"https://orders.blob.core.windows.net/backups/a?sv=2024&sig=canary",
		"https://orders.blob.core.windows.net/backups/a#x",
		"https://user:canary@orders.blob.core.windows.net/backups/a",
		"https://orders.blob.core.windows.net:8443/backups/a",
		"https://a.b.blob.core.windows.net/backups/a",
		"https://or.blob.core.windows.net/backups/a",
		"https://or%64ers.blob.core.windows.net/backups/a",
		"http://azurite:10000/devstoreaccount1",
		"http://azurite:10000/devstoreaccount1/",
		"http://azurite:10000/Dev/backups",
		"ftp://azurite/devstoreaccount1/backups",
		"https://orders.blob.core.windows.net/b--ad/a",
	} {
		if destination, err := ParseAzureDestination(raw); !errors.Is(err, ErrInvalidAzureDestination) {
			t.Errorf("ParseAzureDestination(%q) = %+v, %v; want ErrInvalidAzureDestination", raw, destination, err)
		}
	}
	if strings.Contains(ErrInvalidAzureDestination.Error(), "canary") {
		t.Fatal("the error embeds a value")
	}
}

// TestAzureServiceURL: the parsed Endpoint and account give back the Blob
// service URL Barman addresses, for every kind of destination.
func TestAzureServiceURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://orders.blob.core.windows.net/backups/a":        "https://orders.blob.core.windows.net/",
		"https://orders.blob.core.chinacloudapi.cn/backups/a":   "https://orders.blob.core.chinacloudapi.cn/",
		"https://orders.blob.core.usgovcloudapi.net/backups/a":  "https://orders.blob.core.usgovcloudapi.net/",
		"https://orders.blob.core.cloudapi.de/backups/a":        "https://orders.blob.core.cloudapi.de/",
		"http://azurite:10000/devstoreaccount1/backups/a":       "http://azurite:10000/devstoreaccount1/",
		"https://storage.example.test/devstoreaccount1/backups": "https://storage.example.test/devstoreaccount1/",
	} {
		destination, err := ParseAzureDestination(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := AzureServiceURL(destination.Endpoint, destination.Account); got != want {
			t.Errorf("AzureServiceURL(%s) = %s, want %s", raw, got, want)
		}
	}
	// A national cloud host is never read as an emulator.
	if destination, err := ParseAzureDestination("https://orders.blob.core.chinacloudapi.cn/backups"); err != nil ||
		destination.Account != "orders" || destination.Container != "backups" {
		t.Fatalf("national cloud = %+v, %v", destination, err)
	}
	if _, err := ParseAzureDestination("https://orders.blob.core.chinacloudapi.cn:8443/backups"); err == nil {
		t.Fatal("a national cloud host with a port was accepted")
	}
}

func TestParseGCSDestination(t *testing.T) {
	for _, raw := range []string{
		"", "gs://", "gs:///a", "s3://bucket/a", "gs://Bucket/a", "gs://bucket//a", "gs://bucket/a?x=1", "gs://user@bucket/a",
		"gs://bucket:443/a", "gs://ab/a", "gs://-bucket/a", "gs://bu%63ket/a", "https://bucket/a",
	} {
		if bucket, prefix, err := ParseGCSDestination(raw); !errors.Is(err, ErrInvalidGCSDestination) {
			t.Errorf("ParseGCSDestination(%q) = %q %q %v; want ErrInvalidGCSDestination", raw, bucket, prefix, err)
		}
	}
}

// FuzzParseAzureDestination: whatever it accepts carries no credential
// carrier, is fingerprintable, and is a fixed point of its own spelling.
func FuzzParseAzureDestination(f *testing.F) {
	for _, seed := range []string{
		"https://orders.blob.core.windows.net/backups/team-a",
		"https://orders.blob.core.usgovcloudapi.net/backups/team-a",
		"http://azurite:10000/devstoreaccount1/backups/a%20b",
		"https://orders.blob.core.windows.net/backups/a?sig=x",
		"https://u:p@orders.blob.core.windows.net/backups",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		destination, err := ParseAzureDestination(raw)
		if err != nil {
			if !errors.Is(err, ErrInvalidAzureDestination) {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		if strings.ContainsAny(raw, "?#@") && !strings.Contains(destination.Prefix, "@") {
			t.Fatalf("%q accepted with a query, fragment or userinfo", raw)
		}
		if !validAzureAccount(destination.Account) || !validAzureContainer(destination.Container) {
			t.Fatalf("%q accepted with an invalid account or container: %+v", raw, destination)
		}
		if service := AzureServiceURL(destination.Endpoint, destination.Account); !strings.Contains(service, destination.Account) || strings.ContainsAny(service, "?#@") {
			t.Fatalf("%q: service URL %q does not name the account alone", raw, service)
		}
		if _, err := FingerprintRepository(RepositoryFingerprintInput{
			Provider: ProviderAzure, Endpoint: destination.Endpoint, Account: destination.Account, Bucket: destination.Container,
			Prefix: destination.Prefix, Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders",
		}); err != nil {
			t.Fatalf("%q parsed but is not fingerprintable: %v", raw, err)
		}
	})
}

// FuzzParseGCSDestination: whatever it accepts is a valid bucket and a
// bounded prefix, and is fingerprintable.
func FuzzParseGCSDestination(f *testing.F) {
	for _, seed := range []string{"gs://cnpg-backups/prod", "gs://b/a?x", "gs://u@bucket/a", "gs://bucket/%2F"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		bucket, prefix, err := ParseGCSDestination(raw)
		if err != nil {
			if !errors.Is(err, ErrInvalidGCSDestination) {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		if !strings.HasPrefix(raw, "gs://") || !validGCSBucket(bucket) || strings.Contains(prefix, "//") {
			t.Fatalf("%q accepted as %q %q", raw, bucket, prefix)
		}
		if _, err := FingerprintRepository(RepositoryFingerprintInput{
			Provider: ProviderGCS, Bucket: bucket, Prefix: prefix, Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders",
		}); err != nil {
			t.Fatalf("%q parsed but is not fingerprintable: %v", raw, err)
		}
	})
}
