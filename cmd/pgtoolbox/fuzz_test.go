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
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzReadClusters feeds readClusters what a pull request can hold: any
// bytes as multi-document YAML. Whatever they are, it returns a map or an
// error, never both; every Cluster it returns has a name; and comparing the
// file with itself reports every Cluster unchanged and exits 0 (a manifest
// does not differ from itself, and a gate must not fail on one). The one
// allowed exception is a Cluster the preview cannot decode (a spec that is
// not an object): that is refused loudly, exit 2 naming the Cluster, never
// exit 1 and never a "changed" report.
func FuzzReadClusters(f *testing.F) {
	seeds, _ := filepath.Glob(filepath.Join("testdata", "*.yaml"))
	for _, path := range seeds {
		if data, err := os.ReadFile(path); err == nil { //nolint:gosec // the repository's own testdata.
			f.Add(data)
		}
	}
	for _, seed := range []string{
		"",
		"---\n---\n",
		"null\n",
		"- a\n- b\n",
		"apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata: {}\n",
		"apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: \"\"\n",
		"apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: a\n---\napiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: a\n",
		"apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: a\n  namespace: n\nspec:\n  instances: 3\n",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n",
		"a: &x [*x]\n",
		"\t\x00\xff",
		// A Cluster whose spec is not an object: read, then refused by the preview.
		"apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: a\nspec: 0\n",
	} {
		f.Add([]byte(seed))
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		clusters, ignored, err := readClusters("-", bytes.NewReader(data))
		if err != nil {
			if clusters != nil || ignored != 0 {
				t.Fatalf("an error beside a result: %d Clusters, %d ignored, %v", len(clusters), ignored, err)
			}
			return
		}
		if ignored < 0 {
			t.Fatalf("ignored = %d", ignored)
		}
		for k, raw := range clusters {
			if k.name == "" {
				t.Fatalf("a Cluster without a name (namespace %q) was returned", k.namespace)
			}
			if !json.Valid(raw) {
				t.Fatalf("Cluster %s is not valid JSON: %q", k, raw)
			}
		}
		if len(clusters) == 0 {
			return
		}
		path := filepath.Join(dir, "same.yaml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := runPreview([]string{"--output", "json", path, path}, strings.NewReader(""), &stdout, &stderr)
		if code == exitInvalid {
			if !strings.Contains(stderr.String(), "cluster ") || stdout.Len() != 0 {
				t.Fatalf("a file compared with itself is refused without naming a Cluster: stdout %q, stderr %q", stdout.String(), stderr.String())
			}
			return
		}
		if code != exitOK {
			t.Fatalf("a file compared with itself exits %d; stderr %q", code, stderr.String())
		}
		var rep report
		if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
			t.Fatalf("report is not JSON: %v", err)
		}
		if len(rep.Clusters) != len(clusters) {
			t.Fatalf("%d Clusters reported of %d read", len(rep.Clusters), len(clusters))
		}
		for _, c := range rep.Clusters {
			if c.Change != changeUnchanged || c.Disruptive {
				t.Fatalf("Cluster %s/%s compared with itself is %q (disruptive %v)", c.Namespace, c.Name, c.Change, c.Disruptive)
			}
		}
	})
}
