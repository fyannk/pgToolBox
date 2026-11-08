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

package fixtures

import (
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// DigestID is the newer fixture digest's ID (its end time).
const DigestID = "20261029T0700Z"

func observed(d time.Duration) time.Time { return Now.Add(-d) }

// TimelineEntries are billing's last 24 hours, newest first: a failover
// with the findings it raised, a restart, a failed backup, a revision,
// an Event, and the console's own restart.
func TimelineEntries() []viewmodel.TimelineEntry {
	pod := func(name string) *viewmodel.ObjectRef { return &viewmodel.ObjectRef{Kind: "Pod", Name: name} }
	return []viewmodel.TimelineEntry{
		{
			ID: "j:118", Kind: "finding", Action: "appeared", At: observed(15 * time.Second), Timing: "observed", Scope: "cluster",
			Severity: viewmodel.SeverityCritical, Health: viewmodel.HealthCritical, Ref: "primary.unavailable", Detail: "No ready primary for 90 s",
			Provenance: consoleProv("diagnostics"), Link: "diagnostics",
		},
		{
			ID: "j:117", Kind: "phase", Action: "changed", At: observed(90 * time.Second), Timing: "observed", Scope: "cluster",
			From: "Cluster in healthy state", To: "Failing over", Health: viewmodel.HealthCritical, Detail: "Failing over from billing-1 to billing-2",
			Provenance: operatorProv("Cluster.status.phase"), Link: "",
		},
		{
			ID: "j:116", Kind: "primary", Action: "failover", At: observed(95 * time.Second), Timing: "reported", Scope: "cluster",
			Subject: pod("billing-2"), From: "billing-1", To: "billing-2", Health: viewmodel.HealthCritical,
			Provenance: operatorProv("Cluster.status.targetPrimaryTimestamp"), Link: "replication",
		},
		{
			ID: "e:5b1c", Kind: "event", Action: "Warning", At: observed(100 * time.Second), Timing: "reported", Scope: "cluster",
			Subject: pod("billing-1"), Health: viewmodel.HealthWarning, Ref: "Unhealthy", Detail: "Readiness probe failed: HTTP probe failed with statuscode: 500", Count: 4,
			Provenance: kubeProv("Event"), Link: "events",
		},
		{
			ID: "j:115", Kind: "restart", Action: "restarted", At: observed(2 * time.Hour), Timing: "reported", Scope: "cluster",
			Subject: pod("billing-1"), Health: viewmodel.HealthWarning, Detail: "OOMKilled (exit 137)",
			Provenance: kubeProv("Pod.status.containerStatuses"), Link: "instances/billing-1",
		},
		{
			ID: "j:114", Kind: "backup", Action: "failed", At: observed(5 * time.Hour), Timing: "reported", Scope: "cluster",
			Subject: &viewmodel.ObjectRef{Kind: "Backup", Name: "billing-daily-20261029"}, Health: viewmodel.HealthCritical,
			Detail: "can't upload: AccessDenied", Provenance: operatorProv("Backup.status"), Link: "backups",
		},
		{
			ID: "r:41", Kind: "revision", Action: "spec-changed", At: observed(9 * time.Hour), Timing: "observed", Scope: "cluster",
			Subject: &viewmodel.ObjectRef{Kind: "Cluster", Name: "billing"}, Health: viewmodel.HealthInfo, Ref: "41", Detail: "1 parameter changed",
			Provenance: consoleProv("history"), Link: "history/41",
		},
		{
			ID: "j:112", Kind: "console", Action: "started", At: observed(11 * time.Hour), Timing: "observed", Scope: "console",
			Since: new(observed(11*time.Hour + 4*time.Minute)), Health: viewmodel.HealthInfo,
			Provenance: consoleProv("console"),
		},
		{
			ID: "j:113", Kind: "finding", Action: "cleared", At: observed(11 * time.Hour), Timing: "observed", Scope: "cluster", AfterGap: true,
			Since: new(observed(11*time.Hour + 4*time.Minute)), Severity: viewmodel.SeverityWarning, Health: viewmodel.HealthOK,
			Ref: "wal.archiveLag", Detail: "WAL archiving behind by 12 segments", Provenance: consoleProv("diagnostics"), Link: "diagnostics",
		},
	}
}

// Timeline is billing's incident timeline over the last 24 hours.
func Timeline() viewmodel.Timeline {
	items := TimelineEntries()
	counts := map[string]int{}
	for _, e := range items {
		counts[e.Kind]++
	}
	out := viewmodel.Timeline{
		Ref: billingRef, GeneratedAt: Now, From: Now.Add(-24 * time.Hour), To: Now, Kinds: []string{},
		Source: viewmodel.SourceState{Source: "timeline", State: viewmodel.SourceLive}, Persistent: true,
		ObservedSince: new(Now.Add(-26 * time.Hour)), EventsWindow: "1h0m0s", Counts: []viewmodel.TimelineCount{}, Items: items,
	}
	for _, k := range viewmodel.TimelineKinds {
		if counts[k] > 0 {
			out.Counts = append(out.Counts, viewmodel.TimelineCount{Kind: k, Count: counts[k]})
		}
	}
	return out
}

// RevisionPreview is what Revision sets in motion: a scale-up and a
// postmaster parameter, so the replica Pods are recreated and the
// primary handed over by a switchover.
func RevisionPreview() *viewmodel.ChangePreview {
	return &viewmodel.ChangePreview{
		Effect: "restart", Strategy: "unsupervised", Method: "switchover", Primary: "switchover",
		Parameters: []viewmodel.PreviewParameter{
			{Name: "max_connections", Before: "200", After: "300", Effect: "restart", Context: "postmaster"},
			{Name: "work_mem", Before: "4MB", After: "16MB", Effect: "reload", Context: "reload"},
		},
		Fields: []viewmodel.PreviewField{{Path: ".spec.instances", Effect: "none", Reason: "scale"}},
		Steps: []viewmodel.PreviewStep{
			{Role: "replicas", Instances: []string{"orders-3", "orders-2"}, Action: "recreate"},
			{Role: "primary", Instances: []string{"orders-1"}, Action: "switchover"},
		},
		PostgresMajor: 17, Verified: true, CNPGMinor: "1.30",
	}
}

// Digests are two daily digests, newest first: billing needs attention
// in the newer one.
func Digests() []viewmodel.Digest {
	to := time.Date(2026, 10, 29, 7, 0, 0, 0, time.UTC)
	billing := viewmodel.DigestCluster{
		Ref: billingRef, Health: viewmodel.HealthCritical, Attention: true, Reasons: []string{"Phase", "Pods"},
		Counts: viewmodel.DigestCounts{Revisions: 1, NewFindings: 1, ClearedFindings: 1, Failovers: 1, FailedBackups: 1, Restarts: 1},
	}
	// The timeline's entries end at Now; moved back so they fall inside
	// the digest's window, as DigestEntries would have selected them.
	shift := to.Sub(Now)
	for _, e := range TimelineEntries() {
		if e.Kind != "event" && e.Kind != "phase" && e.Kind != "console" {
			e.At = e.At.Add(shift)
			if e.Since != nil {
				e.Since = new(e.Since.Add(shift))
			}
			billing.Entries = append(billing.Entries, e)
		}
	}
	// orders needs no attention: its counts, no entries.
	orders := viewmodel.DigestCluster{
		Ref: ordersRef, Health: viewmodel.HealthOK, Reasons: []string{},
		Counts: viewmodel.DigestCounts{Revisions: 1}, Entries: []viewmodel.TimelineEntry{},
	}
	newer := viewmodel.Digest{
		DigestSummary: viewmodel.DigestSummary{ID: DigestID, From: to.Add(-24 * time.Hour), To: to, GeneratedAt: to.Add(2 * time.Second)},
		ObservedSince: new(to.Add(-26 * time.Hour)),
		ConsoleGaps: []viewmodel.TimelineEntry{{
			ID: "j:112", Kind: "console", Action: "started", At: to.Add(-11 * time.Hour), Timing: "observed", Scope: "console",
			Since: new(to.Add(-11*time.Hour - 4*time.Minute)), Health: viewmodel.HealthInfo, Provenance: consoleProv("console"),
		}},
		Clusters: []viewmodel.DigestCluster{billing, orders},
	}
	newer.Partial, newer.PartialReasons = true, []string{"consoleRestarted"}
	newer.Recount()
	older := viewmodel.Digest{
		DigestSummary: viewmodel.DigestSummary{ID: "20261028T0700Z", From: to.Add(-48 * time.Hour), To: to.Add(-24 * time.Hour), GeneratedAt: to.Add(-24*time.Hour + 2*time.Second)},
		ConsoleGaps:   []viewmodel.TimelineEntry{},
		Clusters: []viewmodel.DigestCluster{
			{Ref: billingRef, Health: viewmodel.HealthOK, Reasons: []string{}, Entries: []viewmodel.TimelineEntry{}},
			{Ref: ordersRef, Health: viewmodel.HealthOK, Reasons: []string{}, Entries: []viewmodel.TimelineEntry{}},
		},
	}
	older.Recount()
	return []viewmodel.Digest{newer, older}
}

// DigestSchedule is the fixture console's schedule.
func DigestSchedule() viewmodel.DigestSchedule {
	return viewmodel.DigestSchedule{Hour: 7, Retention: "720h0m0s", Next: new(time.Date(2026, 10, 30, 7, 0, 0, 0, time.UTC)), Persistent: true}
}

// DigestStatus is the admin page's digest: chat delivered, mail failing
// and retrying, the generic webhook off.
func DigestStatus() *viewmodel.DigestStatus {
	at := time.Date(2026, 10, 29, 7, 0, 3, 0, time.UTC)
	return &viewmodel.DigestStatus{
		Enabled: true, Schedule: DigestSchedule(), Last: DigestID,
		Channels: []viewmodel.DeliveryState{
			{Channel: "chat", Configured: true, DigestID: DigestID, LastAttempt: new(at), LastSuccess: new(at), Attempts: 1},
			{Channel: "webhook"},
			{
				Channel: "mail", Configured: true, DigestID: DigestID, LastAttempt: new(at.Add(30 * time.Second)), Attempts: 2, Pending: true,
				LastError: "the mail server does not offer STARTTLS; the digest is never sent in clear",
			},
		},
	}
}
