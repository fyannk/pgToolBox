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

import "github.com/fyannk/pgToolBox/pkg/diagnose"

// lifecycleRules are the end-of-life checks of the operator and the
// backup plugin. Their release lines are not in the built-in table, so
// they report that the table lists none until a deployment's lifecycle
// ConfigMap adds them; the PostgreSQL and Kubernetes ones sit with their
// layers.
func lifecycleRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			ID:        "cnpg-eol",
			Component: diagnose.ComponentCNPG,
			Layer:     diagnose.LayerOperator,
			Severity:  diagnose.SeverityWarning,
			Describes: "a CloudNativePG release past its end of life",
			Summary:   "The CloudNativePG operator release is no longer supported upstream.",
			Detail: "Fixes, including security fixes, are released for supported minors only. " +
				"Where support ends is the lifecycle table's claim, quoted below with the version it is applied to.",
			When:      diagnose.EndOfLife{Component: diagnose.ComponentCNPG},
			Link:      "/",
			LinkLabel: "Overview",
		},
		{
			ID:        "barman-plugin-eol",
			Component: diagnose.ComponentBarman,
			Layer:     diagnose.LayerBackups,
			Severity:  diagnose.SeverityWarning,
			Describes: "a Barman Cloud plugin release past its end of life",
			Summary:   "The Barman Cloud plugin release is no longer supported upstream.",
			Detail: "The plugin carries every backup and WAL archive of this cluster. Where its support " +
				"ends is the lifecycle table's claim, quoted below with the version it is applied to.",
			When:      diagnose.EndOfLife{Component: diagnose.ComponentBarman},
			Link:      "/backups",
			LinkLabel: "Backups",
		},
	}
}
