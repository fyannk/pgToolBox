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

package evidenceapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestGeneratedSchemaAcceptsEveryWireGolden(t *testing.T) {
	schema := compileEvidenceSchema(t)
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "pkg", "evidence", "v1alpha1", "testdata", "wire", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 14 {
		t.Fatalf("wire golden count = %d, want 14", len(paths))
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			instance := readJSONValue(t, path)
			if err := schema.Validate(instance); err != nil {
				t.Fatalf("golden does not satisfy generated schema: %v", err)
			}
		})
	}
}

func TestGeneratedSchemaRejectsMissingRequiredAndUnknownEnums(t *testing.T) {
	schema := compileEvidenceSchema(t)
	snapshotPath := filepath.Join("..", "..", "..", "pkg", "evidence", "v1alpha1", "testdata", "wire", "snapshot.json")
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "required field missing", mutate: func(value map[string]any) { delete(value, "completeness") }},
		{name: "unknown state", mutate: func(value map[string]any) { value["state"] = "future" }},
		{name: "wrong resource kind", mutate: func(value map[string]any) { value["kind"] = "EvidenceAPIError" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := readJSONValue(t, snapshotPath).(map[string]any)
			test.mutate(value)
			if err := schema.Validate(value); err == nil {
				t.Fatal("invalid wire value satisfied generated schema")
			}
		})
	}
}

func TestGeneratedSchemaAllowsAdditiveFieldsAndUnknownDetailsTag(t *testing.T) {
	schema := compileEvidenceSchema(t)
	snapshotPath := filepath.Join("..", "..", "..", "pkg", "evidence", "v1alpha1", "testdata", "wire", "snapshot.json")
	value := readJSONValue(t, snapshotPath).(map[string]any)
	value["future_field"] = map[string]any{"ignored": true}
	value["details"] = map[string]any{
		"type":   "future/v1",
		"future": map[string]any{"must_be_discarded": true},
	}
	if err := schema.Validate(value); err != nil {
		t.Fatalf("additive wire value did not satisfy generated schema: %v", err)
	}
}

func compileEvidenceSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join("..", "..", "..", "pkg", "evidence", "v1alpha1", "schema.json")
	document := readJSONValue(t, path)
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource("schema.json", document); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func readJSONValue(t *testing.T, path string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
