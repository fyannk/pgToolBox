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

// Package topology lays out the grouped wiring drawing of one Cluster:
// its "physical wiring", as a pure function from
// boxes (already worded by the read model) to geometry. It reads no
// cluster and invents no fact: what a box says is the caller's, where it
// stands and how the wires run is this package's.
//
// A derivation of the operator's own draw.io sketch of the cluster, laid
// out the same way that sketch was: no layout engine, every placement a
// stated rule.
//
//   - Columns, left to right: poolers, services, the primary, the
//     replicas, storage. The primary stands left of its replicas; the
//     claims stand right of the instances, staggered over two columns
//     so consecutive instances never fight for the same corridor.
//   - A top band holds what feeds and receives the backup path: the
//     ScheduledBackup boxes over the primary, the object store over the
//     Kubernetes storage. Either band lifts the frame below it, so a
//     band frame and the frame under it never overlap.
//   - Rows: rw above ro above r, for the poolers and the services
//     alike. In the Kubernetes storage frame the snapshots sit on top;
//     each claim group is centred on its own instance, so a lone claim's
//     volume wire is a straight line.
//   - Five dotted frames name the categories: Poolers, Cluster, Backups,
//     Kubernetes storage, Object storage. The object store's endpoint
//     rides its frame label — it describes the region, not one box. The
//     trunk buses run in the alleys between frames.
//   - A flow that fans out leaves its box once: one trunk to a bus, one
//     branch per destination, a dot on each tee. Replication leaves the
//     primary at one port and splits; the read service reaches its
//     replicas the same way; an instance reaches its claims the same
//     way. The base-backup path — schedule to cluster, source instance
//     to object store or snapshots — shares one line style, so it can be
//     followed across three frames by colour alone.
//
// Deterministic by construction, so it is safe to screenshot and cheap
// to test: the same input lays out the same drawing, always.
package topology

import (
	"fmt"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Node kinds (viewmodel.TopoNode.Kind).
const (
	KindService  = "service"
	KindPrimary  = "primary"
	KindReplica  = "replica"
	KindInstance = "instance" // role not reported, or the "+N more" box
	KindPooler   = "pooler"
	KindSchedule = "schedule"
	KindStore    = "store"
	KindSnapshot = "snapshot"
	KindVolume   = "volume"
)

// Edge, dot and legend kinds (viewmodel.TopoEdge.Kind).
const (
	EdgeWrite      = "write"
	EdgeRead       = "read"
	EdgeReplicate  = "replicate"
	EdgeWAL        = "wal"
	EdgeBaseBackup = "baseBackup"
	EdgeVolume     = "volume"
)

// Frame kinds (viewmodel.TopoFrame.Kind).
const (
	FramePool          = "pool"
	FrameCluster       = "cluster"
	FrameBackup        = "backup"
	FrameKubeStorage   = "kubeStorage"
	FrameObjectStorage = "objectStorage"
)

// Caption is the drawing's statement of what is observed and what is the
// standard wiring.
const Caption = "Instance, pooler, schedule, volume and object-store states are observed, and the base-backup wire leaves the instance the newest Backup reports; " +
	"the Services, the client paths, replication and WAL archiving are the standard CloudNativePG wiring, not observed traffic."

// Instance roles, as the caller read them from the operator.
const (
	RolePrimary = "primary"
	RoleReplica = "replica"
)

// Base-backup targets.
const (
	TargetStore    = "store"
	TargetSnapshot = "snapshot"
)

const (
	// colGap separates columns inside the cluster frame; the read bus
	// runs down the middle of the services-to-primary gap.
	colGap = 48
	// replGap is the wider primary-to-replicas corridor. Replication
	// runs down it rather than round the far side of the replicas, so
	// the wire sits between the two things it relates; the extra width
	// over colGap is what lets its label stand upright in the lane
	// without touching either column.
	replGap = 76
	// alley separates two frames; the WAL and base-backup buses run in
	// the cluster-to-storage alley, the pooler bus in the pool alley.
	alley = 60
	// rowGap separates stacked boxes in a column, and a top-band frame
	// from the frame below it.
	rowGap = 28
	// pad is a frame's padding around its boxes.
	pad = 16
	// labelBand is the frame headroom holding the category label.
	labelBand = 30
	// portOff separates the ports sharing a box edge, so the WAL, the
	// volume and the replication wires leave the primary as distinct
	// lines rather than one smear.
	portOff = 15
	// margin is the viewBox margin around everything.
	margin = 8
	// Box widths.
	wSvc   = 240 // holds "ClusterIP 255.255.255.255:65535" unclipped
	wSrv   = 230
	wStore = 265
	wPool  = 210
	wPvc   = 200
	wBak   = 210
	// claimGap separates the two claim columns; the second column's
	// drop bus runs inside it.
	claimGap = 24
	// claimStack separates two claims of the same instance.
	claimStack = 10
	// busInset is how far left of its claim column a volume drop bus
	// runs.
	busInset = 10
	// maxRows bounds a box's lines: the height rule below keeps five
	// inside the box, a sixth would touch its border.
	maxRows = 5
	// charW is the width budget of one character of box text: the
	// conservative estimate every geometry test also uses.
	charW = 7
)

// Row is one line of a box, worded by the caller: Class is label, sub,
// disk or note.
type Row struct {
	Class string
	Text  string
}

// Box is one node's content. ID is unique in the drawing; Label is the
// box's short name (the node's Label field, for the text alternative).
type Box struct {
	ID    string
	Kind  string
	Label string
	State viewmodel.Health
	Link  string
	Rows  []Row
	// Count is how many objects a "+N more" box stands for (0: one), so
	// a folded frame counts them all.
	Count int
}

// typeFolded is the Pooler.Type of the folded poolers' box: it feeds
// every Service type the poolers it stands for fed.
const typeFolded = "*"

// Pooler is a Pooler box and the service type it fronts (rw, ro, r).
type Pooler struct {
	Box
	Type string
}

// Service is one of the Cluster's Services (rw, ro, r).
type Service struct {
	Box
	Type string
}

// Instance is an instance box. Role is RolePrimary, RoleReplica or empty
// (not reported, or a summary box): only a replica receives the read and
// replication wires, because only for a replica is the standard wiring
// known to apply. Name matches Claim.Instance and Base.Instance.
type Instance struct {
	Box
	Name string
	Role string
}

// Claim is a PersistentVolumeClaim box and the instance that holds it.
type Claim struct {
	Box
	Instance string
}

// Base is the attributed base-backup flow: the instance the operator
// reported serving it, the box it ends in (TargetStore or
// TargetSnapshot), and the wire's word.
type Base struct {
	Instance string
	Target   string
	Label    string
}

// Input is everything the drawing shows, already bounded by the caller.
type Input struct {
	Poolers   []Pooler
	Services  []Service
	Instances []Instance
	Schedules []Box
	// Store is the object store; StoreNote (its endpoint) rides the
	// Object storage frame's label.
	Store     *Box
	StoreNote string
	Snapshot  *Box
	Claims    []Claim
	// WAL draws the primary's WAL archive wire into the store.
	WAL  bool
	Base *Base
	// Fold lists the frames (Foldable kinds) to draw folded.
	Fold []string
}

// Drawing is the laid-out geometry, in viewBox units.
type Drawing struct {
	Width, Height int
	Frames        []viewmodel.TopoFrame
	Edges         []viewmodel.TopoEdge
	Nodes         []viewmodel.TopoNode
	Dots          []viewmodel.TopoDot
	Legend        []viewmodel.TopoLegend
}

// legendOrder keys every style the drawing can draw, in reading order.
// The two object-store flows are separate everywhere but the box they
// end in: continuous WAL archiving leaves the primary, periodic base
// backups leave whichever instance the operator picked. One wire for
// both would assert a shared origin the resources deny.
var legendOrder = []viewmodel.TopoLegend{
	{Kind: EdgeWrite, Label: "writes"},
	{Kind: EdgeRead, Label: "reads"},
	{Kind: EdgeReplicate, Label: "replication"},
	{Kind: EdgeWAL, Label: "WAL archive"},
	{Kind: EdgeBaseBackup, Label: "base backup"},
	{Kind: EdgeVolume, Label: "volume"},
}

type node = viewmodel.TopoNode

func cy(n *node) float64     { return float64(n.Y) + float64(n.H)/2 }
func cx(n *node) float64     { return float64(n.X) + float64(n.W)/2 }
func right(n *node) float64  { return float64(n.X + n.W) }
func left(n *node) float64   { return float64(n.X) }
func bottom(n *node) float64 { return float64(n.Y + n.H) }

func centre(n *node, x, y float64) {
	n.X = int(x) - n.W/2
	n.Y = int(y) - n.H/2
}

// nodeHeight holds a box's rows: 32 px of border and padding, 10 px a
// row (the rows are 14–17 px apart but centred, see place).
func nodeHeight(rows int) int {
	return 32 + 10*max(rows, 1)
}

// lineStep spaces the rows: the label leads, the second row sits 17 px
// under it, the rest 14 px apart.
func lineStep(i int) int {
	switch i {
	case 0:
		return 0
	case 1:
		return 17
	default:
		return 14
	}
}

func blockHeight(rows int) int {
	h := 14
	for i := range rows {
		h += lineStep(i)
	}
	return h
}

// clip bounds a row to its box: about charW px a character, 14 px of
// left inset and room on the right (more on the label row, whose top
// right corner holds the health glyph).
func clip(text string, w int, label bool) string {
	room := w - 20
	if label {
		room = w - 40
	}
	limit := max(room/charW, 4)
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
}

type drawer struct {
	nodes []*node
	rows  map[*node][]Row
	out   Drawing
	drawn map[string]bool
}

func (d *drawer) add(b Box, w int) *node {
	rows := b.Rows
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}
	n := &node{ID: b.ID, Kind: b.Kind, Label: b.Label, State: b.State, Link: b.Link, W: w, H: nodeHeight(len(rows))}
	d.nodes = append(d.nodes, n)
	d.rows[n] = rows
	return n
}

// place positions a node's rows now that the box has its X and Y: the
// block is centred vertically, 14 px in from the left border.
func place(n *node, rows []Row) {
	x := n.X + 14
	y := n.Y + (n.H-blockHeight(len(rows)))/2 + 11
	for i, row := range rows {
		y += lineStep(i)
		n.Lines = append(n.Lines, viewmodel.TopoText{Class: row.Class, Text: clip(row.Text, n.W, i == 0), X: x, Y: y})
	}
}

func (d *drawer) wire(kind string, points []point) *viewmodel.TopoEdge {
	d.out.Edges = append(d.out.Edges, viewmodel.TopoEdge{Kind: kind, Path: roundedRoute(corners(points))})
	d.drawn[kind] = true
	return &d.out.Edges[len(d.out.Edges)-1]
}

func (d *drawer) dot(kind string, x, y float64) {
	d.out.Dots = append(d.out.Dots, viewmodel.TopoDot{Kind: kind, X: int(x), Y: int(y)})
}

// stackAround places a column of boxes centred on a level, pushed down
// when the level would ride into the space above. Returns the bottom.
func stackAround(boxes []*node, x, level, minTop float64) float64 {
	if len(boxes) == 0 {
		return minTop - rowGap
	}
	total := 0.0
	for _, b := range boxes {
		total += float64(b.H)
	}
	total += float64(len(boxes)-1) * rowGap
	top := max(level-total/2, minTop)
	for _, b := range boxes {
		b.X = int(x)
		b.Y = int(top)
		top += float64(b.H) + rowGap
	}
	return top - rowGap
}

// bounds is the horizontal extent and the bottom edge of a set of boxes
// (no caller needs the top edge).
func bounds(members []*node) (minX, maxX, maxY int) {
	minX = members[0].X
	maxX, maxY = members[0].X+members[0].W, members[0].Y+members[0].H
	for _, m := range members[1:] {
		minX = min(minX, m.X)
		maxX, maxY = max(maxX, m.X+m.W), max(maxY, m.Y+m.H)
	}
	return minX, maxX, maxY
}

// HeadingWidth is the width a frame heading needs (about charW px per
// character, a separator between label and note, 10 px padding a side).
func HeadingWidth(label, note string) int {
	n := utf8.RuneCountInString(label)
	if note != "" {
		n += 3 + utf8.RuneCountInString(note)
	}
	return charW*n + 20
}

// Layout lays out the drawing. An input without instances still draws
// its services and storage: the caller decides whether that is useful.
//
// The passes run in the order the package comment states the rules:
// the content is gathered before any placement; columns are placed
// first, then the top band, then the rows; the wires follow the boxes
// they join; the frames close round what was actually drawn.
func Layout(in Input) Drawing {
	in, f := fold(in)
	l := &layout{
		drawer: &drawer{rows: map[*node][]Row{}, drawn: map[string]bool{}},
		in:     in,
		f:      f,
	}
	l.out = Drawing{
		Frames: []viewmodel.TopoFrame{}, Edges: []viewmodel.TopoEdge{}, Nodes: []viewmodel.TopoNode{},
		Dots: []viewmodel.TopoDot{}, Legend: []viewmodel.TopoLegend{},
	}

	l.gatherPoolers()
	l.gatherServices()
	l.gatherInstances()
	l.gatherBackupPath()
	l.gatherClaims()

	l.placeColumns()
	l.placeTopBand()
	l.placeInstances()
	l.placeKubeStorage()
	l.placeServices()
	l.placePoolers()

	l.wirePoolers()
	l.wireServices()
	l.wireSchedules()
	l.wireReplication()
	l.wireVolumes()
	l.wireArchive()

	l.frameAll()
	return l.finish()
}

// claimGroup is one instance's claims and the claim column they stand in.
type claimGroup struct {
	of    *node
	col   int
	nodes []*node
}

// layout is one drawing in progress: the boxes gathered from the input,
// then the coordinates each pass decides and a later pass reads. Each
// field is written by one pass only.
type layout struct {
	*drawer
	in Input
	f  folds

	// Content (gather*).
	poolByType map[string][]*node
	poolers    []*node
	svc        map[string]*node
	primary    *node
	others     []*node
	standby    map[*node]bool
	byName     map[string]*node
	schedules  []*node
	store      *node
	snapshot   *node
	groups     []claimGroup
	replicas   []*node

	// Columns (placeColumns).
	poolX, svcX, primX, replX float64
	clusterRight              float64
	storeX, regionW           float64
	width                     int

	// Top band (placeTopBand).
	bandTop, clusterTop, k8sTop  float64
	bakLeft, bakRight, bakBottom float64
	objBottom                    float64
	clusterContent               float64
	colX                         [2]float64
	clusterBox                   *node
}

func (l *layout) hasPool() bool   { return len(l.poolers) > 0 }
func (l *layout) hasK8s() bool    { return l.snapshot != nil || len(l.groups) > 0 }
func (l *layout) hasObject() bool { return l.store != nil }
func (l *layout) twoCols() bool   { return len(l.groups) > 1 }
func (l *layout) primCx() float64 { return l.primX + wSrv/2 }

// --- Content, gathered before any placement. ---

// gatherPoolers adds the poolers rw before ro before r, so the write path
// reads on top; a type outside the three is drawn after them, unwired.
func (l *layout) gatherPoolers() {
	l.poolByType = map[string][]*node{}
	for _, typ := range []string{"rw", "ro", "r"} {
		for _, p := range l.in.Poolers {
			if p.Type == typ {
				n := l.add(p.Box, wPool)
				l.poolByType[typ] = append(l.poolByType[typ], n)
				l.poolers = append(l.poolers, n)
			}
		}
	}
	for _, p := range l.in.Poolers { // a type outside rw/ro/r: drawn, unwired
		if p.Type != "rw" && p.Type != "ro" && p.Type != "r" {
			key := "?"
			if p.Type == typeFolded {
				key = typeFolded
			}
			n := l.add(p.Box, wPool)
			l.poolByType[key] = append(l.poolByType[key], n)
			l.poolers = append(l.poolers, n)
		}
	}
}

// gatherServices adds the first Service of each type, rw, ro, r.
func (l *layout) gatherServices() {
	l.svc = map[string]*node{}
	for _, typ := range []string{"rw", "ro", "r"} {
		for _, s := range l.in.Services {
			if s.Type == typ && l.svc[typ] == nil {
				l.svc[typ] = l.add(s.Box, wSvc)
			}
		}
	}
}

// gatherInstances adds the instances, the first reported primary apart
// from the rest; only a reported replica is marked standby, since only
// for a replica is the standard read and replication wiring known.
func (l *layout) gatherInstances() {
	l.standby = map[*node]bool{}
	l.byName = map[string]*node{}
	for _, inst := range l.in.Instances {
		n := l.add(inst.Box, wSrv)
		if inst.Name != "" {
			l.byName[inst.Name] = n
		}
		if inst.Role == RolePrimary && l.primary == nil {
			l.primary = n
			continue
		}
		l.standby[n] = inst.Role == RoleReplica
		l.others = append(l.others, n)
	}
	for _, r := range l.others {
		if l.standby[r] {
			l.replicas = append(l.replicas, r)
		}
	}
}

// gatherBackupPath adds the schedules, the object store and the volume
// snapshots: the boxes of the top band and the storage region.
func (l *layout) gatherBackupPath() {
	l.schedules = make([]*node, 0, len(l.in.Schedules))
	for _, s := range l.in.Schedules {
		l.schedules = append(l.schedules, l.add(s, wBak))
	}
	if l.in.Store != nil {
		l.store = l.add(*l.in.Store, wStore)
	}
	if l.in.Snapshot != nil {
		l.snapshot = l.add(*l.in.Snapshot, wStore)
	}
}

// gatherClaims groups the claims by the instance that holds them and
// staggers the groups over two columns: consecutive instances alternate,
// so one instance's wires cross the other column in the corridor its
// neighbours leave free.
func (l *layout) gatherClaims() {
	order := l.others
	if l.primary != nil {
		order = append([]*node{l.primary}, l.others...)
	}
	instName := map[*node]string{}
	for name, n := range l.byName {
		instName[n] = name
	}
	for _, inst := range order {
		name := instName[inst]
		if name == "" {
			continue
		}
		var group []*node
		for _, c := range l.in.Claims {
			if c.Instance == name {
				group = append(group, l.add(c.Box, wPvc))
			}
		}
		if len(group) > 0 {
			l.groups = append(l.groups, claimGroup{of: inst, col: len(l.groups) % 2, nodes: group})
		}
	}
}

// --- Placement: columns first, then the top band, then rows. ---

// placeColumns sets every column's left edge, left to right: poolers,
// services, the primary, the replicas, storage; and the drawing's width.
func (l *layout) placeColumns() {
	x := float64(margin)
	if l.hasPool() {
		x += pad
		l.poolX = x
		x += wPool + pad + alley
	}
	// A folded cluster is one box: no Services column, no replicas column.
	x += pad
	l.svcX = x
	if !l.f.cluster {
		x += wSvc + colGap
	}
	l.primX = x
	x += wSrv
	if band := float64(len(l.schedules)*wBak + max(len(l.schedules)-1, 0)*pad); l.f.cluster && band > wSrv {
		// The Backups band over a folded cluster still fits above it.
		l.primX += (band - wSrv) / 2
		x = l.svcX + band
	}
	l.replX = x + replGap
	if l.f.cluster {
		l.replX = l.primX // a folded cluster without a primary is still one box there
	} else {
		x = l.replX + wSrv
	}
	x += pad
	l.clusterRight = x
	if l.hasK8s() || l.hasObject() {
		x += alley + pad
		l.storeX = x
		l.regionW = wPvc
		if l.twoCols() {
			l.regionW = 2*wPvc + claimGap
		}
		if (l.hasObject() || l.snapshot != nil) && l.regionW < wStore {
			l.regionW = wStore
		}
		// The endpoint rides the object frame's label; a long one widens
		// the region rather than bleeding past it.
		if l.hasObject() {
			l.regionW = max(l.regionW, float64(HeadingWidth("Object storage", l.in.StoreNote)-2*pad))
		}
		x += l.regionW + pad
	}
	l.width = int(x) + margin
}

// placeTopBand puts the schedules over the primary column and the object
// store over the storage region. Either lifts the frame below it; with
// neither, the drawing keeps its one-row shape.
func (l *layout) placeTopBand() {
	l.bandTop = float64(margin)
	bandContent := l.bandTop + labelBand + pad
	l.clusterTop = l.bandTop
	if len(l.schedules) > 0 {
		total := float64(len(l.schedules)*wBak + (len(l.schedules)-1)*pad)
		// Centred on the primary, but never left of the cluster frame.
		sx := max(l.primCx()-total/2, l.svcX)
		l.bakLeft = sx
		l.bakBottom = bandContent
		for _, s := range l.schedules {
			s.X = int(sx)
			s.Y = int(bandContent)
			sx += wBak + pad
			l.bakBottom = max(l.bakBottom, bottom(s))
		}
		l.bakRight = sx - pad
		l.clusterTop = l.bakBottom + pad + rowGap
	}
	l.k8sTop = l.bandTop
	if l.hasObject() {
		l.store.X = int(l.storeX)
		l.store.Y = int(bandContent)
		l.objBottom = bottom(l.store)
		if l.hasK8s() {
			l.k8sTop = l.objBottom + pad + rowGap
		}
	}
	l.clusterContent = l.clusterTop + labelBand + pad
}

// placeInstances stacks the instance rows below the cluster frame's
// label band: the primary in its column, the others in the replicas'.
func (l *layout) placeInstances() {
	rowY := l.clusterContent
	if l.primary != nil {
		centre(l.primary, l.primX+float64(l.primary.W)/2, rowY+float64(l.primary.H)/2)
		rowY += float64(l.primary.H) + rowGap
	}
	for _, r := range l.others {
		centre(r, l.replX+float64(r.W)/2, rowY+float64(r.H)/2)
		rowY += float64(r.H) + rowGap
	}
}

// placeKubeStorage stacks the Kubernetes storage region: snapshots on
// top, then the claim groups, each centred on its instance and pushed
// down only when its own column is already occupied.
func (l *layout) placeKubeStorage() {
	sY := l.k8sTop + labelBand + pad
	if l.snapshot != nil {
		l.snapshot.X = int(l.storeX)
		l.snapshot.Y = int(sY)
		sY = bottom(l.snapshot) + rowGap
	}
	l.colX = [2]float64{l.storeX, l.storeX + wPvc + claimGap}
	nextFree := [2]float64{sY, sY}
	for _, g := range l.groups {
		total := float64((len(g.nodes) - 1) * claimStack)
		for _, n := range g.nodes {
			total += float64(n.H)
		}
		top := max(cy(g.of)-total/2, nextFree[g.col])
		for _, n := range g.nodes {
			n.X = int(l.colX[g.col])
			n.Y = int(top)
			top += float64(n.H) + claimStack
		}
		nextFree[g.col] = top - claimStack + rowGap
	}
}

// placeServices levels the Services with the servers: the write service
// with the primary, the read service with the first replica, the any
// service under them.
func (l *layout) placeServices() {
	var firstStandby *node
	for _, r := range l.others {
		if l.standby[r] {
			firstStandby = r
			break
		}
	}
	below := l.clusterContent
	if rw := l.svc["rw"]; rw != nil {
		level := l.clusterContent + float64(rw.H)/2
		if l.primary != nil {
			level = cy(l.primary)
		}
		centre(rw, l.svcX+float64(rw.W)/2, level)
		below = bottom(rw) + rowGap
	}
	if ro := l.svc["ro"]; ro != nil {
		level := below + float64(ro.H)/2
		if firstStandby != nil {
			level = max(level, cy(firstStandby))
		}
		centre(ro, l.svcX+float64(ro.W)/2, level)
		below = bottom(ro) + rowGap
	}
	if r := l.svc["r"]; r != nil {
		centre(r, l.svcX+float64(r.W)/2, below+float64(r.H)/2)
	}
}

// placePoolers levels the poolers with their service, singles exactly
// level with it, groups stacked around it; a later group never rides over
// an earlier one.
func (l *layout) placePoolers() {
	if l.f.cluster {
		l.clusterBox = l.primary
		if l.clusterBox == nil && len(l.others) > 0 {
			l.clusterBox = l.others[0]
		}
	}
	poolTop := l.clusterContent
	for _, typ := range []string{typeFolded, "rw", "ro", "r", "?"} {
		level := poolTop
		lead := typ
		if typ == typeFolded && len(l.f.fronts) > 0 {
			lead = l.f.fronts[0]
		}
		if s := l.target(lead); s != nil && typ != "?" {
			level = cy(s)
		}
		poolTop = stackAround(l.poolByType[typ], l.poolX, level, poolTop) + rowGap
	}
}

// target is the box poolers of a type feed: their Service, or the folded
// cluster's box.
func (l *layout) target(typ string) *node {
	if l.clusterBox != nil {
		return l.clusterBox
	}
	return l.svc[typ]
}

// --- Wires: every fan is one trunk, tee dots where it splits. ---

// wirePoolers runs the poolers into their service, fanning in over the
// pool alley. The folded poolers' one exit splits towards each Service
// they fed; into a folded cluster every type ends on its box.
func (l *layout) wirePoolers() {
	if !l.hasPool() {
		return
	}
	bus := l.poolX + wPool + pad + alley/2
	for _, typ := range []string{"rw", "ro", "r", typeFolded} {
		from := l.poolByType[typ]
		types := []string{typ}
		if typ == typeFolded {
			types = l.f.fronts
		}
		var to []*node
		var kinds []string
		for _, t := range types {
			n := l.target(t)
			if n == nil || slices.Contains(to, n) {
				continue
			}
			kind := EdgeRead
			if t == "rw" {
				kind = EdgeWrite
			}
			to, kinds = append(to, n), append(kinds, kind)
		}
		for _, p := range from {
			split := len(to) > 1
			for i, t := range to {
				l.wire(kinds[i], []point{{right(p), cy(p)}, {bus, cy(p)}, {bus, cy(t)}, {left(t), cy(t)}})
				if len(from) > 1 && !closeTo(cy(p), cy(t)) {
					split = true
				}
			}
			if split && len(to) > 0 {
				l.dot(kinds[0], bus, cy(p))
			}
		}
	}
}

// wireServices runs the write service into the primary in one straight
// line, and the read service into every replica: straight into the
// first, one bus down to the rest.
func (l *layout) wireServices() {
	if rw := l.svc["rw"]; rw != nil && l.primary != nil {
		l.wire(EdgeWrite, []point{{right(rw), cy(rw)}, {left(l.primary), cy(l.primary)}})
	}
	if ro := l.svc["ro"]; ro != nil {
		readBus := l.svcX + wSvc + colGap/2
		for i, r := range l.replicas {
			l.wire(EdgeRead, []point{{right(ro), cy(ro)}, {readBus, cy(ro)}, {readBus, cy(r)}, {left(r), cy(r)}})
			if len(l.replicas) > 1 && i < len(l.replicas)-1 {
				l.dot(EdgeRead, readBus, cy(r))
			}
		}
	}
}

// wireSchedules drops each schedule out of the Backups frame onto the
// cluster frame's top line: a collector bus in the band-to-cluster gap,
// one drop. With a single schedule centred on the primary the route
// collapses to a straight vertical line.
//
// It stops at the frame rather than reaching into the primary's box
// because a ScheduledBackup names the Cluster, not an instance — the
// operator picks the instance per run, routinely a standby.
func (l *layout) wireSchedules() {
	if len(l.schedules) == 0 {
		return
	}
	busY := l.clusterTop - rowGap/2
	dropX := l.primCx()
	if l.primary == nil {
		dropX = cx(l.schedules[0])
	}
	for _, s := range l.schedules {
		l.wire(EdgeBaseBackup, []point{{cx(s), bottom(s)}, {cx(s), busY}, {dropX, busY}, {dropX, l.clusterTop}})
		if len(l.schedules) > 1 && !closeTo(cx(s), dropX) {
			l.dot(EdgeBaseBackup, cx(s), busY)
		}
	}
}

// wireReplication runs replication down the corridor between the primary
// and its replicas. Each branch enters a replica's left edge below the
// read wire's port, so the two arrivals stay two arrows.
func (l *layout) wireReplication() {
	if l.primary == nil || len(l.replicas) == 0 {
		return
	}
	replBus := l.primX + wSrv + replGap/2
	out := cy(l.primary) + portOff
	firstIn := out
	var last *viewmodel.TopoEdge
	for i, r := range l.replicas {
		in := cy(r) + portOff
		last = l.wire(EdgeReplicate, []point{{right(l.primary), out}, {replBus, out}, {replBus, in}, {left(r), in}})
		if i < len(l.replicas)-1 {
			l.dot(EdgeReplicate, replBus, in)
		}
		if i == 0 {
			firstIn = in
		}
	}
	// The corridor is tall and narrow, so the label stands upright in
	// it, on the stretch above the first branch — still one trunk, and
	// crossed by no read wire. Vertical labels are drawn centred on
	// (LabelX, LabelY) and turned a quarter left, so the text sits just
	// left of the lane.
	last.Label = "replication"
	last.LabelX = int(replBus) - 4
	last.LabelY = int((out + firstIn) / 2)
	last.LabelVertical = true
}

// wireVolumes runs each instance onto its claims: one exit, a drop bus
// beside the claim column, a branch per claim. A lone level claim keeps
// its straight wire — the route collapses. Folded storage: every
// instance that held a claim feeds the one box, the branches joining on
// a bus beside it.
func (l *layout) wireVolumes() {
	for _, g := range l.groups {
		busX := l.colX[g.col] - busInset
		out := cy(g.of)
		for _, c := range g.nodes {
			l.wire(EdgeVolume, []point{{right(g.of), out}, {busX, out}, {busX, cy(c)}, {left(c), cy(c)}})
		}
		if len(g.nodes) > 1 {
			l.dot(EdgeVolume, busX, out)
		}
	}
	if !l.f.storage || l.snapshot == nil {
		return
	}
	snapshot := l.snapshot
	busX := l.storeX - busInset
	var from []*node
	for _, name := range l.f.volumesFrom {
		if n := l.byName[name]; n != nil {
			from = append(from, n)
		}
	}
	far := -1.0
	for _, n := range from {
		far = max(far, math.Abs(cy(n)-cy(snapshot)))
	}
	for _, n := range from {
		l.wire(EdgeVolume, []point{{right(n), cy(n)}, {busX, cy(n)}, {busX, cy(snapshot)}, {left(snapshot), cy(snapshot)}})
		if len(from) > 1 && math.Abs(cy(n)-cy(snapshot)) < far && !closeTo(cy(n), cy(snapshot)) {
			l.dot(EdgeVolume, busX, cy(n))
		}
	}
}

// wireArchive climbs the two archive trunks up the cluster-to-storage
// alley. WAL archiving is the primary's continuous duty; a base backup
// is served by whichever instance the operator reported, by default a
// standby. One wire for both would assert an origin the Backup resources
// deny, so the base wire leaves the instance the operator named, and is
// not drawn at all when no Backup named one.
//
// The two flows take the whole width of the alley, far enough apart that
// a cluster shipping WAL and taking base backups from the same instance
// still reads as two wires.
func (l *layout) wireArchive() {
	walBus := l.clusterRight + alley*0.3
	baseBus := l.clusterRight + alley*0.7
	if l.in.WAL && l.primary != nil && l.store != nil {
		e := l.trunk(EdgeWAL, l.primary, walBus, -portOff/2, l.store)
		e.Label = "WAL archive"
		e.LabelX = int((right(l.primary) + walBus) / 2)
		e.LabelY = int(cy(l.primary)-portOff) - 7
		if l.f.cluster {
			// No replicas column to the right: the run out of the box is too
			// short for the words, so they stand upright on the climb.
			e.LabelX = int(walBus) - 4
			e.LabelY = int((cy(l.primary) - portOff + cy(l.store) - portOff/2) / 2)
			e.LabelVertical = true
		}
	}
	if l.in.Base == nil {
		return
	}
	src := l.byName[l.in.Base.Instance]
	var to *node
	switch l.in.Base.Target {
	case TargetStore:
		to = l.store
	case TargetSnapshot:
		to = l.snapshot
	}
	if src == nil || to == nil {
		return
	}
	e := l.trunk(EdgeBaseBackup, src, baseBus, portOff/2, to)
	// When the operator picked the primary and WAL leaves the same port,
	// mark the split the way every other fan marks it.
	if src == l.primary && l.in.WAL && l.store != nil {
		l.dot(EdgeBaseBackup, right(src), cy(src)-portOff)
	}
	// The run out of the instance is too short to carry text, so the
	// label stands upright on the climb.
	if l.in.Base.Label != "" {
		e.Label = l.in.Base.Label
		e.LabelX = int(baseBus) - 4
		e.LabelY = int((cy(src) - portOff + cy(to) + portOff/2) / 2)
		e.LabelVertical = true
	}
}

// trunk is one archive wire: out of from's upper port, up or down the bus,
// into to at inOff from its middle.
func (l *layout) trunk(kind string, from *node, bus, inOff float64, to *node) *viewmodel.TopoEdge {
	out := cy(from) - portOff
	in := cy(to) + inOff
	return l.wire(kind, []point{{right(from), out}, {bus, out}, {bus, in}, {left(to), in}})
}

// --- Frames around what was actually drawn. ---

// frameAll draws the five category frames, each only when it holds a box.
// The poolers and the cluster share the main row: both frames hang from
// the cluster's top line, and each bottom follows its content.
func (l *layout) frameAll() {
	if l.hasPool() {
		minX, maxX, maxY := bounds(l.poolers)
		l.frame(viewmodel.TopoFrame{
			Label: "Poolers", Kind: FramePool,
			X: minX - pad, Y: int(l.clusterTop), W: maxX - minX + 2*pad, H: maxY + pad - int(l.clusterTop),
		})
	}
	var clusterMembers []*node
	for _, typ := range []string{"rw", "ro", "r"} {
		if s := l.svc[typ]; s != nil {
			clusterMembers = append(clusterMembers, s)
		}
	}
	if l.primary != nil {
		clusterMembers = append(clusterMembers, l.primary)
	}
	clusterMembers = append(clusterMembers, l.others...)
	if len(clusterMembers) > 0 {
		_, _, maxY := bounds(clusterMembers)
		l.frame(viewmodel.TopoFrame{
			Label: "Cluster", Kind: FrameCluster,
			X: int(l.svcX) - pad, Y: int(l.clusterTop), W: int(l.clusterRight-l.svcX) + pad, H: maxY + pad - int(l.clusterTop),
		})
	}
	if len(l.schedules) > 0 {
		l.frame(viewmodel.TopoFrame{
			Label: "Backups", Kind: FrameBackup,
			X: int(l.bakLeft) - pad, Y: int(l.bandTop), W: int(l.bakRight-l.bakLeft) + 2*pad, H: int(l.bakBottom + pad - l.bandTop),
		})
	}
	if l.hasK8s() {
		var members []*node
		if l.snapshot != nil {
			members = append(members, l.snapshot)
		}
		for _, g := range l.groups {
			members = append(members, g.nodes...)
		}
		_, _, maxY := bounds(members)
		l.frame(viewmodel.TopoFrame{
			Label: "Kubernetes storage", Kind: FrameKubeStorage,
			X: int(l.storeX) - pad, Y: int(l.k8sTop), W: int(l.regionW) + 2*pad, H: maxY + pad - int(l.k8sTop),
		})
	}
	if l.hasObject() {
		l.frame(viewmodel.TopoFrame{
			Label: "Object storage", Note: l.in.StoreNote, Kind: FrameObjectStorage,
			X: int(l.storeX) - pad, Y: int(l.bandTop), W: int(l.regionW) + 2*pad, H: int(l.objBottom + pad - l.bandTop),
		})
	}
}

// frame adds one frame, marked foldable and folded as the input asked.
func (l *layout) frame(fr viewmodel.TopoFrame) {
	folded := map[string]bool{FramePool: l.f.pool, FrameCluster: l.f.cluster, FrameBackup: l.f.backup, FrameKubeStorage: l.f.storage}
	fr.Foldable = slices.Contains(Foldable, fr.Kind)
	fr.Folded = folded[fr.Kind]
	l.out.Frames = append(l.out.Frames, fr)
}

// finish sizes the viewBox round every frame and every upright label,
// with the outer margin, places each box's rows and keys the legend to
// the styles drawn.
func (l *layout) finish() Drawing {
	bottomEdge := 0
	for _, f := range l.out.Frames {
		bottomEdge = max(bottomEdge, f.Y+f.H)
	}
	for i := range l.out.Edges {
		e := &l.out.Edges[i]
		if e.Label == "" || !e.LabelVertical {
			continue
		}
		half := charW * utf8.RuneCountInString(e.Label) / 2
		e.LabelY = max(e.LabelY, margin+half)
		bottomEdge = max(bottomEdge, e.LabelY+half)
	}
	l.out.Width = l.width
	l.out.Height = bottomEdge + margin

	for _, n := range l.nodes {
		place(n, l.rows[n])
		l.out.Nodes = append(l.out.Nodes, *n)
	}
	for _, item := range legendOrder {
		if l.drawn[item.Kind] {
			l.out.Legend = append(l.out.Legend, item)
		}
	}
	return l.out
}

// MoreLabel is the label of a "+N more" summary box.
func MoreLabel(n int, what string) string {
	return fmt.Sprintf("+%d more %s", n, what)
}
