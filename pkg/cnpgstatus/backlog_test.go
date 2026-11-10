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
		name        string
		cluster     *cnpgv1.Cluster
		live        *ArchiverLive
		segments    int
		threshold   int
		known       bool
		unconfirmed int
	}{
		{"no reading", inTree(0), nil, 0, 3, true, 0},
		{"below the threshold", inTree(0), &ArchiverLive{ReadyWALFiles: 2, Earlier: readyRun(9, 9)}, 0, 3, true, 0},
		{"one sweep at the threshold (a burst)", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(0, 1)}, 0, 3, true, 0},
		{"two sweeps at the threshold: a backlog", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(0, 5)}, 7, 3, true, 0},
		{"two sweeps after one file", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(1, 5)}, 7, 3, true, 0},
		{"[0,1,2,3] then 3: a backlog", inTree(0), &ArchiverLive{ReadyWALFiles: 3, Earlier: readyRun(0, 1, 2, 3)}, 3, 3, true, 0},
		{"a failed read neither extends nor breaks the run", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(0, -1, 4, -1)}, 7, 3, true, 0},
		{"[low, failed, high] then high", inTree(0), &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(1, -1, 5)}, 6, 3, true, 0},
		{"the run reaches the oldest sweep kept", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(4, 5)}, 7, 3, true, 0},
		{"a console that just started: unconfirmed, not ok", inTree(0), &ArchiverLive{ReadyWALFiles: 40}, 0, 3, true, 40},
		{"only failed reads kept: unconfirmed", inTree(0), &ArchiverLive{ReadyWALFiles: 40, Earlier: readyRun(-1, -1)}, 0, 3, true, 40},
		{"in-tree maxParallel 8: eight in flight twice is not a backlog", inTree(8), &ArchiverLive{ReadyWALFiles: 8, Earlier: readyRun(0, 8)}, 0, 9, true, 0},
		{"barman plugin maxParallel 8: six ready on one sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(2, 1), PluginMaxParallel: ptrInt(8)}, 0, 9, true, 0},
		{"barman plugin maxParallel 8: six ready on every sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(2, 6, 8, 5), PluginMaxParallel: ptrInt(8)}, 0, 9, true, 0},
		{"barman plugin maxParallel 8: a persistent backlog above it", barman, &ArchiverLive{ReadyWALFiles: 14, Earlier: readyRun(0, 3, 10, 12), PluginMaxParallel: ptrInt(8)}, 14, 9, true, 0},
		{"barman plugin, ObjectStore unread: steady handful", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(0, 5, 7, 6)}, 0, 3, false, 0},
		{"barman plugin, ObjectStore unread: two sweeps are not enough", barman, &ArchiverLive{ReadyWALFiles: 8, Earlier: readyRun(0, 6)}, 0, 3, false, 0},
		{"barman plugin, ObjectStore unread: two sweeps kept, unconfirmed", barman, &ArchiverLive{ReadyWALFiles: 8, Earlier: readyRun(6)}, 0, 3, false, 8},
		{"barman plugin, ObjectStore unread: growing over three sweeps", barman, &ArchiverLive{ReadyWALFiles: 9, Earlier: readyRun(0, 6, 6)}, 9, 3, false, 0},
		{"pgBackRest plugin: parallelism never known", plugin(PgBackRestDaliboPlugin), &ArchiverLive{ReadyWALFiles: 5, Earlier: readyRun(0, 5, 5)}, 0, 3, false, 0},
		// CloudNativePG's archiver is the isWALArchiver plugin, not the
		// first backup plugin: a backups-only barman-cloud entry with its
		// ObjectStore at maxParallel 16 does not raise pgBackRest's threshold.
		{"barman backups only, pgBackRest archives: the growth rule at 3", mixed(), &ArchiverLive{ReadyWALFiles: 10, Earlier: readyRun(0, 6, 8), PluginMaxParallel: ptrInt(16)}, 10, 3, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.live != nil {
				tt.live.ObservedAt = backlogNow
			}
			got := ArchiveBacklogOf(tt.cluster, tt.live)
			if got.Segments != tt.segments || got.Threshold != tt.threshold || got.ParallelismKnown != tt.known || got.Unconfirmed != tt.unconfirmed {
				t.Errorf("ArchiveBacklogOf = %+v, want segments %d threshold %d known %v unconfirmed %d", got, tt.segments, tt.threshold, tt.known, tt.unconfirmed)
			}
			if got.Behind() != (tt.segments > 0) {
				t.Errorf("Behind() = %v", got.Behind())
			}
		})
	}
}

// mixed lists a backups-only barman-cloud plugin before the pgBackRest
// plugin that archives WAL (isWALArchiver).
func mixed() *cnpgv1.Cluster {
	yes := true
	c := clusterWith(nil)
	c.Spec.Plugins = []cnpgv1.PluginConfiguration{
		{Name: BarmanCloudPlugin, Parameters: map[string]string{"barmanObjectName": "backups"}},
		{Name: PgBackRestDaliboPlugin, IsWALArchiver: &yes},
	}
	return c
}

// TestWALArchiver is CloudNativePG's archiver rule: the first enabled
// isWALArchiver plugin, else the in-tree object store, else the first
// backup plugin.
func TestWALArchiver(t *testing.T) {
	yes, no := true, false
	inTree := clusterWith(nil)
	inTree.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{}}
	withPlugins := func(p ...cnpgv1.PluginConfiguration) *cnpgv1.Cluster {
		c := clusterWith(nil)
		c.Spec.Plugins = p
		return c
	}
	for _, tc := range []struct {
		name    string
		cluster *cnpgv1.Cluster
		want    Archiver
	}{
		{"none", clusterWith(nil), Archiver{}},
		{"nil", nil, Archiver{}},
		{"in-tree", inTree, Archiver{InTree: true}},
		{"isWALArchiver wins over an earlier backup plugin", mixed(), Archiver{Plugin: PgBackRestDaliboPlugin}},
		{"a disabled archiver is skipped", withPlugins(cnpgv1.PluginConfiguration{Name: PgBackRestDaliboPlugin, IsWALArchiver: &yes, Enabled: &no}, cnpgv1.PluginConfiguration{Name: BarmanCloudPlugin}), Archiver{Plugin: BarmanCloudPlugin}},
		{"no isWALArchiver: the first backup plugin", withPlugins(cnpgv1.PluginConfiguration{Name: "other.example"}, cnpgv1.PluginConfiguration{Name: BarmanCloudPlugin}), Archiver{Plugin: BarmanCloudPlugin}},
	} {
		got := WALArchiver(tc.cluster)
		if got.Plugin != tc.want.Plugin || got.InTree != tc.want.InTree {
			t.Errorf("%s: WALArchiver = %+v, want %+v", tc.name, got, tc.want)
		}
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
	// Hibernated: no primary runs, and the reading says so.
	hibernated := clusterWith(map[string]string{AnnotationHibernation: "on"}, ok, cond(ConditionHibernation, "True", "Hibernated"))
	hibernated.Spec.Plugins = c.Spec.Plugins
	if got := Archiving(hibernated, nil); got.Class != ClassUnknown || got.Reason != ArchivingReasonHibernated || !strings.Contains(got.Message, "hibernated") {
		t.Errorf("Archiving(hibernated) = %+v", got)
	}
	noCondition := clusterWith(map[string]string{AnnotationHibernation: "on"}, cond(ConditionHibernation, "True", "Hibernated"))
	noCondition.Spec.Plugins = c.Spec.Plugins
	if got := Archiving(noCondition, nil); got.Reason != ArchivingReasonHibernated {
		t.Errorf("Archiving(hibernated, no condition) = %+v", got)
	}
}

// TestArchivingTooFewSweepsIsNotOK: after a console restart a count at
// the threshold is unknown until enough sweeps can judge it.
func TestArchivingTooFewSweepsIsNotOK(t *testing.T) {
	c := clusterWith(nil, cond(ConditionContinuousArchiving, "True", "ContinuousArchivingSuccess"))
	c.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{}}
	live := &ArchiverLive{IsArchivingWAL: true, ReadyWALFiles: 500, ObservedAt: backlogNow}
	if got := Archiving(c, live); got.Class != ClassUnknown || got.Reason != ArchivingReasonBacklogUnconfirmed || !strings.HasPrefix(got.Message, "500 ") {
		t.Errorf("first sweep, 500 ready: %+v", got)
	}
	live.Earlier = readyRun(400)
	if got := Archiving(c, live); got.Reason != ArchivingReasonBacklog {
		t.Errorf("second sweep, still high: %+v", got)
	}
	live.Earlier, live.ReadyWALFiles = nil, 1
	if got := Archiving(c, live); got.Class != ClassOK {
		t.Errorf("first sweep, one ready: %+v", got)
	}
	noCondition := clusterWith(nil)
	noCondition.Spec.Backup = c.Spec.Backup
	live.ReadyWALFiles = 500
	if got := Archiving(noCondition, live); got.Reason != ArchivingReasonBacklogUnconfirmed {
		t.Errorf("no condition, first sweep, 500 ready: %+v", got)
	}
}

// TestArchivingDesignatedPrimaryIsNotOK (round 7, CNPG P1): a replica
// cluster's designated primary archives (archive_mode=always) but its
// /pg/status carries no readyWalFiles (probes.go v1.30.1:461-463), so its
// 0 is no reading: with ContinuousArchiving True archiving is unknown
// ArchiveBacklogNotReported, never ok, and no backlog is read from it.
func TestArchivingDesignatedPrimaryIsNotOK(t *testing.T) {
	c := clusterWith(nil, cond(ConditionContinuousArchiving, "True", "ContinuousArchivingSuccess"))
	c.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{}}
	live := &ArchiverLive{IsArchivingWAL: true, ReadyNotReported: true, ObservedAt: backlogNow, Earlier: readyRun(0, 0, 0)}
	if got := Archiving(c, live); got.Class != ClassUnknown || got.Reason != ArchivingReasonBacklogNotReported || !strings.Contains(got.Message, "designated primary") {
		t.Errorf("designated primary, condition True: %+v", got)
	}
	if b := ArchiveBacklogOf(c, live); !b.NotReported || b.Behind() || b.Unconfirmed != 0 {
		t.Errorf("backlog of a designated primary: %+v", b)
	}
	noCondition := clusterWith(nil)
	noCondition.Spec.Backup = c.Spec.Backup
	if got := Archiving(noCondition, live); got.Class != ClassUnknown || got.Reason != ArchivingReasonBacklogNotReported {
		t.Errorf("designated primary, no condition: %+v", got)
	}
	// A true primary with the same zero is ok.
	live.ReadyNotReported = false
	if got := Archiving(c, live); got.Class != ClassOK {
		t.Errorf("primary, zero ready: %+v", got)
	}
}
