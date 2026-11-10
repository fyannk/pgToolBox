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

// Package fixtures holds deterministic view-model scenarios: the golden
// JSON of the viewmodel package, the API's OpenAPI examples and the
// SPA's MSW fixtures are all dumped from these values, so the
// three cannot drift. Every scenario is anchored at Now.
package fixtures

import (
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Now is the fixed instant every scenario is built around.
var Now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) *time.Time {
	at := Now.Add(-d)
	return &at
}

func operatorProv(source string) viewmodel.Provenance {
	return viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: source, ObservedAt: ago(2 * time.Second)}
}

func instanceProv() viewmodel.Provenance {
	return viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", ObservedAt: ago(8 * time.Second)}
}

var liveSources = []viewmodel.SourceState{
	{Source: "clusters", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
	{Source: "pods", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
	{Source: "events", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
	{Source: "backups", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
	{Source: "instanceStatus", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
	{Source: "diagnostics", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
}

var replicationBlindSpot = viewmodel.BlindSpot{
	ID:    "replication.streamingReplicaOnly",
	Panel: "replication",
	Text:  "The instance manager lists only this cluster's streaming_replica walsenders: replica clusters, logical walsenders and external base backups are not shown.",
}

var pluginBackupBlindSpot = viewmodel.BlindSpot{
	ID:    "backups.pluginNotReported",
	Panel: "backups",
	Text:  "Backups go through a CNPG-I plugin: Cluster.status recoverability fields and cnpg_collector backup metrics are not reported; the ObjectStore recovery window is the source.",
}

// HealthyRow is a three-instance cluster with everything confirmed.
func HealthyRow() viewmodel.ClusterRow {
	return viewmodel.ClusterRow{
		Ref:    viewmodel.ClusterRef{Namespace: "team-a", Name: "orders", UID: "0b6a2c1e-8f0d-4a55-9a3e-7c1f2b3d4e5f"},
		Labels: map[string]string{"app": "orders"},
		Verdict: viewmodel.Verdict{Health: viewmodel.HealthOK, Contributors: []viewmodel.Contributor{
			{ID: "phase", Health: viewmodel.HealthOK, Label: "Phase", Detail: "Cluster in healthy state", Provenance: operatorProv("Cluster.status.phase"), Link: ""},
		}},
		Phase:      viewmodel.Phase{Text: "Cluster in healthy state", Health: viewmodel.HealthOK, Known: true, Since: ago(26 * time.Hour), Provenance: operatorProv("Cluster.status.phase")},
		Primary:    viewmodel.PrimarySummary{Current: "orders-1", Target: "orders-1", State: viewmodel.PrimaryStable},
		Instances:  viewmodel.InstanceCounts{Desired: new(3), Ready: new(3), Reported: new(3), PodsReady: new(3), Health: viewmodel.HealthOK},
		Backup:     viewmodel.BackupRecency{Health: viewmodel.HealthOK, LastCompletedAt: ago(5 * time.Hour), Configured: true, PluginBased: true, Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "Backup.status", ObservedAt: ago(2 * time.Second)}},
		Findings:   viewmodel.FindingCounts{RunAt: ago(20 * time.Second)},
		Version:    viewmodel.VersionInfo{CNPG: "1.30.1", CNPGMinor: "1.30", Confidence: viewmodel.VersionReported, DetectedBy: "instanceManagerVersion", PostgresMajor: new(18), Image: "ghcr.io/cloudnative-pg/postgresql:18.1-system-trixie"},
		Provenance: operatorProv("Cluster"),
	}
}

// FailingOverRow is a cluster whose primary is being replaced, read
// through a stale pod informer.
func FailingOverRow() viewmodel.ClusterRow {
	stale := viewmodel.Provenance{Origin: viewmodel.OriginKubernetes, Source: "Pod", ObservedAt: ago(4 * time.Minute), Stale: true, StaleReason: "pod watch lost: connection refused"}
	return viewmodel.ClusterRow{
		Ref: viewmodel.ClusterRef{Namespace: "team-a", Name: "billing", UID: "4d2f1e0a-1b2c-4d3e-8f90-a1b2c3d4e5f6"},
		Verdict: viewmodel.Verdict{Health: viewmodel.HealthCritical, Stale: true, Contributors: []viewmodel.Contributor{
			{ID: "phase", Health: viewmodel.HealthCritical, Label: "Phase", Detail: "Failing over", Provenance: operatorProv("Cluster.status.phase")},
			{ID: "source:pods", Health: viewmodel.HealthUnknown, Label: "Pods", Detail: "stale for 4m", Provenance: stale, Link: "instances"},
			{ID: "instances", Health: viewmodel.HealthWarning, Label: "Instances ready", Detail: "1 of 2", Provenance: operatorProv("Cluster.status.readyInstances"), Link: "instances"},
		}},
		Phase:      viewmodel.Phase{Text: "Failing over", Reason: "Failing over from billing-1 to billing-2", Health: viewmodel.HealthCritical, Known: true, Since: ago(90 * time.Second), Provenance: operatorProv("Cluster.status.phase")},
		Primary:    viewmodel.PrimarySummary{Current: "billing-1", Target: "billing-2", State: viewmodel.PrimaryFailover, Since: ago(95 * time.Second)},
		Instances:  viewmodel.InstanceCounts{Desired: new(2), Ready: new(1), Reported: new(2), Health: viewmodel.HealthWarning},
		Backup:     viewmodel.BackupRecency{Health: viewmodel.HealthUnknown, Configured: true, Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "Backup.status", ObservedAt: ago(2 * time.Second)}},
		Findings:   viewmodel.FindingCounts{Critical: 1, Warning: 1, Unavailable: 2, RunAt: ago(15 * time.Second)},
		Version:    viewmodel.VersionInfo{CNPG: ">=1.30", CNPGMinor: "1.30", Confidence: viewmodel.VersionInferred, DetectedBy: "featureProbe"},
		Provenance: operatorProv("Cluster"),
	}
}

// ClusterList is two rows in one namespace and a namespace the console
// may not list clusters in.
func ClusterList() viewmodel.ClusterList {
	return viewmodel.ClusterList{
		GeneratedAt: Now,
		Namespaces: []viewmodel.NamespaceState{
			{Name: "team-a", Sources: []viewmodel.SourceState{{Source: "clusters", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)}}},
			{Name: "team-a-staging", Sources: []viewmodel.SourceState{{Source: "clusters", State: viewmodel.SourceForbidden, Since: ago(3 * time.Hour), Reason: "the console ServiceAccount may not list clusters.postgresql.cnpg.io in team-a-staging"}}},
		},
		Items: []viewmodel.ClusterRow{FailingOverRow(), HealthyRow()},
	}
}

// HealthyOverview is the overview of HealthyRow.
func HealthyOverview() viewmodel.ClusterOverview {
	row := HealthyRow()
	row.Verdict.Contributors = append(row.Verdict.Contributors,
		viewmodel.Contributor{ID: "readyCondition", Health: viewmodel.HealthOK, Label: "Ready condition", Detail: "True", Provenance: operatorProv("Cluster.status.conditions[Ready]")},
		viewmodel.Contributor{ID: "instances", Health: viewmodel.HealthOK, Label: "Instances ready", Detail: "3 of 3", Provenance: operatorProv("Cluster.status.readyInstances"), Link: "instances"},
		viewmodel.Contributor{ID: "archiving", Health: viewmodel.HealthOK, Label: "WAL archiving", Detail: "ContinuousArchiving True; primary archiving", Provenance: instanceProv(), Link: "backups"},
		viewmodel.Contributor{ID: "backupRecency", Health: viewmodel.HealthOK, Label: "Last backup", Detail: "completed 5h ago", Provenance: operatorProv("Backup.status"), Link: "backups"},
		viewmodel.Contributor{ID: "diagnostics", Health: viewmodel.HealthOK, Label: "Diagnostics", Detail: "no finding; 0 checks could not run", Provenance: viewmodel.Provenance{Origin: viewmodel.OriginConsole, Source: "diagnostics", ObservedAt: ago(20 * time.Second)}, Link: "diagnostics"},
	)
	return viewmodel.ClusterOverview{
		Ref:         row.Ref,
		Labels:      row.Labels,
		GeneratedAt: Now,
		Present:     true,
		Verdict:     row.Verdict,
		Phase:       row.Phase,
		Primary: viewmodel.PrimaryElection{
			PrimarySummary: row.Primary,
			LivePrimaries:  []string{"orders-1"},
			LabelPrimaries: []string{"orders-1"},
			Lease:          &viewmodel.Lease{Holder: "orders-1", RenewedAt: ago(3 * time.Second), Provenance: viewmodel.Provenance{Origin: viewmodel.OriginKubernetes, Source: "Lease/orders", ObservedAt: ago(3 * time.Second)}},
			Provenance:     operatorProv("Cluster.status.currentPrimary"),
		},
		Instances: row.Instances,
		Cards: []viewmodel.InstanceCard{
			{Name: "orders-1", Role: viewmodel.RolePrimary, Health: viewmodel.HealthOK, Ready: new(true), StatusRead: viewmodel.StatusReadOK},
			{Name: "orders-2", Role: viewmodel.RoleReplica, Health: viewmodel.HealthOK, Ready: new(true), StatusRead: viewmodel.StatusReadOK, Detail: "lag 0 B"},
			{Name: "orders-3", Role: viewmodel.RoleReplica, Health: viewmodel.HealthOK, Ready: new(true), StatusRead: viewmodel.StatusReadOK, Detail: "lag 16 kB"},
		},
		Conditions: []viewmodel.Condition{
			{Type: "Ready", Status: "True", Reason: "ClusterIsReady", Message: "Cluster is Ready", LastTransition: ago(26 * time.Hour), Health: viewmodel.HealthOK, Known: true},
			{Type: "ContinuousArchiving", Status: "True", Reason: "ContinuousArchivingSuccess", Message: "Continuous archiving is working", LastTransition: ago(26 * time.Hour), Health: viewmodel.HealthOK, Known: true},
			{Type: "LastBackupSucceeded", Status: "True", Reason: "LastBackupSucceeded", Message: "Backup was successful", LastTransition: ago(5 * time.Hour), Health: viewmodel.HealthOK, Known: true},
			{Type: "example.com/Custom", Status: "True", Reason: "Whatever", Health: viewmodel.HealthInfo, Known: false},
		},
		RecentWarnings: []viewmodel.EventRow{},
		EscapeHatches:  []viewmodel.EscapeHatch{},
		Fencing:        viewmodel.Fencing{State: viewmodel.FencingNone},
		Failover:       OrdersFailover(),
		Hibernation:    viewmodel.Hibernation{State: viewmodel.HibernationOff},
		Backups: viewmodel.BackupSummary{
			BackupRecency: row.Backup,
			Archiving: viewmodel.Archiving{
				Health: viewmodel.HealthOK, ConditionStatus: "True", Reason: "ContinuousArchivingSuccess",
				LiveArchiving: new(true), ReadyWALFiles: new(0), Provenance: instanceProv(),
				LastArchivedWAL: "00000003000000000000002A", LastArchivedAt: ago(70 * time.Second),
			},
			RecoveryWindow:  &viewmodel.RecoveryWindow{First: ago(7 * 24 * time.Hour), Last: ago(40 * time.Second), Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "ObjectStore.status.serverRecoveryWindow[orders]", ObservedAt: ago(2 * time.Second)}},
			NextScheduledAt: new(Now.Add(19 * time.Hour)),
			NotReported:     []string{"firstRecoverabilityPoint", "lastSuccessfulBackup"},
		},
		Version:    row.Version,
		Findings:   row.Findings,
		Sources:    liveSources,
		BlindSpots: []viewmodel.BlindSpot{replicationBlindSpot, pluginBackupBlindSpot},
	}
}

// FailingOverOverview is the overview of FailingOverRow: the operator
// waits, one instance is not verifiable, a warning names the cause.
func FailingOverOverview() viewmodel.ClusterOverview {
	row := FailingOverRow()
	warning := viewmodel.EventRow{
		Type: "Warning", Reason: "FailingOver", Message: "Primary billing-1 is not healthy, failing over to billing-2",
		Object: viewmodel.ObjectRef{Kind: "Cluster", Name: "billing"}, Count: 1, FirstSeen: ago(95 * time.Second), LastSeen: Now.Add(-95 * time.Second),
		Reporter: "cloudnative-pg", Origin: viewmodel.OriginOperator, Health: viewmodel.HealthCritical,
	}
	return viewmodel.ClusterOverview{
		Ref: row.Ref, GeneratedAt: Now, Present: true, Verdict: row.Verdict, Phase: row.Phase,
		Primary: viewmodel.PrimaryElection{
			PrimarySummary: row.Primary,
			FailingSince:   ago(100 * time.Second),
			LivePrimaries:  []string{},
			LabelPrimaries: []string{"billing-1"},
			Provenance:     operatorProv("Cluster.status.targetPrimary"),
		},
		Instances: row.Instances,
		Cards: []viewmodel.InstanceCard{
			{Name: "billing-1", Role: viewmodel.RolePrimary, Health: viewmodel.HealthCritical, Ready: new(false), StatusRead: viewmodel.StatusReadUnreachable, Detail: "not reachable from this console"},
			{Name: "billing-2", Role: viewmodel.RoleReplica, Health: viewmodel.HealthUnknown, Ready: new(true), StatusRead: viewmodel.StatusReadNotVerifiable, Detail: "status port not verifiable: CA not in the console's bundle"},
		},
		Conditions: []viewmodel.Condition{
			{Type: "Ready", Status: "False", Reason: "ClusterIsNotReady", Message: "Cluster Is Not Ready", LastTransition: ago(95 * time.Second), Health: viewmodel.HealthCritical, Known: true},
		},
		Waiting: &viewmodel.OperatorWaiting{PhaseReason: row.Phase.Reason, Causes: []viewmodel.EventRow{warning}, Reasons: []viewmodel.WaitReason{
			{ID: "switchover-pending", Text: "The operator asked billing-2 to become primary and waits for it.", Evidence: []string{
				"Cluster.status.currentPrimary = billing-1", "Cluster.status.targetPrimary = billing-2",
			}},
			{ID: "crash-loop", Text: "These instance containers keep crashing; read the postgres container's logs.", Heuristic: true, Evidence: []string{
				"Pod/billing-1 container postgres waiting: CrashLoopBackOff",
			}},
		}},
		RecentWarnings: []viewmodel.EventRow{warning},
		EscapeHatches: []viewmodel.EscapeHatch{
			{Annotation: "cnpg.io/skipWalArchiving", Value: "enabled", Effect: "WAL archiving is skipped: the cluster is not recoverable from its archive", Health: viewmodel.HealthWarning},
		},
		Fencing:     viewmodel.Fencing{State: viewmodel.FencingNone},
		Failover:    BillingFailover(),
		Hibernation: viewmodel.Hibernation{State: viewmodel.HibernationOff},
		Backups: viewmodel.BackupSummary{
			BackupRecency: row.Backup,
			Archiving:     viewmodel.Archiving{Health: viewmodel.HealthUnknown, Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", Stale: true, StaleReason: "the primary's status port was not read"}},
		},
		Version:  row.Version,
		Findings: row.Findings,
		Sources: []viewmodel.SourceState{
			{Source: "clusters", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
			{Source: "pods", State: viewmodel.SourceStale, Since: ago(4 * time.Minute), Reason: "pod watch lost: connection refused"},
			{Source: "instanceStatus", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
			{Source: "failoverQuorums", State: viewmodel.SourceNotInstalled, Since: ago(3 * time.Hour), Reason: "failoverquorums.postgresql.cnpg.io is not served by this cluster"},
		},
		BlindSpots: []viewmodel.BlindSpot{replicationBlindSpot},
	}
}

// Instances is the instance list of FailingOverRow, with one row per
// error kind the sweep can produce.
func Instances() viewmodel.InstanceList {
	return viewmodel.InstanceList{
		Ref:         FailingOverRow().Ref,
		GeneratedAt: Now,
		Sweep:       viewmodel.SourceState{Source: "instanceStatus", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
		Items: []viewmodel.InstanceRow{
			{
				Name: "billing-1", Health: viewmodel.HealthCritical,
				Role:       viewmodel.Roles{Operator: viewmodel.RolePrimary, Label: viewmodel.RolePrimary, Live: viewmodel.RoleUnknown},
				Pod:        &viewmodel.PodView{Phase: "Running", Ready: false, Node: "node-a", Restarts: 4, Waiting: "CrashLoopBackOff", Image: "ghcr.io/cloudnative-pg/postgresql:18.1-system-trixie", CreatedAt: ago(30 * 24 * time.Hour), Provenance: viewmodel.Provenance{Origin: viewmodel.OriginKubernetes, Source: "Pod", ObservedAt: ago(4 * time.Minute), Stale: true, StaleReason: "pod watch lost: connection refused"}},
				StatusRead: viewmodel.StatusRead{State: viewmodel.StatusReadUnreachable, Reason: "not reachable from this console: dial tcp 10.0.0.11:8000: i/o timeout", At: ago(8 * time.Second), Since: ago(100 * time.Second)},
				Live: &viewmodel.LiveStatus{
					IsPrimary: true, TimelineID: new(3), CurrentLSN: "0/9000060", ArchivingWAL: true, ManagerVersion: "1.30.1",
					Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", ObservedAt: ago(100 * time.Second), Stale: true, StaleReason: "last read failed"},
				},
				Series: []viewmodel.Series{{Key: "readyWalFiles", Unit: "files", Points: []viewmodel.Point{
					{At: Now.Add(-130 * time.Second), Value: new(0.0)},
					{At: Now.Add(-100 * time.Second), Value: new(2.0)},
					{At: Now.Add(-70 * time.Second), Value: nil},
				}}},
			},
			{
				Name: "billing-2", Health: viewmodel.HealthUnknown,
				Role:       viewmodel.Roles{Operator: viewmodel.RoleReplica, Label: viewmodel.RoleReplica, Live: viewmodel.RoleUnknown},
				Pod:        &viewmodel.PodView{Phase: "Running", Ready: true, Node: "node-b", Provenance: viewmodel.Provenance{Origin: viewmodel.OriginKubernetes, Source: "Pod", ObservedAt: ago(4 * time.Minute), Stale: true, StaleReason: "pod watch lost: connection refused"}},
				StatusRead: viewmodel.StatusRead{State: viewmodel.StatusReadNotVerifiable, Reason: "status port not verifiable: CA not in the console's bundle", At: ago(8 * time.Second), Since: ago(3 * time.Hour)},
			},
			{
				Name: "billing-3", Health: viewmodel.HealthWarning,
				Role:       viewmodel.Roles{Operator: viewmodel.RoleReplica, Label: viewmodel.RoleReplica, Live: viewmodel.RoleUnknown},
				Pod:        &viewmodel.PodView{Phase: "Running", Ready: true, Node: "node-c", Provenance: viewmodel.Provenance{Origin: viewmodel.OriginKubernetes, Source: "Pod", ObservedAt: ago(4 * time.Minute)}},
				StatusRead: viewmodel.StatusRead{State: viewmodel.StatusReadNoTLS, Reason: "status port without TLS (Pod created before CNPG 1.24): restart the instance", At: ago(8 * time.Second), Since: ago(3 * time.Hour)},
			},
		},
		BlindSpots: []viewmodel.BlindSpot{
			{ID: "instances.isFencedNotReported", Panel: "instances", Text: "A direct /pg/status read never carries isFenced; fencing is read from the cnpg.io/fencedInstances annotation."},
		},
	}
}

// Events is the event list of FailingOverRow.
func Events() viewmodel.EventList {
	ov := FailingOverOverview()
	return viewmodel.EventList{
		Ref: ov.Ref, GeneratedAt: Now,
		Source: viewmodel.SourceState{Source: "events", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
		Window: "1h0m0s",
		Items: append(ov.RecentWarnings, viewmodel.EventRow{
			Type: "Normal", Reason: "Pulled", Message: "Container image already present on machine",
			Object: viewmodel.ObjectRef{Kind: "Pod", Name: "billing-1"}, Count: 5, LastSeen: Now.Add(-3 * time.Minute),
			Reporter: "kubelet", Origin: viewmodel.OriginKubernetes, Health: viewmodel.HealthInfo,
		}),
	}
}

// Diagnostics is a run with one incident, one standalone note (an overdue
// restore drill) and the two kinds of unavailable checks.
func Diagnostics() viewmodel.Diagnostics {
	root := viewmodel.Finding{
		ID: "pod-crashloop/billing-1", Check: "pod-crashloop", Subject: viewmodel.ObjectRef{Kind: "Pod", Name: "billing-1"},
		Severity: viewmodel.SeverityCritical, Summary: "Instance billing-1 is restarting in a loop.",
		At: ago(70 * time.Second), Since: ago(100 * time.Second),
		Evidence:  []viewmodel.Evidence{{Origin: "Kubernetes", Object: "Pod/billing-1", Detail: "CrashLoopBackOff: back-off 40s restarting failed container=postgres"}},
		NextSteps: "kubectl logs -n team-a billing-1 -c postgres --previous",
		Link:      "instances/billing-1", LinkLabel: "Instance billing-1",
	}
	consequence := viewmodel.Finding{
		ID: "phase-failing-over", Check: "cnpg-1.30/phase-failing-over", Subject: viewmodel.ObjectRef{Kind: "Cluster"},
		Severity: viewmodel.SeverityWarning, Summary: "The operator is failing over.", Since: ago(95 * time.Second),
		Evidence: []viewmodel.Evidence{{Origin: "operator", Object: "Cluster/billing", Detail: "phase: Failing over"}},
		Relation: "same cluster · established",
	}
	// A note stands on its own: no incident to attach to, lowest severity.
	drill := viewmodel.Finding{
		ID: "restore-drill-overdue", Check: "restore-drill-overdue", Subject: viewmodel.ObjectRef{Kind: "Cluster"},
		Severity: viewmodel.SeverityNote,
		Summary:  "No restore drill run from this console has passed for this Cluster in the last 90 days.",
		Detail: "A backup is proven restorable only by restoring it. A drill restores the latest backup into a throwaway Cluster, " +
			"checks it and deletes it; its duration is the measured recovery time. Drills run by hand or by another console are not in this console's record.",
		Since:     ago(9 * 24 * time.Hour),
		Evidence:  []viewmodel.Evidence{{Origin: "console-recorded", Object: "restore drill record", Detail: "no drill of this Cluster recorded in the last 90 days"}},
		NextSteps: "Run a restore drill from the Backups screen.",
		Link:      "backups", LinkLabel: "Backups",
	}
	return viewmodel.Diagnostics{
		Ref: FailingOverRow().Ref, RunAt: ago(15 * time.Second), InputGeneration: 42,
		Runner:      viewmodel.SourceState{Source: "diagnostics", State: viewmodel.SourceLive, Since: ago(3 * time.Hour)},
		CatalogPins: []string{"1.29", "1.30"}, Applied: "1.30",
		Counts: viewmodel.FindingCounts{Critical: 1, Warning: 1, Note: 1, Unavailable: 2, RunAt: ago(15 * time.Second)},
		Incidents: []viewmodel.Incident{
			{Root: root, Consequences: []viewmodel.Finding{consequence}, Severity: viewmodel.SeverityCritical},
			{Root: drill, Severity: viewmodel.SeverityNote},
		},
		Checks: []viewmodel.Check{
			{Name: "pod-crashloop", Layer: "Kubernetes", Describes: "containers restarting in a loop", Outcome: viewmodel.CheckMatched},
			{Name: "quota", Layer: "Kubernetes", Describes: "namespace quota refusals", Outcome: viewmodel.CheckClear},
			{Name: "log-wal-full", Layer: "PostgreSQL", Describes: "WAL volume full in the instance log", Outcome: viewmodel.CheckUnavailable, Because: "log following is switched off", SourceOff: true},
			{Name: "failover-quorum", Layer: "Replication", Describes: "failover quorum not reached", Outcome: viewmodel.CheckUnavailable, Because: "failoverquorums.postgresql.cnpg.io is not served by this cluster"},
			{Name: "cnpg-1.29/phase-x", Layer: "Operator", Describes: "a 1.29-only phase", Outcome: viewmodel.CheckNotApplicable},
			{Name: "restore-drill-overdue", Layer: "Backups and archive", Describes: "a Cluster this console can drill with no passed restore drill in its record for 90 days", Outcome: viewmodel.CheckMatched},
		},
	}
}

// Triage is one playbook walked against Diagnostics.
func Triage() viewmodel.Triage {
	return viewmodel.Triage{
		Ref: FailingOverRow().Ref, RunAt: ago(15 * time.Second),
		Playbooks: []viewmodel.Playbook{{
			ID: "primary-unavailable", Symptom: "Applications cannot write", Upstream: "https://cloudnative-pg.io/documentation/current/failover/",
			StartAt: 1,
			Steps: []viewmodel.TriageStep{
				{Question: "Does the cluster exist?", Outcome: viewmodel.TriageRuledOut, Explain: "Cluster/billing is present."},
				{Question: "Is the primary pod running?", Outcome: viewmodel.TriageFound, Findings: []string{"pod-crashloop/billing-1"}, Yourself: "kubectl get pod -n team-a billing-1 -o wide"},
				{Question: "Is a failover quorum reached?", Outcome: viewmodel.TriageUnknown, Because: "failoverquorums.postgresql.cnpg.io is not served by this cluster"},
			},
		}},
	}
}
