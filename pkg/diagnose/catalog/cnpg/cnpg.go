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

// Package cnpg is the CloudNativePG rule catalog: the states in which
// the operator is stuck, waiting, or refusing, and why. One file per
// evidence kind — phases, conditions, events, metrics, backups, logs.
//
// Every string here was read out of the operator's own source, and
// every rule is pinned to the span of releases that reading was
// actually performed against — currently 1.30.0 and 1.30.1, the one
// minor this console supports (CloudNativePG 1.30 and later; 1.29 was
// dropped on 2026-11-06, after its upstream end of life). The pin is the
// claim's scope: a rule pinned supported was verified verbatim in every
// tree listed in VerifiedReleases. Verifying another release means
// checking the strings in its tree and widening the span, not trusting
// that nothing moved.
//
// The operator version itself is observed, not assumed — parsed from
// the bootstrap-controller init container the operator injects into
// every instance pod. On an unverified release these checks answer
// "does not apply", never a false clear and never a wrong finding.
package cnpg

import (
	"slices"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// Rules is the full CloudNativePG catalog.
func Rules() []diagnose.Rule {
	return slices.Concat(
		phaseRules(),
		conditionRules(),
		eventRules(),
		metricRules(),
		backupRules(),
		databaseRules(),
		logRules(),
		resourceRules(),
		replicationRules(),
		statusRules(),
		instanceStatusRules(),
	)
}

// VerifiedReleases are the operator releases whose source trees the
// catalog's strings were read from, verbatim. The pin verification
// (make verify-pins) fetches each tree and greps every pinned rule's
// strings in it; widening a span means adding the release here and
// letting that check pass, not trusting that nothing moved.
var VerifiedReleases = []string{"1.30.0", "1.30.1"}

// VerifiedReviewBy is the date by which this list stops being safe to
// leave alone. Pin verification proves the strings still say what the
// catalog claims in the releases named above; nothing proves the list
// is still the current set, because a release the console has never
// heard of looks exactly like no release at all. On this date the
// catalog's tests fail until someone checks what CloudNativePG has
// shipped since — and either verifies it and widens the spans, or moves
// the date on having looked.
const VerifiedReviewBy = "2026-11-30"

// supported is the verified span: 1.30.0 and 1.30.1, the minor this
// console supports. The lower bound is the support floor (an operator
// older than 1.30 answers "does not apply"); the upper bound is the next
// minor, which widens only when its tree is verified.
const supported = ">=1.30 <1.31"

// pinSupported builds a rule's requirement list for the verified span.
// Every CloudNativePG rule carries it; a narrower span comes back as a
// parameter when a minor with different machinery is verified.
func pinSupported() []diagnose.Requirement {
	return []diagnose.Requirement{{Component: diagnose.ComponentCNPG, Constraint: supported}}
}

// streamCaveat is the standing qualifier on every log-backed finding.
const streamCaveat = "Read from the container's log while following it. Following is " +
	"best effort, so the count below is a floor and an absence here " +
	"rules nothing out."
