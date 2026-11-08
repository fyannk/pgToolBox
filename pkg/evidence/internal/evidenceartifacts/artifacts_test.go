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

package evidenceartifacts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGenerateIsDeterministicAndMatchesCommittedArtifacts(t *testing.T) {
	first, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("evidence artifacts changed across identical generations")
	}
	for _, artifact := range first {
		committed, err := os.ReadFile(filepath.Join("..", "..", "v1alpha1", filepath.FromSlash(artifact.Path)))
		if err != nil {
			t.Fatalf("read committed %s: %v", artifact.Path, err)
		}
		if !bytes.Equal(committed, artifact.Data) {
			t.Fatalf("committed %s is stale; run make generate-evidence-artifacts", artifact.Path)
		}
	}
}

func TestSchemaDeclaresEveryClosedResponseResource(t *testing.T) {
	artifacts, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Draft string                     `json:"$schema"`
		Roots []map[string]string        `json:"oneOf"`
		Defs  map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(artifacts[0].Data, &document); err != nil {
		t.Fatal(err)
	}
	if document.Draft != schemaDraft {
		t.Fatalf("schema draft = %q", document.Draft)
	}
	wantRoots := []string{
		"#/$defs/RepositoryEvidenceSnapshot",
		"#/$defs/BarmanBackupPage",
		"#/$defs/BarmanWALRangePage",
		"#/$defs/BarmanWALGapPage",
		"#/$defs/BarmanRecoveryPathPage",
		"#/$defs/PGBackRestBackupPage",
		"#/$defs/PGBackRestWALRangePage",
		"#/$defs/PGBackRestWALGapPage",
		"#/$defs/PGBackRestRecoveryPathPage",
		"#/$defs/EvidenceAPIError",
		"#/$defs/ServiceStatus",
	}
	if len(document.Roots) != len(wantRoots) {
		t.Fatalf("root count = %d, want %d", len(document.Roots), len(wantRoots))
	}
	for index, want := range wantRoots {
		if got := document.Roots[index]["$ref"]; got != want {
			t.Fatalf("root %d = %q, want %q", index, got, want)
		}
		if _, ok := document.Defs[filepath.Base(want)]; !ok {
			t.Fatalf("root definition %q is absent", want)
		}
	}
}
