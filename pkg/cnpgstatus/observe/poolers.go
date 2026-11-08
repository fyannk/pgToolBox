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

package observe

import (
	"time"
)

// PoolerFacts is the bounded operator-reported state of one Pooler.
//
// The connection pooler sits between the applications and the database,
// so what an operator needs from this screen is which endpoint it fronts
// and whether it is accepting connections. Nothing from
// status.secrets crosses this boundary: the console never reads Secret
// material, and a pooler's credential references are not review data.
type PoolerFacts struct {
	// Name is the resource name.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Type is the endpoint the pooler fronts: rw, ro or r.
	Type string
	// PoolMode is the configured PgBouncer pooling mode, session or
	// transaction. Empty when the resource did not report one.
	PoolMode string
	// DesiredInstances is the requested pooler pod count. Nil when the
	// resource did not report one.
	DesiredInstances *int32
	// ReadyInstances is the operator-reported scheduled pod count.
	ReadyInstances int32
	// Phase is the operator-reported lifecycle phase: active, paused,
	// inactive or failed.
	Phase string
	// PhaseReason is the operator's explanation of the phase.
	PhaseReason string
	// Image is the resolved pgbouncer image the operator reports. While
	// the phase is inactive or failed it may be the last known one.
	Image string
	// CreatedAt is the resource creation time.
	CreatedAt time.Time
}

// PoolersSnapshot is the rendered pooler set, immutable and carrying its
// own staleness: sources fail independently.
type PoolersSnapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful contact.
	ObservedAt time.Time
	// Stale reports lost contact; the set below is the retained
	// last-good observation.
	Stale bool
	// Truncated reports that more matched than the bound.
	Truncated bool
	// Poolers is sorted by name; the producer bounds it.
	Poolers []PoolerFacts
}
