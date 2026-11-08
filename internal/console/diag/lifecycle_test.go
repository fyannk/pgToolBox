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

package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// The deployment's table wins when it reads; an absent file is the
// built-in one; a refused file is the built-in one, saying so where the
// findings quote the table's source.
func TestLifecycleTableFallsBackToTheBuiltIn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "lifecycle.json")

	if l := NewLifecycleTable(path, nil).Current(); l.Source != "built-in" {
		t.Errorf("absent file: source %q, want built-in", l.Source)
	}

	good := `{"reviewed":"2026-11-02","lines":{"CloudNativePG":[{"line":"1.30","eol":"2027-05-01"}]}}`
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	l := NewLifecycleTable(path, nil).Current()
	if !strings.HasPrefix(l.Source, "ConfigMap") || len(l.Lines[diagnose.ComponentCNPG]) != 1 {
		t.Errorf("good file read as %+v", l)
	}

	if err := os.WriteFile(path, []byte(`{"lines":{"PostgreSQL":[{"line":"14","eol":"soon"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l = NewLifecycleTable(path, nil).Current()
	if !strings.Contains(l.Source, "refused") || len(l.Lines[diagnose.ComponentPostgreSQL]) == 0 {
		t.Errorf("bad file read as %+v, want the built-in table marked refused", l)
	}
}
