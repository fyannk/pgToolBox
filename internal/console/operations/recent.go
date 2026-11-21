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

package operations

import (
	"cmp"
	"slices"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// recentScope is what Recent covers, in words.
const recentScope = "rebuilt from what the Cluster's objects record, so every console replica shows the same list: " +
	"Backups carrying the " + ops.AnnotationRequestedAt + " annotation (a console sets it; so may any client that may create a Backup), the latest restart and reload annotations (set by any client) " +
	"and the console's restore-drill record. A promote leaves no marker the console can tell from a failover: " +
	"only the replica that issued it lists it, until it restarts. The API server's audit log is the complete record, and names who asked."

// markerSkew is how far a marker's time may lie from the clock it is
// checked against: past it, the marker is not the console's (a console
// writes its acceptance instant, which is now, into a create or patch
// that lands within it).
const markerSkew = time.Minute

// recentEntry is one recent operation and where it was found.
type recentEntry struct {
	id   ID
	in   viewmodel.OperationRecord
	from string
}

// fromCluster rebuilds the recent operations of st from the markers its
// objects carry (the read model, so every replica derives the same
// list), and names what it could not read.
func (s *Service) fromCluster(st ClusterState) (out []recentEntry, gaps []string) {
	c := st.Cluster
	if c == nil {
		return nil, nil
	}
	ns, name := c.Namespace, c.Name
	add := func(id ID, from string) {
		// Only a handle the tracker accepts is listed: a marker another
		// client wrote in another shape is not an operation the console
		// can follow.
		if _, err := ParseID(id.Encode()); err == nil {
			out = append(out, recentEntry{id: id, in: viewmodel.RecordCluster, from: from})
		}
	}
	for key, kind := range map[string]viewmodel.OperationKind{ops.RestartAnnotation: viewmodel.OperationRestart, ops.ReloadAnnotation: viewmodel.OperationReload} {
		v, ok := c.Annotations[key]
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, v)
		if err != nil || at.After(s.opts.Now().Add(markerSkew)) {
			// A marker dated in the future would sort first and never
			// time out: it is not an operation's acceptance instant.
			continue
		}
		add(ID{Kind: kind, Namespace: ns, Cluster: name, Marker: v, AcceptedAt: at.Unix()},
			"Cluster.metadata.annotations["+key+"] (the latest one, set by any client)")
	}
	if !st.BackupsListed {
		gaps = append(gaps, "the console cannot read Backups in "+ns+" now: on-demand backups are not listed")
	}
	for _, b := range st.Backups {
		v, ok := b.Annotations[ops.AnnotationRequestedAt]
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339, v)
		created := b.CreationTimestamp.Time
		if err != nil || created.IsZero() || at.Sub(created).Abs() > markerSkew {
			// The console writes the annotation in the create itself: a
			// time far from the Backup's creation is not its request.
			continue
		}
		add(ID{Kind: viewmodel.OperationBackup, Namespace: ns, Cluster: name, Created: b.Name, AcceptedAt: at.Unix()},
			"Backup "+b.Name+" metadata.annotations["+ops.AnnotationRequestedAt+"]")
	}
	if d := s.opts.Drills; d != nil && d.Records != nil {
		live := drills.Live{UID: string(c.UID), CreatedAt: c.CreationTimestamp.Time}
		for _, r := range d.Records.SourceOf(drills.SourceKey{Namespace: ns, Name: name}, live) {
			add(ID{
				Kind: viewmodel.OperationRestoreDrill, Namespace: ns, Cluster: name,
				Created: r.Drill.Name, CreatedNamespace: r.Drill.Namespace, AcceptedAt: r.StartedAt.Unix(),
			}, "the console's restore-drill record (rebuilt from the drill Cluster's sealed annotation)")
		}
		if !d.Records.Persistent() {
			gaps = append(gaps, "restore drills finished before "+d.Records.RecordedSince().Format(time.RFC3339)+
				" are not listed: the drill record is not kept across restarts here")
		}
	}
	return out, gaps
}

// recent merges what the cluster records with what only this replica
// remembers, newest first, at most RecentPerCluster.
func (s *Service) recentOf(st ClusterState, mine []record) ([]recentEntry, []string) {
	out, gaps := s.fromCluster(st)
	seen := make(map[string]bool, len(out))
	for _, e := range out {
		seen[e.id.Encode()] = true
	}
	for _, r := range mine {
		if k := r.id.Encode(); !seen[k] {
			seen[k] = true
			out = append(out, recentEntry{id: r.id, in: viewmodel.RecordReplica, from: "this console replica's memory, since it started"})
		}
	}
	slices.SortFunc(out, func(a, b recentEntry) int {
		if c := cmp.Compare(b.id.AcceptedAt, a.id.AcceptedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.id.Encode(), b.id.Encode())
	})
	if len(out) > s.opts.RecentPerCluster {
		out = out[:s.opts.RecentPerCluster]
	}
	return out, gaps
}

// asRecorded marks op as found on the cluster: its request step is
// attributed to the marker, not to this console, and a restart or reload
// past its deadline is closed instead of judged on the Cluster's live
// state (a months-old restartedAt would read as "2 of 3 restarted"
// during any later rollout).
func asRecorded(op viewmodel.Operation, e recentEntry, now time.Time) viewmodel.Operation {
	op.RecordedIn, op.RecordedFrom = e.in, e.from
	if e.in != viewmodel.RecordCluster {
		return op
	}
	if len(op.Steps) > 0 {
		op.Steps[0].Source = e.from
	}
	marker := e.id.Kind == viewmodel.OperationRestart || e.id.Kind == viewmodel.OperationReload
	if marker && now.After(op.Deadline) && len(op.Steps) >= 2 {
		op.Steps = append(op.Steps[:2:2], viewmodel.OperationStep{
			Phase: viewmodel.OpPhaseNotObservable, At: op.Deadline, Source: e.from,
			Text: "recorded on the Cluster at " + op.AcceptedAt.Format(time.RFC3339) + "; not followed after its deadline",
		})
		op.Phase, op.Blocked = viewmodel.OpPhaseNotObservable, false
		op.Health = viewmodel.OperationHealth(op.Phase, false)
	}
	return op
}
