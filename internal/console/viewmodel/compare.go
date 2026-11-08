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

// Comparison is the compare screen: two Clusters side by
// side, section by section, with the rows that differ marked. The rows
// are decided here (which fields, what counts as the same value); the UI
// only renders them. Values are declared intent (the spec) or operator
// claims (the status), each section saying which.
type Comparison struct {
	GeneratedAt time.Time        `json:"generatedAt"`
	Left        ComparedCluster  `json:"left"`
	Right       ComparedCluster  `json:"right"`
	Sections    []CompareSection `json:"sections"`
	// Differences counts the rows that differ across every section.
	Differences int         `json:"differences"`
	BlindSpots  []BlindSpot `json:"blindSpots"`
}

// ComparedCluster is one side's identity and verdict.
type ComparedCluster struct {
	Ref        ClusterRef `json:"ref"`
	Health     Health     `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info" doc:"The Cluster's verdict, as on the cluster list."`
	Phase      string     `json:"phase,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// CompareSection is one group of rows.
type CompareSection struct {
	ID          string       `json:"id" enum:"version,instances,parameters,resources,storage,backup,topology" doc:"Stable key, used for the section title's translation."`
	Origin      Origin       `json:"origin" enum:"operator,kubernetes,instance,console,evidence" doc:"Whose claim the values are."`
	Source      string       `json:"source" doc:"The fields the values are read from."`
	Rows        []CompareRow `json:"rows"`
	Differences int          `json:"differences"`
}

// CompareValue is one side of a row.
type CompareValue struct {
	State string `json:"state" enum:"set,unset,unknown" doc:"set: the value; unset: declared nowhere (the default applies); unknown: the console cannot read it."`
	Value string `json:"value,omitempty"`
}

// CompareRow is one field on both sides.
type CompareRow struct {
	Key   string       `json:"key" doc:"The field or parameter name, verbatim."`
	Left  CompareValue `json:"left"`
	Right CompareValue `json:"right"`
	// Same is decided in Go: equal text, or for a memory parameter the
	// same number of bytes; an unknown side is never the same.
	Same bool `json:"same"`
	// Note qualifies Same, such as "the same 1 GiB, spelled differently".
	Note string `json:"note,omitempty"`
}
