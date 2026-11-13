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

// withArchived sets every sample's last_archived_time to at (the
// instance's clock, as the sweep read it).
func withArchived(run []ReadySample, at *time.Time) []ReadySample {
	for i := range run {
		run[i].LastArchivedAt = at
	}
	return run
}

// archivedNone marks every sample as read with nothing archived yet.
func archivedNone(run []ReadySample) []ReadySample {
	for i := range run {
		run[i].ArchivedNone = true
	}
	return run
}

// archivedAgo is a last_archived_time d before backlogNow.
func archivedAgo(d time.Duration) *time.Time { t := backlogNow.Add(-d); return &t }

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
	}{
		{"no reading", inTree(0), nil, 0, 3, true},
		{"below the threshold", inTree(0), &ArchiverLive{ReadyWALFiles: 2, Earlier: readyRun(9, 9)}, 0, 3, true},
		{"one sweep at the threshold (a burst)", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(0, 1)}, 0, 3, true},
		{"two sweeps at the threshold: a backlog", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(0, 5)}, 7, 3, true},
		{"two sweeps after one file", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(1, 5)}, 7, 3, true},
		{"[0,1,2,3] then 3: a backlog", inTree(0), &ArchiverLive{ReadyWALFiles: 3, Earlier: readyRun(0, 1, 2, 3)}, 3, 3, true},
		{"a failed read neither extends nor breaks the run", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(0, -1, 4, -1)}, 7, 3, true},
		{"[low, failed, high] then high", inTree(0), &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(1, -1, 5)}, 6, 3, true},
		{"the run reaches the oldest sweep kept", inTree(0), &ArchiverLive{ReadyWALFiles: 7, Earlier: readyRun(4, 5)}, 7, 3, true},
		{"a console that just started: not judged yet", inTree(0), &ArchiverLive{ReadyWALFiles: 40}, 0, 3, true},
		{"only failed reads kept: not judged yet", inTree(0), &ArchiverLive{ReadyWALFiles: 40, Earlier: readyRun(-1, -1)}, 0, 3, true},
		{"in-tree maxParallel 8: eight in flight twice is not a backlog", inTree(8), &ArchiverLive{ReadyWALFiles: 8, Earlier: readyRun(0, 8)}, 0, 9, true},
		{"barman plugin maxParallel 8: six ready on one sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(2, 1), PluginMaxParallel: ptrInt(8)}, 0, 9, true},
		{"barman plugin maxParallel 8: six ready on every sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(2, 6, 8, 5), PluginMaxParallel: ptrInt(8)}, 0, 9, true},
		{"barman plugin maxParallel 8: a persistent backlog above it", barman, &ArchiverLive{ReadyWALFiles: 14, Earlier: readyRun(0, 3, 10, 12), PluginMaxParallel: ptrInt(8)}, 14, 9, true},
		{"barman plugin, ObjectStore unread: steady handful", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(0, 5, 7, 6), LastArchivedAt: archivedAgo(10 * time.Second)}, 0, 3, false},
		// Round 7, DBA P2-5: a steady count over the sweeps while nothing
		// is archived (last_archived_time not after the oldest of them, or
		// none reported) is a hung archiver, not a healthy parallel one.
		{"barman plugin, ObjectStore unread: steady, nothing archived since", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(0, 5, 7, 6), LastArchivedAt: archivedAgo(10 * time.Minute)}, 6, 3, false},
		{"barman plugin, ObjectStore unread: steady, archived at the oldest sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(0, 5, 7, 6), LastArchivedAt: archivedAgo(60 * time.Second)}, 6, 3, false},
		// CNPG P3-3: last_archived_time is the instance's clock. Without
		// the instance's own reading at the oldest sweep, an archive counts
		// only once it is more than one sweep interval after that sweep,
		// so a skew within one interval invents no progress.
		{"barman plugin, ObjectStore unread: steady, archived within one interval after the oldest sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(0, 5, 7, 6), LastArchivedAt: archivedAgo(59 * time.Second)}, 6, 3, false},
		{"barman plugin, ObjectStore unread: steady, archived over one interval after the oldest sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: readyRun(0, 5, 7, 6), LastArchivedAt: archivedAgo(29 * time.Second)}, 0, 3, false},
		// With the instance's last_archived_time at the oldest sweep kept,
		// both are on its clock: a skew of an hour changes nothing.
		{"steady, same clock: archived since the oldest sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: withArchived(readyRun(0, 5, 7, 6), archivedAgo(2*time.Hour)), LastArchivedAt: archivedAgo(119 * time.Minute)}, 0, 3, false},
		{"steady, same clock: nothing archived since the oldest sweep", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: withArchived(readyRun(0, 5, 7, 6), archivedAgo(-time.Hour)), LastArchivedAt: archivedAgo(-time.Hour)}, 6, 3, false},
		// CNPG P3-b: the oldest sweep read the archiver with nothing
		// archived yet; its first archive since is progress, even within
		// one interval of that sweep.
		{"steady, first archive since a sweep that read none", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: archivedNone(readyRun(0, 5, 7, 6)), LastArchivedAt: archivedAgo(59 * time.Second)}, 0, 3, false},
		{"steady, still nothing archived since a sweep that read none", barman, &ArchiverLive{ReadyWALFiles: 6, Earlier: archivedNone(readyRun(0, 5, 7, 6))}, 6, 3, false},
		{"pgBackRest plugin: steady, never archived", plugin(PgBackRestDaliboPlugin), &ArchiverLive{ReadyWALFiles: 5, Earlier: readyRun(0, 5, 5)}, 5, 3, false},
		{"barman plugin, ObjectStore unread: two sweeps are not enough", barman, &ArchiverLive{ReadyWALFiles: 8, Earlier: readyRun(0, 6)}, 0, 3, false},
		{"barman plugin, ObjectStore unread: two sweeps kept, not yet judged", barman, &ArchiverLive{ReadyWALFiles: 8, Earlier: readyRun(6)}, 0, 3, false},
		{"barman plugin, ObjectStore unread: growing over three sweeps", barman, &ArchiverLive{ReadyWALFiles: 9, Earlier: readyRun(0, 6, 6)}, 9, 3, false},
		{"pgBackRest plugin: parallelism never known", plugin(PgBackRestDaliboPlugin), &ArchiverLive{ReadyWALFiles: 5, Earlier: readyRun(0, 5, 5), LastArchivedAt: archivedAgo(5 * time.Second)}, 0, 3, false},
		// CloudNativePG's archiver is the isWALArchiver plugin, not the
		// first backup plugin: a backups-only barman-cloud entry with its
		// ObjectStore at maxParallel 16 does not raise pgBackRest's threshold.
		{"barman backups only, pgBackRest archives: the growth rule at 3", mixed(), &ArchiverLive{ReadyWALFiles: 10, Earlier: readyRun(0, 6, 8), PluginMaxParallel: ptrInt(16)}, 10, 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.live != nil {
				tt.live.ObservedAt = backlogNow
			}
			got := ArchiveBacklogOf(tt.cluster, tt.live)
			if got.Segments != tt.segments || got.Threshold != tt.threshold || got.ParallelismKnown != tt.known {
				t.Errorf("ArchiveBacklogOf = %+v, want segments %d threshold %d known %v", got, tt.segments, tt.threshold, tt.known)
			}
			if got.Behind() != (tt.segments > 0) {
				t.Errorf("Behind() = %v", got.Behind())
			}
			if got.StalledSince != nil && (!got.Behind() || tt.known) {
				t.Errorf("StalledSince = %v without a backlog of unknown parallelism", got.StalledSince)
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
	if strings.Contains(got.Message, "without reading PostgreSQL") {
		t.Errorf("unread primary worded as masked: %q", got.Message)
	}
	// A masked primary (fenced or restarting: its manager answered without
	// reaching PostgreSQL) is the same reading, worded as such, never as a
	// read that did not happen (round 17, DBA).
	masked := ArchivingOf(c, nil, true, false)
	if masked.Class != got.Class || masked.Reason != got.Reason || !strings.Contains(masked.Message, "answered without reading PostgreSQL") ||
		strings.Contains(masked.Message, "was not read") {
		t.Errorf("ArchivingOf(masked) = %+v", masked)
	}
	// The instance sweep switched off (owner decision 3, 2026-11-13): the
	// condition as CloudNativePG reports it, info, attributed and still
	// PrimaryUnread; a False condition stays critical.
	off := ArchivingOf(c, nil, false, true)
	if off.Class != ClassInfo || off.Reason != ArchivingReasonPrimaryUnread || !strings.Contains(off.Message, "per the Cluster status, not checked on the instance") {
		t.Errorf("ArchivingOf(sweep off) = %+v", off)
	}
	failing := clusterWith(nil, cond(ConditionContinuousArchiving, "False", "ContinuousArchivingFailing"))
	failing.Spec.Plugins = c.Spec.Plugins
	if got := ArchivingOf(failing, nil, false, true); got.Class != ClassCritical {
		t.Errorf("ArchivingOf(sweep off, False) = %+v, want critical", got)
	}
	// Hibernated: no primary runs, and the reading says so. It is
	// inactive, as the Cluster is (round 8, DBA): unknown would outrank
	// the hibernation in the headline.
	hibernated := clusterWith(map[string]string{AnnotationHibernation: "on"}, ok, cond(ConditionHibernation, "True", "Hibernated"))
	hibernated.Spec.Plugins = c.Spec.Plugins
	if got := Archiving(hibernated, nil); got.Class != ClassInactive || got.Reason != ArchivingReasonHibernated || !strings.Contains(got.Message, "hibernated") {
		t.Errorf("Archiving(hibernated) = %+v", got)
	}
	noCondition := clusterWith(map[string]string{AnnotationHibernation: "on"}, cond(ConditionHibernation, "True", "Hibernated"))
	noCondition.Spec.Plugins = c.Spec.Plugins
	if got := Archiving(noCondition, nil); got.Reason != ArchivingReasonHibernated || got.Class != ClassInactive {
		t.Errorf("Archiving(hibernated, no condition) = %+v", got)
	}
}

// TestArchivingTooFewSweeps: after a console restart or a failover a
// count at the threshold is judged like any first reading at it, not as
// a backlog until enough sweeps saw it.
func TestArchivingTooFewSweeps(t *testing.T) {
	c := clusterWith(nil, cond(ConditionContinuousArchiving, "True", "ContinuousArchivingSuccess"))
	c.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{}}
	live := &ArchiverLive{IsArchivingWAL: true, ReadyWALFiles: 500, ObservedAt: backlogNow}
	if got := Archiving(c, live); got.Class != ClassOK {
		t.Errorf("first sweep, 500 ready: %+v", got)
	}
	live.Earlier = readyRun(400)
	if got := Archiving(c, live); got.Reason != ArchivingReasonBacklog {
		t.Errorf("second sweep, still high: %+v", got)
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
	enabled := true
	c.Spec.ReplicaCluster = &cnpgv1.ReplicaClusterConfiguration{Enabled: &enabled, Source: "origin"}
	live := &ArchiverLive{IsArchivingWAL: true, ReadyNotReported: true, ObservedAt: backlogNow, Earlier: readyRun(0, 0, 0)}
	// CNPG P3-6: outside a replica cluster the same reading is a primary
	// mid-promotion, not a designated primary.
	primaryCluster := c.DeepCopy()
	primaryCluster.Spec.ReplicaCluster = nil
	if got := Archiving(primaryCluster, live); got.Reason != ArchivingReasonBacklogNotReported || strings.Contains(got.Message, "designated primary") || !strings.Contains(got.Message, "promotion") {
		t.Errorf("not a replica cluster: %+v", got)
	}
	if got := Archiving(c, live); got.Class != ClassUnknown || got.Reason != ArchivingReasonBacklogNotReported || !strings.Contains(got.Message, "designated primary") {
		t.Errorf("designated primary, condition True: %+v", got)
	}
	if b := ArchiveBacklogOf(c, live); !b.NotReported || b.Behind() {
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

// TestArchivingStalledArchiverUnderUnknownParallelism (round 7, DBA
// P2-5): a count held at the threshold over three sweeps, with no
// segment archived since the oldest of them, is a stalled archiver:
// warning ArchiveBacklog, the message naming since when.
// TestIsReplicaCluster follows CloudNativePG's Cluster.IsReplica: enabled
// when set, else the distributed topology's self against primary.
func TestIsReplicaCluster(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		r    *cnpgv1.ReplicaClusterConfiguration
		want bool
	}{
		{"no replica stanza", nil, false},
		{"enabled", &cnpgv1.ReplicaClusterConfiguration{Enabled: &yes}, true},
		{"disabled", &cnpgv1.ReplicaClusterConfiguration{Enabled: &no, Primary: "other"}, false},
		{"distributed, another primary", &cnpgv1.ReplicaClusterConfiguration{Primary: "east"}, true},
		{"distributed, this one primary", &cnpgv1.ReplicaClusterConfiguration{Primary: "pg"}, false},
		{"distributed, self named", &cnpgv1.ReplicaClusterConfiguration{Self: "west", Primary: "west"}, false},
	} {
		c := &cnpgv1.Cluster{}
		c.Name, c.Spec.ReplicaCluster = "pg", tc.r
		if got := IsReplicaCluster(c); got != tc.want {
			t.Errorf("%s: %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestArchivingStalledArchiverUnderUnknownParallelism(t *testing.T) {
	c := clusterWith(nil, cond(ConditionContinuousArchiving, "True", "ContinuousArchivingSuccess"))
	c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: PgBackRestDaliboPlugin}}
	live := &ArchiverLive{IsArchivingWAL: true, ReadyWALFiles: 500, Earlier: readyRun(500, 500), ObservedAt: backlogNow, LastArchivedAt: archivedAgo(time.Hour)}
	b := ArchiveBacklogOf(c, live)
	if want := backlogNow.Add(-time.Minute); !b.Behind() || b.StalledSince == nil || !b.StalledSince.Equal(want) {
		t.Fatalf("backlog %+v, want stalled since %s", b, want)
	}
	if got := Archiving(c, live); got.Class != ClassWarning || got.Reason != ArchivingReasonBacklog || !strings.Contains(got.Message, "stalled") || !strings.Contains(got.Message, "2026-11-10T02:59:00Z") {
		t.Errorf("Archiving = %+v", got)
	}
	// The archiver archived a segment since: a steady handful, ok.
	live.LastArchivedAt = archivedAgo(10 * time.Second)
	if got := Archiving(c, live); got.Class != ClassOK {
		t.Errorf("archiving that progresses: %+v", got)
	}
}
