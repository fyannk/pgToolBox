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

package cnpgstatus

import "strings"

// Cluster.status.phase is a free sentence, not an enum. The table below
// lists every phase constant of api/v1/cluster_types.go at each
// supported tag; all of them reach the status through
// ClusterReconciler.RegisterPhase (internal/controller/cluster_status.go
// v1.30.0:733) or the promote plugin, always with the constant, never a
// literal.
//
// Colour reference: kubectl cnpg status (internal/cmd/plugin/status/status.go,
// v1.30.0:478-484, main:524-530) paints green exactly
// PhaseHealthy, PhaseFirstPrimary and PhaseCreatingReplica, yellow
// exactly PhaseUpgrade ("Upgrading cluster") and PhaseWaitingForUser,
// and red everything else, unknown phases included. Only PhaseUpgrade is
// yellow, not every "Upgrade*" phase, and that is what the Kubectl column
// records.
//
// The console's classes refine those colours:
//   - green Healthy stays ok; green Setting up primary / Creating a new
//     replica become progressing (transitions, not a steady state);
//   - yellow Upgrading cluster is progressing, yellow Waiting for user
//     action is warning (nothing moves until a human acts);
//   - red phases that describe an operation the operator is driving
//     (switchover, major or online upgrade, in-place restarts, applying
//     configuration, replica-cluster promotion) are progressing;
//   - red phases that wait on configuration or on Pods (upgrade delayed,
//     waiting for instances) are warning;
//   - every other red phase, Failing over included (a primary was lost),
//     is critical.
//
// No phase is inactive: hibernation and fencing do not set a phase in
// any supported minor (pkg/reconciler/hibernation/status.go sets only a
// condition); Hibernation and Fencing report them.

// Kubectl colours, as kubectl cnpg status paints the phase.
const (
	kubectlGreen  = "green"
	kubectlYellow = "yellow"
	kubectlRed    = "red"
)

// PhaseEntry is one row of a minor's phase table.
type PhaseEntry struct {
	Phase   string `json:"phase"`
	Class   Class  `json:"class"`
	Kubectl string `json:"kubectl"`
	// Since is the first supported minor that writes this phase.
	Since Minor `json:"since"`
	// Source names the constant and its line at each tag it was read at.
	Source string `json:"source"`
}

// phaseRows is the union of the per-minor tables; Since selects the
// minors. Line numbers are api/v1/cluster_types.go at v1.30.0
// and main (21e24f6, planned 1.31).
var phaseRows = []PhaseEntry{
	// PhaseSwitchover: cluster_types.go v1.30.0:699, main:710.
	{
		Phase: "Switchover in progress", Class: ClassProgressing, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseSwitchover (v1.30.0:699, main:710)",
	},
	// PhaseFailOver: cluster_types.go v1.30.0:702, main:713.
	{
		Phase: "Failing over", Class: ClassCritical, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseFailOver (v1.30.0:702, main:713)",
	},
	// PhaseFirstPrimary: cluster_types.go v1.30.0:705, main:716.
	{
		Phase: "Setting up primary", Class: ClassProgressing, Kubectl: kubectlGreen, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseFirstPrimary (v1.30.0:705, main:716)",
	},
	// PhaseCreatingReplica: cluster_types.go v1.30.0:708, main:719.
	{
		Phase: "Creating a new replica", Class: ClassProgressing, Kubectl: kubectlGreen, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseCreatingReplica (v1.30.0:708, main:719)",
	},
	// PhaseUpgrade: cluster_types.go v1.30.0:711, main:722.
	{
		Phase: "Upgrading cluster", Class: ClassProgressing, Kubectl: kubectlYellow, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseUpgrade (v1.30.0:711, main:722)",
	},
	// PhaseMajorUpgrade: cluster_types.go v1.30.0:714, main:725.
	{
		Phase: "Upgrading Postgres major version", Class: ClassProgressing, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseMajorUpgrade (v1.30.0:714, main:725)",
	},
	// PhaseUpgradeDelayed: cluster_types.go v1.30.0:718, main:729.
	// "delayed by the operator configuration": a wait, not a fault.
	{
		Phase: "Cluster upgrade delayed", Class: ClassWarning, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseUpgradeDelayed (v1.30.0:718, main:729)",
	},
	// PhaseWaitingForUser: cluster_types.go v1.30.0:721, main:732.
	{
		Phase: "Waiting for user action", Class: ClassWarning, Kubectl: kubectlYellow, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseWaitingForUser (v1.30.0:721, main:732)",
	},
	// PhaseInplacePrimaryRestart: cluster_types.go v1.30.0:724, main:735.
	{
		Phase: "Primary instance is being restarted in-place", Class: ClassProgressing, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseInplacePrimaryRestart (v1.30.0:724, main:735)",
	},
	// PhaseInplaceDeletePrimaryRestart: cluster_types.go v1.30.0:727, main:738.
	{
		Phase: "Primary instance is being restarted without a switchover", Class: ClassProgressing, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseInplaceDeletePrimaryRestart (v1.30.0:727, main:738)",
	},
	// PhaseHealthy: cluster_types.go v1.30.0:730, main:741.
	{
		Phase: "Cluster in healthy state", Class: ClassOK, Kubectl: kubectlGreen, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseHealthy (v1.30.0:730, main:741)",
	},
	// PhaseUnknownPlugin: cluster_types.go v1.30.0:734, main:745.
	{
		Phase: "Cluster cannot proceed to reconciliation due to an unknown plugin being required", Class: ClassCritical, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseUnknownPlugin (v1.30.0:734, main:745)",
	},
	// PhaseFailurePlugin: cluster_types.go v1.30.0:737, main:748.
	{
		Phase: "Cluster cannot proceed to reconciliation due to an error while interacting with plugins", Class: ClassCritical, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseFailurePlugin (v1.30.0:737, main:748)",
	},
	// PhaseImageCatalogError: cluster_types.go v1.30.0:741, main:752.
	{
		Phase: "Cluster has incomplete or invalid image catalog", Class: ClassCritical, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseImageCatalogError (v1.30.0:741, main:752)",
	},
	// PhaseUnrecoverable: cluster_types.go v1.30.0:744, main:755.
	{
		Phase: "Cluster is unrecoverable and needs manual intervention", Class: ClassCritical, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseUnrecoverable (v1.30.0:744, main:755)",
	},
	// PhaseArchitectureBinaryMissing: cluster_types.go v1.30.0:747, main:758.
	{
		Phase: "Cluster cannot execute instance online upgrade due to missing architecture binary", Class: ClassCritical, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseArchitectureBinaryMissing (v1.30.0:747, main:758)",
	},
	// PhaseWaitingForInstancesToBeActive: cluster_types.go v1.30.0:750, main:761.
	// "a waiting phase ... triggered when an instance pod is not active".
	{
		Phase: "Waiting for the instances to become active", Class: ClassWarning, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseWaitingForInstancesToBeActive (v1.30.0:750, main:761)",
	},
	// PhaseOnlineUpgrading: cluster_types.go v1.30.0:753, main:764.
	{
		Phase: "Online upgrade in progress", Class: ClassProgressing, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseOnlineUpgrading (v1.30.0:753, main:764)",
	},
	// PhaseApplyingConfiguration: cluster_types.go v1.30.0:757, main:768.
	{
		Phase: "Applying configuration", Class: ClassProgressing, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseApplyingConfiguration (v1.30.0:757, main:768)",
	},
	// PhaseReplicaClusterPromotion: cluster_types.go v1.30.0:760, main:771.
	{
		Phase: "Promoting to primary cluster", Class: ClassProgressing, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseReplicaClusterPromotion (v1.30.0:760, main:771)",
	},
	// PhaseCannotCreateClusterObjects: cluster_types.go v1.30.0:763, main:774.
	{
		Phase: "Unable to create required cluster objects", Class: ClassCritical, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseCannotCreateClusterObjects (v1.30.0:763, main:774)",
	},
	// PhaseDefinitionInvalid: new in 1.30; cluster_types.go v1.30.0:766, main:777.
	{
		Phase: "Invalid cluster definition", Class: ClassCritical, Kubectl: kubectlRed, Since: Minor130,
		Source: "api/v1/cluster_types.go PhaseDefinitionInvalid (v1.30.0:766, main:777)",
	},
}

// PhaseReading is how the console renders one phase string.
type PhaseReading struct {
	Class Class `json:"class"`
	// Known is false when the phase is not in the minor's table: the UI
	// shows the text verbatim with a warning, never ok or critical.
	Known bool `json:"known"`
	// Reason is set when the class was capped below the table's: the
	// phase is in the table, but the table was not verified against the
	// minor the operator runs, so its healthy meaning is not asserted.
	Reason string `json:"reason,omitempty"`
}

// phaseVerifiedMinors are the minors whose phase table was read at a
// released tag and is checked there by make verify-pins-cnpgstatus
// (pins_test.go verifiedReleases: 1.30 at v1.30.0). 1.31 is planned: its
// rows were read at upstream main (21e24f6), not at a 1.31 tag.
var phaseVerifiedMinors = []Minor{Minor130}

// phaseVerified reports whether minor's phase table was verified at a
// released tag.
func phaseVerified(minor Minor) bool {
	for _, verified := range phaseVerifiedMinors {
		if minor == verified {
			return true
		}
	}
	return false
}

// PhaseTable returns the rows of minor's table in source order. An
// unsupported minor returns nil.
func PhaseTable(minor Minor) []PhaseEntry {
	if !minor.supported() {
		return nil
	}
	rows := make([]PhaseEntry, 0, len(phaseRows))
	for _, row := range phaseRows {
		if minor.atLeast(row.Since) {
			rows = append(rows, row)
		}
	}
	return rows
}

// ClassifyPhase classifies a Cluster.status.phase string for minor. An
// empty phase is unknown (the operator has not written one, or the
// status could not be read). A phase absent from the minor's table is a
// warning, not known: the text is new or foreign and is shown verbatim.
// An unsupported minor is evaluated against the newest released table
// (1.30); a phase found there is known, any other is not.
//
// The phase is a free sentence: a later minor can keep a string and
// change what it means. So a row the table reads as ok is capped at
// warning, with a "phase table not verified for CNPG <minor>" reason,
// whenever minor names a CloudNativePG minor whose table was not
// verified at a released tag (the planned 1.31, a newer 1.32, the
// unsupported 1.29). Rows that are not ok keep their class: a warning or
// a critical is never made to look better. An empty or unparseable
// minor (the version was not detected) is read against the newest
// released table without a cap, because nothing says it differs from it.
func ClassifyPhase(minor Minor, phase string) PhaseReading {
	reading := classifyPhaseRow(minor, phase)
	if reading.Known && reading.Class == ClassOK && namesMinor(minor) && !phaseVerified(minor) {
		reading.Class = ClassWarning
		reading.Reason = "phase table not verified for CNPG " + string(minor)
	}
	return reading
}

// classifyPhaseRow is the table's own reading of phase, without the cap
// for an unverified minor.
func classifyPhaseRow(minor Minor, phase string) PhaseReading {
	if phase == "" {
		return PhaseReading{Class: ClassUnknown, Known: false}
	}
	if !minor.supported() {
		minor = newestReleased
	}
	for _, row := range PhaseTable(minor) {
		if row.Phase == phase {
			return PhaseReading{Class: row.Class, Known: true}
		}
	}
	return PhaseReading{Class: ClassWarning, Known: false}
}

// namesMinor reports whether m is a MAJOR.MINOR CloudNativePG minor,
// supported or not, rather than empty or garbage.
func namesMinor(m Minor) bool {
	if m == "" || strings.Count(string(m), ".") != 1 {
		return false
	}
	v, ok := parseSemver(string(m))
	return ok && v.minorString() == m
}
