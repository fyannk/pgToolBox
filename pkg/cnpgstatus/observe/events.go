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

// EventFacts is one Kubernetes-observed event prepared for the console.
type EventFacts struct {
	// Name is the event object's name, the deduplication key.
	Name string
	// UID distinguishes incarnations sharing a name.
	UID string
	// Kind is the involved object's kind, such as "Cluster" or "Pod".
	Kind string
	// Object is the involved object's name.
	Object string
	// Type is the event type, such as "Normal" or "Warning".
	Type string
	// Reason is the machine reason.
	Reason string
	// Message is the bounded human message.
	Message string
	// Count is the delivery count of the event.
	Count int
	// LastSeen is the most recent occurrence known to the API server.
	LastSeen time.Time
}

// EventsSnapshot is the rendered event list: newest first, age-pruned,
// bounded, with its own staleness.
type EventsSnapshot struct {
	// Generation increases by one on every publication.
	Generation uint64
	// ObservedAt is the time of the last successful contact.
	ObservedAt time.Time
	// Stale reports lost contact.
	Stale bool
	// Truncated reports that more candidates existed than the bound.
	Truncated bool
	// Events is sorted newest first, then by name.
	Events []EventFacts
}
