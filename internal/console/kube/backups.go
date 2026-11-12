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
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

const (
	backupListPageSize     = 200
	maxBackupCandidates    = 2000
	maxScheduledCandidates = 1000
	barmanPluginName       = "barman-cloud.cloudnative-pg.io"
	barmanObjectName       = "barmanObjectName"
	barmanServerName       = "serverName"
)

// nestedInstant reads one RFC3339 timestamp out of an unstructured map;
// nil for an absent or unparseable value, never a wrong instant.
func nestedInstant(object map[string]any, field string) *time.Time {
	raw, found, _ := unstructured.NestedString(object, field)
	if !found {
		return nil
	}
	return operatorInstant(raw)
}

// objectStoreName is the referenced ObjectStore and the server name the
// cluster files its backups under, from the enabled barman-cloud plugin
// stanza; both empty when the plugin is not enabled, the server name
// alone empty when the stanza leaves it to default.
func objectStoreName(content map[string]any) (name, serverName string, err error) {
	var cluster cnpgv1.Cluster
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, &cluster); err != nil {
		return "", "", redact.NewError("cluster plugin convert", redact.CategoryInternal, err)
	}
	for _, plugin := range cluster.Spec.Plugins {
		if plugin.Name != barmanPluginName || (plugin.Enabled != nil && !*plugin.Enabled) {
			continue
		}
		return plugin.Parameters[barmanObjectName], plugin.Parameters[barmanServerName], nil
	}
	return "", "", nil
}

func (c *Client) convertBackup(content map[string]any) (observe.BackupFacts, bool, error) {
	var backup cnpgv1.Backup
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, &backup); err != nil {
		return observe.BackupFacts{}, false, redact.NewError("backup convert", redact.CategoryInternal, err)
	}
	method := backup.Status.Method
	if method == "" {
		method = backup.Spec.Method
	}
	facts := observe.BackupFacts{
		Name: backup.Name, UID: string(backup.UID), Phase: string(backup.Status.Phase),
		Method: string(method), BackupID: backup.Status.BackupID,
		ScheduledBackup: scheduledParent(&backup),
		CreatedAt:       backup.CreationTimestamp.Time.UTC(),
	}
	facts.StartedAt, facts.StoppedAt = observe.SaneBackupTimes(backup.CreationTimestamp.Time,
		metaTime(backup.Status.StartedAt), metaTime(backup.Status.StoppedAt))
	if backup.Spec.PluginConfiguration != nil {
		facts.PluginName = backup.Spec.PluginConfiguration.Name
	}
	if backup.Status.InstanceID != nil {
		facts.SourceInstance = backup.Status.InstanceID.PodName
	}
	return facts, backup.Namespace == c.opts.Namespace && backup.Spec.Cluster.Name == c.opts.ClusterName, nil
}

func (c *Client) convertScheduledBackup(content map[string]any) (observe.ScheduledBackupFacts, bool, error) {
	var schedule cnpgv1.ScheduledBackup
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, &schedule); err != nil {
		return observe.ScheduledBackupFacts{}, false, redact.NewError("scheduled backup convert", redact.CategoryInternal, err)
	}
	facts := observe.ScheduledBackupFacts{
		Name: schedule.Name, UID: string(schedule.UID), Method: string(schedule.Spec.Method),
		Schedule: schedule.Spec.Schedule, Suspended: schedule.Spec.Suspend,
		CreatedAt:        schedule.CreationTimestamp.Time.UTC(),
		LastScheduleTime: metaTime(schedule.Status.LastScheduleTime),
		NextScheduleTime: metaTime(schedule.Status.NextScheduleTime),
	}
	return facts, schedule.Namespace == c.opts.Namespace && schedule.Spec.Cluster.Name == c.opts.ClusterName, nil
}

// scheduledBackupLabel is the label CloudNativePG puts on every Backup a
// ScheduledBackup creates, whatever its backupOwnerReference
// (utils.ParentScheduledBackupLabelName).
const scheduledBackupLabel = "cnpg.io/scheduled-backup"

// scheduledParent names the ScheduledBackup that created a Backup: the
// operator's label, else a controller owner of that kind; "" for an
// on-demand backup.
func scheduledParent(backup *cnpgv1.Backup) string {
	if name := backup.Labels[scheduledBackupLabel]; name != "" {
		return name
	}
	for _, ref := range backup.OwnerReferences {
		if ref.Kind == "ScheduledBackup" && strings.HasPrefix(ref.APIVersion, cnpgv1.SchemeGroupVersion.Group+"/") {
			return ref.Name
		}
	}
	return ""
}

func metaTime(value *metav1.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	t := value.Time.UTC()
	return &t
}
