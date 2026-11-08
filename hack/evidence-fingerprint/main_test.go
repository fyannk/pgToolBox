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

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// golden is one row of the producer's published S3 fingerprint vectors.
type golden struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Endpoint    string `json:"endpoint"`
	ScopeName   string `json:"scope_name"`
	Fingerprint string `json:"fingerprint"`
}

// loadGoldens reads testdata/s3-fingerprints.json from the exact contract
// module version go.mod pins: the tool, pgtoolbox and the viewer must all
// reproduce the same vectors from the raw destination.
func loadGoldens(t *testing.T) []golden {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/fyannk/pgToolBox/pkg/evidence").Output()
	if err != nil {
		t.Fatalf("locate the contract module: %v", err)
	}
	path := filepath.Join(strings.TrimSpace(string(output)), "v1alpha1", "testdata", "s3-fingerprints.json")
	content, err := os.ReadFile(path) // #nosec G304 -- module cache path resolved by the go tool.
	if err != nil {
		t.Fatalf("read goldens: %v", err)
	}
	var goldens []golden
	if err := json.Unmarshal(content, &goldens); err != nil {
		t.Fatalf("decode goldens: %v", err)
	}
	if len(goldens) == 0 {
		t.Fatal("no goldens")
	}
	return goldens
}

func TestComputeMatchesProducerGoldens(t *testing.T) {
	t.Parallel()
	for _, g := range loadGoldens(t) {
		t.Run(g.Name, func(t *testing.T) {
			t.Parallel()
			if g.Destination == "" {
				t.Fatal("golden carries no destination column")
			}
			got, err := compute(evidencev1alpha1.S3FingerprintInput{Endpoint: g.Endpoint, ScopeName: g.ScopeName}, g.Destination)
			if err != nil {
				t.Fatalf("compute: %v", err)
			}
			if got != g.Fingerprint {
				t.Fatalf("fingerprint = %s, want %s", got, g.Fingerprint)
			}
		})
	}
}

func TestComputeRejectsAmbiguousInput(t *testing.T) {
	t.Parallel()
	if _, err := compute(evidencev1alpha1.S3FingerprintInput{Bucket: "b", ScopeName: "orders"}, "s3://b/p"); err == nil {
		t.Error("-destination combined with -bucket was accepted")
	}
	if _, err := compute(evidencev1alpha1.S3FingerprintInput{ScopeName: "orders"}, "s3://user:pw@b/p"); err == nil {
		t.Error("a credential-bearing destination was accepted")
	}
}
