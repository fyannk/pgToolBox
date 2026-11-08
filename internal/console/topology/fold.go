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

package topology

import (
	"fmt"
	"strings"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Foldable are the frames a reader may fold. A folded frame keeps its
// place and its heading but holds one summary box: how many objects it
// stands for, their worst state, and where to see each. The wires that
// crossed into the frame end on the summary box; the wires inside it are
// gone. The drawing is laid out again for the folded input, so the rest
// closes up rather than leaving a hole. The object store is one box and
// does not fold.
var Foldable = []string{FramePool, FrameCluster, FrameBackup, FrameKubeStorage}

// foldedName is the Instance.Name of the folded cluster's box: claims and
// the base-backup source move onto it. No pod can carry the name.
const foldedName = "\x00cluster"

// folds is what the layout must do differently for a folded input.
type folds struct {
	pool, cluster, backup, storage bool
	// fronts are the Service types the folded poolers fed, rw first.
	fronts []string
	// volumesFrom are the instances whose claims went into the folded
	// storage box, in drawing order.
	volumesFrom []string
}

// count is how many objects a box stands for: a "+N more" box says so.
func count(b Box) int {
	if b.Count > 0 {
		return b.Count
	}
	return 1
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// names lists the boxes' labels for a summary row; the box clips it.
func names(boxes []Box) string {
	out := make([]string, 0, len(boxes))
	for _, b := range boxes {
		out = append(out, b.Label)
	}
	return strings.Join(out, " · ")
}

func worst(boxes []Box) viewmodel.Health {
	states := make([]viewmodel.Health, 0, len(boxes))
	for _, b := range boxes {
		states = append(states, b.State)
	}
	return viewmodel.Worst(states...)
}

// fold replaces each requested frame's boxes with its summary box. A frame
// with nothing in it does not fold. The frames fold in drawing order: the
// storage summary reads whether the cluster folded.
func fold(in Input) (Input, folds) {
	var f folds
	want := map[string]bool{}
	for _, k := range in.Fold {
		want[k] = true
	}
	if want[FramePool] && len(in.Poolers) > 0 {
		foldPoolers(&in, &f)
	}
	if want[FrameCluster] && len(in.Instances) > 0 {
		foldCluster(&in, &f)
	}
	if want[FrameBackup] && len(in.Schedules) > 0 {
		foldBackups(&in, &f)
	}
	if want[FrameKubeStorage] && (in.Snapshot != nil || len(in.Claims) > 0) {
		foldStorage(&in, &f)
	}
	return in, f
}

// foldPoolers summarises the poolers in one box that remembers which
// Service types they fronted, so its wires still reach each of them.
func foldPoolers(in *Input, f *folds) {
	f.pool = true
	boxes := make([]Box, 0, len(in.Poolers))
	n := 0
	typed := map[string]bool{}
	for _, p := range in.Poolers {
		boxes = append(boxes, p.Box)
		n += count(p.Box)
		typed[p.Type] = true
	}
	for _, typ := range []string{"rw", "ro", "r"} {
		if typed[typ] {
			f.fronts = append(f.fronts, typ)
		}
	}
	label := plural(n, "pooler", "poolers")
	in.Poolers = []Pooler{{Type: typeFolded, Box: Box{
		ID: "fold-" + FramePool, Kind: KindPooler, Label: label, State: worst(boxes), Link: "objects",
		Rows: []Row{
			{Class: "label", Text: label},
			{Class: "sub", Text: "pgbouncer " + strings.Join(f.fronts, " · ")},
			{Class: "disk", Text: names(boxes)},
		},
	}}}
}

// foldCluster summarises the instances and the Services in one box, and
// moves the claims and the base-backup source of its members onto it.
func foldCluster(in *Input, f *folds) {
	f.cluster = true
	boxes := make([]Box, 0, len(in.Instances))
	n, replicas := 0, 0
	primary := ""
	member := map[string]bool{}
	for _, i := range in.Instances {
		boxes = append(boxes, i.Box)
		n += count(i.Box)
		member[i.Name] = i.Name != ""
		switch i.Role {
		case RolePrimary:
			primary = i.Name
		case RoleReplica:
			replicas++
		}
	}
	label := plural(n, "instance", "instances")
	role := ""
	kind := KindInstance
	if primary != "" {
		// It stands for the primary as much as for the rest: WAL leaves it.
		role, kind = RolePrimary, KindPrimary
	}
	in.Instances = []Instance{{Name: foldedName, Role: role, Box: Box{
		ID: "fold-" + FrameCluster, Kind: kind, Label: label, State: worst(boxes), Link: "instances",
		Rows: clusterRows(label, primary, replicas, in.Services),
	}}}
	in.Services = nil
	claims := make([]Claim, 0, len(in.Claims))
	for _, c := range in.Claims {
		if member[c.Instance] {
			c.Instance = foldedName
		}
		claims = append(claims, c)
	}
	in.Claims = claims
	if in.Base != nil && member[in.Base.Instance] {
		b := *in.Base
		b.Instance = foldedName
		in.Base = &b
	}
}

// clusterRows words the folded cluster: how many instances, the primary
// (or its absence), the replicas, the Service types it hides.
func clusterRows(label, primary string, replicas int, services []Service) []Row {
	rows := []Row{{Class: "label", Text: label}}
	if primary != "" {
		rows = append(rows, Row{Class: "sub", Text: "primary " + primary})
	} else {
		rows = append(rows, Row{Class: "sub", Text: "no primary reported"})
	}
	if replicas > 0 {
		rows = append(rows, Row{Class: "sub", Text: plural(replicas, "replica", "replicas")})
	}
	if len(services) > 0 {
		types := make([]string, 0, len(services))
		for _, s := range services {
			types = append(types, s.Type)
		}
		rows = append(rows, Row{Class: "disk", Text: "Services " + strings.Join(types, " · ")})
	}
	return rows
}

// foldBackups summarises the ScheduledBackups in one box.
func foldBackups(in *Input, f *folds) {
	f.backup = true
	n := 0
	for _, s := range in.Schedules {
		n += count(s)
	}
	label := plural(n, "backup schedule", "backup schedules")
	in.Schedules = []Box{{
		ID: "fold-" + FrameBackup, Kind: KindSchedule, Label: label, State: worst(in.Schedules), Link: "backups",
		Rows: []Row{{Class: "label", Text: label}, {Class: "disk", Text: names(in.Schedules)}},
	}}
}

// foldStorage summarises the claims and the volume snapshots in one box,
// standing in the snapshot's slot, and records which instances fed it so
// the layout can run their volume wires into it.
func foldStorage(in *Input, f *folds) {
	f.storage = true
	var boxes []Box
	n := 0
	held := map[string]bool{}
	for _, c := range in.Claims {
		boxes = append(boxes, c.Box)
		n += count(c.Box)
		held[c.Instance] = true
	}
	for _, i := range in.Instances {
		if i.Name != "" && held[i.Name] {
			f.volumesFrom = append(f.volumesFrom, i.Name)
		}
	}
	label := plural(n, "volume", "volumes")
	if n == 0 {
		label = "Volume snapshots"
	}
	rows := []Row{{Class: "label", Text: label}}
	if len(f.volumesFrom) > 0 && !f.cluster {
		rows = append(rows, Row{Class: "sub", Text: "of " + plural(len(f.volumesFrom), "instance", "instances")})
	}
	if in.Snapshot != nil {
		boxes = append(boxes, *in.Snapshot)
		if n > 0 {
			rows = append(rows, Row{Class: "sub", Text: "and volume snapshots"})
		}
	}
	in.Snapshot = &Box{ID: "fold-" + FrameKubeStorage, Kind: KindVolume, Label: label, State: worst(boxes), Link: "storage", Rows: rows}
	in.Claims = nil
}
