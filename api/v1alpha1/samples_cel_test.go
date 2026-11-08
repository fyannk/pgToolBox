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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// TestSamplesSchema applies every PgAdmin, PgAccessRequest and PgToolBoxConsole sample under
// config/samples to a real API server: a sample the CRD refuses is a broken
// document.
func TestSamplesSchema(t *testing.T) {
	c := apiServer(t)
	paths, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "pgadmin*.yaml"))
	more, _ := filepath.Glob(filepath.Join("..", "..", "config", "samples", "pgaccessrequest*.yaml"))
	paths = append(paths, more...)
	consoles, _ := filepath.Glob(filepath.Join("..", "..", "config", "samples", "pgtoolboxconsole*.yaml"))
	paths = append(paths, consoles...)
	if err != nil || len(paths) == 0 {
		t.Fatalf("no samples: %v", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path) // #nosec G304 -- repository sample
			if err != nil {
				t.Fatal(err)
			}
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(raw, &obj.Object); err != nil {
				t.Fatal(err)
			}
			obj.SetNamespace("default")
			obj.SetName(strings.TrimSuffix(strings.ReplaceAll(filepath.Base(path), "_", "-"), ".yaml"))
			if err := c.Create(context.Background(), obj); err != nil {
				t.Fatalf("%s is refused: %v", path, err)
			}
			_ = c.Delete(context.Background(), obj)
		})
	}
}
