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

package cnpgstatus

import (
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	barmanapi "github.com/cloudnative-pg/barman-cloud/pkg/api"
)

var backlogNow = time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)

func ptrInt(v int) *int { return &v }

// readyRun is the earlier sweeps, oldest first, one every 30 s up to
// 30 s before backlogNow; a negative count is a failed read.
func readyRun(counts ...int) []ReadySample {
	out := make([]ReadySample, 0, len(counts))
	for i, n := range counts {
		s := ReadySample{At: backlogNow.Add(-time.Duration(len(counts)-i) * 30 * time.Second)}
		if n >= 0 {
			s.Files = ptrInt(n)
		}
		out = append(out, s)
	}
	return out
}

func TestArchiveBacklogOf(t *testing.T) {
	inTree := func(parallel int) *cnpgv1.Cluster {
		c := clusterWith(nil)
		c.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{}}
		if parallel > 0 {
			c.Spec.Backup.BarmanObjectStore.Wal = &barmanapi.WalBackupConfiguration{MaxParallel: parallel}
		}
		return c
	}
	plugin := func(name string) *cnpgv1.Cluster {
		c := clusterWith(nil)
		c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: name}}
		return c
	}
	barman := plugin(BarmanCloudPlugin)
	tests := []struct {
		name      string
		cluster   *cnpgv1.Cluster
		live      *ArchiverLive
		segments  int
		threshold int
		known     bool
		since     time.Time
	}{
		{"no reading", inTree(0), nil, 0, 3, true, time.Time{}},
		{"below the threshold", inTree(0), &ArchiverLive{ReadyWALFiles: 2, Earlier: readyRun(9, 9)}, 0, 3, true, time.Time{}},
		{"one sweep at the threshold (a burst)", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(0, 1)}, 0, 3, true, time.Time{}},
		{"two sweeps: a backlog from the first", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(1, 5)}, 7, 3, true, backlogNow.Add(-30 * time.Second)},
		{"a failed read neither extends nor breaks the run", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(1, 4, -1)}, 7, 3, true, backlogNow.Add(-60 * time.Second)},
		{"the run reaches the oldest sweep kept: onset unknown", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(4, 5)}, 7, 3, true, time.Time{}},
		{"in-tree maxParallel 8: eight in flight twice is not a backlog", inTree(8), &ArchiverLive{ReadyWALFiles: 8, Earlier: readyRun(0, 8)}, 0, 9, true, time.Time{}},
		{"barman plugin maxParallel 8: six ready on one sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(2, 1), PluginMaxParallel: ptrInt(8)}, 0, 9, true, time.Time{}},
		{"barman plugin maxParallel 8: six ready on every sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(2, 6, 8, 5), PluginMaxParallel: ptrInt(8)}, 0, 9, true, time.Time{}},
		{"barman plugin maxParallel 8: a persistent backlog above it", barman, &ArchiverLive{ReadyWALFiles: 14, Earlier: readyRun(3, 10, 12), PluginMaxParallel: ptrInt(8)}, 14, 9, true, backlogNow.Add(-60 * time.Second)},
		{"barman plugin, ObjectStore unread: steady handful", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(0, 5, 7, 6)}, 0, 3, false, time.Time{}},
		{"barman plugin, ObjectStore unread: two sweeps are not enough", barman, &ArchiverLive{ReadyWALFiles: 8, Earlier: readyRun(0, 6)}, 0, 3, false, time.Time{}},
		{"barman plugin, ObjectStore unread: growing over three sweeps", barman, &ArchiverLive{ReadyWALFiles: 9, Earlier: readyRun(0, 6, 6)}, 9, 3, false, backlogNow.Add(-60 * time.Second)},
		{"pgBackRest plugin: parallelism never known", plugin(PgBackRestDaliboPlugin), &ArchiverLive{ReadyWALFiles: 5, Earlier: readyRun(0, 5, 5)}, 0, 3, false, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.live != nil {
				tt.live.ObservedAt = backlogNow
			}
			got := ArchiveBacklogOf(tt.cluster, tt.live)
			if got.Segments != tt.segments || got.Threshold != tt.threshold || got.ParallelismKnown != tt.known || !got.Since.Equal(tt.since) {
				t.Errorf("ArchiveBacklogOf = %+v, want segments %d threshold %d known %v since %s", got, tt.segments, tt.threshold, tt.known, tt.since)
			}
			if got.Behind() != (tt.segments > 0) {
				t.Errorf("Behind() = %v", got.Behind())
			}
		})
	}
}

// TestArchivingFollowsTheBacklogRule: the archiving verdict and the
// recovery screens read the same persistence rule.
func TestArchivingFollowsTheBacklogRule(t *testing.T) {
	c := clusterWith(nil, cond(ConditionContinuousArchiving, "True", "ContinuousArchivingSuccess"))
	c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: BarmanCloudPlugin}}
	for _, live := range []*ArchiverLive{
		{IsArchivingWAL: true, ReadyWALFiles: 6, Earlier: readyRun(2, 1), PluginMaxParallel: ptrInt(8)},
		{IsArchivingWAL: true, ReadyWALFiles: 14, Earlier: readyRun(3, 10, 12), PluginMaxParallel: ptrInt(8)},
	} {
		want := ArchiveBacklogOf(c, live).Behind()
		got := Archiving(c, live).Reason == ArchivingReasonBacklog
		if got != want {
			t.Errorf("%+v: Archiving says backlog %v, ArchiveBacklogOf %v", live, got, want)
		}
	}
}

// TestArchivingUnreadPrimaryIsNotOK: a True condition with the primary's
// archiver unread is unknown and says since when the condition held.
func TestArchivingUnreadPrimaryIsNotOK(t *testing.T) {
	ok := cond(ConditionContinuousArchiving, "True", "ContinuousArchivingSuccess")
	ok.LastTransitionTime.Time = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	c := clusterWith(nil, ok)
	c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: BarmanCloudPlugin}}
	got := Archiving(c, nil)
	if got.Class != ClassUnknown || got.Reason != ArchivingReasonPrimaryUnread || got.ConditionStatus != "True" {
		t.Fatalf("Archiving(unread) = %+v", got)
	}
	if want := "since 2026-11-01T00:00:00Z"; !strings.Contains(got.Message, want) {
		t.Errorf("message %q does not name the condition's age (%s)", got.Message, want)
	}
}
