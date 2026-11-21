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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/preview"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// TestPreviewGolden runs the command over the testdata manifests and
// compares its exit code, stdout and stderr with testdata/golden.
func TestPreviewGolden(t *testing.T) {
	td := func(name string) string { return filepath.Join("testdata", name) }
	cases := []struct {
		name  string
		args  []string
		stdin string // a testdata file fed to stdin
		exit  int
	}{
		{"reload", []string{"--cnpg-version", "1.30", td("old.yaml"), td("new-reload.yaml")}, "", exitOK},
		{"reload-json", []string{"--output", "json", "--cnpg-version", "1.30", td("old.yaml"), td("new-reload.yaml")}, "", exitOK},
		{"reload-unverified", []string{td("old.yaml"), td("new-reload.yaml")}, "", exitOK},
		{"unchanged", []string{"--cnpg-version", "v1.30.1", td("old.yaml"), td("old.yaml")}, "", exitOK},
		{"restart-switchover", []string{"--cnpg-version", "1.30", td("old.yaml"), td("new-restart.yaml")}, "", exitDisruptive},
		{"restart-switchover-json", []string{"--output", "json", "--cnpg-version", "1.30", td("old.yaml"), td("new-restart.yaml")}, "", exitDisruptive},
		{"refused", []string{"--cnpg-version", "1.30", td("old.yaml"), td("new-refused.yaml")}, "", exitDisruptive},
		{"unknown-field", []string{"--cnpg-version", "1.30", td("old.yaml"), td("new-unknown.yaml")}, "", exitDisruptive},
		{"renamed", []string{"--cnpg-version", "1.30", td("old.yaml"), td("new-renamed.yaml")}, "", exitDisruptive},
		{"renamed-json", []string{"--output", "json", td("old.yaml"), td("new-renamed.yaml")}, "", exitDisruptive},
		{"stdin-old", []string{"--cnpg-version", "1.30", "-", td("new-reload.yaml")}, "old.yaml", exitOK},
		{"cnpg-unsupported-minor", []string{"--cnpg-version", "1.40", td("old.yaml"), td("new-reload.yaml")}, "", exitOK},
		{"invalid-yaml", []string{td("old.yaml"), td("invalid.yaml")}, "", exitInvalid},
		{"scalar-document", []string{td("scalar.yaml"), td("old.yaml")}, "", exitInvalid},
		{"no-cluster", []string{td("not-clusters.yaml"), td("not-clusters.yaml")}, "", exitInvalid},
		{"duplicate", []string{td("duplicate.yaml"), td("old.yaml")}, "", exitInvalid},
		{"missing-file", []string{td("old.yaml"), td("absent.yaml")}, "", exitInvalid},
		{"both-stdin", []string{"-", "-"}, "old.yaml", exitInvalid},
		{"one-argument", []string{td("old.yaml")}, "", exitInvalid},
		{"bad-output", []string{"--output", "yaml", td("old.yaml"), td("old.yaml")}, "", exitInvalid},
		{"bad-cnpg-version", []string{"--cnpg-version", "latest", td("old.yaml"), td("old.yaml")}, "", exitInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdin bytes.Buffer
			if tc.stdin != "" {
				b, err := os.ReadFile(td(tc.stdin))
				if err != nil {
					t.Fatal(err)
				}
				stdin.Write(b)
			}
			var stdout, stderr bytes.Buffer
			code := run(append([]string{"preview"}, tc.args...), &stdin, &stdout, &stderr)
			if code != tc.exit {
				t.Errorf("exit code %d, want %d (stderr: %s)", code, tc.exit, stderr.String())
			}
			if tc.exit == exitInvalid && stdout.Len() > 0 {
				t.Errorf("invalid input wrote a report: %s", stdout.String())
			}
			got := fmt.Sprintf("exit: %d\n--- stdout\n%s--- stderr\n%s", code, stdout.String(), stderr.String())
			path := filepath.Join("testdata", "golden", tc.name+".txt")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update)", err)
			}
			if got != string(want) {
				t.Errorf("output differs from %s (run with -update):\n%s", path, got)
			}
		})
	}
}

// TestPreviewJSONDecodes checks the JSON report against the preview
// package's own type, so the contract is the console's.
func TestPreviewJSONDecodes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"preview", "--output", "json", "--cnpg-version", "1.30", "testdata/old.yaml", "testdata/new-restart.yaml"}, nil, &stdout, &stderr)
	if code != exitDisruptive {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var rep report
	dec := json.NewDecoder(&stdout)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Disruptive || rep.Ignored != 4 || len(rep.Clusters) != 2 {
		t.Fatalf("report %+v", rep)
	}
	orders := rep.Clusters[1]
	if orders.Name != "orders" || orders.Preview == nil || orders.Preview.Effect != preview.EffectRestart ||
		orders.Preview.Primary != preview.PrimarySwitchover || !orders.Preview.Verified {
		t.Fatalf("orders %+v", orders)
	}
	if billing := rep.Clusters[0]; billing.Disruptive || billing.Change != changeUnchanged || billing.Preview != nil {
		t.Fatalf("billing %+v", billing)
	}
}

func TestDisruptive(t *testing.T) {
	cases := []struct {
		effect preview.Effect
		params []preview.ParameterChange
		want   bool
	}{
		{preview.EffectNone, nil, false},
		{preview.EffectReload, nil, false},
		{preview.EffectStart, nil, false},
		{preview.EffectRestart, nil, true},
		{preview.EffectRollout, nil, true},
		{preview.EffectShutdown, nil, true},
		{preview.EffectMajorUpgrade, nil, true},
		{preview.EffectUnknown, nil, true},
		// A refused parameter leaves the ordered effect at none.
		{preview.EffectNone, []preview.ParameterChange{{Name: "listen_addresses", Effect: preview.EffectRefused}}, true},
		{preview.EffectReload, []preview.ParameterChange{{Name: "work_mem", Effect: preview.EffectReload}}, false},
	}
	for _, tc := range cases {
		if got := disruptive(preview.Preview{Effect: tc.effect, Parameters: tc.params}); got != tc.want {
			t.Errorf("disruptive(%s, %v) = %v, want %v", tc.effect, tc.params, got, tc.want)
		}
	}
}

// TestWordsCoverCodes fails when the preview package grows a code the
// report would print raw.
func TestWordsCoverCodes(t *testing.T) {
	for _, e := range []preview.Effect{
		preview.EffectNone, preview.EffectReload, preview.EffectStart, preview.EffectRestart, preview.EffectRollout,
		preview.EffectShutdown, preview.EffectMajorUpgrade, preview.EffectRefused, preview.EffectUnknown,
	} {
		if _, ok := effectWords[e]; !ok {
			t.Errorf("no word for effect %s", e)
		}
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "pkg", "cnpgstatus", "preview", "preview.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "=" {
			continue
		}
		code := strings.Trim(f[2], `"`)
		switch {
		case strings.HasPrefix(f[0], "Reason"):
			if _, ok := reasonWords[code]; !ok {
				t.Errorf("no word for reason %s", code)
			}
		case strings.HasPrefix(f[0], "Primary"):
			if _, ok := actionWords[code]; !ok {
				t.Errorf("no word for primary action %s", code)
			}
		case strings.HasPrefix(f[0], "Context"):
			if _, ok := contextWords[code]; !ok {
				t.Errorf("no word for context %s", code)
			}
		}
	}
}
