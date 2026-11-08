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

package evidence

import (
	"context"
	"slices"
	"time"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// The pgBackRest profile of the collection routes: the
// same routes and bounds as Barman's, PGBackRest* pages, the scope a
// stanza. A backup's identity is its label; the console keeps the
// labels as the backup id set, so a CNPG Backup is matched by the label
// its plugin records (Dalibo status.backupName, Opera status.backupId).

func decodePGBackRestBackupPage(body []byte) (*evidencev1alpha1.PGBackRestBackupPage, error) {
	return decodePage(body, func(p *evidencev1alpha1.PGBackRestBackupPage) evidencev1alpha1.PageHeader { return p.PageHeader })
}

func decodePGBackRestRecoveryPage(body []byte) (*evidencev1alpha1.PGBackRestRecoveryPathPage, error) {
	return decodePage(body, func(p *evidencev1alpha1.PGBackRestRecoveryPathPage) evidencev1alpha1.PageHeader { return p.PageHeader })
}

// pgbackrestBackups assembles the labels of the snapshot's stanza and
// their compact items, bounded as the Barman collection is. The end WAL
// of a backup is its archive stop, located with the stanza's segment size
// (inferred by the agent, or its stated 16 MiB assumption).
func (s agentSession) pgbackrestBackups(ctx context.Context, snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot) (backups, error) {
	if snapshot.EvidenceGeneration == 0 {
		return backups{ids: map[string]bool{}}, nil
	}
	stanza := snapshot.Identity.Repository.Scope.Name
	var segmentSize *uint64
	if summary := snapshot.Details.PGBackRest; summary != nil {
		segmentSize = summary.WALSegmentSizeBytes
	}
	decode := func(body []byte) (evidencev1alpha1.PageHeader, []evidencev1alpha1.PGBackRestBackup, error) {
		page, err := decodePGBackRestBackupPage(body)
		if err != nil {
			return evidencev1alpha1.PageHeader{}, nil, err
		}
		return page.PageHeader, page.Items, nil
	}
	return collectBackups(ctx, s, snapshot, decode, func(item evidencev1alpha1.PGBackRestBackup) (string, bool) {
		return item.Label, item.Stanza == stanza
	}, func(item evidencev1alpha1.PGBackRestBackup) BackupItem { return pgbackrestItem(item, segmentSize) })
}

// pgbackrestItem is one backup as the recovery window and the backup
// list need it.
func pgbackrestItem(item evidencev1alpha1.PGBackRestBackup, segmentSize *uint64) BackupItem {
	endTimeline, endPosition := walPosition(item.ArchiveStop, segmentSize)
	return BackupItem{
		ID: item.Label, State: item.State, Reason: item.Reason, BeginAt: item.StartAt, EndAt: item.StopAt,
		EndTimeline: endTimeline, EndPosition: endPosition, Failed: item.Error != nil && *item.Error,
		PgBackRest: &PgBackRestItem{
			Stanza: item.Stanza, Type: item.Type, Prior: item.Prior, References: slices.Clone(item.References),
			Usable: item.Usable, UnusableReasons: slices.Clone(item.UnusableReasons), Timeline: item.Timeline,
			CNPGBackupName: item.CNPGBackupName, SizeBytes: item.SizeBytes, RepositorySizeBytes: item.RepositorySizeBytes,
		},
	}
}

// pgbackrestRecoveryPaths reads the stanza's recovery paths. A pgBackRest
// path carries no restore lower bound of its own: it is its backup's
// stop time, from the backup collection of the same publication (a path
// whose backup was cut from the collection has none, and is not drawn).
// Its start timeline is the first it crosses.
func (s agentSession) pgbackrestRecoveryPaths(ctx context.Context, snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot, items []BackupItem) (Recovery, error) {
	if out, done := recoveryPrecheck(snapshot); done {
		return out, nil
	}
	stops := make(map[string]*time.Time, len(items))
	for _, item := range items {
		stops[item.ID] = item.EndAt
	}
	stanza := snapshot.Identity.Repository.Scope.Name
	out := Recovery{State: RecoveryAvailable}
	decode := func(body []byte) (evidencev1alpha1.PageHeader, []evidencev1alpha1.PGBackRestRecoveryPath, error) {
		page, err := decodePGBackRestRecoveryPage(body)
		if err != nil {
			return evidencev1alpha1.PageHeader{}, nil, err
		}
		return page.PageHeader, page.Items, nil
	}
	complete, err := collectPages(ctx, s, snapshot, evidencev1alpha1.RecoveryPathsPath, maximumRecoveryPages, maximumRecoveryBytes, decode, func(item evidencev1alpha1.PGBackRestRecoveryPath) bool {
		if item.Stanza != stanza {
			return true
		}
		if len(out.Paths) >= maximumRecoveryPaths {
			return false
		}
		path := RecoveryPath{
			BackupID: item.Label, TargetTimeline: item.TargetTimeline, State: item.State, Reason: item.Reason, Stop: item.Stop,
			LowerBoundAt: stops[item.Label], FrontierReceiptAt: item.FrontierReceiptAt, Assumptions: slices.Clone(item.AssumptionCodes),
			FrontierTimeline: item.FrontierTimeline, FrontierPosition: item.FrontierPosition, Consistent: &item.Consistent,
		}
		if len(item.Timelines) > 0 {
			start := item.Timelines[0]
			path.StartTimeline = &start
		}
		out.Paths = append(out.Paths, path)
		return true
	})
	return recoveryOutcome(out, complete, err)
}
