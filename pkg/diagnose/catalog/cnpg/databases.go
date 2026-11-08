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

package cnpg

import "github.com/fyannk/pgToolBox/pkg/diagnose"

// databaseRules read the declarative database objects' reconciliation
// reports. A Database whose owner role does not exist, or a
// Subscription that cannot reach its source, sits failed indefinitely
// while every cluster-level status reads healthy — and the operator's
// error message is on the object, observed, quoted here.
//
// One rule covers all four kinds; it reads only what was observed, so
// the pin is about the report's semantics, which hold across the span.
// The DatabaseRole kind ships with 1.30, the support floor; an install
// whose CRDs lack it simply has no such objects to read.
func databaseRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			ID:        "cnpg-declared-object-failed",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerDeclared,
			Requires:  pinSupported(),
			Severity:  diagnose.SeverityWarning,
			Describes: "a declared database object the operator cannot apply",
			Summary:   "A declared database object cannot be applied.",
			Detail: "The declaration exists but its effect does not: whatever expects " +
				"this database, role, publication or subscription to be there finds " +
				"it missing or outdated. The operator's own message below names the " +
				"refusal, and nothing changes until its cause is fixed.",
			When:   diagnose.DeclaredObjectFailed{},
			Pinned: []string{`json:"applied,omitempty"`},
		},
	}
}
