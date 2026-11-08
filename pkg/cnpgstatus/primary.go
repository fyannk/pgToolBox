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

import (
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
)

// Primary states.
const (
	PrimaryStable          = "stable"
	PrimarySwitchover      = "switchover"
	PrimaryFailoverPending = "failoverPending"
	PrimaryFailover        = "failover"
	PrimaryNone            = "none"
	PrimaryUnknown         = "unknown"
)

// Phase strings Primary reads (api/v1/cluster_types.go PhaseSwitchover
// and PhaseFailOver, identical at v1.30.0 and main).
const (
	phaseSwitchover = "Switchover in progress"
	phaseFailOver   = "Failing over"
)

// PendingFailoverMarker is the targetPrimary value that means "the
// primary is failing and the failover delay is running"
// (api/v1/cluster_types.go PendingFailoverMarker v1.30.0:108).
const PendingFailoverMarker = "pending"

// PrimaryReading is the operator's claim about the primary. It is not
// the live truth: compare it with each instance's isPrimary and the Pod
// role label.
type PrimaryReading struct {
	Current string `json:"current,omitempty"`
	Target  string `json:"target,omitempty"`
	// State is stable, switchover, failoverPending, failover, none or
	// unknown.
	State string `json:"state"`
	// Since is when the state began, as the operator recorded it; nil
	// when it did not record it or the timestamp does not parse.
	Since *time.Time `json:"since,omitempty"`
}

// Primary reads status.currentPrimary / targetPrimary, the phase and
// the timestamps (api/v1/cluster_types.go v1.30.0:1061-1072; all three
// are written with pgTime.GetCurrentTimestamp, RFC3339 with
// microseconds, machinery pkg/postgres/time/time.go:72-75).
//
//   - targetPrimary "pending": failoverPending, since
//     currentPrimaryFailingSinceTimestamp (internal/controller/replicas.go
//     v1.30.0:443).
//   - phase "Failing over": failover; phase "Switchover in progress" or
//     currentPrimary != targetPrimary: switchover; both since
//     targetPrimaryTimestamp (internal/controller/cluster_status.go
//     v1.30.0:289, :727; the promote plugin, promote.go:64).
//   - no primary recorded yet: none; a nil Cluster: unknown.
//   - otherwise stable, since currentPrimaryTimestamp.
func Primary(c *cnpgv1.Cluster) PrimaryReading {
	if c == nil {
		return PrimaryReading{State: PrimaryUnknown}
	}
	status := c.Status
	reading := PrimaryReading{
		Current: bound(status.CurrentPrimary, maxAnnotationValue),
		Target:  bound(status.TargetPrimary, maxAnnotationValue),
	}
	switch {
	case status.TargetPrimary == PendingFailoverMarker:
		reading.State = PrimaryFailoverPending
		reading.Since = parseOperatorTimestamp(status.CurrentPrimaryFailingSinceTimestamp)
		if reading.Since == nil {
			reading.Since = parseOperatorTimestamp(status.TargetPrimaryTimestamp)
		}
	case status.Phase == phaseFailOver:
		reading.State = PrimaryFailover
		reading.Since = parseOperatorTimestamp(status.TargetPrimaryTimestamp)
	case status.CurrentPrimary == "":
		reading.State = PrimaryNone
	case status.Phase == phaseSwitchover || (status.TargetPrimary != "" && status.TargetPrimary != status.CurrentPrimary):
		reading.State = PrimarySwitchover
		reading.Since = parseOperatorTimestamp(status.TargetPrimaryTimestamp)
	default:
		reading.State = PrimaryStable
		reading.Since = parseOperatorTimestamp(status.CurrentPrimaryTimestamp)
	}
	return reading
}

// parseOperatorTimestamp parses the operator's RFC3339 timestamps (with
// or without fractional seconds) into UTC; anything else is nil.
func parseOperatorTimestamp(s string) *time.Time {
	if s == "" || len(s) > maxVersionLength {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}
