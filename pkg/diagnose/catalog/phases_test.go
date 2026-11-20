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
	"testing"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// TestEveryCatalogPhaseHasATableRow: a phase a check names is a phase
// the operator writes, so the read model must classify it rather than
// show it as unknown beside a finding about it.
func TestEveryCatalogPhaseHasATableRow(t *testing.T) {
	seen := 0
	for _, rule := range Rules() {
		for _, phase := range phasesOf(rule.When) {
			seen++
			for _, minor := range cnpgstatus.SupportedMinors() {
				if !cnpgstatus.ClassifyPhase(minor, phase).Known {
					t.Errorf("%s: phase %q has no row in the %s phase table", rule.ID, phase, minor)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no rule names a phase: phasesOf misses a condition type")
	}
}

// phasesOf lists the cluster phases a condition names, through AllOf.
func phasesOf(condition diagnose.Condition) []string {
	switch c := condition.(type) {
	case diagnose.ClusterPhase:
		return c.AnyOf
	case diagnose.ClusterPhaseHeld:
		return c.AnyOf
	case diagnose.AllOf:
		var out []string
		for _, branch := range c.Of {
			out = append(out, phasesOf(branch)...)
		}
		return out
	}
	return nil
}
