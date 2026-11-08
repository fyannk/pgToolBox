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

package status

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/topology"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// The drawing's bounds: more objects than these are summarised in one
// "+N more" box, so the drawing stays readable and its size bounded.
const (
	topoMaxInst   = 9 // instances, the summary box included
	topoMaxPool   = 6 // poolers, the summary box included
	topoMaxSched  = 3 // ScheduledBackups, the summary box included
	topoMaxClaims = 3 // PVCs per instance, the summary box included
)

// Topology is the grouped wiring drawing of one Cluster (its "physical
// wiring"): poolers into the rw/ro Services, the Services into the
// instances, replication down the primary–replica corridor, each
// instance onto its PVCs, the ScheduledBackups over the cluster, WAL
// archiving and the attributed base backup into the object store or the
// volume snapshots. Every box is worded here from the informer cache;
// internal/console/topology places it.
//
// fold lists the frames (topology.Foldable kinds) to draw folded; an
// unknown kind is ignored.
func (s *Service) Topology(ctx context.Context, namespace, name string, fold ...string) (viewmodel.Topology, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return viewmodel.Topology{}, err
	}
	out := viewmodel.Topology{
		Ref: ref(g.cluster), GeneratedAt: s.now().UTC(), Title: "How " + name + " is wired",
		Caption: topology.Caption, BlindSpots: []viewmodel.BlindSpot{},
	}
	in, facts := s.topologyInput(ctx, g)
	in.Fold = fold
	d := topology.Layout(in)
	out.Width, out.Height = d.Width, d.Height
	out.Frames, out.Edges, out.Nodes, out.Dots, out.Legend = d.Frames, d.Edges, d.Nodes, d.Dots, d.Legend
	out.Aria = topoAria(name, facts)

	blind := func(id, text string) {
		out.BlindSpots = append(out.BlindSpots, viewmodel.BlindSpot{ID: id, Panel: "topology", Text: text})
	}
	if facts.primary == "" {
		blind("topology.noPrimary", "The operator reports no current primary: the write path cannot be drawn.")
	}
	if !s.cache.Readable(g.key.Namespace, cache.SourceServices) {
		blind("topology.servicesNotRead", "Services could not be read: the Service boxes carry the standard CloudNativePG names and routing, not an observed address or port.")
	}
	if !facts.pvcsRead {
		blind("topology.pvcsNotRead", "PersistentVolumeClaims could not be read: no volume is drawn.")
	}
	if in.Snapshot != nil {
		blind("topology.snapshotsNotRead", "VolumeSnapshots are outside the console's Role: the snapshot box counts Backups of method volumeSnapshot, not the snapshots themselves.")
	}
	if facts.storeUnread {
		blind("topology.objectStoreNotRead", "The ObjectStore "+facts.storeName+" could not be read: its destination and retention are not shown.")
	}
	out.BlindSpots = append(out.BlindSpots, panelBlindSpots(g, "topology")...)
	return out, nil
}

// topoFacts are what the text alternative and the blind spots need.
type topoFacts struct {
	primary     string
	replicas    []string
	poolers     int
	schedules   int
	volumes     int
	archived    bool
	pvcsRead    bool
	storeUnread bool
	storeName   string
	base        *topology.Base
}

// topologyInput words every box of the drawing from the gathered
// snapshot, group by group in the drawing's order, and keeps the facts
// the text alternative and the blind spots need.
func (s *Service) topologyInput(ctx context.Context, g *gathered) (topology.Input, topoFacts) {
	var in topology.Input
	var facts topoFacts
	var drawn map[string]bool
	in.Instances, drawn = s.topoInstances(g, &facts)
	in.Services = s.topoServices(ctx, g)
	in.Poolers = topoPoolers(g, &facts)
	in.Schedules = topoSchedules(g, s.now().UTC(), &facts)

	// The object store: the barman-cloud ObjectStore (plugin) or the
	// in-tree barmanObjectStore, when the Cluster archives to one.
	cfg := cnpgstatus.Backups(g.cluster)
	inTree := g.cluster.Spec.Backup != nil && g.cluster.Spec.Backup.BarmanObjectStore != nil
	facts.archived = cfg.PluginBased || inTree
	if facts.archived {
		box, note := s.storeBox(g, cfg, inTree, &facts)
		in.Store, in.StoreNote = &box, note
		in.WAL = true
	}

	in.Snapshot = topoSnapshot(g)

	// The base-backup flow: the newest Backup that names the instance
	// that served it. Without one the wire is not drawn.
	in.Base = baseFlow(g, in.Store != nil, in.Snapshot != nil)
	facts.base = in.Base

	// PVCs of the drawn instances.
	in.Claims, facts.pvcsRead = s.claimBoxes(ctx, g, drawn)
	facts.volumes = len(in.Claims)
	return in, facts
}

// topoInstances words the instances, primary first (instanceRows sorts
// them so), bounded by topoMaxInst with a "+N more" box for the rest.
// drawn names the instances that have a box, whose PVCs are drawn.
func (s *Service) topoInstances(g *gathered, facts *topoFacts) (boxes []topology.Instance, drawn map[string]bool) {
	rows := s.instanceRows(g, false)
	shown := rows
	if len(rows) > topoMaxInst {
		shown = rows[:topoMaxInst-1]
	}
	standbys := attachedStandbys(rows)
	drawn = map[string]bool{}
	for _, r := range shown {
		boxes = append(boxes, instanceBox(g, r, standbys))
		drawn[r.Name] = true
		switch r.Role.Operator {
		case viewmodel.RolePrimary:
			facts.primary = r.Name
		case viewmodel.RoleReplica:
			facts.replicas = append(facts.replicas, r.Name)
		case viewmodel.RoleUnknown:
			// Neither: an instance whose role is unknown is not counted.
		}
	}
	if extra := len(rows) - len(shown); extra > 0 {
		worst := make([]viewmodel.Health, 0, extra)
		for _, r := range rows[len(shown):] {
			worst = append(worst, r.Health)
		}
		label := topology.MoreLabel(extra, "instances")
		boxes = append(boxes, topology.Instance{Box: topology.Box{
			ID: "inst-more", Kind: topology.KindInstance, Label: label, State: viewmodel.Worst(worst...), Link: "instances", Count: extra,
			Rows: []topology.Row{{Class: "label", Text: label}, {Class: "sub", Text: "see the instances screen"}},
		}})
	}
	return boxes, drawn
}

// attachedStandbys is the primary's walsender report by standby name, or
// nil. Only a current read of the primary says who is attached: a stale
// reading would place a replica out of the set on old news.
func attachedStandbys(rows []viewmodel.InstanceRow) map[string]viewmodel.Standby {
	var standbys map[string]viewmodel.Standby
	for _, r := range rows {
		if r.Role.Operator == viewmodel.RolePrimary && r.Live != nil && r.Live.IsPrimary && r.StatusRead.State == viewmodel.StatusReadOK && !r.Live.Provenance.Stale {
			standbys = map[string]viewmodel.Standby{}
			for _, sb := range r.Live.Standbys {
				standbys[sb.Name] = sb
			}
		}
	}
	return standbys
}

// topoServices words the Services: rw always, ro unless disabled, r only
// when a Pooler fronts it (the any-instance Service would otherwise add a
// column of wires that says nothing the ro and rw wires do not).
func (s *Service) topoServices(ctx context.Context, g *gathered) []topology.Service {
	disabled := map[string]bool{}
	if m := g.cluster.Spec.Managed; m != nil && m.Services != nil {
		for _, t := range m.Services.DisabledDefaultServices {
			disabled[string(t)] = true
		}
	}
	poolerTypes := map[string]bool{}
	for _, p := range g.poolers {
		poolerTypes[cnpgstatus.PoolerState(g.minor, p).Type] = true
	}
	var out []topology.Service
	for _, svc := range []struct{ typ, routing string }{
		{"rw", "routes to the primary"}, {"ro", "routes to the replicas"}, {"r", "routes to any instance"},
	} {
		if disabled[svc.typ] || (svc.typ == "r" && !poolerTypes["r"]) {
			continue
		}
		label, routing, state := g.cluster.Name+"-"+svc.typ, svc.routing, viewmodel.HealthInfo
		if svc.typ == "ro" && g.cluster.Spec.Instances < 2 {
			// The ro Service selects the replicas only: a Cluster that
			// declares one instance has none, and the Service refuses
			// every connection. Read from the spec, like
			// the operator's pgAdmin offer, so a restarting replica does
			// not make the note flap.
			routing, state = "routes to the replicas: none", viewmodel.HealthInactive
		}
		out = append(out, topology.Service{Type: svc.typ, Box: topology.Box{
			ID: "svc-" + svc.typ, Kind: topology.KindService, Label: label, State: state,
			Rows: []topology.Row{{Class: "label", Text: label}, {Class: "sub", Text: routing}, {Class: "disk", Text: s.serviceAddress(ctx, g.key.Namespace, label)}},
		}})
	}
	return out
}

// topoPoolers words the Poolers by name, bounded by topoMaxPool with a
// "+N more" box for the rest.
func topoPoolers(g *gathered, facts *topoFacts) []topology.Pooler {
	poolers := slices.Clone(g.poolers)
	sort.Slice(poolers, func(i, j int) bool { return poolers[i].Name < poolers[j].Name })
	facts.poolers = len(poolers)
	shown := poolers
	if len(poolers) > topoMaxPool {
		shown = poolers[:topoMaxPool-1]
	}
	var out []topology.Pooler
	for _, p := range shown {
		out = append(out, poolerBox(g, p))
	}
	if extra := len(poolers) - len(shown); extra > 0 {
		worst := make([]viewmodel.Health, 0, extra)
		for _, p := range poolers[len(shown):] {
			worst = append(worst, health(cnpgstatus.PoolerState(g.minor, p).Class))
		}
		label := topology.MoreLabel(extra, "poolers")
		out = append(out, topology.Pooler{Box: topology.Box{
			ID: "pooler-more", Kind: topology.KindPooler, Label: label, State: viewmodel.Worst(worst...), Link: "objects", Count: extra,
			Rows: []topology.Row{{Class: "label", Text: label}, {Class: "sub", Text: "see the objects screen"}},
		}})
	}
	return out
}

// topoSchedules words the ScheduledBackups by name, bounded by
// topoMaxSched with a "+N more" box for the rest: the schedule, and
// whether it is suspended or when it next runs.
func topoSchedules(g *gathered, now time.Time, facts *topoFacts) []topology.Box {
	scheduled := slices.Clone(g.scheduled)
	sort.Slice(scheduled, func(i, j int) bool { return scheduled[i].Name < scheduled[j].Name })
	facts.schedules = len(scheduled)
	shown := scheduled
	if len(scheduled) > topoMaxSched {
		shown = scheduled[:topoMaxSched-1]
	}
	var out []topology.Box
	for _, sb := range shown {
		r := cnpgstatus.Schedule(sb, now)
		rows := []topology.Row{{Class: "label", Text: sb.Name}}
		if r.Schedule != "" {
			rows = append(rows, topology.Row{Class: "disk", Text: r.Schedule})
		}
		switch {
		case r.Suspended:
			rows = append(rows, topology.Row{Class: "sub", Text: "suspended"})
		case r.NextScheduleTime != nil:
			rows = append(rows, topology.Row{Class: "sub", Text: "next " + r.NextScheduleTime.UTC().Format("Jan 2 15:04 UTC")})
		}
		out = append(out, topology.Box{
			ID: "sched-" + sb.Name, Kind: topology.KindSchedule, Label: sb.Name, State: health(r.Class), Link: "backups", Rows: rows,
		})
	}
	if extra := len(scheduled) - len(shown); extra > 0 {
		label := topology.MoreLabel(extra, "schedules")
		out = append(out, topology.Box{
			ID: "sched-more", Kind: topology.KindSchedule, Label: label, State: viewmodel.HealthInfo, Link: "backups", Count: extra,
			Rows: []topology.Row{{Class: "label", Text: label}, {Class: "sub", Text: "see the backups screen"}},
		})
	}
	return out
}

// topoSnapshot words the volume snapshots box when snapshots are
// configured or used by a Backup; nil otherwise. It counts Backups of
// method volumeSnapshot: the snapshots themselves are not readable.
func topoSnapshot(g *gathered) *topology.Box {
	var snapshotBackups int
	for _, b := range g.backups {
		if cnpgstatus.BackupObject(g.minor, b).Method == cnpgstatus.BackupMethodVolumeSnapshot {
			snapshotBackups++
		}
	}
	snapCfg := g.cluster.Spec.Backup != nil && g.cluster.Spec.Backup.VolumeSnapshot != nil
	if !snapCfg && snapshotBackups == 0 {
		return nil
	}
	rows := []topology.Row{{Class: "label", Text: "Volume snapshots"}}
	if g.listed(cache.SourceBackups) {
		rows = append(rows, topology.Row{Class: "sub", Text: fmt.Sprintf("%d snapshot backup(s)", snapshotBackups)})
	} else {
		rows = append(rows, topology.Row{Class: "sub", Text: "Backups not read"})
	}
	if snapCfg && g.cluster.Spec.Backup.VolumeSnapshot.ClassName != "" {
		rows = append(rows, topology.Row{Class: "disk", Text: "class " + g.cluster.Spec.Backup.VolumeSnapshot.ClassName})
	}
	return &topology.Box{ID: "snapshot", Kind: topology.KindSnapshot, Label: "Volume snapshots", State: viewmodel.HealthInfo, Link: "backups", Rows: rows}
}

// instanceBox words one instance: role, timeline (primary) or the
// primary's walsender report (replica), pod phase and readiness, node,
// image. Each row is an observed or operator-reported fact; a fact the
// snapshot does not carry has no row.
func instanceBox(g *gathered, r viewmodel.InstanceRow, standbys map[string]viewmodel.Standby) topology.Instance {
	inst := topology.Instance{Name: r.Name, Box: topology.Box{
		ID: "inst-" + r.Name, Label: r.Name, State: r.Health, Link: "instances/" + r.Name,
	}}
	var rows []topology.Row
	switch r.Role.Operator {
	case viewmodel.RolePrimary:
		inst.Kind, inst.Role = topology.KindPrimary, topology.RolePrimary
		rows = append(rows, topology.Row{Class: "label", Text: r.Name + " — primary"})
		if t := g.cluster.Status.TimelineID; t > 0 {
			rows = append(rows, topology.Row{Class: "sub", Text: fmt.Sprintf("timeline %d", t)})
		}
	case viewmodel.RoleReplica:
		inst.Kind, inst.Role = topology.KindReplica, topology.RoleReplica
		rows = append(rows, topology.Row{Class: "label", Text: r.Name + " — replica"})
		if standbys != nil {
			if sb, ok := standbys[r.Name]; ok {
				rows = append(rows, topology.Row{Class: "sub", Text: joinNonEmpty(sb.State, sb.SyncState)})
			} else {
				rows = append(rows, topology.Row{Class: "sub", Text: "no walsender on the primary"})
			}
		}
	default:
		inst.Kind = topology.KindInstance
		rows = append(rows, topology.Row{Class: "label", Text: r.Name + " — role not reported"})
	}
	rows = append(rows, topology.Row{Class: "sub", Text: podCondition(r)})
	if r.Pod != nil {
		if r.Pod.Node != "" {
			rows = append(rows, topology.Row{Class: "disk", Text: "node " + r.Pod.Node})
		}
		if image := shortImage(r.Pod.Image); image != "" {
			rows = append(rows, topology.Row{Class: "disk", Text: image})
		}
	}
	inst.Rows = rows
	return inst
}

// podCondition states the pod's phase and readiness in one line, with
// the restart count only when there is one to report.
func podCondition(r viewmodel.InstanceRow) string {
	if r.Pod == nil {
		return "no pod"
	}
	if r.Pod.Waiting != "" && !r.Pod.Deleting {
		// The waiting reason says more than the phase, and the box has
		// room for one of them beside the restart count.
		return joinNonEmpty(r.Pod.Waiting, restarts(r.Pod.Restarts), fenced(r.Fenced))
	}
	phase := r.Pod.Phase
	if phase == "" {
		phase = "phase not reported"
	}
	parts := []string{phase}
	switch {
	case r.Pod.Deleting:
		parts = append(parts, "deleting")
	case r.Pod.Ready:
		parts = append(parts, "ready")
	default:
		parts = append(parts, "not ready")
	}
	return joinNonEmpty(append(parts, restarts(r.Pod.Restarts), fenced(r.Fenced))...)
}

func restarts(n int32) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 restart"
	default:
		return fmt.Sprintf("%d restarts", n)
	}
}

func fenced(f bool) string {
	if f {
		return "fenced"
	}
	return ""
}

// shortImage keeps the image's final element, the repository and tag a
// reader recognises without the registry path.
func shortImage(image string) string {
	if cut := strings.LastIndex(image, "/"); cut >= 0 {
		return image[cut+1:]
	}
	return image
}

func poolerBox(g *gathered, p *cnpgv1.Pooler) topology.Pooler {
	r := cnpgstatus.PoolerState(g.minor, p)
	sub := joinNonEmpty("pgbouncer "+r.Type, r.PoolMode)
	if r.Paused {
		sub += " · paused"
	}
	rows := []topology.Row{
		{Class: "label", Text: p.Name},
		{Class: "sub", Text: sub},
		// status.instances is the Deployment's replicas: Pods created,
		// not Pods ready.
		{Class: "disk", Text: fmt.Sprintf("%d of %d pods created", r.Instances, r.DesiredInstances)},
	}
	return topology.Pooler{Type: r.Type, Box: topology.Box{
		ID: "pooler-" + p.Name, Kind: topology.KindPooler, Label: p.Name, State: health(r.Class), Link: "objects", Rows: rows,
	}}
}

// storeBox words the object store from what the console read: the
// ObjectStore (plugin) or the Cluster's barmanObjectStore (in-tree). The
// endpoint goes to the frame note. URLs lose any user information, query
// and fragment before they are shown.
func (s *Service) storeBox(g *gathered, cfg cnpgstatus.BackupConfig, inTree bool, facts *topoFacts) (topology.Box, string) {
	box := topology.Box{ID: "store", Kind: topology.KindStore, State: health(cnpgstatus.Archiving(g.cluster, primaryArchiver(g)).Class), Link: "backups"}
	var destination, endpoint, retention string
	switch {
	case inTree:
		box.Label = "barmanObjectStore"
		destination = g.cluster.Spec.Backup.BarmanObjectStore.DestinationPath
		endpoint = g.cluster.Spec.Backup.BarmanObjectStore.EndpointURL
		retention = g.cluster.Spec.Backup.RetentionPolicy
	case objectStoreName(g.cluster) != "":
		facts.storeName = objectStoreName(g.cluster)
		box.Label = "ObjectStore " + facts.storeName
		if g.store == nil {
			facts.storeUnread = true
			break
		}
		destination = nestedString(g.store, "spec", "configuration", "destinationPath")
		endpoint = nestedString(g.store, "spec", "configuration", "endpointURL")
		retention = nestedString(g.store, "spec", "retentionPolicy")
	default:
		box.Label = "object store"
	}
	rows := []topology.Row{{Class: "label", Text: box.Label}, {Class: "sub", Text: "WAL archive · base backups"}}
	if d := displayURL(destination); d != "" {
		rows = append(rows, topology.Row{Class: "disk", Text: d})
	}
	if retention != "" {
		rows = append(rows, topology.Row{Class: "sub", Text: "retention " + retention})
	}
	if cfg.PluginBased && cfg.PluginName != "" {
		rows = append(rows, topology.Row{Class: "note", Text: "plugin " + shortPlugin(cfg.PluginName)})
	}
	box.Rows = rows
	return box, displayURL(endpoint)
}

func nestedString(u *unstructured.Unstructured, fields ...string) string {
	v, _, _ := unstructured.NestedString(u.Object, fields...)
	return v
}

// displayURL returns a URL without user information, query or fragment, or
// nothing when it does not parse: a credential pasted into a URL never
// reaches the drawing.
func displayURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return ""
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}

// baseFlow is the newest Backup that names its instance, wired to the
// box its method writes to; nil when none names one or its box is not
// drawn.
func baseFlow(g *gathered, store, snapshot bool) *topology.Base {
	backups := slices.Clone(g.backups)
	sort.SliceStable(backups, func(i, j int) bool {
		if !backups[i].CreationTimestamp.Equal(&backups[j].CreationTimestamp) {
			return backups[i].CreationTimestamp.After(backups[j].CreationTimestamp.Time)
		}
		return backups[i].Name < backups[j].Name
	})
	for _, b := range backups {
		r := cnpgstatus.BackupObject(g.minor, b)
		if r.PodName == "" {
			continue
		}
		switch r.Method {
		case cnpgstatus.BackupMethodVolumeSnapshot:
			if snapshot {
				return &topology.Base{Instance: r.PodName, Target: topology.TargetSnapshot, Label: "base backup · volume snapshot"}
			}
		case cnpgstatus.BackupMethodPlugin:
			if store {
				return &topology.Base{Instance: r.PodName, Target: topology.TargetStore, Label: joinNonEmpty("base backup", shortPlugin(r.PluginName))}
			}
		case cnpgstatus.BackupMethodBarmanObjectStore:
			if store {
				return &topology.Base{Instance: r.PodName, Target: topology.TargetStore, Label: "base backup · barman-cloud"}
			}
		}
		return nil
	}
	return nil
}

// claimBoxes words the PVCs of the drawn instances, data before WAL
// before tablespaces, bounded per instance. ok is false when the PVCs
// could not be read.
func (s *Service) claimBoxes(ctx context.Context, g *gathered, drawn map[string]bool) (claims []topology.Claim, ok bool) {
	if !s.cache.Readable(g.key.Namespace, cache.SourcePVCs) {
		return nil, false
	}
	list := &corev1.PersistentVolumeClaimList{}
	if err := s.cache.Reader().List(ctx, list, client.InNamespace(g.key.Namespace), client.MatchingFields{cache.IndexCluster: g.key.Name}); err != nil {
		return nil, false
	}
	rank := map[string]int{cnpgstatus.PVCRolePgData: 0, cnpgstatus.PVCRolePgWal: 1, cnpgstatus.PVCRolePgTablespace: 2}
	type claim struct {
		pvc  *corev1.PersistentVolumeClaim
		role cnpgstatus.PVCRoleReading
	}
	byInstance := map[string][]claim{}
	for i := range list.Items {
		pvc := &list.Items[i]
		role := cnpgstatus.PVCRole(pvc.Labels)
		if drawn[role.Instance] {
			byInstance[role.Instance] = append(byInstance[role.Instance], claim{pvc, role})
		}
	}
	instances := make([]string, 0, len(byInstance))
	for name := range byInstance {
		instances = append(instances, name)
	}
	sort.Strings(instances)
	for _, name := range instances {
		group := byInstance[name]
		sort.Slice(group, func(i, j int) bool {
			ri, okI := rank[group[i].role.Role]
			rj, okJ := rank[group[j].role.Role]
			if !okI {
				ri = 3
			}
			if !okJ {
				rj = 3
			}
			if ri != rj {
				return ri < rj
			}
			return group[i].pvc.Name < group[j].pvc.Name
		})
		shown := group
		if len(group) > topoMaxClaims {
			shown = group[:topoMaxClaims-1]
		}
		for _, c := range shown {
			claims = append(claims, topology.Claim{Instance: name, Box: s.claimBox(g, c.pvc, c.role)})
		}
		if extra := len(group) - len(shown); extra > 0 {
			label := topology.MoreLabel(extra, "volumes")
			claims = append(claims, topology.Claim{Instance: name, Box: topology.Box{
				ID: "pvc-more-" + name, Kind: topology.KindVolume, Label: label, State: viewmodel.HealthInfo, Link: "storage", Count: extra,
				Rows: []topology.Row{{Class: "label", Text: label}, {Class: "sub", Text: "see the storage screen"}},
			}})
		}
	}
	return claims, true
}

func (s *Service) claimBox(g *gathered, pvc *corev1.PersistentVolumeClaim, role cnpgstatus.PVCRoleReading) topology.Box {
	group := cnpgstatus.PVCGroup(g.minor, g.cluster, pvc.Name)
	purpose := role.Role
	if role.Tablespace != "" {
		purpose += " " + role.Tablespace
	}
	rows := []topology.Row{{Class: "label", Text: pvc.Name}}
	if line := joinNonEmpty(purpose, string(pvc.Status.Phase)); line != "" {
		rows = append(rows, topology.Row{Class: "sub", Text: line})
	}
	var capacity, class string
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		capacity = q.String()
	}
	if pvc.Spec.StorageClassName != nil {
		class = *pvc.Spec.StorageClassName
	}
	if line := joinNonEmpty(capacity, class); line != "" {
		rows = append(rows, topology.Row{Class: "disk", Text: line})
	}
	return topology.Box{ID: "pvc-" + pvc.Name, Kind: topology.KindVolume, Label: pvc.Name, State: health(group.Class), Link: "storage", Rows: rows}
}

// shortPlugin is a plugin's first name label (barman-cloud for
// barman-cloud.cloudnative-pg.io): the name a reader recognises.
func shortPlugin(name string) string {
	if cut := strings.Index(name, "."); cut > 0 {
		return name[:cut]
	}
	return name
}

// joinNonEmpty joins the non-empty parts with a middle dot, the
// separator of every topology sub-line.
func joinNonEmpty(parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
}

func topoAria(name string, f topoFacts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The wiring of %s", name)
	if f.poolers > 0 {
		fmt.Fprintf(&b, "; %d pooler(s) front its Services", f.poolers)
	}
	if f.primary != "" {
		fmt.Fprintf(&b, "; %s-rw leads to the primary %s", name, f.primary)
	} else {
		b.WriteString("; no primary is reported")
	}
	if len(f.replicas) > 0 {
		fmt.Fprintf(&b, "; %s-ro leads to the replicas %s, which stream from the primary", name, strings.Join(f.replicas, ", "))
	}
	if f.volumes > 0 {
		fmt.Fprintf(&b, "; %d volume(s) are drawn beside their instances", f.volumes)
	}
	if f.schedules > 0 {
		fmt.Fprintf(&b, "; %d backup schedule(s) target the cluster", f.schedules)
	}
	if f.archived {
		b.WriteString("; WAL is archived to the object store")
	}
	if f.base != nil {
		fmt.Fprintf(&b, "; the newest attributed base backup left %s", f.base.Instance)
	}
	return b.String() + "."
}

// serviceAddress words where a Service answers, as clients dial it:
// "ClusterIP 10.96.0.12:5432", the type first when it is not ClusterIP.
// It says so plainly when the Service is absent or cannot be read.
func (s *Service) serviceAddress(ctx context.Context, namespace, name string) string {
	if !s.cache.Readable(namespace, cache.SourceServices) {
		return "Service not read"
	}
	svc := &corev1.Service{}
	if err := s.cache.Reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return "Service not found"
		}
		return "Service not read"
	}
	addr := svc.Spec.ClusterIP
	if addr == "" || addr == corev1.ClusterIPNone {
		addr = "headless"
	}
	if len(svc.Spec.Ports) > 0 {
		addr += fmt.Sprintf(":%d", svc.Spec.Ports[0].Port)
	}
	typ := string(svc.Spec.Type)
	if typ == "" {
		typ = string(corev1.ServiceTypeClusterIP)
	}
	return typ + " " + addr
}
