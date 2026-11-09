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

// ClusterList is the "/" screen: one row per Cluster of the watched
// namespaces. The read model returns every Cluster it watches; the API
// layer removes the rows the caller may not get (one SAR per row) before
// serving it, so a row's presence is itself an authorization outcome.
type ClusterList struct {
	GeneratedAt time.Time `json:"generatedAt" doc:"When the read model assembled this list."`
	// Namespaces are the watched namespaces with the state of the
	// Cluster informer in each, so "no clusters" is distinguishable from
	// "could not list clusters here".
	Namespaces []NamespaceState `json:"namespaces" doc:"The watched namespaces and whether their Clusters could be listed."`
	Items      []ClusterRow     `json:"items" doc:"One row per Cluster."`
}

// NamespaceState is one watched namespace and the state of its sources.
type NamespaceState struct {
	Name    string        `json:"name" doc:"Namespace name."`
	Sources []SourceState `json:"sources" doc:"State of the namespace-wide informers."`
}

// ClusterRow is one row of the cluster list: enough to triage without
// opening the cluster.
type ClusterRow struct {
	Ref    ClusterRef        `json:"ref"`
	Labels map[string]string `json:"labels,omitempty" doc:"Cluster labels, for filtering." untrusted:"true"`
	// Verdict is the roll-up; on a row it carries only its worst
	// contributors (at most three), the overview carries all of them.
	Verdict   Verdict        `json:"verdict"`
	Phase     Phase          `json:"phase"`
	Primary   PrimarySummary `json:"primary"`
	Instances InstanceCounts `json:"instances"`
	Backup    BackupRecency  `json:"backup"`
	Findings  FindingCounts  `json:"findings"`
	Version   VersionInfo    `json:"version"`
	// Provenance is the Cluster object's own: when the informer last
	// delivered it and whether that informer is stale.
	Provenance Provenance `json:"provenance"`

	// encoded is the row's own JSON, attached by WithJSON; nil means
	// the list writer encodes the row itself. Unexported: neither
	// encoding/json nor the OpenAPI schema sees it.
	encoded []byte
}

// Verdict is the real health roll-up: the worst of the phase
// class, the Ready condition, pod readiness against spec.instances,
// stale sources, diagnostics, backup recency and WAL archiving — never a
// string match on the phase.
type Verdict struct {
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info" doc:"Worst health among the contributors."`
	// Stale is set when any contributor rests on a stale source: the
	// headline is then shown desaturated with its age.
	Stale bool `json:"stale,omitempty" doc:"At least one contributor rests on a stale source."`
	// Contributors are ordered worst first. Every one of them is listed
	// on the overview, including the ok ones, so the reader sees what was
	// looked at.
	Contributors []Contributor `json:"contributors" doc:"What the verdict was computed from, worst first."`
}

// Contributor is one item of the roll-up: what was judged, what it reads
// and where its detail lives.
type Contributor struct {
	// ID is a stable key, such as "phase", "readyCondition", "instances",
	// "archiving", "backupRecency", "diagnostics", "source:pods".
	ID     string `json:"id" doc:"Stable key of the contributor."`
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Label names what was judged, in English.
	Label string `json:"label" doc:"What was judged."`
	// Detail is what it reads, in English, such as "2 of 3 ready".
	Detail     string     `json:"detail,omitempty" doc:"What it reads."`
	Provenance Provenance `json:"provenance"`
	// Link is the client route that explains it, such as "instances" or
	// "diagnostics"; relative to the cluster.
	Link string `json:"link,omitempty" doc:"Cluster-relative route with the detail."`
}

// Phase is the operator's phase, verbatim, with its class from the
// per-minor table. A phase the table does not know is
// warning with the text verbatim, never ok or critical.
type Phase struct {
	Text   string `json:"text,omitempty" doc:"The operator's phase, verbatim; absent when unreported." untrusted:"true"`
	Reason string `json:"reason,omitempty" doc:"The operator's phaseReason, verbatim." untrusted:"true"`
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info" doc:"Class of the phase from the per-minor table."`
	// Known is false for a phase absent from the table of the observed
	// CNPG minor.
	Known bool `json:"known" doc:"The phase is in the table of the observed CNPG minor."`
	// Since is the console's own clock: when it first saw this phase.
	// A floor, since the console may have started after the phase began.
	Since      *time.Time `json:"since,omitempty" doc:"When the console first saw this phase (a lower bound)."`
	Provenance Provenance `json:"provenance"`
}

// PrimaryState is whether the primary is settled or moving.
type PrimaryState string

// The primary states.
const (
	// PrimaryStable means currentPrimary equals targetPrimary.
	PrimaryStable PrimaryState = "stable"
	// PrimarySwitchover means the operator is moving the primary to a
	// named target.
	PrimarySwitchover PrimaryState = "switchover"
	// PrimaryFailoverPending means targetPrimary is "pending": the
	// failover delay is running and no candidate is chosen yet.
	PrimaryFailoverPending PrimaryState = "failoverPending"
	// PrimaryFailover means the operator reports failing over.
	PrimaryFailover PrimaryState = "failover"
	// PrimaryNone means no primary is named.
	PrimaryNone PrimaryState = "none"
	// PrimaryUnknown means the Cluster status was not readable.
	PrimaryUnknown PrimaryState = "unknown"
)

// PrimarySummary is the compact primary fact of a row.
type PrimarySummary struct {
	Current string       `json:"current,omitempty" doc:"status.currentPrimary."`
	Target  string       `json:"target,omitempty" doc:"status.targetPrimary."`
	State   PrimaryState `json:"state" enum:"stable,switchover,failoverPending,failover,none,unknown"`
	// Since is when the move started, from targetPrimaryTimestamp or
	// currentPrimaryFailingSinceTimestamp.
	Since *time.Time `json:"since,omitempty" doc:"When the move started, per the operator."`
}

// InstanceCounts compares what is declared, what the operator counts and
// what Kubernetes reports ready.
type InstanceCounts struct {
	Desired  *int `json:"desired,omitempty" doc:"spec.instances."`
	Ready    *int `json:"ready,omitempty" doc:"status.readyInstances (operator claim)."`
	Reported *int `json:"reported,omitempty" doc:"status.instances: PVC groups the operator knows of."`
	// PodsReady is the count of member pods whose Ready condition is
	// True, from the Pod informer; absent when pods are not observable.
	PodsReady *int   `json:"podsReady,omitempty" doc:"Member pods Ready per Kubernetes."`
	Health    Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
}

// BackupRecency is the compact backup fact of a row.
type BackupRecency struct {
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// LastCompletedAt is the newest completed Backup's stop time.
	LastCompletedAt *time.Time `json:"lastCompletedAt,omitempty" doc:"Newest completed Backup."`
	// LastFailedAt is the newest failed Backup, when newer than the last
	// completed one.
	LastFailedAt *time.Time `json:"lastFailedAt,omitempty" doc:"Newest failed Backup newer than the last completed one."`
	// Configured is false when the Cluster has no backup configuration
	// and no schedule: "no backup configured" rather than "no backup".
	Configured bool `json:"configured" doc:"The Cluster declares a backup method or schedule."`
	// PluginBased reports CNPG-I plugin archiving (barman-cloud plugin):
	// the deprecated in-tree recoverability fields are then never set
	// and are rendered "not reported by plugin-based backups".
	PluginBased bool `json:"pluginBased,omitempty" doc:"Backups go through a CNPG-I plugin."`
	// Cadence is the age past which the last completed backup is
	// overdue: twice the ScheduledBackup period when one is known, a
	// conservative default otherwise. Absent when no backup is
	// configured or Backups were not read.
	Cadence    *BackupCadence `json:"cadence,omitempty"`
	Provenance Provenance     `json:"provenance"`
}

// BackupCadence is the age threshold of the last completed backup.
type BackupCadence struct {
	// Basis is "schedule" when the threshold comes from a
	// ScheduledBackup's lastScheduleTime/nextScheduleTime period, or
	// "default" when no cadence is known and the console applies its
	// conservative default.
	Basis string `json:"basis" enum:"schedule,default"`
	// IntervalSeconds is the shortest observed schedule period; zero
	// on the default basis.
	IntervalSeconds int64 `json:"intervalSeconds,omitempty"`
	// ThresholdSeconds is the age after which the last completed backup
	// is overdue.
	ThresholdSeconds int64 `json:"thresholdSeconds"`
	// Overdue: the last completed backup is older than the threshold,
	// or none completed and the Cluster is older than it.
	Overdue bool `json:"overdue,omitempty"`
}

// FindingCounts summarises the latest diagnostics run.
type FindingCounts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Warning  int `json:"warning"`
	Note     int `json:"note"`
	// Unavailable counts checks that could not run: a non-zero value
	// keeps an empty list from reading as "nothing wrong".
	Unavailable int `json:"unavailable" doc:"Checks that could not run."`
	// RunAt is when the counted run happened; absent before the first run.
	RunAt *time.Time `json:"runAt,omitempty"`
}

// VersionConfidence says how a CNPG version was established.
type VersionConfidence string

// The confidence levels of the detected CNPG version, strongest first.
const (
	// VersionReported comes from the instance manager itself.
	VersionReported VersionConfidence = "reported"
	// VersionImage comes from an image tag or the operator annotation.
	VersionImage VersionConfidence = "image"
	// VersionInferred comes from feature probing: a range, not a number.
	VersionInferred VersionConfidence = "inferred"
	// VersionUnknown means no source answered.
	VersionUnknown VersionConfidence = "unknown"
)

// VersionInfo is what the console knows of the versions in play.
type VersionInfo struct {
	// CNPG is the operator version as best established, such as "1.30.1",
	// or a floor such as ">=1.30" when inferred.
	CNPG string `json:"cnpg,omitempty" doc:"CloudNativePG version, or a floor when inferred."`
	// CNPGMinor is the minor whose phase and condition tables applied,
	// such as "1.30"; absent when none matched.
	CNPGMinor  string            `json:"cnpgMinor,omitempty" doc:"The CNPG minor whose tables were applied."`
	Confidence VersionConfidence `json:"confidence" enum:"reported,image,inferred,unknown"`
	// DetectedBy names the method, such as "instanceManagerVersion",
	// "bootstrapControllerImage", "featureProbe".
	DetectedBy    string `json:"detectedBy,omitempty" doc:"Which source established the version."`
	PostgresMajor *int   `json:"postgresMajor,omitempty" doc:"Reported PostgreSQL major version."`
	Image         string `json:"image,omitempty" doc:"status.image."`
	// Components are the versions in play, each placed in the lifecycle
	// table, in reading order: Kubernetes, CloudNativePG, the plugin,
	// PostgreSQL. A component the console has not observed is absent.
	Components []ComponentVersion `json:"components,omitempty" doc:"The versions in play and where each stands in the lifecycle table."`
}

// Support says where a version stands in the lifecycle table.
type Support string

const (
	// SupportSupported is listed, and its end of life is more than six months away or not announced.
	SupportSupported Support = "supported"
	// SupportEndingSoon is listed, and its end of life is within six months.
	SupportEndingSoon Support = "endingSoon"
	// SupportEnded is past its end of life (or older than every listed line).
	SupportEnded Support = "ended"
	// SupportUnlisted is a line the table says nothing about.
	SupportUnlisted Support = "unlisted"
)

// ComponentVersion is one version in play.
type ComponentVersion struct {
	Component string  `json:"component" doc:"Kubernetes, CloudNativePG, Barman Cloud plugin or PostgreSQL."`
	Version   string  `json:"version"`
	Source    string  `json:"source" doc:"Where the version was read, such as the sidecar image of a pod."`
	Support   Support `json:"support" enum:"supported,endingSoon,ended,unlisted"`
	EndsOn    string  `json:"endsOn,omitempty" doc:"The day the line leaves upstream support (YYYY-MM-DD), when the table gives one."`
}
