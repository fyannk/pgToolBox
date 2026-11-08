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

// EventList is the events screen of one Cluster: the events about the
// Cluster and its child objects (pods, PVCs, backups, jobs, poolers),
// newest first, bounded by age and count.
type EventList struct {
	Ref         ClusterRef  `json:"ref"`
	GeneratedAt time.Time   `json:"generatedAt"`
	Source      SourceState `json:"source" doc:"State of the event informer."`
	// Window is the age window applied, such as "1h0m0s".
	Window string     `json:"window" doc:"Age window applied."`
	Items  []EventRow `json:"items"`
	// Truncated is set when the bound cut the list.
	Truncated bool `json:"truncated,omitempty"`
}

// EventRow is one Kubernetes event. Message is bounded and rendered as
// escaped text; it never carries secret material the console added.
type EventRow struct {
	Type      string     `json:"type" enum:"Normal,Warning" doc:"Event type, verbatim."`
	Reason    string     `json:"reason" untrusted:"true"`
	Message   string     `json:"message,omitempty"`
	Object    ObjectRef  `json:"object"`
	Count     int32      `json:"count"`
	FirstSeen *time.Time `json:"firstSeen,omitempty"`
	LastSeen  time.Time  `json:"lastSeen"`
	// Reporter is the component that emitted it, such as
	// cloudnative-pg or kubelet; it decides the origin.
	Reporter string `json:"reporter,omitempty"`
	Origin   Origin `json:"origin" enum:"operator,kubernetes,instance,console,evidence"`
	Health   Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
}
