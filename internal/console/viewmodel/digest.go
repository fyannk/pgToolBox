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

// DigestList is the /digests screen: the console's daily
// summaries, newest first, each reduced to the Clusters the caller may
// get.
type DigestList struct {
	GeneratedAt time.Time       `json:"generatedAt"`
	Source      SourceState     `json:"source" doc:"The digest: live, or disabled by configuration."`
	Schedule    DigestSchedule  `json:"schedule"`
	Items       []DigestSummary `json:"items"`
}

// DigestSchedule is when digests are made and how long they are kept.
type DigestSchedule struct {
	// Hour is the UTC hour a digest covers up to (DIGEST_HOUR).
	Hour int `json:"hour" minimum:"0" maximum:"23" doc:"UTC hour at which each digest closes its 24 hours."`
	// Retention is how long digests are kept (DIGEST_RETENTION).
	Retention string `json:"retention"`
	// Next is the next digest's time.
	Next *time.Time `json:"next,omitempty"`
	// Persistent reports that digests survive restarts (with history).
	Persistent bool `json:"persistent"`
}

// DigestSummary is one digest's headline.
type DigestSummary struct {
	// ID is the digest's stable handle (its end time, such as
	// 20261103T0700Z) and the last segment of its shareable link.
	ID          string       `json:"id"`
	From        time.Time    `json:"from"`
	To          time.Time    `json:"to"`
	GeneratedAt time.Time    `json:"generatedAt"`
	Counts      DigestCounts `json:"counts"`
	// Attention are the Clusters whose verdict needed attention when the
	// digest was made.
	Attention []ClusterRef `json:"attention"`
	// Partial reports that the console did not observe the whole window;
	// PartialReasons says why. A partial digest is never an all-clear.
	Partial bool `json:"partial,omitempty"`
	// PartialReasons: notObserved (the console's memory begins after
	// From), consoleRestarted (the console was down during the window),
	// sourceGap (a source the timeline rests on was not live during it),
	// clustersUnreadable (a namespace's Clusters cannot be read),
	// noTimeline.
	PartialReasons []string `json:"partialReasons,omitempty" enum:"notObserved,consoleRestarted,sourceGap,clustersUnreadable,noTimeline"`
}

// DigestCounts are the headline numbers.
type DigestCounts struct {
	Clusters        int `json:"clusters"`
	Attention       int `json:"attention"`
	Revisions       int `json:"revisions" doc:"Spec changes of the Clusters themselves; their other objects' revisions are on each Cluster's history."`
	NewFindings     int `json:"newFindings"`
	WorseFindings   int `json:"worseFindings"`
	ClearedFindings int `json:"clearedFindings"`
	Failovers       int `json:"failovers"`
	Switchovers     int `json:"switchovers"`
	FailedBackups   int `json:"failedBackups"`
	// Restarts counts container restarts the kubelet reported; rollouts
	// and in-place PostgreSQL restarts are not counted.
	Restarts int `json:"restarts" doc:"Container restarts reported by the kubelet; rollouts and in-place restarts are not counted."`
}

// Digest is one daily summary.
type Digest struct {
	DigestSummary
	// ObservedSince is the oldest entry the console's memory held when it
	// made the digest; after From, the window was not fully observed.
	ObservedSince *time.Time `json:"observedSince,omitempty"`
	// ConsoleGaps are the console's own restarts and source gaps inside
	// the window.
	ConsoleGaps []TimelineEntry `json:"consoleGaps"`
	Clusters    []DigestCluster `json:"clusters"`
	// Hidden counts the Clusters of the digest left out because you may
	// not get them (or the API server could not be asked); their names
	// are not disclosed.
	Hidden int `json:"hidden,omitempty"`
}

// DigestCluster is one Cluster's last 24 hours.
type DigestCluster struct {
	Ref ClusterRef `json:"ref"`
	// Health is the verdict when the digest was made.
	Health    Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Attention bool   `json:"attention"`
	// Reasons are the labels of the worst verdict contributors.
	Reasons []string     `json:"reasons"`
	Counts  DigestCounts `json:"counts"`
	// Entries are what happened, newest first: spec changes, findings
	// that appeared, worsened or cleared, primary changes, failed backups
	// and container restarts. Listed for the Clusters that need attention
	// only (at most 50 each, 1000 per digest); the others carry counts.
	Entries   []TimelineEntry `json:"entries"`
	Truncated bool            `json:"truncated,omitempty"`
}

// DigestStatus is the digest's account on the admin page: the schedule
// and every delivery channel's last outcome.
type DigestStatus struct {
	Enabled  bool           `json:"enabled"`
	Schedule DigestSchedule `json:"schedule"`
	// Last is the newest digest's ID.
	Last     string          `json:"last,omitempty"`
	Channels []DeliveryState `json:"channels"`
}

// DeliveryState is one delivery channel.
type DeliveryState struct {
	Channel string `json:"channel" enum:"chat,webhook,mail"`
	// Configured is false for a channel that is off (the default).
	Configured  bool       `json:"configured"`
	DigestID    string     `json:"digestId,omitempty" doc:"The digest of the last attempt."`
	LastAttempt *time.Time `json:"lastAttempt,omitempty"`
	LastSuccess *time.Time `json:"lastSuccess,omitempty"`
	// Attempts is the number of attempts for the last digest.
	Attempts int `json:"attempts,omitempty"`
	// Pending reports retries still scheduled.
	Pending bool `json:"pending,omitempty"`
	// LastError is bounded and secret-free (never the URL or a password).
	LastError string `json:"lastError,omitempty"`
}

// Recount recomputes the headline from the Clusters the digest holds:
// after generation, and after the API removed the Clusters a caller may
// not get.
func (d *Digest) Recount() {
	d.Counts = DigestCounts{Clusters: len(d.Clusters)}
	d.Attention = []ClusterRef{}
	for _, c := range d.Clusters {
		if c.Attention {
			d.Counts.Attention++
			d.Attention = append(d.Attention, c.Ref)
		}
		d.Counts.Revisions += c.Counts.Revisions
		d.Counts.NewFindings += c.Counts.NewFindings
		d.Counts.WorseFindings += c.Counts.WorseFindings
		d.Counts.ClearedFindings += c.Counts.ClearedFindings
		d.Counts.Failovers += c.Counts.Failovers
		d.Counts.Switchovers += c.Counts.Switchovers
		d.Counts.FailedBackups += c.Counts.FailedBackups
		d.Counts.Restarts += c.Counts.Restarts
	}
}
