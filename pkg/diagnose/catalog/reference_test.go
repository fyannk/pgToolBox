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

package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/diagnose/triage"
)

// referencePath is the documentation site's check reference, relative
// to this package.
const referencePath = "../../../docs/site/docs/reference/checks.md"

// TestCheckReferenceIsCurrent keeps the documented check list equal to
// the catalog. The site follows the code, never the reverse: the
// reference is generated from the declarations, and this test fails
// when the file on disk differs from what they generate. Regenerate
// with make catalog-docs.
func TestCheckReferenceIsCurrent(t *testing.T) {
	t.Parallel()
	want := renderReference()
	path := filepath.Clean(referencePath)
	if os.Getenv("UPDATE_CATALOG_DOCS") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run make catalog-docs)", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s is out of date with the catalog: run make catalog-docs", path)
	}
}

// renderReference writes the reference page: every check by layer,
// then every playbook with the checks behind each step.
func renderReference() string {
	var b strings.Builder
	b.WriteString(`---
sidebar_position: 4
title: Checks and playbooks
---

# Checks and playbooks

This page is generated from the diagnostics catalog by ` + "`make catalog-docs`" + `
and kept equal to it by a test. It lists every check the console runs,
grouped by the layer of the stack it looks at, and every triage
playbook with the checks behind each of its steps. What a check
*means* — its four outcomes, how findings relate, what each kind of
evidence needs — is in the [diagnostics guide](../guides/diagnostics.md).

A check's **applies to** column is its version pin: on an observed
version outside it the check answers "does not apply". A check with
none applies everywhere. **Follows from** lists the checks the catalog
relates this one to as consequences of, with the relation's scope and
strength where they differ from *same cluster, established*.

`)
	byLayer := map[diagnose.Layer][]diagnose.Rule{}
	for _, rule := range Rules() {
		byLayer[rule.Layer] = append(byLayer[rule.Layer], rule)
	}
	detectors := map[diagnose.Layer][]diagnose.Detector{}
	for _, detector := range diagnose.Detectors() {
		detectors[detector.Layer()] = append(detectors[detector.Layer()], detector)
	}
	total := len(Rules()) + len(diagnose.Detectors())
	fmt.Fprintf(&b, "%d checks: %d catalog rules and %d hand-written detectors.\n", total, len(Rules()), len(diagnose.Detectors()))
	for _, layer := range diagnose.Layers() {
		rules := byLayer[layer]
		found := detectors[layer]
		if len(rules) == 0 && len(found) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n", layer)
		fmt.Fprintf(&b, "| Check | Severity | Looks for | Finding | Applies to | Follows from |\n|---|---|---|---|---|---|\n")
		rows := make([]string, 0, len(rules)+len(found))
		for _, detector := range found {
			rows = append(rows, fmt.Sprintf("| `%s` | by finding | %s | *(hand-written detector; see the guide)* | every version | |",
				detector.Name(), cell(detector.Describes())))
		}
		for _, rule := range rules {
			rows = append(rows, fmt.Sprintf("| `%s` | %s | %s | %s | %s | %s |",
				rule.ID, rule.Severity.String(), cell(describes(rule)), cell(rule.Summary), pins(rule), relations(rule)))
		}
		sort.Strings(rows)
		b.WriteString(strings.Join(rows, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("\n## Declined upstream signals\n\n")
	b.WriteString("Signals the verified operator releases can write that no check listens for, each with the reason. The coverage verification fails on a signal that is neither listened for nor listed here.\n\n| Signal | Why no check |\n|---|---|\n")
	declined := Undiagnosed()
	keys := make([]string, 0, len(declined))
	for key := range declined {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&b, "| `%s` | %s |\n", key, cell(declined[key]))
	}
	b.WriteString("\n## Triage playbooks\n\n")
	b.WriteString("Each step is answered by the checks named beside it; a step with no checks is a question for the reader.\n")
	for _, playbook := range triage.Playbooks() {
		fmt.Fprintf(&b, "\n### %s\n\n%s.\n\n", playbook.Symptom, playbook.Describes)
		for i, step := range playbook.Steps {
			fmt.Fprintf(&b, "%d. **%s**", i+1, step.Question)
			if step.Fact != nil {
				fmt.Fprintf(&b, " — fact: %s", step.Fact.Describe())
			}
			if len(step.Checks) > 0 {
				fmt.Fprintf(&b, " — `%s`", strings.Join(step.Checks, "`, `"))
			}
			if step.Yourself != "" {
				fmt.Fprintf(&b, " — or run `%s`", strings.ReplaceAll(step.Yourself, "`", "'"))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// describes is the check row's own words, which fold the condition's
// description in when the rule states none of its own.
func describes(rule diagnose.Rule) string {
	if rule.Describes != "" {
		return rule.Describes
	}
	return rule.Summary
}

func pins(rule diagnose.Rule) string {
	if len(rule.Requires) == 0 {
		return "every version"
	}
	parts := make([]string, 0, len(rule.Requires))
	for _, requirement := range rule.Requires {
		parts = append(parts, requirement.String())
	}
	return "`" + strings.Join(parts, "`, `") + "`"
}

func relations(rule diagnose.Rule) string {
	parts := make([]string, 0, len(rule.ConsequenceOf))
	for _, relation := range rule.ConsequenceOf {
		part := "`" + relation.Cause + "`"
		var terms []string
		if relation.Scope == diagnose.ScopePod {
			terms = append(terms, "same pod")
		}
		if relation.Within > 0 {
			terms = append(terms, "within "+relation.Within.String())
		}
		if relation.Strength == diagnose.StrengthPlausible {
			terms = append(terms, "plausible")
		}
		if len(terms) > 0 {
			part += " (" + strings.Join(terms, ", ") + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// cell escapes a value for a Markdown table cell.
func cell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}
