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

package kube

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

func rawBackup(name, cluster, method string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": name, "namespace": "payments", "uid": "u-" + name, "creationTimestamp": "2026-07-28T10:00:00Z"},
		"spec":     map[string]any{"cluster": map[string]any{"name": cluster}, "method": method},
		"status":   map[string]any{"phase": "completed", "method": method, "startedAt": "2026-07-28T10:01:00Z", "stoppedAt": "2026-07-28T10:02:00Z"},
	}}
}

func rawSchedule(name, cluster string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "ScheduledBackup",
		"metadata": map[string]any{"name": name, "namespace": "payments", "uid": "u-" + name, "creationTimestamp": "2026-07-28T09:00:00Z"},
		"spec":     map[string]any{"cluster": map[string]any{"name": cluster}, "method": "plugin", "schedule": "0 0 2 * * *", "suspend": false},
		"status":   map[string]any{"lastScheduleTime": "2026-07-28T10:00:00Z", "nextScheduleTime": "2026-07-29T10:00:00Z"},
	}}
}

func rawClusterWithObjectStore(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "orders", "namespace": "payments"},
		"spec": map[string]any{"instances": int64(3), "plugins": []any{map[string]any{
			"name": barmanPluginName, "parameters": map[string]any{barmanObjectName: name},
		}}},
	}}
}

func TestConvertBackupAndScheduleRequireExactClusterReference(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	facts, member, err := c.convertBackup(rawBackup("b1", "orders", "volumeSnapshot").Object)
	if err != nil || !member {
		t.Fatalf("target Backup conversion: member=%v err=%v", member, err)
	}
	if facts.Method != "volumeSnapshot" || facts.Phase != "completed" || facts.StartedAt == nil || facts.StoppedAt == nil {
		t.Fatalf("Backup facts incomplete: %+v", facts)
	}
	_, member, err = c.convertBackup(rawBackup("foreign", "other", "plugin").Object)
	if err != nil || member {
		t.Fatalf("foreign Backup membership: member=%v err=%v", member, err)
	}
	schedule, member, err := c.convertScheduledBackup(rawSchedule("daily", "orders").Object)
	if err != nil || !member || schedule.Method != "plugin" || schedule.Suspended == nil || *schedule.Suspended {
		t.Fatalf("target ScheduledBackup conversion: member=%v facts=%+v err=%v", member, schedule, err)
	}
	_, member, err = c.convertScheduledBackup(rawSchedule("foreign-daily", "other").Object)
	if err != nil || member {
		t.Fatalf("foreign ScheduledBackup membership: member=%v err=%v", member, err)
	}
}

func TestConvertBackupCarriesCorrelationEvidence(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)

	plugin := rawBackup("b-plugin", "orders", "plugin")
	plugin.Object["spec"].(map[string]any)["pluginConfiguration"] = map[string]any{"name": barmanPluginName}
	plugin.Object["status"].(map[string]any)["backupId"] = "20260729T060000"
	facts, member, err := c.convertBackup(plugin.Object)
	if err != nil || !member {
		t.Fatalf("plugin Backup conversion: member=%v err=%v", member, err)
	}
	if facts.BackupID != "20260729T060000" || facts.PluginName != barmanPluginName {
		t.Errorf("correlation facts = %q %q", facts.BackupID, facts.PluginName)
	}

	snapshot, member, err := c.convertBackup(rawBackup("b-vs", "orders", "volumeSnapshot").Object)
	if err != nil || !member {
		t.Fatalf("volumeSnapshot Backup conversion: member=%v err=%v", member, err)
	}
	if snapshot.BackupID != "" || snapshot.PluginName != "" {
		t.Errorf("unreported correlation facts must stay empty: %q %q", snapshot.BackupID, snapshot.PluginName)
	}
}

func TestObjectStoreNameHonorsEnabledPlugin(t *testing.T) {
	t.Parallel()
	cluster := rawClusterWithObjectStore("orders-store")
	name, serverName, err := objectStoreName(cluster.Object)
	if err != nil || name != "orders-store" || serverName != "" {
		t.Fatalf("objectStoreName = %q, %q, %v", name, serverName, err)
	}
	plugins := cluster.Object["spec"].(map[string]any)["plugins"].([]any)
	plugins[0].(map[string]any)["parameters"].(map[string]any)[barmanServerName] = "orders-eu"
	name, serverName, err = objectStoreName(cluster.Object)
	if err != nil || name != "orders-store" || serverName != "orders-eu" {
		t.Fatalf("objectStoreName with serverName = %q, %q, %v", name, serverName, err)
	}
	plugins[0].(map[string]any)["enabled"] = false
	name, serverName, err = objectStoreName(cluster.Object)
	if err != nil || name != "" || serverName != "" {
		t.Fatalf("disabled plugin reference = %q, %q, %v", name, serverName, err)
	}
}

// TestObjectStoreRecoveryWindowIsReadForThisServerOnly proves the
// plugin's per-server summary is read under the cluster's own server
// name — the default when the stanza names none — and not under any
// other cluster's.
func TestObjectStoreRecoveryWindowIsReadForThisServerOnly(t *testing.T) {
	t.Parallel()
	store := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "barmancloud.cnpg.io/v1", "kind": "ObjectStore",
		"metadata": map[string]any{"name": "orders-store", "namespace": "payments"},
		"spec": map[string]any{"configuration": map[string]any{
			"destinationPath": "s3://pgbackups/", "endpointURL": "http://minio.minio.svc:9000",
		}},
		"status": map[string]any{"serverRecoveryWindow": map[string]any{
			"orders": map[string]any{
				"firstRecoverabilityPoint": "2026-09-01T00:00:00Z",
				"lastSuccessfulBackupTime": "2026-09-16T02:00:00Z",
				"lastFailedBackupTime":     "not a time",
			},
			"billing": map[string]any{"lastSuccessfulBackupTime": "2026-09-17T02:00:00Z"},
		}},
	}}
	ref := NewFacts("payments", "orders").ObjectStoreReference(rawClusterWithObjectStore("orders-store"), store, true)
	if ref.State != observe.ObjectStorePresent || ref.ServerName != "orders" || ref.RecoveryWindow == nil {
		t.Fatalf("reference = %+v", ref)
	}
	w := ref.RecoveryWindow
	if w.FirstRecoverabilityPoint == nil || !w.FirstRecoverabilityPoint.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("FirstRecoverabilityPoint = %v", w.FirstRecoverabilityPoint)
	}
	if w.LastSuccessfulBackup == nil || !w.LastSuccessfulBackup.Equal(time.Date(2026, 9, 16, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("LastSuccessfulBackup = %v", w.LastSuccessfulBackup)
	}
	if w.LastFailedBackup != nil {
		t.Errorf("an unparseable instant became %v", w.LastFailedBackup)
	}
}

// TestConvertBackupNamesItsSchedule: the operator's label names the
// ScheduledBackup that created a backup, a controller owner of that kind
// stands in for it, and an on-demand backup names none.
func TestConvertBackupNamesItsSchedule(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	for _, tc := range []struct {
		name string
		meta map[string]any
		want string
	}{
		{name: "labelled", meta: map[string]any{"labels": map[string]any{"cnpg.io/scheduled-backup": "nightly"}}, want: "nightly"},
		{name: "owned", meta: map[string]any{"ownerReferences": []any{map[string]any{
			"apiVersion": "postgresql.cnpg.io/v1", "kind": "ScheduledBackup", "name": "hourly", "uid": "s1",
		}}}, want: "hourly"},
		{name: "foreign owner", meta: map[string]any{"ownerReferences": []any{map[string]any{
			"apiVersion": "example.io/v1", "kind": "ScheduledBackup", "name": "other", "uid": "s2",
		}}}, want: ""},
		{name: "on demand", meta: nil, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := rawBackup("b1", "orders", "plugin")
			metadata := raw.Object["metadata"].(map[string]any)
			for k, v := range tc.meta {
				metadata[k] = v
			}
			facts, _, err := c.convertBackup(raw.Object)
			if err != nil {
				t.Fatal(err)
			}
			if facts.ScheduledBackup != tc.want {
				t.Fatalf("ScheduledBackup = %q, want %q", facts.ScheduledBackup, tc.want)
			}
		})
	}
}

// TestConvertBackupReadsAnEpochStopAsUnknown (round 18, CNPG P3): a
// plugin that leaves stopped_at unset gives a Completed Backup stopped
// 1970-01-01 (time.Unix(0, 0)); the console reads it as no stop time.
func TestConvertBackupReadsAnEpochStopAsUnknown(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	raw := rawBackup("b-epoch", "orders", "plugin")
	raw.Object["status"].(map[string]any)["stoppedAt"] = "1970-01-01T00:00:00Z"
	facts, _, err := c.convertBackup(raw.Object)
	if err != nil {
		t.Fatal(err)
	}
	if facts.StoppedAt != nil {
		t.Errorf("epoch stoppedAt read as %s", facts.StoppedAt)
	}
	if facts.StartedAt == nil || !facts.StartedAt.Equal(time.Date(2026, 7, 28, 10, 1, 0, 0, time.UTC)) {
		t.Errorf("plausible startedAt lost: %v", facts.StartedAt)
	}
	raw.Object["status"].(map[string]any)["stoppedAt"] = "2026-07-28T10:00:30Z"
	if facts, _, _ := c.convertBackup(raw.Object); facts.StoppedAt != nil {
		t.Errorf("a stop before the start read as %s", facts.StoppedAt)
	}
}
