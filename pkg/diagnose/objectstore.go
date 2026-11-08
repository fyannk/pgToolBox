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

package diagnose

import (
	"fmt"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// The conditions here read the barman-cloud plugin's own summary of the
// object store: the recovery window it reports per server in the
// ObjectStore status. With the in-tree Barman support gone, this is
// where the operator's first recoverability point and last backup
// times live, and the only place a console that reads no object store
// can learn them.

// recoveryWindow is the plugin's window for this cluster, or the reason
// it cannot be read. A cluster that references no store is readable
// and has no window: nothing to judge, which is clear, not could-not-run.
func recoveryWindow(in Input) (observe.ObjectStoreReference, *observe.RecoveryWindow, string) {
	if !in.HasBackups {
		return observe.ObjectStoreReference{}, nil, "the backup catalog has not been observed yet"
	}
	if in.Backups.Stale {
		return observe.ObjectStoreReference{}, nil, "the backup catalog is stale, so the object store's report is unknown"
	}
	ref := in.Backups.ObjectStore
	switch ref.State {
	case observe.ObjectStoreNotReferenced:
		return ref, nil, ""
	case observe.ObjectStorePresent:
		return ref, ref.RecoveryWindow, ""
	default: // observe.ObjectStoreUnknown
		return ref, nil, fmt.Sprintf("the referenced ObjectStore %q could not be read, so its report of the backups is unknown", ref.Name)
	}
}

// StoreBackupFailed matches the plugin reporting the last backup for
// this cluster's server as failed: a failure more recent than the last
// success, or a failure with no success at all.
type StoreBackupFailed struct{}

func (StoreBackupFailed) describe() string {
	return "the barman-cloud plugin reporting this cluster's last backup as failed"
}

func (StoreBackupFailed) evaluate(_ string, in Input) ([]conditionMatch, string) {
	ref, window, reason := recoveryWindow(in)
	if reason != "" {
		return nil, reason
	}
	if window == nil || window.LastFailedBackup == nil {
		return nil, ""
	}
	if window.LastSuccessfulBackup != nil && !window.LastFailedBackup.After(*window.LastSuccessfulBackup) {
		return nil, ""
	}
	detail := "lastFailedBackupTime " + window.LastFailedBackup.UTC().Format(time.RFC3339)
	if window.LastSuccessfulBackup == nil {
		detail += ", no successful backup reported"
	} else {
		detail += ", lastSuccessfulBackupTime " + window.LastSuccessfulBackup.UTC().Format(time.RFC3339)
	}
	return []conditionMatch{{
		subject: EntityRef{Kind: "ObjectStore", Name: ref.Name},
		at:      *window.LastFailedBackup,
		evidence: []Evidence{{
			Origin: "plugin-reported",
			Object: "ObjectStore/" + ref.Name + " server " + ref.ServerName,
			Detail: detail,
		}},
		link:      "/backups",
		linkLabel: "Backups",
	}}, ""
}

// StoreNoBackup matches the plugin reporting no successful backup for
// this cluster's server while the cluster has had a backup schedule for
// at least Since. The schedule's age is what makes the absence a
// finding: a cluster created an hour ago with a nightly schedule has
// simply not reached its first run.
type StoreNoBackup struct {
	// Since is how old the oldest schedule must be.
	Since time.Duration
}

func (c StoreNoBackup) describe() string {
	return fmt.Sprintf("the barman-cloud plugin reporting no successful backup, on a cluster scheduled for at least %s", c.Since)
}

func (c StoreNoBackup) evaluate(_ string, in Input) ([]conditionMatch, string) {
	ref, window, reason := recoveryWindow(in)
	if reason != "" {
		return nil, reason
	}
	if ref.State != observe.ObjectStorePresent {
		return nil, ""
	}
	if window != nil && window.LastSuccessfulBackup != nil {
		return nil, ""
	}
	var oldest *observe.ScheduledBackupFacts
	for i := range in.Backups.ScheduledBackups {
		schedule := &in.Backups.ScheduledBackups[i]
		if oldest == nil || schedule.CreatedAt.Before(oldest.CreatedAt) {
			oldest = schedule
		}
	}
	if oldest == nil || in.Now.Sub(oldest.CreatedAt) < c.Since {
		return nil, ""
	}
	detail := "no serverRecoveryWindow entry for server " + ref.ServerName
	if window != nil {
		detail = "serverRecoveryWindow for server " + ref.ServerName + " carries no lastSuccessfulBackupTime"
		if window.LastFailedBackup != nil {
			detail += ", lastFailedBackupTime " + window.LastFailedBackup.UTC().Format(time.RFC3339)
		}
	}
	return []conditionMatch{{
		subject: EntityRef{Kind: "ObjectStore", Name: ref.Name},
		evidence: []Evidence{
			{Origin: "plugin-reported", Object: "ObjectStore/" + ref.Name, Detail: detail},
			{
				Origin: "operator-reported", Object: "ScheduledBackup/" + oldest.Name,
				Detail: fmt.Sprintf("created %s — %s ago", oldest.CreatedAt.UTC().Format(time.RFC3339),
					in.Now.Sub(oldest.CreatedAt).Round(time.Hour)),
			},
		},
		link:      "/backups",
		linkLabel: "Backups",
	}}, ""
}
