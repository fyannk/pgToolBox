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

// The per-Cluster screens: replication, backups, storage and objects. Every fact comes from a pkg/cnpgstatus reading (cited per
// CNPG minor there) or verbatim from the object; this file only arranges
// readings into view models and attaches provenance.

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// panelBlindSpots are the Cluster's blind spots for some panels.
func panelBlindSpots(g *gathered, panels ...string) []viewmodel.BlindSpot {
	out := []viewmodel.BlindSpot{}
	for _, b := range blindSpots(g) {
		if slices.Contains(panels, b.Panel) {
			out = append(out, b)
		}
	}
	return out
}

// pickSources returns the namespace's states of the named sources, plus
// the sweep's when asked for "instanceStatus".
func (s *Service) pickSources(namespace string, names ...string) []viewmodel.SourceState {
	out := []viewmodel.SourceState{}
	for _, name := range names {
		if name == sourceInstanceStatus {
			if sw := s.sweeper(); sw != nil {
				out = append(out, sw.State())
			} else {
				out = append(out, viewmodel.SourceState{Source: sourceInstanceStatus, State: viewmodel.SourceDisabled, Reason: "the status sweep is switched off"})
			}
			continue
		}
		out = append(out, s.cache.Source(namespace, name))
	}
	return out
}

const sourceInstanceStatus = "instanceStatus"

// unread marks a source whose list failed at read time: the panel is
// empty because the console could not read it, not because nothing is
// there.
func unread(states []viewmodel.SourceState, source string) []viewmodel.SourceState {
	for i := range states {
		if states[i].Source == source {
			states[i].State, states[i].Reason = viewmodel.SourceUnavailable, "the console's cache could not be read for this view; retry"
			return states
		}
	}
	return append(states, viewmodel.SourceState{Source: source, State: viewmodel.SourceUnavailable, Reason: "the console's cache could not be read for this view; retry"})
}

func intPtr(v int) *int { return &v }

// instanceProvenance is a self-report's provenance: when it was read and
// whether the last read failed.
func instanceProvenance(inst sweep.Instance, field string) viewmodel.Provenance {
	p := viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: field}
	if inst.Reading != nil {
		at := inst.Reading.ObservedAt.UTC()
		p.ObservedAt = &at
	}
	if inst.State != viewmodel.StatusReadOK {
		p.Stale, p.StaleReason = true, inst.Reason
	}
	return p
}

// primaryInstance is the swept instance the operator names primary, or
// the one reporting isPrimary when the operator names none.
func primaryInstance(g *gathered) (string, sweep.Instance, bool) {
	if inst, ok := g.swept.Instances[g.cluster.Status.CurrentPrimary]; ok && inst.Reading != nil {
		return g.cluster.Status.CurrentPrimary, inst, true
	}
	names := make([]string, 0, len(g.swept.Instances))
	for name := range g.swept.Instances {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if inst := g.swept.Instances[name]; inst.Reading != nil && inst.Reading.IsPrimary {
			return name, inst, true
		}
	}
	return "", sweep.Instance{}, false
}

// Replication is the /replication screen.
func (s *Service) Replication(ctx context.Context, namespace, name string) (viewmodel.Replication, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return viewmodel.Replication{}, err
	}
	now := s.now().UTC()
	sync := cnpgstatus.SyncConfig(g.minor, g.cluster)
	out := viewmodel.Replication{
		Ref: ref(g.cluster), GeneratedAt: now,
		Primary: s.primaryElection(g, now),
		Synchronous: viewmodel.SyncConfig{
			Mode: sync.Mode, Method: sync.Method, DataDurability: sync.DataDurability,
			StandbyNamesPre: sync.StandbyNamesPre, StandbyNamesPost: sync.StandbyNamesPost, FailoverQuorum: sync.FailoverQuorum,
			Provenance: s.provenance(namespace, cache.SourceClusters, viewmodel.OriginOperator, "Cluster.spec.postgresql.synchronous", now),
		},
		Standbys: []viewmodel.StandbyRow{}, Receivers: []viewmodel.ReceiverRow{}, Slots: []viewmodel.SlotRow{},
		Sources:    g.markFailed(s.pickSources(namespace, cache.SourceClusters, cache.SourcePods, sourceInstanceStatus, cache.SourceFailoverQuorums, cache.SourceLeases)),
		BlindSpots: panelBlindSpots(g, "replication", "primary", "topology"),
	}
	switch sync.Mode {
	case cnpgstatus.SyncModeExplicit:
		out.Synchronous.Number = intPtr(sync.Number)
	case cnpgstatus.SyncModeLegacy:
		out.Synchronous.MinSync, out.Synchronous.MaxSync = intPtr(sync.MinSync), intPtr(sync.MaxSync)
		out.Synchronous.Provenance.Source = "Cluster.spec.minSyncReplicas/maxSyncReplicas"
	}
	out.Failover = s.failover(g, now)
	if sync.FailureDomainKeysUnreadable {
		out.Synchronous.Unreadable = []string{"spec.postgresql.synchronous.podFailureDomainKeys", "spec.postgresql.synchronous.nodeFailureDomainKeys"}
	}
	if g.quorum != nil {
		q := cnpgstatus.FailoverQuorumState(g.quorum, g.cluster.Status.CurrentPrimary)
		out.Quorum = &viewmodel.QuorumView{
			Health: health(q.Class), Method: q.Method, StandbyNames: q.StandbyNames, Primary: q.Primary, Text: q.Text,
			Provenance: s.provenance(namespace, cache.SourceFailoverQuorums, viewmodel.OriginOperator, "FailoverQuorum.status", now),
		}
		if q.Number > 0 || q.Method != "" {
			out.Quorum.Number = intPtr(q.Number)
		}
	}

	primaryName, primary, hasPrimary := primaryInstance(g)
	primaryLSN := ""
	if hasPrimary {
		// A lag is computed only against a primary LSN read in good
		// standing: a stale one would make every replica look caught up.
		if primary.State == viewmodel.StatusReadOK {
			primaryLSN = primary.Reading.CurrentLSN
		}
		for _, sb := range primary.Reading.Standbys {
			state := cnpgstatus.ClassifyWalSenderState(sb.State)
			syncState := cnpgstatus.ClassifySyncState(sb.SyncState)
			row := viewmodel.StandbyRow{
				Name: bound(sb.ApplicationName), State: sb.State, SyncState: sb.SyncState, SyncPriority: sb.SyncPriority,
				Synchronous: syncState.Synchronous, SentLSN: sb.SentLSN, WriteLSN: sb.WriteLSN, FlushLSN: sb.FlushLSN, ReplayLSN: sb.ReplayLSN,
				WriteLag: sb.WriteLag, FlushLag: sb.FlushLag, ReplayLag: sb.ReplayLag,
				Health: health(state.Class), Provenance: instanceProvenance(primary, "/pg/status replicationInfo of "+primaryName),
			}
			if primary.State == viewmodel.StatusReadOK {
				if lag, ok := sweep.LagBytes(primaryLSN, sb.ReplayLSN); ok {
					row.LagBytes = &lag
				}
			} else {
				row.Health = viewmodel.HealthUnknown
			}
			out.Standbys = append(out.Standbys, row)
		}
		for _, slot := range primary.Reading.Slots {
			wal := cnpgstatus.ClassifySlotWALStatus(slot.WALStatus)
			h := health(wal.Class)
			if primary.State != viewmodel.StatusReadOK {
				h = viewmodel.HealthUnknown
			}
			out.Slots = append(out.Slots, viewmodel.SlotRow{
				Instance: primaryName, Name: bound(slot.Name), Type: slot.Type, Database: bound(slot.Database), Plugin: bound(slot.Plugin),
				WALStatus: slot.WALStatus, Active: slot.Active, Health: h, Provenance: instanceProvenance(primary, "/pg/status replicationSlotsInfo"),
			})
		}
	}
	for _, pod := range g.pods {
		if pod.Name == primaryName || pod.Name == g.cluster.Status.CurrentPrimary {
			continue
		}
		read, live := s.statusRead(g, pod.Name, primaryLSN)
		row := viewmodel.ReceiverRow{Name: pod.Name, StatusRead: read, Health: viewmodel.HealthUnknown}
		inst, swept := g.swept.Instances[pod.Name]
		if swept {
			row.Provenance = instanceProvenance(inst, "/pg/status")
		} else {
			row.Provenance = viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status"}
		}
		if live != nil {
			row.ReceivedLSN, row.ReplayLSN, row.LagBytes, row.ReplayPaused, row.TimelineID = live.ReceivedLSN, live.ReplayLSN, live.LagBytes, live.ReplayPaused, live.TimelineID
			if read.State == viewmodel.StatusReadOK {
				active := live.WALReceiverActive
				row.WALReceiverActive = &active
				switch {
				case live.IsPrimary:
					// A second live primary: the old one not yet demoted.
					row.Health = viewmodel.HealthCritical
				case !active || live.ReplayPaused:
					row.Health = viewmodel.HealthWarning
				default:
					row.Health = viewmodel.HealthOK
				}
			}
		}
		out.Receivers = append(out.Receivers, row)
	}
	return out, nil
}

// Backups is the /backups screen.
func (s *Service) Backups(ctx context.Context, namespace, name string) (viewmodel.Backups, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return viewmodel.Backups{}, err
	}
	now := s.now().UTC()
	cfg := cnpgstatus.Backups(g.cluster)
	out := viewmodel.Backups{
		Ref: ref(g.cluster), GeneratedAt: now,
		Summary: s.backupSummary(g, now),
		Method: viewmodel.BackupMethod{
			Configured: cfg.Configured, PluginBased: cfg.PluginBased, PluginName: cfg.PluginName, ObjectStore: objectStoreName(g.cluster),
			InTreeObjectStore: g.cluster.Spec.Backup != nil && g.cluster.Spec.Backup.BarmanObjectStore != nil,
			VolumeSnapshot:    g.cluster.Spec.Backup != nil && g.cluster.Spec.Backup.VolumeSnapshot != nil,
			Provenance:        s.provenance(namespace, cache.SourceClusters, viewmodel.OriginOperator, "Cluster.spec.backup / Cluster.spec.plugins", now),
		},
		Items: []viewmodel.BackupRow{}, Schedules: []viewmodel.ScheduleRow{},
		Drills:     s.drillsBlock(namespace, name),
		Sources:    g.markFailed(s.pickSources(namespace, cache.SourceBackups, cache.SourceScheduledBackups, cache.SourceObjectStores)),
		BlindSpots: panelBlindSpots(g, "backups"),
	}
	evidence := s.evidenceFor(ctx, g)
	prior := make(map[*cnpgv1.Backup]bool, len(g.priorBackups))
	for _, b := range g.priorBackups {
		prior[b] = true
	}
	backups := slices.Concat(g.backups, g.priorBackups)
	sort.SliceStable(backups, func(i, j int) bool {
		return backups[i].CreationTimestamp.After(backups[j].CreationTimestamp.Time)
	})
	for _, b := range backups {
		r := cnpgstatus.BackupObject(g.minor, b)
		row := viewmodel.BackupRow{
			Name: b.Name, Phase: r.Phase, Health: health(r.Reading.Class), Known: r.Reading.Known, Terminal: r.Reading.Terminal,
			Method: r.Method, PluginName: r.PluginName, Instance: r.PodName, Online: r.Online, BackupID: r.BackupID, Error: r.Error,
			CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, StoppedAt: r.StoppedAt, NotReportedWhileRunning: r.NotReportedWhileRunning,
			Repository: evidence.presence(r), PredatesCluster: prior[b],
			Provenance: s.provenance(namespace, cache.SourceBackups, viewmodel.OriginOperator, "Backup.status", now),
		}
		if cfg.PgBackRest != nil && !prior[b] {
			row.PgBackRest = evidence.pgbackrestRef(b.Name, r)
		}
		if owner := ownerOfKind(b.OwnerReferences, "ScheduledBackup"); owner != "" {
			row.Schedule = owner
		}
		out.Items = append(out.Items, row)
	}
	for _, sb := range g.scheduled {
		r := cnpgstatus.Schedule(sb, now)
		out.Schedules = append(out.Schedules, viewmodel.ScheduleRow{
			Name: sb.Name, Schedule: r.Schedule, Note: r.ScheduleNote, State: r.State, Health: health(r.Class), Text: r.Text,
			Suspended: r.Suspended, Immediate: r.Immediate, Method: r.Method, Target: r.Target,
			LastScheduledAt: r.LastScheduleTime, NextScheduledAt: r.NextScheduleTime, Error: r.Error,
			Provenance: s.provenance(namespace, cache.SourceScheduledBackups, viewmodel.OriginOperator, "ScheduledBackup.status", now),
		})
	}
	out.Method.PgBackRest = pgbackrestMethod(cfg.PgBackRest, evidence.result.Snapshot)
	out.Evidence = evidence.view
	out.RecoveryTimeline = s.recoveryTimeline(evidence, out.Summary, out.Items, recoveryFactsOf(g, now))
	out.Sources = append(out.Sources, evidence.source)
	return out, nil
}

func ownerOfKind(refs []metav1.OwnerReference, kind string) string {
	for _, r := range refs {
		if r.Kind == kind {
			return r.Name
		}
	}
	return ""
}

// Storage is the /storage screen.
func (s *Service) Storage(ctx context.Context, namespace, name string) (viewmodel.Storage, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return viewmodel.Storage{}, err
	}
	now := s.now().UTC()
	out := viewmodel.Storage{
		Ref: ref(g.cluster), GeneratedAt: now, Requests: []viewmodel.VolumeSpec{}, Volumes: []viewmodel.VolumeRow{},
		Sources:    g.markFailed(s.pickSources(namespace, cache.SourceClusters, cache.SourcePVCs)),
		BlindSpots: panelBlindSpots(g, "storage"),
	}
	facts := s.volumeFactsOf(ctx, g)
	if facts.autoGrowUnread {
		// autoGrow then reads "not set, or not readable": say which.
		out.Sources = unread(out.Sources, cache.SourceClusterStorage)
	}
	for _, r := range cnpgstatus.StorageRequests(g.cluster) {
		spec := viewmodel.VolumeSpec{
			Role: r.Role, Tablespace: r.Tablespace, Size: r.Size, StorageClass: r.StorageClass, ResizeInUseVolumes: r.ResizeInUseVolumes, Temporary: r.Temporary,
			Provenance: s.provenance(namespace, cache.SourceClusters, viewmodel.OriginOperator, "Cluster.spec.storage", now),
		}
		if _, grow := facts.forecastOptions(r.VolumeName()); grow != nil {
			spec.AutoGrow = &viewmodel.AutoGrow{MaxSize: grow.MaxSize, Threshold: grow.Threshold, Step: grow.Step}
		}
		out.Requests = append(out.Requests, spec)
	}
	seen := map[string]bool{}
	if s.cache.Readable(namespace, cache.SourcePVCs) {
		list := &corev1.PersistentVolumeClaimList{}
		if err := s.cache.Reader().List(ctx, list, client.InNamespace(namespace), client.MatchingFields{cache.IndexCluster: name}); err != nil {
			out.Sources = unread(out.Sources, cache.SourcePVCs)
		} else {
			for i := range list.Items {
				pvc := &list.Items[i]
				seen[pvc.Name] = true
				row := s.volumeRow(g, pvc.Name, pvc, now)
				row.Usage = s.volumeUsage(g, facts, row.Instance, cnpgstatus.VolumeRequest{Role: row.Role, Tablespace: row.Tablespace}.VolumeName(), now)
				out.Volumes = append(out.Volumes, row)
			}
		}
	}
	for _, listed := range [][]string{
		g.cluster.Status.HealthyPVC, g.cluster.Status.InitializingPVC, g.cluster.Status.ResizingPVC,
		g.cluster.Status.DanglingPVC, g.cluster.Status.UnusablePVC,
	} {
		for _, pvcName := range listed {
			if !seen[pvcName] {
				seen[pvcName] = true
				out.Volumes = append(out.Volumes, s.volumeRow(g, pvcName, nil, now))
			}
		}
	}
	sort.Slice(out.Volumes, func(i, j int) bool { return out.Volumes[i].Name < out.Volumes[j].Name })
	out.Forecasts = s.storageForecasts(g, facts, now)
	return out, nil
}

// volumeRow is one PVC: the operator's group for it and, when the PVC
// itself was read, its Kubernetes facts.
func (s *Service) volumeRow(g *gathered, pvcName string, pvc *corev1.PersistentVolumeClaim, now time.Time) viewmodel.VolumeRow {
	group := cnpgstatus.PVCGroup(g.minor, g.cluster, pvcName)
	row := viewmodel.VolumeRow{
		Name: pvcName, Group: group.Group, GroupText: group.Text, Health: health(group.Class),
		Provenance: s.provenance(g.key.Namespace, cache.SourceClusters, viewmodel.OriginOperator, "Cluster.status."+group.Group+"PVC", now),
	}
	if pvc == nil {
		return row
	}
	role := cnpgstatus.PVCRole(pvc.Labels)
	row.Instance, row.Role, row.Tablespace = role.Instance, role.Role, role.Tablespace
	row.Phase = string(pvc.Status.Phase)
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		row.Capacity = q.String()
	}
	if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		row.Requested = q.String()
	}
	if pvc.Spec.StorageClassName != nil {
		row.StorageClass = *pvc.Spec.StorageClassName
	}
	for _, c := range pvc.Status.Conditions {
		if (c.Type == corev1.PersistentVolumeClaimResizing || c.Type == corev1.PersistentVolumeClaimFileSystemResizePending) && c.Status == corev1.ConditionTrue {
			row.Resizing = true
		}
	}
	row.Provenance = s.provenance(g.key.Namespace, cache.SourcePVCs, viewmodel.OriginKubernetes, "PersistentVolumeClaim", now)
	if group.Group != cnpgstatus.PVCUnlisted {
		row.Provenance.Source = "PersistentVolumeClaim; group: Cluster.status." + group.Group + "PVC"
	}
	return row
}

// Objects is the /objects screen.
func (s *Service) Objects(ctx context.Context, namespace, name string) (viewmodel.Objects, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return viewmodel.Objects{}, err
	}
	now := s.now().UTC()
	out := viewmodel.Objects{
		Ref: ref(g.cluster), GeneratedAt: now,
		Databases: []viewmodel.DeclaredObject{}, Roles: []viewmodel.DeclaredObject{}, ManagedRoles: []viewmodel.ManagedRole{},
		Tablespaces: []viewmodel.TablespaceView{}, Publications: []viewmodel.DeclaredObject{}, Subscriptions: []viewmodel.DeclaredObject{},
		Poolers: []viewmodel.PoolerRow{},
		Sources: s.pickSources(namespace, cache.SourceDatabases, cache.SourceDatabaseRoles, cache.SourcePublications,
			cache.SourceSubscriptions, cache.SourcePoolers, cache.SourceImageCatalogs),
		BlindSpots: []viewmodel.BlindSpot{},
	}
	reader := s.cache.Reader()
	inNS := client.InNamespace(namespace)
	byCluster := client.MatchingFields{cache.IndexCluster: name}
	prov := func(source, field string) viewmodel.Provenance {
		return s.provenance(namespace, source, viewmodel.OriginOperator, field, now)
	}
	gens := func(o client.Object, observed int64) (*int64, *int64) {
		gen := o.GetGeneration()
		return &gen, &observed
	}
	if s.cache.Readable(namespace, cache.SourceDatabases) {
		list := &cnpgv1.DatabaseList{}
		if err := reader.List(ctx, list, inNS, byCluster); err != nil {
			out.Sources = unread(out.Sources, cache.SourceDatabases)
		} else {
			for i := range list.Items {
				d := &list.Items[i]
				r := cnpgstatus.DeclaredDatabase(d)
				gen, og := gens(d, d.Status.ObservedGeneration)
				out.Databases = append(out.Databases, viewmodel.DeclaredObject{
					Kind: "Database", Name: d.Name, PGName: d.Spec.Name, Ensure: string(d.Spec.Ensure), State: r.State, Health: health(r.Class),
					Message: r.Message, FailedObjects: r.FailedObjects, Generation: gen, ObservedGeneration: og, Provenance: prov(cache.SourceDatabases, "Database.status"),
				})
			}
		}
	}
	if s.cache.Readable(namespace, cache.SourceDatabaseRoles) {
		list := &cnpgv1.DatabaseRoleList{}
		if err := reader.List(ctx, list, inNS, byCluster); err != nil {
			out.Sources = unread(out.Sources, cache.SourceDatabaseRoles)
		} else {
			for i := range list.Items {
				d := &list.Items[i]
				r := cnpgstatus.DeclaredRole(g.minor, d)
				state := r.State
				if !r.Applicable {
					state = "notApplicable"
				}
				gen, og := gens(d, d.Status.ObservedGeneration)
				out.Roles = append(out.Roles, viewmodel.DeclaredObject{
					Kind: "DatabaseRole", Name: d.Name, PGName: d.Spec.Name, Ensure: string(d.Spec.Ensure), State: state, Health: health(r.Class),
					Message: r.Message, Generation: gen, ObservedGeneration: og, Provenance: prov(cache.SourceDatabaseRoles, "DatabaseRole.status"),
				})
			}
		}
	}
	if s.cache.Readable(namespace, cache.SourcePublications) {
		list := &cnpgv1.PublicationList{}
		if err := reader.List(ctx, list, inNS, byCluster); err != nil {
			out.Sources = unread(out.Sources, cache.SourcePublications)
		} else {
			for i := range list.Items {
				p := &list.Items[i]
				r := cnpgstatus.DeclaredPublication(p)
				gen, og := gens(p, p.Status.ObservedGeneration)
				out.Publications = append(out.Publications, viewmodel.DeclaredObject{
					Kind: "Publication", Name: p.Name, PGName: p.Spec.Name, Database: p.Spec.DBName, State: r.State, Health: health(r.Class),
					Message: r.Message, Generation: gen, ObservedGeneration: og, Provenance: prov(cache.SourcePublications, "Publication.status"),
				})
			}
		}
	}
	if s.cache.Readable(namespace, cache.SourceSubscriptions) {
		list := &cnpgv1.SubscriptionList{}
		if err := reader.List(ctx, list, inNS, byCluster); err != nil {
			out.Sources = unread(out.Sources, cache.SourceSubscriptions)
		} else {
			for i := range list.Items {
				p := &list.Items[i]
				r := cnpgstatus.DeclaredSubscription(p)
				gen, og := gens(p, p.Status.ObservedGeneration)
				out.Subscriptions = append(out.Subscriptions, viewmodel.DeclaredObject{
					Kind: "Subscription", Name: p.Name, PGName: p.Spec.Name, Database: p.Spec.DBName, State: r.State, Health: health(r.Class),
					Message: r.Message, Generation: gen, ObservedGeneration: og, Provenance: prov(cache.SourceSubscriptions, "Subscription.status"),
				})
			}
		}
	}
	for _, r := range cnpgstatus.ManagedRoles(g.cluster).Roles {
		out.ManagedRoles = append(out.ManagedRoles, viewmodel.ManagedRole{
			Name: r.Name, Status: r.Status, Health: health(r.Class), Declared: r.Declared, Errors: r.Errors,
			Provenance: prov(cache.SourceClusters, "Cluster.status.managedRolesStatus"),
		})
	}
	for _, t := range cnpgstatus.Tablespaces(g.cluster).Tablespaces {
		out.Tablespaces = append(out.Tablespaces, viewmodel.TablespaceView{
			Name: t.Name, Owner: t.Owner, State: t.State, Health: health(t.Class), Error: t.Error, Declared: t.Declared, Temporary: t.Temporary,
			Provenance: prov(cache.SourceClusters, "Cluster.status.tablespacesStatus"),
		})
	}
	for _, p := range g.poolers {
		r := cnpgstatus.PoolerState(g.minor, p)
		row := viewmodel.PoolerRow{
			Name: p.Name, Type: r.Type, PoolMode: r.PoolMode, Paused: r.Paused,
			DesiredInstances: intPtr(int(r.DesiredInstances)), Instances: intPtr(int(r.Instances)),
			Phase: r.Phase, PhaseReason: r.PhaseReason, Image: r.Image, Error: r.Error, Text: r.Text, Health: health(r.Class),
			Provenance: prov(cache.SourcePoolers, "Pooler.status"),
		}
		if ready, ok := s.poolerPodsReady(ctx, namespace, p.Name); ok {
			row.PodsReady = &ready
		}
		out.Poolers = append(out.Poolers, row)
	}
	sort.Slice(out.Poolers, func(i, j int) bool { return out.Poolers[i].Name < out.Poolers[j].Name })
	out.ImageCatalog = s.imageCatalog(ctx, g, now)
	if s.opts.AllowClusterCatalogs {
		out.Sources = append(out.Sources, s.cache.Source(namespace, cache.SourceClusterImageCatalogs))
	}
	return out, nil
}

// poolerPodsReady counts a Pooler's Ready pods. Pooler pods carry
// cnpg.io/cluster (the pod informer's filter) and cnpg.io/poolerName.
func (s *Service) poolerPodsReady(ctx context.Context, namespace, pooler string) (int, bool) {
	if !s.cache.Readable(namespace, cache.SourcePods) {
		return 0, false
	}
	pods := &corev1.PodList{}
	if s.cache.Reader().List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{"cnpg.io/poolerName": pooler}) != nil {
		return 0, false
	}
	ready := 0
	for i := range pods.Items {
		if podReady(&pods.Items[i]) {
			ready++
		}
	}
	return ready, true
}

// imageCatalog reads the catalog spec.imageCatalogRef names.
func (s *Service) imageCatalog(ctx context.Context, g *gathered, now time.Time) *viewmodel.CatalogView {
	ref := g.cluster.Spec.ImageCatalogRef
	if ref == nil {
		return nil
	}
	view := &viewmodel.CatalogView{Kind: ref.Kind, Name: ref.Name, Major: intPtr(ref.Major), Health: viewmodel.HealthUnknown}
	var images []cnpgv1.CatalogImage
	switch ref.Kind {
	case "ClusterImageCatalog":
		view.Provenance = viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "ClusterImageCatalog/" + ref.Name}
		if !s.opts.AllowClusterCatalogs || !s.cache.Readable("", cache.SourceClusterImageCatalogs) {
			view.State, view.Reason = "notReadable", "catalog not readable: the console is not granted the opt-in ClusterImageCatalog read"
			return view
		}
		c := &cnpgv1.ClusterImageCatalog{}
		if err := s.cache.Reader().Get(ctx, types.NamespacedName{Name: ref.Name}, c); err != nil {
			view.State, view.Reason = catalogMiss(err, "no ClusterImageCatalog of that name")
			return view
		}
		images = c.Spec.Images
		view.Provenance = s.provenance("", cache.SourceClusterImageCatalogs, viewmodel.OriginOperator, "ClusterImageCatalog/"+ref.Name, now)
	default:
		view.Provenance = viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "ImageCatalog/" + ref.Name}
		if !s.cache.Readable(g.key.Namespace, cache.SourceImageCatalogs) {
			view.State, view.Reason = "notReadable", "ImageCatalogs are not readable by the console in this namespace"
			return view
		}
		c := &cnpgv1.ImageCatalog{}
		if err := s.cache.Reader().Get(ctx, types.NamespacedName{Namespace: g.key.Namespace, Name: ref.Name}, c); err != nil {
			view.State, view.Reason = catalogMiss(err, "no ImageCatalog of that name in the namespace")
			return view
		}
		images = c.Spec.Images
		view.Provenance = s.provenance(g.key.Namespace, cache.SourceImageCatalogs, viewmodel.OriginOperator, "ImageCatalog/"+ref.Name, now)
	}
	view.State = "readable"
	for _, img := range images {
		view.Images = append(view.Images, viewmodel.CatalogImage{Major: img.Major, Image: bound(img.Image)})
		if img.Major == ref.Major {
			view.Image = bound(img.Image)
		}
	}
	// The catalog names an image for the selected major, or the operator
	// cannot resolve one; whether the Cluster runs it is the overview's.
	if view.Image != "" {
		view.Health = viewmodel.HealthInfo
	} else {
		view.Health, view.Reason = viewmodel.HealthWarning, "the catalog has no image for major "+strconv.Itoa(ref.Major)
	}
	return view
}

// catalogMiss tells an absent catalog from one the cache could not read.
func catalogMiss(err error, absent string) (string, string) {
	if apierrors.IsNotFound(err) {
		return "notFound", absent
	}
	return "notReadable", "the catalog could not be read from the console's cache"
}
