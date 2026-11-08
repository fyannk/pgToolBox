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

// Undiagnosed is the catalog's list of upstream signals it has read and
// decided not to listen for, each with the reason. It exists so that
// ignoring a signal is a decision on record rather than an omission:
// the coverage verification (make verify-pins) fails on any phase,
// condition reason, backup or pooler phase, or Warning event reason in
// the verified releases that neither a rule listens for nor this list
// declines, and on an entry here that upstream no longer says or that
// a rule has since started listening for.
func Undiagnosed() map[string]string {
	return map[string]string{
		// Phases.
		"Cluster in healthy state": "the healthy phase is the absence of a finding; the sidebar has no zero state for the same reason",
		"Failing over":             "read through currentPrimary and targetPrimary by cnpg-primary-move-stuck, which also knows how long the move has taken",
		"Switchover in progress":   "read through currentPrimary and targetPrimary by cnpg-primary-move-stuck, which also knows how long the move has taken",
		// Condition reasons that mean success or progress. The rules
		// read the condition's status; the failing reasons are listed
		// where a status carries two meanings.
		"BackupStarted":              "a backup in progress is not a finding; cnpg-backup-stuck-pending reads the Backup's own phase and age",
		"BootstrapCompleted":         "success",
		"BootstrapPending":           "cnpg-bootstrap-stuck reads the phase and how long it has held, which the condition does not carry",
		"ClusterIsReady":             "success; cnpg-not-ready reads the condition's False status",
		"ContinuousArchivingFailing": "cnpg-wal-archiving-failing reads the condition's False status, whose only reason this is",
		"ContinuousArchivingSuccess": "success",
		// Pooler phases.
		"active": "success",
		// Events on objects outside this console's authority. The plugin
		// controller reconciles the plugins' Services in the operator's
		// namespace and records its events there; the console watches one
		// cluster's namespace and never sees them.
		"ClientSecretNotFound":     "recorded on the plugin's Service in the operator's namespace",
		"FinalizerRemovalFailed":   "recorded on the plugin's Service in the operator's namespace",
		"InvalidClientCertificate": "recorded on the plugin's Service in the operator's namespace",
		"InvalidPortAnnotation":    "recorded on the plugin's Service in the operator's namespace",
		"InvalidServerCertificate": "recorded on the plugin's Service in the operator's namespace",
		"PluginRegistrationFailed": "recorded on the plugin's Service in the operator's namespace",
		"ServerSecretNotFound":     "recorded on the plugin's Service in the operator's namespace",
		// Events the catalog reads another way.
		"DiscoverImage":  "the same failure sets the image-catalog phase, which cnpg-image-catalog-unusable reads with the catalog checks beneath it",
		"FindingCluster": "a Backup naming a Cluster the operator cannot find is not a member of this cluster's catalog, so its events are never attributed here",
	}
}
