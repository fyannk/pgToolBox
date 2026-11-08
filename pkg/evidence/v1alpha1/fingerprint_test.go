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
	"os"
	"testing"
)

func TestFingerprintS3GoldenVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/s3-fingerprints.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Name        string `json:"name"`
		Destination string `json:"destination"`
		Endpoint    string `json:"endpoint"`
		Bucket      string `json:"bucket"`
		Prefix      string `json:"prefix"`
		Format      string `json:"format"`
		ScopeKind   string `json:"scope_kind"`
		ScopeName   string `json:"scope_name"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors {
		t.Run(vector.Name, func(t *testing.T) {
			bucket, prefix, err := ParseS3Destination(vector.Destination)
			if err != nil || bucket != vector.Bucket || prefix != vector.Prefix {
				t.Fatalf("ParseS3Destination(%q) = %q, %q, %v; want %q, %q", vector.Destination, bucket, prefix, err, vector.Bucket, vector.Prefix)
			}
			input := S3FingerprintInput{
				Endpoint: vector.Endpoint, Bucket: vector.Bucket,
				Prefix: vector.Prefix, Format: vector.Format, ScopeKind: vector.ScopeKind, ScopeName: vector.ScopeName,
			}
			got, err := FingerprintS3(input)
			if err != nil {
				t.Fatal(err)
			}
			if got != vector.Fingerprint {
				t.Fatalf("fingerprint = %q, want %q", got, vector.Fingerprint)
			}
		})
	}
}

// The domain string is part of the cross-component contract: changing it changes
// every fingerprint, so it is pinned independently of the golden vectors.
func TestFingerprintDomainIsV2(t *testing.T) {
	if FingerprintDomain != "pgtoolbox-evidence-repository-v1" {
		t.Fatalf("FingerprintDomain = %q", FingerprintDomain)
	}
}

func TestCanonicalS3FingerprintInput(t *testing.T) {
	canonical, err := CanonicalS3FingerprintInput(S3FingerprintInput{
		Endpoint: "HTTPS://Store.Example.TEST:443/", Bucket: "Case-Sensitive", Prefix: "/Team/Prod/",
		Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "Orders",
	})
	if err != nil {
		t.Fatal(err)
	}
	if canonical.Endpoint != "https://store.example.test" || canonical.Bucket != "Case-Sensitive" || canonical.Prefix != "Team/Prod" {
		t.Fatalf("unexpected canonical input: %#v", canonical)
	}
}

func TestFingerprintS3RejectsCredentialBearingOrAmbiguousInput(t *testing.T) {
	for _, endpoint := range []string{
		"https://user:secret@example.test",
		"https://example.test/path",
		"https://example.test?token=secret",
		"https://example.test/#fragment",
		"ftp://example.test",
	} {
		input := S3FingerprintInput{Endpoint: endpoint, Bucket: "bucket", Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "server"}
		if _, err := FingerprintS3(input); err == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
}
