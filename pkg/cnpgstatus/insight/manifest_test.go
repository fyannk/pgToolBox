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

package insight

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const plainManifest = "../../../deploy/insight-pack/insight-pack.yaml"

// renderManifest is the plain ConfigMap of the pack, for a team without
// the chart: kubectl apply -n <namespace> -f deploy/insight-pack/insight-pack.yaml.
func renderManifest(queries string) string {
	var b strings.Builder
	b.WriteString("# Generated from charts/pgtools/files/insight-pack/queries.yaml by\n")
	b.WriteString("# `make insight-pack`; do not edit. Apply it in a Cluster's namespace:\n")
	b.WriteString("#   kubectl apply -n <namespace> -f deploy/insight-pack/insight-pack.yaml\n")
	b.WriteString("# then opt the Cluster in with\n")
	fmt.Fprintf(&b, "#   %s\n", SpecChange)
	b.WriteString("apiVersion: v1\nkind: ConfigMap\nmetadata:\n")
	fmt.Fprintf(&b, "  name: %s\n  labels:\n    cnpg.io/reload: \"\"\n    %s: \"%d\"\n", ConfigMapName, VersionLabel, PackVersion)
	fmt.Fprintf(&b, "  annotations:\n    %s: \"%d\"\ndata:\n  %s: |\n", VersionLabel, PackVersion, ConfigMapKey)
	for _, line := range strings.Split(strings.TrimRight(queries, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// TestPlainManifestIsCurrent holds deploy/insight-pack equal to the
// chart's file; UPDATE_INSIGHT_PACK=1 (make insight-pack) rewrites it.
func TestPlainManifestIsCurrent(t *testing.T) {
	queries, err := os.ReadFile(packFile)
	if err != nil {
		t.Fatal(err)
	}
	want := renderManifest(string(queries))
	if os.Getenv("UPDATE_INSIGHT_PACK") == "1" {
		if err := os.WriteFile(plainManifest, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(plainManifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s is out of date with the chart's pack: run make insight-pack", plainManifest)
	}
	// It is a ConfigMap whose data is the pack, byte for byte.
	var cm struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name        string            `json:"name"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := yaml.UnmarshalStrict(got, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Kind != "ConfigMap" || cm.Metadata.Name != ConfigMapName || cm.Data[ConfigMapKey] != string(queries) ||
		cm.Metadata.Labels[VersionLabel] != strconv.Itoa(PackVersion) {
		t.Fatalf("manifest = %+v", cm.Metadata)
	}
}
