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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func box(id, kind, label string, rows int) Box {
	b := Box{ID: id, Kind: kind, Label: label, State: viewmodel.HealthOK, Rows: []Row{{Class: "label", Text: label}}}
	for i := 1; i < rows; i++ {
		b.Rows = append(b.Rows, Row{Class: "sub", Text: fmt.Sprintf("fact %d", i)})
	}
	return b
}

func instance(name, role string) Instance {
	kind := KindReplica
	if role == RolePrimary {
		kind = KindPrimary
	}
	return Instance{Box: box("inst-"+name, kind, name, 5), Name: name, Role: role}
}

// richInput has everything the drawing can show: two poolers, two
// services, a primary with two replicas, one claim per instance, a
// schedule, the object store, and a base backup
// the operator served from a replica — CloudNativePG's default target,
// the case the drawing must not redraw as if it had left the primary.
func richInput() Input {
	store := box("store", KindStore, "store", 3)
	return Input{
		Poolers: []Pooler{
			{Box: box("pool-ro", KindPooler, "orders-pool-ro", 3), Type: "ro"},
			{Box: box("pool-rw", KindPooler, "orders-pool-rw", 3), Type: "rw"},
		},
		Services: []Service{
			{Box: box("svc-ro", KindService, "orders-ro", 3), Type: "ro"},
			{Box: box("svc-rw", KindService, "orders-rw", 3), Type: "rw"},
		},
		Instances: []Instance{instance("orders-1", RolePrimary), instance("orders-2", RoleReplica), instance("orders-3", RoleReplica)},
		Schedules: []Box{box("sched-daily", KindSchedule, "daily", 3)},
		Store:     &store,
		Claims: []Claim{
			{Box: box("pvc-1", KindVolume, "orders-1", 3), Instance: "orders-1"},
			{Box: box("pvc-2", KindVolume, "orders-2", 3), Instance: "orders-2"},
			{Box: box("pvc-3", KindVolume, "orders-3", 3), Instance: "orders-3"},
		},
		WAL:  true,
		Base: &Base{Instance: "orders-2", Target: TargetStore, Label: "base backup · barman-cloud"},
	}
}

func draw(in Input) viewmodel.Topology {
	d := Layout(in)
	return viewmodel.Topology{Width: d.Width, Height: d.Height, Frames: d.Frames, Edges: d.Edges, Nodes: d.Nodes, Dots: d.Dots, Legend: d.Legend}
}

func nodes(t viewmodel.Topology) map[string]viewmodel.TopoNode {
	out := map[string]viewmodel.TopoNode{}
	for _, n := range t.Nodes {
		out[n.ID] = n
	}
	return out
}

func checkClean(t *testing.T, tp viewmodel.Topology) {
	t.Helper()
	for _, v := range Check(tp) {
		t.Error(v)
	}
}

// The placement is a set of stated rules, so each one is asserted as
// geometry.
func TestLayoutPinsEveryStatedPlacement(t *testing.T) {
	t.Parallel()
	tp := draw(richInput())
	checkClean(t, tp)
	n := nodes(tp)
	above := func(top, below string) {
		t.Helper()
		if n[top].Y+n[top].H > n[below].Y {
			t.Errorf("%s (y=%d..%d) is not above %s (y=%d)", top, n[top].Y, n[top].Y+n[top].H, below, n[below].Y)
		}
	}
	leftOf := func(l, r string) {
		t.Helper()
		if n[l].X+n[l].W > n[r].X {
			t.Errorf("%s does not stand left of %s", l, r)
		}
	}
	// rw above ro, for poolers and services alike, whatever the input order.
	above("pool-rw", "pool-ro")
	above("svc-rw", "svc-ro")
	// Poolers left of services left of the primary left of its replicas
	// left of the storage.
	leftOf("pool-rw", "svc-rw")
	leftOf("svc-rw", "inst-orders-1")
	leftOf("inst-orders-1", "inst-orders-2")
	leftOf("inst-orders-2", "pvc-1")
	leftOf("inst-orders-2", "store")
	// The top band: the schedule above the primary and centred on it,
	// the object store above the snapshots, the snapshots above every
	// claim.
	above("sched-daily", "inst-orders-1")
	above("store", "pvc-1")
	if sc, pc := n["sched-daily"].X+n["sched-daily"].W/2, n["inst-orders-1"].X+n["inst-orders-1"].W/2; sc != pc {
		t.Errorf("the schedule (centre %d) is not centred on the primary (centre %d)", sc, pc)
	}
	// The write service level with the primary, the read service level
	// with the first replica, a lone pooler level with its service.
	level := func(a, b string) {
		t.Helper()
		if ca, cb := n[a].Y*2+n[a].H, n[b].Y*2+n[b].H; ca != cb {
			t.Errorf("%s (centre %d) is not level with %s (centre %d)", a, ca/2, b, cb/2)
		}
	}
	level("svc-rw", "inst-orders-1")
	level("svc-ro", "inst-orders-2")
	level("pool-rw", "svc-rw")
	level("pool-ro", "svc-ro")
	// Each claim is level with its instance.
	level("pvc-1", "inst-orders-1")
	level("pvc-2", "inst-orders-2")
	level("pvc-3", "inst-orders-3")
	// Consecutive instances stagger their claims over two columns, and
	// the columns alternate.
	if n["pvc-1"].X >= n["pvc-2"].X {
		t.Errorf("the second claim column (x=%d) does not stand right of the first (x=%d)", n["pvc-2"].X, n["pvc-1"].X)
	}
	if n["pvc-1"].X != n["pvc-3"].X {
		t.Errorf("the third claim (x=%d) does not return to the first column (x=%d)", n["pvc-3"].X, n["pvc-1"].X)
	}

	// The volume snapshots stand on top of the Kubernetes storage
	// frame, under the object store, above every claim.
	in := richInput()
	in.Snapshot = &Box{ID: "snapshot", Kind: KindSnapshot, Label: "Volume snapshots", Rows: []Row{{Class: "label", Text: "Volume snapshots"}}}
	snap := draw(in)
	checkClean(t, snap)
	sn := nodes(snap)
	for _, below := range []string{"pvc-1", "pvc-2", "pvc-3"} {
		if sn["store"].Y+sn["store"].H > sn["snapshot"].Y || sn["snapshot"].Y+sn["snapshot"].H > sn[below].Y {
			t.Errorf("store, snapshot and %s are not stacked top to bottom", below)
		}
	}

	// Five dotted frames, each carrying its domain kind.
	got := make([]string, 0, len(tp.Frames))
	for _, f := range tp.Frames {
		got = append(got, f.Label+"/"+f.Kind)
	}
	if want := "Poolers/pool,Cluster/cluster,Backups/backup,Kubernetes storage/kubeStorage,Object storage/objectStorage"; strings.Join(got, ",") != want {
		t.Errorf("frames = %s, want %s", strings.Join(got, ","), want)
	}
}

// A fanned flow leaves its box once, every tee carries a dot in the
// flow's own style, and each flow of the backup path leaves the box the
// operator attributed it to.
func TestLayoutTrunksItsFans(t *testing.T) {
	t.Parallel()
	tp := draw(richInput())
	n := nodes(tp)
	kinds := map[string][]viewmodel.TopoEdge{}
	for _, e := range tp.Edges {
		kinds[e.Kind] = append(kinds[e.Kind], e)
		if strings.Contains(e.Path, "C") {
			t.Errorf("edge %s is a cubic curve rather than an orthogonal run: %q", e.Kind, e.Path)
		}
	}
	prefix := func(path string) string { before, _, _ := strings.Cut(path, " L"); return before }

	// Two replicas, two replication branches, one shared exit.
	repl := kinds[EdgeReplicate]
	if len(repl) != 2 {
		t.Fatalf("%d replication branches, want 2", len(repl))
	}
	if prefix(repl[0].Path) != prefix(repl[1].Path) {
		t.Errorf("replication branches leave from different points: %q vs %q", repl[0].Path, repl[1].Path)
	}
	// The branches arrive on the replicas' left edges, across a corridor
	// wider than the column gap so the label can stand upright in it.
	replLeft := n["inst-orders-2"].X
	for _, r := range repl {
		_, last, _ := ends(r.Path)
		if last[0] != replLeft {
			t.Errorf("a replication branch ends at x=%d, want the replicas' left edge %d", last[0], replLeft)
		}
	}
	if corridor := replLeft - (n["inst-orders-1"].X + n["inst-orders-1"].W); corridor <= colGap {
		t.Errorf("the primary-to-replica corridor is %d wide, want more than %d", corridor, colGap)
	}
	labelled := 0
	for _, r := range repl {
		if r.Label != "" {
			labelled++
			if r.Label != "replication" || !r.LabelVertical {
				t.Errorf("replication label %q vertical=%v", r.Label, r.LabelVertical)
			}
		}
	}
	if labelled != 1 {
		t.Errorf("%d replication labels, want 1", labelled)
	}

	// The read fan: one pooler wire and one branch per replica.
	if got := len(kinds[EdgeRead]); got != 3 {
		t.Errorf("%d read wires, want 3", got)
	}
	// Volume wires of lone, level claims are straight: no corner.
	if got := len(kinds[EdgeVolume]); got != 3 {
		t.Fatalf("%d volume wires, want 3", got)
	}
	for _, v := range kinds[EdgeVolume] {
		if strings.Contains(v.Path, "Q") {
			t.Errorf("a volume wire turns a corner: %q", v.Path)
		}
	}

	// The base-backup path: the schedule's drop (straight, onto the
	// cluster frame's top line, not into the primary) and the attributed
	// replica's wire into the store.
	base := kinds[EdgeBaseBackup]
	if len(base) != 2 {
		t.Fatalf("%d base-backup wires, want 2", len(base))
	}
	var clusterTop int
	for _, f := range tp.Frames {
		if f.Kind == FrameCluster {
			clusterTop = f.Y
		}
	}
	drop := base[0]
	if strings.Contains(drop.Path, "Q") {
		t.Errorf("the lone schedule's drop is not straight: %q", drop.Path)
	}
	if _, last, _ := ends(drop.Path); last[1] != clusterTop || clusterTop >= n["inst-orders-1"].Y {
		t.Errorf("the schedule drop ends at y=%d, want the cluster frame top %d above the primary %d", last[1], clusterTop, n["inst-orders-1"].Y)
	}
	from := func(e viewmodel.TopoEdge) string {
		first, _, _ := ends(e.Path)
		for _, m := range tp.Nodes {
			if first[0] == m.X+m.W && first[1] >= m.Y && first[1] <= m.Y+m.H {
				return m.ID
			}
		}
		return ""
	}
	if got := from(base[1]); got != "inst-orders-2" {
		t.Errorf("the base-backup wire leaves %q, want the attributed instance orders-2", got)
	}
	if base[1].Label != "base backup · barman-cloud" || !base[1].LabelVertical {
		t.Errorf("base wire label %q vertical=%v", base[1].Label, base[1].LabelVertical)
	}
	wal := kinds[EdgeWAL]
	if len(wal) != 1 || from(wal[0]) != "inst-orders-1" || wal[0].Label != "WAL archive" {
		t.Errorf("WAL wires %+v, want one labelled wire from the primary", wal)
	}

	dots := map[string]int{}
	for _, d := range tp.Dots {
		dots[d.Kind]++
	}
	if dots[EdgeReplicate] == 0 || dots[EdgeRead] == 0 {
		t.Errorf("tee dots %v: the replication and read trunks split without a dot", dots)
	}
	legend := make([]string, 0, len(tp.Legend))
	for _, l := range tp.Legend {
		legend = append(legend, l.Kind)
	}
	if got, want := strings.Join(legend, ","), "write,read,replicate,wal,baseBackup,volume"; got != want {
		t.Errorf("legend = %s, want %s", got, want)
	}
}

// The drawing shrinks honestly: with nothing observed but the services
// and the instances there is one frame, and no wire into storage.
func TestLayoutDropsAbsentGroups(t *testing.T) {
	t.Parallel()
	in := richInput()
	in.Poolers, in.Schedules, in.Store, in.Snapshot, in.Claims, in.Base, in.WAL = nil, nil, nil, nil, nil, nil, false
	tp := draw(in)
	checkClean(t, tp)
	if len(tp.Frames) != 1 || tp.Frames[0].Kind != FrameCluster {
		t.Errorf("frames %+v, want the cluster frame alone", tp.Frames)
	}
	for _, e := range tp.Edges {
		if e.Kind == EdgeWAL || e.Kind == EdgeBaseBackup || e.Kind == EdgeVolume {
			t.Errorf("a %s wire is drawn with nothing to reach", e.Kind)
		}
	}
}

// No Backup attributed to an instance buys no base-backup wire; WAL
// archiving is the primary's duty regardless. An attribution to an
// instance that is not drawn, or to a box that is not drawn, buys none
// either.
func TestLayoutDrawsNoBaseWireWithoutAnAttributedInstance(t *testing.T) {
	t.Parallel()
	for name, base := range map[string]*Base{
		"none":            nil,
		"unknown source":  {Instance: "orders-9", Target: TargetStore, Label: "base backup"},
		"no such target":  {Instance: "orders-2", Target: "tape", Label: "base backup"},
		"snapshot absent": {Instance: "orders-2", Target: TargetSnapshot, Label: "base backup"},
	} {
		in := richInput()
		in.Schedules, in.Snapshot, in.Base = nil, nil, base
		tp := draw(in)
		checkClean(t, tp)
		wal := 0
		for _, e := range tp.Edges {
			if e.Kind == EdgeBaseBackup {
				t.Errorf("%s: a base-backup wire is drawn: %q", name, e.Path)
			}
			if e.Kind == EdgeWAL {
				wal++
			}
		}
		if wal != 1 {
			t.Errorf("%s: %d WAL wires, want 1", name, wal)
		}
	}
}

// Regression: an earlier grid drew the Cluster frame over the Backups frame.
// A top-band frame always ends above the frame under it, so no two frames
// overlap.
func TestLayoutBandFramesNeverOverlapTheFrameBelow(t *testing.T) {
	t.Parallel()
	in := richInput()
	tp := draw(in)
	frames := map[string]viewmodel.TopoFrame{}
	for _, f := range tp.Frames {
		frames[f.Kind] = f
	}
	if b, c := frames[FrameBackup], frames[FrameCluster]; b.Y+b.H >= c.Y {
		t.Errorf("Backups frame ends at y=%d, Cluster frame starts at y=%d", b.Y+b.H, c.Y)
	}
	if o, k := frames[FrameObjectStorage], frames[FrameKubeStorage]; o.Y+o.H >= k.Y {
		t.Errorf("Object storage frame ends at y=%d, Kubernetes storage starts at y=%d", o.Y+o.H, k.Y)
	}
}

// combination is richInput with the optional groups of mask turned on
// or off, one bit per group, so a test can sweep every shape the drawing
// can take.
func combination(mask int) Input {
	on := func(bit int) bool { return mask&(1<<bit) != 0 }
	in := richInput()
	if !on(0) {
		in.Poolers = nil
	} else if on(8) {
		in.Poolers = append(in.Poolers,
			Pooler{Box: box("pool-rw2", KindPooler, "orders-pool-rw2", 3), Type: "rw"},
			Pooler{Box: box("pool-r", KindPooler, "orders-pool-r", 3), Type: "r"},
			Pooler{Box: box("pool-x", KindPooler, "orders-pool-x", 1), Type: "x"})
		in.Services = append(in.Services, Service{Box: box("svc-r", KindService, "orders-r", 3), Type: "r"})
	}
	if !on(1) {
		in.Schedules = nil
	} else if on(7) {
		in.Schedules = append(in.Schedules, box("sched-hourly", KindSchedule, "hourly", 3), box("sched-more", KindSchedule, "+2 more", 2))
	}
	if !on(2) {
		in.Store, in.WAL = nil, false
	}
	if on(3) {
		snap := box("snapshot", KindSnapshot, "Volume snapshots", 3)
		in.Snapshot = &snap
	}
	if !on(4) {
		in.Claims = nil
	} else if on(7) {
		in.Claims = append(in.Claims, Claim{Box: box("pvc-1w", KindVolume, "orders-1-wal", 3), Instance: "orders-1"})
	}
	if !on(5) {
		in.Instances = in.Instances[1:] // no primary
	}
	if on(6) {
		in.Instances = append(in.Instances, instance("orders-4", RoleReplica), instance("orders-5", ""),
			Instance{Box: box("inst-more", KindInstance, "+3 more instances", 2)})
		in.Base = &Base{Instance: "orders-1", Target: TargetSnapshot, Label: "base backup · volume snapshot"}
	}
	if on(8) && !on(6) {
		in.Base = &Base{Instance: "orders-1", Target: TargetStore, Label: "base backup · barman-cloud"}
	}
	return in
}

// withFold is in with the frames of the bitmask fm (over Foldable) folded.
func withFold(in Input, fm int) Input {
	in.Fold = nil
	for i, k := range Foldable {
		if fm&(1<<i) != 0 {
			in.Fold = append(in.Fold, k)
		}
	}
	return in
}

// Every combination of the optional groups lays out clean: no
// overlapping frames, no box in two frames, every edge on box borders,
// every label inside the drawing and clear of the boxes, width and
// height covering everything.
func TestLayoutEveryCombinationIsClean(t *testing.T) {
	t.Parallel()
	for mask := range 1 << 9 {
		in := combination(mask)
		for fm := range 1 << len(Foldable) {
			in := withFold(in, fm)
			tp := draw(in)
			for _, v := range Check(tp) {
				t.Errorf("mask %09b fold %v: %s", mask, in.Fold, v)
			}
			if len(tp.Nodes) == 0 || tp.Width <= 0 || tp.Height <= 0 {
				t.Errorf("mask %09b fold %v: empty drawing %dx%d", mask, in.Fold, tp.Width, tp.Height)
			}
		}
		tp := draw(in)
		if len(tp.Nodes) == 0 || tp.Width <= 0 || tp.Height <= 0 {
			t.Errorf("mask %09b: empty drawing %dx%d", mask, tp.Width, tp.Height)
		}
	}
}

// A folded frame holds one summary box that counts what it stands for and
// carries the worst state; the wires that crossed into it end on that box,
// and the rest of the drawing closes up.
func TestLayoutFoldsFrames(t *testing.T) {
	t.Parallel()
	open := draw(richInput())
	in := richInput()
	in.Instances[2].State = viewmodel.HealthWarning
	in.Poolers[1].Count = 0
	in.Fold = []string{FramePool, FrameCluster, FrameBackup, FrameKubeStorage}
	tp := draw(in)
	checkClean(t, tp)
	n := nodes(tp)
	if len(tp.Nodes) != 5 { // poolers, cluster, schedule, volumes, store
		t.Fatalf("folded drawing has %d nodes, want 5", len(tp.Nodes))
	}
	cl := n["fold-cluster"]
	if cl.Label != "3 instances" || cl.State != viewmodel.HealthWarning || cl.Kind != KindPrimary {
		t.Errorf("cluster summary = %q %s %s", cl.Label, cl.State, cl.Kind)
	}
	if n["fold-pool"].Label != "2 poolers" || n["fold-kubeStorage"].Label != "3 volumes" {
		t.Errorf("summaries = %q, %q", n["fold-pool"].Label, n["fold-kubeStorage"].Label)
	}
	if tp.Width >= open.Width {
		t.Errorf("folded width %d, want less than the open %d", tp.Width, open.Width)
	}
	kinds := map[string]int{}
	for _, e := range tp.Edges {
		kinds[e.Kind]++
	}
	// Poolers into the cluster (write and read share the one box, so one
	// wire), WAL and base backup to the store, the cluster onto its volumes,
	// the schedule's drop.
	want := map[string]int{EdgeWrite: 1, EdgeWAL: 1, EdgeBaseBackup: 2, EdgeVolume: 1}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("edges = %v, want %v", kinds, want)
	}
	for _, f := range tp.Frames {
		if f.Foldable != (f.Kind != FrameObjectStorage) || f.Folded != f.Foldable {
			t.Errorf("frame %s foldable=%v folded=%v", f.Kind, f.Foldable, f.Folded)
		}
	}
}

// Folded poolers alone keep one exit that splits towards each Service they fed.
func TestLayoutFoldedPoolersSplit(t *testing.T) {
	t.Parallel()
	in := richInput()
	in.Fold = []string{FramePool}
	tp := draw(in)
	checkClean(t, tp)
	var write, read int
	for _, e := range tp.Edges {
		switch {
		case e.Kind == EdgeWrite && strings.HasPrefix(e.Path, "M "+strconv.Itoa(nodes(tp)["fold-pool"].X+wPool)):
			write++
		case e.Kind == EdgeRead && strings.HasPrefix(e.Path, "M "+strconv.Itoa(nodes(tp)["fold-pool"].X+wPool)):
			read++
		}
	}
	if write != 1 || read != 1 || len(tp.Dots) == 0 {
		t.Errorf("folded poolers: %d write, %d read wires, %d dots", write, read, len(tp.Dots))
	}
}

// The same input lays out the same drawing, always.
func TestLayoutIsDeterministic(t *testing.T) {
	t.Parallel()
	first := Layout(richInput())
	for range 20 {
		if again := Layout(richInput()); !reflect.DeepEqual(first, again) {
			t.Fatal("two layouts of the same input differ")
		}
	}
}

// An input without instances still lays out (services alone) rather
// than failing the endpoint.
func TestLayoutWithoutInstances(t *testing.T) {
	t.Parallel()
	in := richInput()
	in.Instances, in.Claims, in.Base = nil, nil, nil
	checkClean(t, draw(in))
	checkClean(t, draw(Input{}))
}

func TestClip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text  string
		w     int
		label bool
		want  string
	}{
		{"short", 200, false, "short"},
		{strings.Repeat("a", 25), 200, false, strings.Repeat("a", 25)},
		{strings.Repeat("a", 26), 200, false, strings.Repeat("a", 24) + "…"},
		{strings.Repeat("é", 26), 200, false, strings.Repeat("é", 24) + "…"},
		{strings.Repeat("a", 23), 200, true, strings.Repeat("a", 21) + "…"},
		{"anything", 10, false, "any…"},
	} {
		if got := clip(tc.text, tc.w, tc.label); got != tc.want {
			t.Errorf("clip(%q, %d, %v) = %q, want %q", tc.text, tc.w, tc.label, got, tc.want)
		}
	}
}

func TestRoutes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   []point
		want string
	}{
		{"straight collapses", []point{{0, 10}, {50, 10}, {50, 10}, {100, 10}}, "M 0 10 L 100 10"},
		{"one elbow", []point{{0, 0}, {50, 0}, {50, 40}}, "M 0 0 L 44 0 Q 50 0 50 6 L 50 40"},
		{"short leg shrinks the corner", []point{{0, 0}, {4, 0}, {4, 40}}, "M 0 0 L 2 0 Q 4 0 4 2 L 4 40"},
		{"half units", []point{{0, 0.5}, {10.5, 0.5}}, "M 0 0.5 L 10.5 0.5"},
	} {
		if got := roundedRoute(corners(tc.in)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestHeadingWidth(t *testing.T) {
	t.Parallel()
	if got := HeadingWidth("Cluster", ""); got != 7*7+20 {
		t.Errorf("HeadingWidth(Cluster) = %d", got)
	}
	if got := HeadingWidth("Object storage", "https://s3"); got != 7*(14+3+10)+20 {
		t.Errorf("HeadingWidth with note = %d", got)
	}
}

// Check itself catches what it promises to: an overlapping frame pair, a
// box in two frames, an edge off every border, a label on a box.
func TestCheckCatchesViolations(t *testing.T) {
	t.Parallel()
	good := draw(richInput())
	for name, mutate := range map[string]func(*viewmodel.Topology){
		"frames overlap":   func(t *viewmodel.Topology) { t.Frames[1].Y = t.Frames[2].Y }, // the earlier grid's bug
		"edge off borders": func(t *viewmodel.Topology) { t.Edges[0].Path = "M 1 1 L 2 2" },
		"label on a box": func(t *viewmodel.Topology) {
			for i := range t.Edges {
				if t.Edges[i].Label != "" {
					t.Edges[i].LabelX, t.Edges[i].LabelY, t.Edges[i].LabelVertical = t.Nodes[0].X+40, t.Nodes[0].Y+20, false
				}
			}
		},
		"drawing too small": func(t *viewmodel.Topology) { t.Width -= 40 },
		"box outside text":  func(t *viewmodel.Topology) { t.Nodes[0].Lines[0].Y = t.Nodes[0].Y },
	} {
		tp := good
		tp.Frames = append([]viewmodel.TopoFrame(nil), good.Frames...)
		tp.Edges = append([]viewmodel.TopoEdge(nil), good.Edges...)
		tp.Nodes = append([]viewmodel.TopoNode(nil), good.Nodes...)
		tp.Nodes[0].Lines = append([]viewmodel.TopoText(nil), good.Nodes[0].Lines...)
		mutate(&tp)
		if len(Check(tp)) == 0 {
			t.Errorf("%s: Check found nothing", name)
		}
	}
}

// The layout is pinned byte for byte: the rich drawing as a readable
// golden, and every combination and fold of TestLayoutEveryCombinationIsClean
// as one digest, so a refactor of Layout that moves any coordinate of any
// shape fails here even when the drawing stays clean. Rewrite with -update.
func TestLayoutGolden(t *testing.T) {
	t.Parallel()
	rich, err := json.MarshalIndent(Layout(richInput()), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.New()
	for mask := range 1 << 9 {
		for fm := range 1 << len(Foldable) {
			b, err := json.Marshal(Layout(withFold(combination(mask), fm)))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fmt.Fprintf(sum, "%09b/%04b ", mask, fm)
			_, _ = sum.Write(b)
			_, _ = sum.Write([]byte{'\n'})
		}
	}
	digest := hex.EncodeToString(sum.Sum(nil)) + "\n"
	golden(t, "layout-rich.golden.json", append(rich, '\n'))
	golden(t, "layout-combinations.sha256", []byte(digest))
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from the golden file (run with -update to accept)", name)
	}
}
