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

// TimelineKinds are the entry kinds of the incident timeline, in the
// order the filter offers them.
var TimelineKinds = []string{"phase", "primary", "restart", "backup", "finding", "revision", "event", "gap", "console"}

// Timeline is the /history screen: one time axis per
// Cluster merging what the console holds — its own journal of
// transitions (phase, primary, restarts, backup results, findings, its
// own gaps), the revision history and the Kubernetes Events still
// retained — newest first, within a window.
type Timeline struct {
	Ref         ClusterRef `json:"ref"`
	GeneratedAt time.Time  `json:"generatedAt"`
	// From and To are the window applied (To exclusive).
	From time.Time `json:"from" doc:"Start of the window applied."`
	To   time.Time `json:"to" doc:"End of the window applied (exclusive)."`
	// Kinds is the kind filter applied; empty is every kind.
	Kinds []string `json:"kinds" doc:"Kinds selected; empty means every kind."`
	// Source is "timeline": live, or disabled with history.
	Source SourceState `json:"source"`
	// Persistent reports that the journal and the revisions survive
	// console restarts; otherwise memory begins at this replica's start.
	Persistent bool `json:"persistent"`
	// Evicted reports that retention bounds dropped older entries.
	Evicted bool `json:"evicted,omitempty"`
	// ObservedSince is the oldest entry the console's own memory retains:
	// before it, only Kubernetes Events and server timestamps can speak.
	ObservedSince *time.Time `json:"observedSince,omitempty" doc:"Oldest entry the console's own journal retains."`
	// EventsWindow is the age bound of the Kubernetes Events served
	// (EVENTS_MAX_AGE): older Events are not shown even inside the window.
	EventsWindow string `json:"eventsWindow" doc:"Age bound of the Kubernetes Events served."`
	// Counts are the entries per kind inside the window, before the
	// kind filter and the limit.
	Counts []TimelineCount `json:"counts"`
	// Items are newest first.
	Items []TimelineEntry `json:"items"`
	// Truncated reports that the limit cut the list.
	Truncated bool `json:"truncated,omitempty"`
}

// TimelineCount is the number of entries of one kind.
type TimelineCount struct {
	Kind  string `json:"kind" enum:"phase,primary,restart,backup,finding,revision,event,gap,console"`
	Count int    `json:"count"`
}

// TimelineEntry is one point (or the end of a span) on the axis.
type TimelineEntry struct {
	// ID is stable within its source: j:<seq> for the journal,
	// r:<seq> for a revision, e:<uid> for a Kubernetes Event.
	ID   string `json:"id"`
	Kind string `json:"kind" enum:"phase,primary,restart,backup,finding,revision,event,gap,console"`
	// Action is the kind's verb: phase/primary changed, firstObserved,
	// failover, switchover; restart restarted; backup completed, failed;
	// finding appeared, cleared, worsened, improved, firstObserved;
	// revision the history change (created, spec-changed, deleted, ...);
	// event the Event type (Normal, Warning); gap began, ended; console
	// started.
	Action string    `json:"action"`
	At     time.Time `json:"at"`
	// Timing says whose clock At is: "observed" is the console's (the
	// change happened at or shortly before), "reported" is a timestamp
	// the operator, the kubelet or the API server wrote.
	Timing string `json:"timing" enum:"observed,reported"`
	// Since starts the window of an entry found after the console was not
	// looking (AfterGap), the gap an "ended" entry closes, or the previous
	// console life's last heartbeat on a start.
	Since    *time.Time `json:"since,omitempty"`
	AfterGap bool       `json:"afterGap,omitempty" doc:"The change happened at an unobserved point between since and at."`
	// Uncertain marks a primary change whose cause the console did not
	// see: no failover or switchover phase was observed since the
	// previous change, or both were.
	Uncertain bool `json:"uncertain,omitempty" doc:"The console did not observe what caused this change."`
	// Scope is cluster, namespace (a source gap of the namespace) or
	// console (the console itself).
	Scope    string     `json:"scope" enum:"cluster,namespace,console"`
	Subject  *ObjectRef `json:"subject,omitempty"`
	From     string     `json:"from,omitempty"`
	To       string     `json:"to,omitempty"`
	Severity Severity   `json:"severity,omitempty" enum:"critical,high,warning,note"`
	// Health is how the entry is drawn.
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Detail is bounded source text (phase reason, finding summary, Event
	// message, exit reason), rendered escaped.
	Detail string `json:"detail,omitempty"`
	// Ref is the finding ID, the source name of a gap, the Event reason,
	// or the revision's seq.
	Ref        string     `json:"ref,omitempty"`
	Count      int32      `json:"count,omitempty" doc:"An Event's count."`
	Provenance Provenance `json:"provenance"`
	// Link is the cluster-relative route that explains the entry.
	Link string `json:"link,omitempty"`
}

// ChangePreview is what a Cluster revision sets in motion,
// predicted from the operator's rules (pkg/cnpgstatus/preview); the
// instances' pendingRestart remains the fact once applied.
type ChangePreview struct {
	Effect     string             `json:"effect" enum:"none,reload,start,restart,rollout,shutdown,majorUpgrade,unknown" doc:"The most disruptive ordered effect; unknown as soon as one change could not be classified (atLeast then gives the floor)."`
	AtLeast    string             `json:"atLeast,omitempty" enum:"none,reload,start,restart,rollout,shutdown,majorUpgrade" doc:"Set when effect is unknown: the most disruptive effect of the changes that were classified, which the change does at least."`
	Parameters []PreviewParameter `json:"parameters"`
	Fields     []PreviewField     `json:"fields"`
	Strategy   string             `json:"strategy" enum:"unsupervised,supervised" doc:"primaryUpdateStrategy in force after the change."`
	Method     string             `json:"method" enum:"restart,switchover" doc:"primaryUpdateMethod in force after the change."`
	Primary    string             `json:"primary" enum:"none,reload,restartInPlace,recreate,switchover,waitForUser,restartFirst,majorUpgrade,shutdown,start,unknown" doc:"What happens to the primary; unknown when a change could not be classified."`
	Steps      []PreviewStep      `json:"steps" doc:"The order in which instances are touched."`
	// PostgresMajor is the major the parameter contexts were read for.
	PostgresMajor int `json:"postgresMajor,omitempty"`
	// Verified reports that both the PostgreSQL major and the operator
	// minor are covered by the tables and that every change was
	// classified.
	Verified bool `json:"verified"`
	// Unknown counts the changes the tables cannot classify.
	Unknown int `json:"unknown,omitempty"`
	// CNPGMinor is the operator minor the rules were applied for.
	CNPGMinor string `json:"cnpgMinor,omitempty"`
}

// PreviewParameter is one changed PostgreSQL parameter.
type PreviewParameter struct {
	Name     string `json:"name"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"`
	Effect   string `json:"effect" enum:"reload,restart,refused,unknown"`
	Context  string `json:"context,omitempty" enum:"postmaster,reload,fixed,blocked,absent" doc:"absent: the Cluster's PostgreSQL major has no such parameter and rejects the configuration."`
	Decrease bool   `json:"decrease,omitempty" doc:"A lower hot-standby-sensitive value: the primary restarts first."`
}

// PreviewField is one changed spec field outside the parameters.
type PreviewField struct {
	Path   string `json:"path"`
	Effect string `json:"effect" enum:"none,reload,start,restart,rollout,shutdown,majorUpgrade,unknown"`
	Reason string `json:"reason" enum:"image,majorVersion,podTemplate,newVolume,volumeResize,scale,restartAnnotation,hibernation,fencing,sharedPreloadLibraries,managedExtension,certificates,hba,synchronous,replicaRole,operatorOnly,unknownField"`
}

// PreviewStep is one stage of the restart order.
type PreviewStep struct {
	Role      string   `json:"role" enum:"replicas,primary,all"`
	Instances []string `json:"instances" doc:"Instance names from the Cluster status; replicas go most-lagged first at the time of the update."`
	Action    string   `json:"action" doc:"reload, recreate (the Pod is deleted and recreated on its volumes), start, shutdown, or one of the primary's actions."`
}
