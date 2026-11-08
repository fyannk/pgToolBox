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
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fingerprintGolden is one row of the producer's published S3 vectors.
type fingerprintGolden struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Endpoint    string `json:"endpoint"`
	Bucket      string `json:"bucket"`
	Prefix      string `json:"prefix"`
	ScopeName   string `json:"scope_name"`
	Fingerprint string `json:"fingerprint"`
}

// contractModule is the shared contract module whose goldens this package
// must reproduce byte for byte.
const contractModule = "github.com/fyannk/pgToolBox/pkg/evidence"

// loadProducerGoldens reads testdata/s3-fingerprints.json from the exact
// contract module version go.mod pins, so a producer change that moves a
// vector fails here — not in a cluster.
func loadProducerGoldens(t *testing.T) []fingerprintGolden {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", contractModule).Output()
	if err != nil {
		t.Fatalf("locate %s: %v", contractModule, err)
	}
	directory := strings.TrimSpace(string(output))
	path := filepath.Join(directory, "v1alpha1", "testdata", "s3-fingerprints.json")
	content, err := os.ReadFile(path) // #nosec G304 -- module cache path resolved by the go tool.
	if err != nil {
		t.Fatalf("read producer goldens: %v", err)
	}
	var goldens []fingerprintGolden
	if err := json.Unmarshal(content, &goldens); err != nil {
		t.Fatalf("decode producer goldens: %v", err)
	}
	if len(goldens) == 0 {
		t.Fatal("producer goldens are empty")
	}
	return goldens
}

// TestFingerprintMatchesProducerGoldens drives the operator's own path —
// raw Barman destinationPath through ParseDestination, then Fingerprint —
// against the producer's vectors, so the operator's expected fingerprint
// and the agent's claimed one cannot drift apart.
func TestFingerprintMatchesProducerGoldens(t *testing.T) {
	for _, golden := range loadProducerGoldens(t) {
		t.Run(golden.Name, func(t *testing.T) {
			if golden.Destination == "" {
				t.Fatal("golden carries no destination column")
			}
			destination, err := ParseDestination(golden.Destination)
			if err != nil {
				t.Fatalf("ParseDestination: %v", err)
			}
			if destination.Bucket != golden.Bucket || destination.Prefix != strings.Trim(golden.Prefix, "/") {
				t.Fatalf("parsed %+v, golden bucket %q prefix %q", destination, golden.Bucket, golden.Prefix)
			}
			got, err := Fingerprint(destination, golden.Endpoint, golden.ScopeName)
			if err != nil {
				t.Fatalf("Fingerprint: %v", err)
			}
			if got != golden.Fingerprint {
				t.Fatalf("fingerprint = %s, want %s", got, golden.Fingerprint)
			}
		})
	}
}

func TestParseDestination(t *testing.T) {
	tests := []struct {
		raw  string
		want Destination
		ok   bool
	}{
		{"s3://bucket", Destination{Bucket: "bucket"}, true},
		{"s3://bucket/prefix/", Destination{Bucket: "bucket", Prefix: "prefix"}, true},
		{"s3://bucket/team%20a/db", Destination{Bucket: "bucket", Prefix: "team a/db"}, true},
		{"S3://bucket/p", Destination{Bucket: "bucket", Prefix: "p"}, true},
		{"", Destination{}, false},
		{"s3://", Destination{}, false},
		{"s3:///prefix", Destination{}, false},
		{"gs://bucket/prefix", Destination{}, false},
		{"s3://bucket//double", Destination{}, false},
		{"s3://user:secret@bucket/p", Destination{}, false},
		{"s3://bucket/p?X-Amz-Signature=x", Destination{}, false},
		{"s3://bucket:9000/p", Destination{}, false},
		{"s3://bu%63ket/p", Destination{}, false},
	}
	for _, test := range tests {
		got, err := ParseDestination(test.raw)
		if test.ok != (err == nil) || got != test.want {
			t.Errorf("ParseDestination(%q) = %+v, %v; want %+v ok=%v", test.raw, got, err, test.want, test.ok)
		}
		if err != nil {
			if !errors.Is(err, ErrInvalidDestination) {
				t.Errorf("ParseDestination(%q) error %v is not ErrInvalidDestination", test.raw, err)
			}
			if err.Error() != ErrInvalidDestination.Error() {
				t.Errorf("ParseDestination(%q) error %q is not the fixed, value-free message", test.raw, err)
			}
		}
	}
}

func TestRegionFromEndpoint(t *testing.T) {
	tests := []struct{ endpoint, want string }{
		{"", ""},
		{"https://s3.eu-west-1.amazonaws.com", "eu-west-1"},
		{"https://S3.EU-WEST-3.AMAZONAWS.COM:443/", "eu-west-3"},
		{"https://s3-eu-west-1.amazonaws.com", "eu-west-1"},
		{"https://s3.dualstack.ap-southeast-2.amazonaws.com", "ap-southeast-2"},
		{"https://s3-fips.us-gov-west-1.amazonaws.com", "us-gov-west-1"},
		{"https://bucket.vpce-0a1b-2c3d.s3.us-east-2.vpce.amazonaws.com", "us-east-2"},
		{"https://s3.cn-north-1.amazonaws.com.cn", "cn-north-1"},
		{"https://s3.amazonaws.com", "us-east-1"},
		{"https://s3-external-1.amazonaws.com", "us-east-1"},
		{"https://minio.example.test:9000", ""},
		{"http://minio.minio-system.svc:9000", ""},
		{"https://amazonaws.com.evil.test", ""},
		{"https://s3.eu-west-1.amazonaws.com.evil.test", ""},
		{"https://storage.googleapis.com", ""},
		{"https://eu-west-1.s3.amazonaws.com", "us-east-1"},
		{"https://s3-eu-west-1.s3.amazonaws.com", "us-east-1"},
		{"https://eu-west-1.s3.eu-central-1.amazonaws.com", "eu-central-1"},
		{"https://ec2.eu-west-1.amazonaws.com", ""},
		{"https://s3.eu-west-1.vpce.amazonaws.com", "eu-west-1"},
		{"https://s3.eusc-de-east-1.amazonaws.eu", "eusc-de-east-1"},
		{"https://s3.amazonaws.com.cn", ""},
		{"https://s3.amazonaws.eu", ""},
		{"::not a url", ""},
	}
	for _, test := range tests {
		if got := RegionFromEndpoint(test.endpoint); got != test.want {
			t.Errorf("RegionFromEndpoint(%q) = %q, want %q", test.endpoint, got, test.want)
		}
	}
}
