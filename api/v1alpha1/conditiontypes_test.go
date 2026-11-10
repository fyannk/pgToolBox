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

package v1alpha1_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// conditionTypes reads every Condition* string constant the package
// declares (outside tests), as "Name=Value", sorted: the source is the
// registry, so a type added without a golden entry cannot slip past.
func conditionTypes(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var got []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "zz_generated") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, ident := range value.Names {
					if !strings.HasPrefix(ident.Name, "Condition") || i >= len(value.Values) {
						continue
					}
					literal, ok := value.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					text, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, ident.Name+"="+text)
				}
			}
		}
	}
	slices.Sort(got)
	return got
}

// TestConditionTypesFrozen holds every condition type the API declares
// against testdata/conditions.golden. Condition types are frozen API
// surface like reasons: one that disappears or changes value fails; a new
// one fails until the golden file is regenerated with -update, so
// additions are deliberate and reviewed.
func TestConditionTypesFrozen(t *testing.T) {
	path := filepath.Join("testdata", "conditions.golden")
	got := conditionTypes(t)
	if len(got) == 0 {
		t.Fatal("no Condition* constant found: the scan is broken")
	}
	if *updateGolden {
		if err := os.WriteFile(path, []byte(strings.Join(got, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- test fixture path
	if err != nil {
		t.Fatalf("%v (go test ./api/v1alpha1 -run TestConditionTypesFrozen -update)", err)
	}
	want := strings.Fields(string(raw))
	for _, entry := range want {
		if !slices.Contains(got, entry) {
			t.Errorf("condition type %s was removed or renamed; condition types are frozen API surface", entry)
		}
	}
	for _, entry := range got {
		if !slices.Contains(want, entry) {
			t.Errorf("condition type %s is new; regenerate the golden file with -update", entry)
		}
	}
}
