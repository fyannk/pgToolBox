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

package viewmodel

import "time"

// DrillResult is where a restore drill stands.
type DrillResult string

// The drill results. Running is the only non-final one.
const (
	DrillRunning   DrillResult = "running"
	DrillPassed    DrillResult = "passed"
	DrillFailed    DrillResult = "failed"
	DrillTimedOut  DrillResult = "timedOut"
	DrillCancelled DrillResult = "cancelled"
)

// DrillCheckName names one check of a drill.
type DrillCheckName string

// The checks, in the order they are judged.
const (
	DrillCheckHealthy  DrillCheckName = "healthy"
	DrillCheckPromoted DrillCheckName = "promoted"
	DrillCheckSize     DrillCheckName = "size"
)

// DrillCheckOutcome is one check's outcome.
type DrillCheckOutcome string

// The outcomes. NotChecked is a blind spot, not a pass or a failure.
const (
	DrillCheckPending    DrillCheckOutcome = "pending"
	DrillCheckPassed     DrillCheckOutcome = "passed"
	DrillCheckFailed     DrillCheckOutcome = "failed"
	DrillCheckNotChecked DrillCheckOutcome = "notChecked"
)

// RestoreDrills is the restore-drill block of the Backups screen:
// the drills of this Cluster the console holds, newest
// first, and what their check does and does not prove.
type RestoreDrills struct {
	// Enabled is RESTORE_DRILL_ENABLED; off, the block says so and holds
	// no item.
	Enabled bool `json:"enabled"`
	// Namespace is where a drill of this Cluster runs.
	Namespace string `json:"namespace,omitempty"`
	// TimeLimitSeconds is the configured time limit.
	TimeLimitSeconds int64 `json:"timeLimitSeconds,omitempty"`
	// SizeTolerancePercent is the size check's tolerance.
	SizeTolerancePercent int `json:"sizeTolerancePercent,omitempty"`
	// LastPassedAt is when the newest passed drill was judged.
	LastPassedAt *time.Time `json:"lastPassedAt,omitempty"`
	// Items are the drills, newest first (running ones included).
	Items []RestoreDrill `json:"items"`
	// Scope says where the record lives and how long it lasts.
	Scope string `json:"scope"`
	// NotChecked lists what a passed drill does not prove.
	NotChecked []string `json:"notChecked"`
}

// RestoreDrill is one drill.
type RestoreDrill struct {
	Drill  ClusterRef `json:"drill"`
	Source ClusterRef `json:"source"`
	// Backup is the newest completed Backup when the drill started; the
	// recovery restores the newest base backup of the archive, normally
	// that one, and replays the archived WAL after it.
	Backup string `json:"backup,omitempty"`
	// BackupLabel is the pgBackRest backup restored
	// (recoveryTarget.backupID); absent for Barman.
	BackupLabel     string     `json:"backupLabel,omitempty" untrusted:"true"`
	BackupStoppedAt *time.Time `json:"backupStoppedAt,omitempty"`
	StartedAt       time.Time  `json:"startedAt"`
	Deadline        time.Time  `json:"deadline"`
	// HealthyAt is when the console first saw the drill Cluster healthy
	// (to within one supervision interval).
	HealthyAt *time.Time `json:"healthyAt,omitempty"`
	// FinishedAt is when the result was decided.
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	// TornDownAt is when the console saw the drill Cluster deleted.
	TornDownAt *time.Time `json:"tornDownAt,omitempty"`
	// DurationSeconds is start → healthy: the measured restore time.
	DurationSeconds *int64 `json:"durationSeconds,omitempty"`
	// RestoredBytes is the restored instance's database size (exporter).
	RestoredBytes *int64 `json:"restoredBytes,omitempty"`
	// SourceBytes is the source primary's database size at the start.
	SourceBytes *int64 `json:"sourceBytes,omitempty"`
	// Timeline is the restored instance's timeline after promotion.
	Timeline *int        `json:"timeline,omitempty"`
	Result   DrillResult `json:"result" enum:"running,passed,failed,timedOut,cancelled"`
	Health   Health      `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Reason says why a drill failed, timed out or was cancelled.
	Reason string       `json:"reason,omitempty"`
	Checks []DrillCheck `json:"checks"`
	// TeardownError is set while the console could not delete the drill
	// Cluster (the teardown Role missing, for instance).
	TeardownError string `json:"teardownError,omitempty"`
}

// DrillCheck is one check, attributed to where it was read.
type DrillCheck struct {
	Name    DrillCheckName    `json:"name" enum:"healthy,promoted,size"`
	Outcome DrillCheckOutcome `json:"outcome" enum:"pending,passed,failed,notChecked"`
	Text    string            `json:"text"`
	Source  string            `json:"source" doc:"Where it was read, such as Cluster.status.phase or /pg/status."`
}

// DrillHealth maps a drill result onto the health vocabulary.
func DrillHealth(r DrillResult) Health {
	switch r {
	case DrillPassed:
		return HealthOK
	case DrillFailed, DrillTimedOut:
		return HealthCritical
	case DrillCancelled:
		return HealthInactive
	case DrillRunning:
		return HealthProgressing
	default:
		return HealthUnknown
	}
}
