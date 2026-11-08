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

// Package catalog is the declarative half of diagnostics: the rules,
// grouped by component. Small components keep a file here; large ones
// grow a package of their own, like cnpg, and are aggregated by Rules.
//
// The framework — the Rule shape, the conditions, the version gating —
// lives in the diagnose package; this tree holds only claims. Adding a
// diagnostic means adding a Rule to its component's file: declare the
// version pins the claim was verified against, the observation (a log
// line, an event, a condition, a phase, a metric flag, a backup phase),
// and the finding it means. A rule earns its place by catching
// something no hand-written detector already reports, and a log rule by
// naming a failure that appears in no object status at all.
package catalog

import (
	"slices"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog/cnpg"
)

// Rules is every catalog rule, in the order their checks are listed.
func Rules() []diagnose.Rule {
	return slices.Concat(
		cnpg.Rules(),
		postgresRules(),
		sizingRules(),
		barmanRules(),
		repositoryRules(),
		kubernetesRules(),
		lifecycleRules(),
	)
}

// LogRules derives the continuous matcher's rule set from the full
// catalog, so a log line is declared once — with its pins and its
// finding — and matched continuously.
func LogRules() []logstream.Rule {
	return diagnose.LogRules(Rules())
}
