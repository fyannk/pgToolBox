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

//go:build catalogpins

package catalog

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog/cnpg"
)

// operatorModule is the CloudNativePG operator's Go module: the tree
// every pinned string was read from. Its zip carries the api/v1 types
// too, so one download per release covers phases, conditions, event
// reasons, log lines and the exporter's metric names alike.
const operatorModule = "github.com/cloudnative-pg/cloudnative-pg"

// TestPinnedStringsExistInVerifiedReleases is the catalog's provenance
// made executable. A rule pinned to a CloudNativePG span claims its
// strings were read verbatim from each verified release; this fetches
// each release's source through the Go module proxy and greps every
// such rule's strings in it. A rule whose pin admits a release must
// state something to verify there — its condition's literals, or the
// Pinned strings it names instead — and each must be present.
//
// It runs under the catalogpins tag because it needs the network and a
// few tens of megabytes of operator source; make verify-pins is the
// front door, and CI runs it beside the other repository checks.
// Widening a span means adding the release to cnpg.VerifiedReleases and
// letting this pass, not trusting that nothing moved.
func TestPinnedStringsExistInVerifiedReleases(t *testing.T) {
	for _, release := range cnpg.VerifiedReleases {
		corpus := operatorSources(t, release)
		verified := 0
		for _, rule := range Rules() {
			pin, pinned := cnpgPin(rule)
			if !pinned || !pin.Satisfied(release) {
				continue
			}
			literals := rule.Pinned
			if len(literals) == 0 {
				literals = conditionLiterals(rule.When)
			}
			if len(literals) == 0 {
				t.Errorf("rule %q is pinned to CloudNativePG %s but states nothing to verify", rule.ID, pin.Constraint)
				continue
			}
			for _, literal := range literals {
				if !bytes.Contains(corpus, []byte(literal)) {
					t.Errorf("%s: rule %q pins %q, which that release's tree does not contain", release, rule.ID, literal)
				}
			}
			verified++
		}
		t.Logf("%s: %d rules verified against %d bytes of operator source", release, verified, len(corpus))
	}
}

// cnpgPin returns the rule's CloudNativePG requirement, if any.
func cnpgPin(rule diagnose.Rule) (diagnose.Requirement, bool) {
	for _, requirement := range rule.Requires {
		if requirement.Component == diagnose.ComponentCNPG {
			return requirement, true
		}
	}
	return diagnose.Requirement{}, false
}

// conditionLiterals are the strings a condition compares against the
// observed world verbatim: the phrases upstream would have to keep
// unchanged for the rule to stay true.
func conditionLiterals(when diagnose.Condition) []string {
	switch condition := when.(type) {
	case diagnose.ClusterPhase:
		return condition.AnyOf
	case diagnose.ClusterPhaseHeld:
		return condition.AnyOf
	case diagnose.PoolerPhase:
		return condition.AnyOf
	case diagnose.ClusterCondition:
		literals := []string{condition.Type}
		if condition.Reason != "" {
			literals = append(literals, condition.Reason)
		}
		return literals
	case diagnose.EventMatch:
		return condition.Reasons
	case diagnose.LogContains:
		return condition.Substrings
	case diagnose.LogFields:
		literals := make([]string, 0, len(condition.Fields))
		for _, field := range condition.Fields {
			// Both halves have to exist in the tree the rule is pinned
			// to. The path is the console's reading of the component's
			// schema, verified a segment at a time because the dotted
			// form is the console's own construction while each segment
			// is a JSON tag the component declares. The value is the
			// component's own string.
			literals = append(literals, strings.Split(field.Path, ".")...)
			if field.Equals != "" {
				literals = append(literals, field.Equals)
			}
			if field.Contains != "" {
				literals = append(literals, field.Contains)
			}
		}
		return literals
	case diagnose.BackupPhase:
		return condition.AnyOf
	case diagnose.AllOf:
		var literals []string
		for _, branch := range condition.Of {
			literals = append(literals, conditionLiterals(branch)...)
		}
		return literals
	}
	return nil
}

// operatorSources fetches one release of the operator module and
// concatenates its Go sources (tests excluded) and its YAML — the
// default monitoring queries live there — into one searchable corpus.
// The download goes through the Go module proxy and its checksum
// database, so the tree is the release's, not whatever a mirror served.
func operatorSources(t *testing.T, release string) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "mod", "download", "-json", operatorModule+"@v"+release)
	// Outside any module on purpose: the download must not touch this
	// repository's go.mod, and GOFLAGS from the environment could carry
	// -mod=vendor or similar.
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("download %s@v%s: %v\n%s", operatorModule, release, err, out)
	}
	var info struct {
		Dir   string
		Error string
	}
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("parse download result for %s: %v\n%s", release, err, out)
	}
	if info.Error != "" || info.Dir == "" {
		t.Fatalf("download %s@v%s: %s", operatorModule, release, info.Error)
	}
	var corpus bytes.Buffer
	err = filepath.WalkDir(info.Dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		source := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
		manifest := strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
		if !source && !manifest {
			return nil
		}
		content, err := os.ReadFile(path) // #nosec G304 -- paths come from walking the module cache tree the Go toolchain just verified.
		if err != nil {
			return err
		}
		corpus.Write(content)
		corpus.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("read %s tree: %v", release, err)
	}
	return corpus.Bytes()
}
