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

// Cluster condition types, as written into Cluster.status.conditions.
const (
	ConditionReady                            = "Ready"
	ConditionContinuousArchiving              = "ContinuousArchiving"
	ConditionLastBackupSucceeded              = "LastBackupSucceeded"
	ConditionConsistentSystemID               = "ConsistentSystemID"
	ConditionInitialized                      = "Initialized"
	ConditionSyncReplicationTopologySatisfied = "SyncReplicationTopologySatisfied"
	ConditionHibernation                      = "cnpg.io/hibernation"

	// Fork condition types (fyannk/cloudnative-pg dev branches, upstream
	// proposals). Upstream never writes them.
	ConditionTopologyUnavailable    = "TopologyUnavailable"
	ConditionVolumeExpansionRefused = "VolumeExpansionRefused"
	ConditionVolumeAutoGrowStopped  = "VolumeAutoGrowStopped"
)

// Condition statuses (metav1.ConditionStatus values).
const (
	statusTrue    = "True"
	statusFalse   = "False"
	statusUnknown = "Unknown"
)

// anyReason matches every reason of a status not matched more precisely.
const anyReason = "*"

// ConditionRule is one row of a condition table: a (type, status,
// reason) triple and its class. Reason "*" matches any reason of that
// status not listed on its own row.
type ConditionRule struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
	Class  Class  `json:"class"`
	// Since is the first supported minor that writes this type; empty
	// for fork types.
	Since Minor `json:"since,omitempty"`
	// Fork names the fork branch that defines the type; empty upstream.
	Fork string `json:"fork,omitempty"`
	// Source names where the type and reason are written, per tag.
	Source string `json:"source"`
}

// Source strings shared by the rows of one type.
const (
	srcReady = "api/v1/cluster_types.go ConditionClusterReady (v1.30.0:1178, main:1189); " +
		"set True/ClusterIsReady iff phase is healthy, else False/ClusterIsNotReady (pkg/resources/status/transactions.go SetClusterReadyCondition v1.30.0:34-56)"
	srcArchiving = "api/v1/cluster_types.go ConditionContinuousArchiving (v1.30.0:1174, main:1185); " +
		"written by the instance on every archive_command (pkg/management/postgres/webserver/local.go v1.30.0:247-263, v1.30.1 and main:264-280; built with no LastTransitionTime, which meta.SetStatusCondition in pkg/resources/status/conditions.go PatchConditionsWithOptimisticLock keeps while the status stays: pins_facts_test.go TestPinContinuousArchivingSince)"
	srcBackup = "api/v1/cluster_types.go ConditionBackup (v1.30.0:1176, main:1187); " +
		"api/v1/cluster_conditions.go v1.30.0:28-53 (BackupSucceededCondition, BackupStartingCondition, BuildClusterBackupFailedCondition)"
	srcSystemID = "api/v1/cluster_types.go ConditionConsistentSystemID (v1.30.0:1181, main:1192); " +
		"reasons NotFound/Unique/Mismatch (internal/controller/cluster_status.go v1.30.0:791-810, main:55-61)"
	srcInitialized = "api/v1/cluster_types.go ConditionInitialized, BootstrapCompleted, BootstrapPending " +
		"(v1.30.0:1185/1237/1241, main:1196/1255/1259)"
	srcSyncTopology = "PLANNED 1.31: api/v1/cluster_types.go ConditionSyncReplicationTopologySatisfied and reasons " +
		"Satisfied/TopologyNotExtracted/InsufficientCrossDomainReplicas (main:1203/1263/1267/1272; absent at v1.30.0); " +
		"set only when failure-domain keys are configured (internal/controller/cluster_status.go main:925-983)"
	srcHibernation = "pkg/reconciler/hibernation/status.go HibernationConditionType and reasons (v1.30.0:45-68, main:45-68); " +
		"EnrichStatus v1.30.0:83-134, isHibernationEnabled v1.30.0:136 (WrongAnnotationValue is declared but never set at v1.30.0)"
	srcTopologyUnavailable = "FORK origin/dev/46 (bc754ecfb) api/v1/cluster_types.go:1201 ConditionTopologyUnavailable; True = topology needs Node labels the operator does not read"
	srcExpansionRefused    = "FORK origin/dev/64 (931cda924) api/v1/cluster_types.go:1208 ConditionVolumeExpansionRefused; True while the API server refuses a PVC expansion"
	srcAutoGrowStopped     = "FORK origin/dev/64 (931cda924) api/v1/cluster_types.go:1214 ConditionVolumeAutoGrowStopped; True while an autoGrow volume above threshold is not grown"
)

// Fork branches the fork rows were read at.
const (
	forkNodeEcho = "origin/dev/46"
	forkAutoGrow = "origin/dev/64"
)

var conditionRules = []ConditionRule{
	// Ready mirrors phase == healthy; the phase carries the detail, so a
	// False Ready is a warning whose cause the phase names.
	{Type: ConditionReady, Status: statusTrue, Reason: anyReason, Class: ClassOK, Since: Minor130, Source: srcReady},
	{Type: ConditionReady, Status: statusFalse, Reason: anyReason, Class: ClassWarning, Since: Minor130, Source: srcReady},

	// ContinuousArchiving False means the last archive_command failed:
	// WAL piles up in pg_wal and the recovery window stops growing.
	// Archiving() refines it with the primary's live archiver.
	{Type: ConditionContinuousArchiving, Status: statusTrue, Reason: anyReason, Class: ClassOK, Since: Minor130, Source: srcArchiving},
	{Type: ConditionContinuousArchiving, Status: statusFalse, Reason: anyReason, Class: ClassCritical, Since: Minor130, Source: srcArchiving},

	// LastBackupSucceeded False/BackupStarted is a backup in flight; a
	// failed last backup is a warning: recoverability is judged on the
	// backup set and the recovery window, not on the last attempt alone.
	{Type: ConditionLastBackupSucceeded, Status: statusTrue, Reason: anyReason, Class: ClassOK, Since: Minor130, Source: srcBackup},
	{Type: ConditionLastBackupSucceeded, Status: statusFalse, Reason: "BackupStarted", Class: ClassProgressing, Since: Minor130, Source: srcBackup},
	{Type: ConditionLastBackupSucceeded, Status: statusFalse, Reason: anyReason, Class: ClassWarning, Since: Minor130, Source: srcBackup},

	// ConsistentSystemID (1.27+): Mismatch means instances belong to
	// different PostgreSQL systems; NotFound means none reported yet.
	{Type: ConditionConsistentSystemID, Status: statusTrue, Reason: anyReason, Class: ClassOK, Since: Minor130, Source: srcSystemID},
	{Type: ConditionConsistentSystemID, Status: statusFalse, Reason: "Mismatch", Class: ClassCritical, Since: Minor130, Source: srcSystemID},
	{Type: ConditionConsistentSystemID, Status: statusFalse, Reason: anyReason, Class: ClassWarning, Since: Minor130, Source: srcSystemID},

	// Initialized (1.30): False until a first instance ran, then True
	// forever.
	{Type: ConditionInitialized, Status: statusTrue, Reason: anyReason, Class: ClassOK, Since: Minor130, Source: srcInitialized},
	{Type: ConditionInitialized, Status: statusFalse, Reason: anyReason, Class: ClassProgressing, Since: Minor130, Source: srcInitialized},

	// SyncReplicationTopologySatisfied (planned 1.31): False means the
	// failure-domain constraint is not applied.
	{Type: ConditionSyncReplicationTopologySatisfied, Status: statusTrue, Reason: anyReason, Class: ClassOK, Since: Minor131, Source: srcSyncTopology},
	{Type: ConditionSyncReplicationTopologySatisfied, Status: statusFalse, Reason: anyReason, Class: ClassWarning, Since: Minor131, Source: srcSyncTopology},

	// cnpg.io/hibernation exists only while the annotation is "on".
	{Type: ConditionHibernation, Status: statusTrue, Reason: anyReason, Class: ClassInactive, Since: Minor130, Source: srcHibernation},
	{Type: ConditionHibernation, Status: statusFalse, Reason: "DeletingPods", Class: ClassProgressing, Since: Minor130, Source: srcHibernation},
	{Type: ConditionHibernation, Status: statusFalse, Reason: "WaitingPodsDeletion", Class: ClassProgressing, Since: Minor130, Source: srcHibernation},
	{Type: ConditionHibernation, Status: statusFalse, Reason: "WaitingForHealthy", Class: ClassWarning, Since: Minor130, Source: srcHibernation},
	{Type: ConditionHibernation, Status: statusFalse, Reason: anyReason, Class: ClassWarning, Since: Minor130, Source: srcHibernation},

	// Fork types: True is the problem state, False its clearance.
	{Type: ConditionTopologyUnavailable, Status: statusTrue, Reason: anyReason, Class: ClassWarning, Fork: forkNodeEcho, Source: srcTopologyUnavailable},
	{Type: ConditionTopologyUnavailable, Status: statusFalse, Reason: anyReason, Class: ClassOK, Fork: forkNodeEcho, Source: srcTopologyUnavailable},
	{Type: ConditionVolumeExpansionRefused, Status: statusTrue, Reason: anyReason, Class: ClassWarning, Fork: forkAutoGrow, Source: srcExpansionRefused},
	{Type: ConditionVolumeExpansionRefused, Status: statusFalse, Reason: anyReason, Class: ClassOK, Fork: forkAutoGrow, Source: srcExpansionRefused},
	{Type: ConditionVolumeAutoGrowStopped, Status: statusTrue, Reason: anyReason, Class: ClassWarning, Fork: forkAutoGrow, Source: srcAutoGrowStopped},
	{Type: ConditionVolumeAutoGrowStopped, Status: statusFalse, Reason: anyReason, Class: ClassOK, Fork: forkAutoGrow, Source: srcAutoGrowStopped},
}

// ConditionReading is how the console renders one condition.
type ConditionReading struct {
	Class Class `json:"class"`
	// Known is false when the type is not in the minor's table (render it
	// generically: type, status, reason, message, age), when its status
	// is not True/False/Unknown, or for a fork type read without a fork
	// pin.
	Known bool `json:"known"`
	// Reason is set when the class was capped below the table's, as for
	// phases: the type and reason are in the table, but the table was
	// not verified against the minor the operator runs.
	Reason string `json:"reason,omitempty"`
}

// ConditionTable returns the rules of minor's table: the upstream types
// the minor writes, then, when fork is true, the fork types. An
// unsupported minor returns nil.
func ConditionTable(minor Minor, fork bool) []ConditionRule {
	if !minor.supported() {
		return nil
	}
	rules := make([]ConditionRule, 0, len(conditionRules))
	for _, rule := range conditionRules {
		if rule.Fork != "" {
			if fork {
				rules = append(rules, rule)
			}
			continue
		}
		if minor.atLeast(rule.Since) {
			rules = append(rules, rule)
		}
	}
	return rules
}

// ClassifyCondition classifies one Cluster condition for minor, without
// a fork pin. See ClassifyConditionPinned.
func ClassifyCondition(minor Minor, conditionType, status, reason string) ConditionReading {
	return ClassifyConditionPinned(minor, false, conditionType, status, reason)
}

// ClassifyConditionPinned classifies one Cluster condition for minor.
//
//   - A type the minor does not write (an unknown type, or Initialized
//     read under a minor that predates it) is info and not known: it is rendered generically,
//     never hidden and never coloured by guess.
//   - Status "Unknown" of a known type is unknown; a status that is not
//     True/False/Unknown is unknown and not known.
//   - Fork types carry their fork semantics (True = warning) either way,
//     because only the fork writes them, but are Known only when fork is
//     true: the caller asserts the operator build is the fork the rows
//     were read from (origin/dev/46, origin/dev/64), whose semantics may
//     still change before upstream adopts them.
//   - An unsupported minor is evaluated against the newest released
//     table (1.30); types outside it are not known.
//   - A known ok reading is capped at warning, with Reason, when minor's
//     table was not verified at a released tag (an empty minor
//     included), as ClassifyPhase caps phases.
func ClassifyConditionPinned(minor Minor, fork bool, conditionType, status, reason string) ConditionReading {
	reading, isFork := classifyConditionRow(TableMinor(minor), fork, conditionType, status, reason)
	// A fork type's rows were read at the fork's branch, not at a minor:
	// the fork pin, not the minor, is what they rest on.
	if reading.Known && reading.Class == ClassOK && !isFork {
		if why := capReason("condition", minor); why != "" {
			reading.Class, reading.Reason = ClassWarning, why
		}
	}
	return reading
}

// classifyConditionRow is the table's own reading, without the cap for
// an unverified minor; minor has tables. isFork reports a fork type.
func classifyConditionRow(minor Minor, fork bool, conditionType, status, reason string) (reading ConditionReading, isFork bool) {
	var typeRules []ConditionRule
	for _, rule := range conditionRules {
		if rule.Type != conditionType {
			continue
		}
		if rule.Fork != "" {
			isFork = true
			typeRules = append(typeRules, rule)
			continue
		}
		if minor.atLeast(rule.Since) {
			typeRules = append(typeRules, rule)
		}
	}
	if len(typeRules) == 0 {
		return ConditionReading{Class: ClassInfo, Known: false}, isFork
	}
	known := !isFork || fork
	switch status {
	case statusTrue, statusFalse:
	case statusUnknown:
		return ConditionReading{Class: ClassUnknown, Known: known}, isFork
	default:
		return ConditionReading{Class: ClassUnknown, Known: false}, isFork
	}
	for _, rule := range typeRules {
		if rule.Status == status && rule.Reason == reason {
			return ConditionReading{Class: rule.Class, Known: known}, isFork
		}
	}
	for _, rule := range typeRules {
		if rule.Status == status && rule.Reason == anyReason {
			return ConditionReading{Class: rule.Class, Known: known}, isFork
		}
	}
	// Every known type has a wildcard row for True and False; reaching
	// here means the table lost one, which the tests forbid.
	return ConditionReading{Class: ClassUnknown, Known: false}, isFork
}
