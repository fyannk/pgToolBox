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
	"strconv"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/opstate"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// recentScope is what Recent covers, in words.
const recentScope = "rebuilt from what the Cluster's objects record, so every console replica shows the same list: " +
	"every Backup of the Cluster (on-demand from any client, or created by a ScheduledBackup, each attributed), " +
	"the latest restart and reload annotations (set by any client) and the console's restore-drill record; " +
	"plus the primary changes (switchovers and failovers) this console observed, which it cannot tell apart from a promote " +
	"unless this replica issued it. The API server's audit log is the complete record, and names who asked."

// markerSkew is how far a marker's time may lie from the clock it is
// checked against: past it, the marker is not the console's (a console
// writes its acceptance instant, which is now, into a create or patch
// that lands within it).
const markerSkew = time.Minute

// scheduledBackupLabel is CloudNativePG's label naming the
// ScheduledBackup that created a Backup, set whatever the schedule's
// backupOwnerReference (the owner reference is set only for "self").
const scheduledBackupLabel = "cnpg.io/scheduled-backup"

// recentEntry is one recent operation and where it was found; change is
// set for a primary change the timeline observed (listed as recorded,
// never followed).
type recentEntry struct {
	id     ID
	in     viewmodel.OperationRecord
	from   string
	change *opstate.PrimaryChange
}

// fromCluster rebuilds the recent operations of st from its objects (the
// read model, so every replica derives the same list) and from this
// console's timeline, and names what it could not read.
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
		gaps = append(gaps, "the console cannot read Backups in "+ns+" now: backups are not listed")
	}
	for _, b := range st.Backups {
		if id, from, ok := backupEntry(b, ns, name); ok {
			add(id, from)
		}
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
	changes, gap := observedChanges(st.Observed, ns, name)
	out = append(out, changes...)
	if gap != "" {
		gaps = append(gaps, gap)
	}
	return out, gaps
}

// backupEntry is the recent entry of one Backup of the Cluster, with its
// origin named: the console's requested-at annotation, when it was
// written in the create itself (so the handle is the one the issuing
// replica returned), else the ScheduledBackup that created it, else an
// on-demand Backup from any client.
func backupEntry(b *cnpgv1.Backup, ns, cluster string) (ID, string, bool) {
	created := b.CreationTimestamp.Time
	if b.Spec.Cluster.Name != cluster || created.IsZero() {
		return ID{}, "", false
	}
	id := ID{Kind: viewmodel.OperationBackup, Namespace: ns, Cluster: cluster, Created: b.Name, AcceptedAt: created.Unix()}
	if v, ok := b.Annotations[ops.AnnotationRequestedAt]; ok {
		// The console writes the annotation in the create itself: a time
		// far from the Backup's creation is not its request.
		if at, err := time.Parse(time.RFC3339, v); err == nil && at.Sub(created).Abs() <= markerSkew {
			id.AcceptedAt = at.Unix()
			return id, "Backup " + b.Name + " metadata.annotations[" + ops.AnnotationRequestedAt + "]", true
		}
	}
	for _, o := range b.OwnerReferences {
		if o.Kind == "ScheduledBackup" && strings.HasPrefix(o.APIVersion, cnpgv1.SchemeGroupVersion.Group+"/") {
			return id, "Backup " + b.Name + " (created by ScheduledBackup " + o.Name + ": metadata.ownerReferences)", true
		}
	}
	if sb := b.Labels[scheduledBackupLabel]; sb != "" {
		return id, "Backup " + b.Name + " (created by ScheduledBackup " + sb + ": metadata.labels[" + scheduledBackupLabel + "])", true
	}
	return id, "Backup " + b.Name + " (on-demand, any client: metadata.creationTimestamp)", true
}

// observedChanges are the primary changes this console's timeline
// retains, and what it may miss.
func observedChanges(o opstate.Observed, ns, cluster string) ([]recentEntry, string) {
	if !o.Enabled {
		return nil, "primary changes (switchovers, failovers) are not listed: this console keeps no timeline (HISTORY_ENABLED is false)"
	}
	var gap string
	since := "the console started"
	if !o.Since.IsZero() {
		since = o.Since.UTC().Format(time.RFC3339)
	}
	switch {
	case !o.Persistent:
		gap = "primary changes before " + since + " are not listed: this console's timeline is not kept across restarts here, and each replica keeps its own"
	case o.Evicted:
		gap = "primary changes before " + since + " are not listed: the timeline's bounds dropped older events"
	}
	out := make([]recentEntry, 0, len(o.PrimaryChanges))
	for i := range o.PrimaryChanges {
		c := o.PrimaryChanges[i]
		if c.At.IsZero() || c.To == "" {
			continue
		}
		out = append(out, recentEntry{
			id: ID{Kind: viewmodel.OperationPrimaryChange, Namespace: ns, Cluster: cluster, Instance: c.To, AcceptedAt: c.At.Unix()},
			in: viewmodel.RecordTimeline, from: "this console's timeline (Cluster.status.currentPrimary, observed)", change: &c,
		})
	}
	return out, gap
}

// changeText says what the timeline saw, and no more: the cause only
// when the announcing phase was seen, the window when the console was
// not looking.
func changeText(c *opstate.PrimaryChange) string {
	text := "primary changed from " + c.From + " to " + c.To
	if c.From == "" {
		text = "primary changed to " + c.To
	}
	switch {
	case c.Cause != "" && !c.Uncertain:
		text += " (" + c.Cause + ")"
	case c.Cause != "":
		text += " (probably a " + c.Cause + ")"
	default:
		text += " (a switchover or a failover: the console did not see which)"
	}
	if !c.Since.IsZero() {
		text += "; it happened while the console was not looking, after " + c.Since.UTC().Format(time.RFC3339)
	}
	return text
}

// changeOperation is a primary change as a recorded, settled entry: it
// is not an operation the console follows.
func changeOperation(e recentEntry, st ClusterState) viewmodel.Operation {
	c := e.change
	src := "Cluster.status.currentPrimary (observed by this console)"
	if c.Reported {
		src = "Cluster.status.currentPrimaryTimestamp"
	}
	op := viewmodel.Operation{
		OperationID: e.id.Encode(), Kind: viewmodel.OperationPrimaryChange, Target: e.id.Ref(), Instance: c.To,
		AcceptedAt: c.At.UTC(), Deadline: c.At.UTC(), Phase: viewmodel.OpPhaseSettled,
		Steps:      []viewmodel.OperationStep{{Phase: viewmodel.OpPhaseSettled, At: c.At.UTC(), Source: src, Text: changeText(c)}},
		RecordedIn: e.in, RecordedFrom: e.from,
	}
	if st.Cluster != nil {
		op.Target.UID = string(st.Cluster.UID)
	}
	op.Health = viewmodel.OperationHealth(op.Phase, false)
	return op
}

// recent merges what the cluster records and the console observed with
// what only this replica remembers, newest first, at most
// RecentPerCluster, and says how many older entries the cap cut.
func (s *Service) recentOf(st ClusterState, mine []record) ([]recentEntry, []string) {
	out, gaps := s.fromCluster(st)
	seen := make(map[string]bool, len(out))
	for _, e := range out {
		seen[e.id.Encode()] = true
	}
	for _, r := range mine {
		k := r.id.Encode()
		if seen[k] {
			continue
		}
		seen[k] = true
		e := recentEntry{id: r.id, in: viewmodel.RecordReplica, from: "this console replica's memory, since it started"}
		if r.id.Kind == viewmodel.OperationPromote {
			// The promote this replica issued and the change its timeline
			// observed are one entry: the promote, followed as before.
			if i := matchingChange(out, r.id); i >= 0 {
				c := out[i].change
				e.from = "this console replica's memory (it issued this promote); its timeline observed, at " +
					c.At.UTC().Format(time.RFC3339) + ": " + changeText(c)
				out = slices.Delete(out, i, i+1)
			}
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b recentEntry) int {
		if c := cmp.Compare(b.id.AcceptedAt, a.id.AcceptedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.id.Encode(), b.id.Encode())
	})
	if cut := len(out) - s.opts.RecentPerCluster; cut > 0 {
		out = out[:s.opts.RecentPerCluster]
		gaps = append(gaps, strconv.Itoa(cut)+" older entries are not listed: the list keeps the newest "+strconv.Itoa(s.opts.RecentPerCluster))
	}
	return out, gaps
}

// matchingChange is the index in out of the observed primary change a
// promote this replica issued caused: to its target, within its
// deadline; -1 when none.
func matchingChange(out []recentEntry, id ID) int {
	from, to := id.Accepted().Add(-markerSkew), id.Accepted().Add(markerSkew)
	if d, ok := describe(viewmodel.OperationPromote); ok {
		to = to.Add(d.deadline)
	}
	for i, e := range out {
		if e.change != nil && e.change.To == id.Instance && !e.change.At.Before(from) && !e.change.At.After(to) {
			return i
		}
	}
	return -1
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
