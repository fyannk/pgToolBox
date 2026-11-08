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
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog/cnpg"
)

// The pin verification proves every string the catalog listens for is
// one upstream still says. This is the inverse: every string upstream
// can say, in the kinds the catalog reads, is one the catalog either
// listens for or has declined by name. A release that adds a phase, a
// condition reason or a Warning event the catalog has never heard of
// fails the build here, so the decision to ignore it is made once, in
// writing, rather than by omission.

// upstreamSignal is one string the operator can write that a check
// could read.
type upstreamSignal struct {
	kind, value string
}

// signalPatterns extract the signals from the operator's Go sources.
// Each pattern's first group is the value. The phase and condition
// patterns match the api/v1 constant declarations; the event pattern
// matches the recorder calls in the controllers, Warning only — a
// Normal event is the operator narrating, not refusing.
var signalPatterns = []struct {
	kind    string
	pattern *regexp.Regexp
}{
	{"cluster phase", regexp.MustCompile(`\n\tPhase[A-Za-z]+ = "([^"]+)"`)},
	{"condition reason", regexp.MustCompile(`\n\t[A-Za-z]+ ConditionReason = "([^"]+)"`)},
	{"backup phase", regexp.MustCompile(`\n\tBackupPhase[A-Za-z]+ BackupPhase = "([^"]+)"`)},
	{"pooler phase", regexp.MustCompile(`\n\tPoolerPhase[A-Za-z]+ PoolerPhase = "([^"]+)"`)},
	{"warning event", regexp.MustCompile(`Eventf?\([^,\n]+,\s*"Warning",\s*"([A-Za-z]+)"`)},
}

// TestEveryUpstreamSignalIsDecided fails on a signal in any verified
// release's tree that no rule listens for and no entry in Undiagnosed
// declines.
func TestEveryUpstreamSignalIsDecided(t *testing.T) {
	listened := map[upstreamSignal]bool{}
	for _, rule := range Rules() {
		for _, literal := range append(conditionLiterals(rule.When), rule.Pinned...) {
			for _, kind := range []string{"cluster phase", "condition reason", "backup phase", "pooler phase", "warning event"} {
				listened[upstreamSignal{kind, literal}] = true
			}
		}
	}
	declined := Undiagnosed()
	seen := map[upstreamSignal]bool{}
	var undecided []string
	for _, release := range cnpg.VerifiedReleases {
		corpus := string(operatorSources(t, release))
		for _, sp := range signalPatterns {
			for _, match := range sp.pattern.FindAllStringSubmatch(corpus, -1) {
				signal := upstreamSignal{sp.kind, match[1]}
				if seen[signal] {
					continue
				}
				seen[signal] = true
				if listened[signal] {
					continue
				}
				if _, ok := declined[signal.value]; ok {
					continue
				}
				undecided = append(undecided, sp.kind+": "+match[1]+" (first seen in "+release+")")
			}
		}
	}
	sort.Strings(undecided)
	if len(undecided) > 0 {
		t.Errorf("%d upstream signals are neither listened for nor declined; add a rule, or decline each in Undiagnosed with its reason:\n  %s",
			len(undecided), strings.Join(undecided, "\n  "))
	}
	// A declined signal that upstream no longer says, or that a rule
	// now listens for, is a stale decision.
	for value, reason := range declined {
		found := false
		for signal := range seen {
			if signal.value == value {
				found = true
				if listened[signal] {
					t.Errorf("%q is declined (%s) but a rule listens for it: drop the decision", value, reason)
				}
				break
			}
		}
		if !found {
			t.Errorf("%q is declined (%s) but no verified release says it: drop the decision", value, reason)
		}
	}
	t.Logf("%d upstream signals seen across %d releases, %d declined by name", len(seen), len(cnpg.VerifiedReleases), len(declined))
}
