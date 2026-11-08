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

// PrimaryLeaseFacts is the Kubernetes Lease CloudNativePG 1.30 keeps
// as its primary-election gate, named after the cluster. The instance
// promoted to primary acquires it and renews it continuously; a primary
// shutting down releases it. Everything here is the Lease object's own
// spec, read verbatim: the console adds no judgement of its own.
type PrimaryLeaseFacts struct {
	// Present reports that the API server confirmed the Lease exists.
	// False is a successful observation of absence: an operator before
	// 1.30 keeps none.
	Present bool
	// Name is the Lease's own name — the cluster's — so evidence can
	// name the object that was actually observed.
	Name string
	// Holder is the instance holding the lease, empty when released.
	Holder string
	// AcquiredAt and RenewedAt are the holder's own stamps; nil when
	// the Lease carries none.
	AcquiredAt *time.Time
	RenewedAt  *time.Time
	// DurationSeconds is how long a renewal is good for; nil when the
	// Lease carries none.
	DurationSeconds *int32
	// Transitions counts how many times the holder changed, as the
	// holders themselves maintain it; nil when the Lease carries none.
	Transitions *int32
}

// PrimaryLeaseSnapshot is what the console reads: the last observed
// lease and its own staleness.
type PrimaryLeaseSnapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful contact.
	ObservedAt time.Time
	// Stale reports lost contact; the facts below are the retained
	// last-good observation.
	Stale bool
	// Lease is the last observed state.
	Lease PrimaryLeaseFacts
}
