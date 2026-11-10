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
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
)

// ReadySample is one sweep's readyWalFiles of an instance; Files is nil
// when that read failed (a gap, which neither extends nor breaks a run).
type ReadySample struct {
	At    time.Time `json:"at"`
	Files *int      `json:"files,omitempty"`
}

const (
	// BacklogSweeps is how many consecutive sweeps must see readyWalFiles
	// at the threshold before the archiver counts as behind, when its
	// parallelism is known: one reading at the threshold is a parallel
	// archiver's segments uploaded ahead, or a burst.
	BacklogSweeps = 2
	// BacklogSweepsUnknownParallelism is the same when the archiver's
	// parallelism is not known (a pgBackRest plugin, whose async push
	// keeps segments pushed ahead as .ready, or the barman-cloud plugin
	// when its ObjectStore was not read): over the newest that many
	// sweeps the backlog must also grow, never shrinking from one sweep
	// to the next and larger at the newest than at the first, since a
	// healthy parallel archiver holds a steady handful of .ready files.
	BacklogSweepsUnknownParallelism = 3
)

// ArchiveBacklogThreshold is the readyWalFiles count at which c's
// archiver may be behind: ArchiveBacklogSegments, or one above the
// archiver's parallelism when that is larger, the in-tree wal.maxParallel
// or the barman-cloud plugin's ObjectStore wal.maxParallel
// (pluginMaxParallel, nil when the ObjectStore was not read).
func ArchiveBacklogThreshold(c *cnpgv1.Cluster, pluginMaxParallel *int) int {
	threshold, _ := archiverThreshold(c, pluginMaxParallel)
	return threshold
}

// archiverThreshold is ArchiveBacklogThreshold and whether the
// archiver's parallelism is known: the in-tree archiver's always is; the
// barman-cloud plugin's when its ObjectStore was read; another plugin's
// never (the console reads none of their objects). The archiver is
// CloudNativePG's (WALArchiver): an enabled plugin with isWALArchiver,
// else the in-tree object store, else the first backup plugin.
func archiverThreshold(c *cnpgv1.Cluster, pluginMaxParallel *int) (int, bool) {
	threshold := ArchiveBacklogSegments
	if c == nil {
		return threshold, true
	}
	archiver := WALArchiver(c)
	switch {
	case archiver.InTree:
		if wal := c.Spec.Backup.BarmanObjectStore.Wal; wal != nil {
			threshold = max(threshold, wal.MaxParallel+1)
		}
		return threshold, true
	case archiver.Plugin == "":
		return threshold, true
	case archiver.Plugin != BarmanCloudPlugin || pluginMaxParallel == nil:
		return threshold, false
	}
	return max(threshold, *pluginMaxParallel+1), true
}

// Archiver names the component that archives a Cluster's WAL.
type Archiver struct {
	// Plugin is the archiving plugin's name; empty for the in-tree
	// archiver or none.
	Plugin string
	// Parameters are that plugin's spec.plugins[].parameters.
	Parameters map[string]string
	// InTree is the in-tree Barman object store (spec.backup.barmanObjectStore).
	InTree bool
}

// WALArchiver is the component that archives c's WAL, by the instance
// manager's own rule (pkg/management/postgres/archiver/archiver.go
// v1.30.1:174, :297): the first enabled plugin with isWALArchiver true
// (api/v1/cluster_funcs.go v1.30.1:1594-1601,
// GetEnabledWALArchivePluginName; enabled unless spec.plugins[].enabled
// is false, :411-416); without one, the in-tree barmanObjectStore; and
// without that, the first backup plugin (IsBackupPlugin), which the
// instance manager still asks to archive for compatibility with the old
// API (archiver.go v1.30.1:165-170). A Cluster that archives nowhere has
// the zero value.
func WALArchiver(c *cnpgv1.Cluster) Archiver {
	if c == nil {
		return Archiver{}
	}
	for _, plugin := range c.Spec.Plugins {
		if (plugin.Enabled == nil || *plugin.Enabled) && plugin.IsWALArchiver != nil && *plugin.IsWALArchiver {
			return Archiver{Plugin: plugin.Name, Parameters: plugin.Parameters}
		}
	}
	if c.Spec.Backup != nil && c.Spec.Backup.BarmanObjectStore != nil {
		return Archiver{InTree: true}
	}
	for _, plugin := range c.Spec.Plugins {
		if IsBackupPlugin(plugin) {
			return Archiver{Plugin: plugin.Name, Parameters: plugin.Parameters}
		}
	}
	return Archiver{}
}

// ArchiveBacklog is the archiver's backlog as the sweeps saw it.
type ArchiveBacklog struct {
	// Segments is the current readyWalFiles when the backlog persists
	// (Behind); 0 otherwise.
	Segments int `json:"segments"`
	// Threshold is the count at which a reading is part of a backlog.
	Threshold int `json:"threshold"`
	// ParallelismKnown says whether Threshold follows the archiver's
	// declared parallelism; when not, the backlog must also grow.
	ParallelismKnown bool `json:"parallelismKnown"`
	// Unconfirmed is the current readyWalFiles when it is at the
	// threshold but the sweeps kept are too few to tell a persistent
	// backlog from a burst (a console that just started); 0 otherwise.
	Unconfirmed int `json:"unconfirmed,omitempty"`
}

// Behind reports a backlog that persists.
func (b ArchiveBacklog) Behind() bool { return b.Segments > 0 }

// ArchiveBacklogOf reads whether the archiver is behind: the current
// reading and the run of earlier sweeps just before it at or above
// ArchiveBacklogThreshold (a failed read is skipped; a reading below the
// threshold ends the run). With the parallelism known the run must span
// BacklogSweeps sweeps; without it, BacklogSweepsUnknownParallelism
// sweeps whose newest readings never shrink and end larger than they
// began. A run too short that reaches back to the oldest sweep kept is
// Unconfirmed: nothing kept says it is a burst. A nil reading is no
// backlog.
func ArchiveBacklogOf(c *cnpgv1.Cluster, live *ArchiverLive) ArchiveBacklog {
	var pluginMaxParallel *int
	if live != nil {
		pluginMaxParallel = live.PluginMaxParallel
	}
	threshold, known := archiverThreshold(c, pluginMaxParallel)
	out := ArchiveBacklog{Threshold: threshold, ParallelismKnown: known}
	if live == nil || live.ReadyWALFiles < threshold {
		return out
	}
	// run holds the readings newest first, starting with the current one.
	run := []ReadySample{{At: live.ObservedAt, Files: &live.ReadyWALFiles}}
	reachedOldest := true
	for i := len(live.Earlier) - 1; i >= 0; i-- {
		s := live.Earlier[i]
		if s.Files == nil {
			continue
		}
		if *s.Files < threshold {
			reachedOldest = false
			break
		}
		run = append(run, s)
	}
	need := BacklogSweeps
	if !known {
		need = BacklogSweepsUnknownParallelism
	}
	if len(run) < need {
		if reachedOldest {
			out.Unconfirmed = live.ReadyWALFiles
		}
		return out
	}
	if !known && !growing(run[:need]) {
		return out
	}
	out.Segments = live.ReadyWALFiles
	return out
}

// growing reports readings (newest first) that never shrink toward the
// newest and end larger than they began.
func growing(run []ReadySample) bool {
	for i := 0; i+1 < len(run); i++ {
		if *run[i].Files < *run[i+1].Files {
			return false
		}
	}
	return *run[0].Files > *run[len(run)-1].Files
}
