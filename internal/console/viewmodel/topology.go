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

package viewmodel

import "time"

// Topology is the wiring diagram of a Cluster with its geometry computed
// in Go (the SPA's TopologyView draws SVG from the Go layout): the client
// draws exactly these boxes and paths in a viewBox of Width × Height and
// does no layout. It is the Cluster's grouped "physical wiring"
// (internal/console/topology): Poolers, Cluster, Backups, Kubernetes
// storage and Object storage frames. Instance, pooler, schedule, volume
// and store boxes are observed (their state is a health); the Services'
// routing, replication and the client paths are the fixed CloudNativePG
// shape, and Caption says which is which.
type Topology struct {
	Ref         ClusterRef   `json:"ref"`
	GeneratedAt time.Time    `json:"generatedAt"`
	Title       string       `json:"title"`
	Aria        string       `json:"aria" doc:"Text alternative of the whole drawing."`
	Width       int          `json:"width"`
	Height      int          `json:"height"`
	Frames      []TopoFrame  `json:"frames"`
	Edges       []TopoEdge   `json:"edges"`
	Nodes       []TopoNode   `json:"nodes"`
	Dots        []TopoDot    `json:"dots"`
	Legend      []TopoLegend `json:"legend"`
	Caption     string       `json:"caption"`
	BlindSpots  []BlindSpot  `json:"blindSpots"`
}

// TopoNode is one box.
type TopoNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind" enum:"app,service,primary,replica,instance,pooler,schedule,store,snapshot,volume" doc:"app is no longer drawn; schedule is a ScheduledBackup; volume a PersistentVolumeClaim; instance an instance whose role is not reported, or a summary box."`
	Label string `json:"label"`
	Sub   string `json:"sub,omitempty"`
	Disk  string `json:"disk,omitempty"`
	// State is the box's health; infrastructure boxes are info.
	State Health     `json:"state" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Lines []TopoText `json:"lines,omitempty"`
	X     int        `json:"x"`
	Y     int        `json:"y"`
	W     int        `json:"w"`
	H     int        `json:"h"`
	// Link is the cluster-relative route of the box's detail.
	Link string `json:"link,omitempty"`
}

// TopoText is one positioned line of text inside a node.
type TopoText struct {
	Class string `json:"class" enum:"label,sub,disk,note"`
	Text  string `json:"text"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
}

// TopoEdge is one flow, as an SVG path in the drawing's coordinates.
type TopoEdge struct {
	Kind          string `json:"kind" enum:"write,read,replicate,wal,archive,disk,baseBackup,volume" doc:"wal is WAL archiving (primary to object store); baseBackup the base-backup path (schedule to cluster, source instance to object store or snapshots); volume an instance to its PVC. archive and disk are no longer drawn."`
	Label         string `json:"label,omitempty"`
	Path          string `json:"path" doc:"SVG path data (M, L, Q and C commands)."`
	LabelX        int    `json:"labelX,omitempty"`
	LabelY        int    `json:"labelY,omitempty"`
	LabelVertical bool   `json:"labelVertical,omitempty" doc:"The label is centred on (labelX, labelY) and turned a quarter left."`
}

// TopoFrame is a dotted category box drawn under the nodes.
type TopoFrame struct {
	Label string `json:"label"`
	Note  string `json:"note,omitempty"`
	Kind  string `json:"kind" enum:"clients,cluster,pool,backup,store,kubeStorage,objectStorage" doc:"kubeStorage holds PVCs and volume snapshots, objectStorage the object store; clients and store are no longer drawn."`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	// Foldable frames can be asked for folded (the topology's fold
	// parameter); Folded ones were, and hold one summary box.
	Foldable bool `json:"foldable,omitempty" doc:"The frame can be drawn folded: ask for it in the fold parameter."`
	Folded   bool `json:"folded,omitempty" doc:"The frame is drawn folded: one summary box stands for its objects."`
}

// TopoDot marks where one trunk splits.
type TopoDot struct {
	Kind string `json:"kind" enum:"write,read,replicate,wal,baseBackup,volume" doc:"The kind of the edge that splits."`
	X    int    `json:"x"`
	Y    int    `json:"y"`
}

// TopoLegend keys one line style actually drawn.
type TopoLegend struct {
	Kind  string `json:"kind" enum:"write,read,replicate,wal,baseBackup,volume"`
	Label string `json:"label"`
}
