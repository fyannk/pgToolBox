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
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/opstate"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// InstanceState is the read model's view of an instance (opstate), named
// here for the offers and the trackers.
type InstanceState = opstate.InstanceState

// ClusterState is the read model's view of a Cluster (opstate).
type ClusterState = opstate.ClusterState

// State reads a Cluster from the read model; it returns an error
// satisfying IsNotFound for a Cluster the console does not watch.
type State interface {
	OperationState(ctx context.Context, namespace, name string) (ClusterState, error)
}

// Writer is the narrow write path (ops.UserWriter): live reads and the
// four pinned writes, all as the user the context carries.
type Writer interface {
	ReadCluster(ctx context.Context, namespace, name string) (*cnpgv1.Cluster, error)
	ListPods(ctx context.Context, namespace, cluster string) ([]corev1.Pod, error)
	CreateBackup(ctx context.Context, b *cnpgv1.Backup) (string, int64, error)
	AnnotateCluster(ctx context.Context, namespace, name, key, value, resourceVersion string) (int64, error)
	Promote(ctx context.Context, namespace, name, instance, timestamp, resourceVersion string) (int64, error)
	CreateDrillCluster(ctx context.Context, c *cnpgv1.Cluster) (types.UID, error)
}

// Options wire a Service.
type Options struct {
	State  State
	Writer Writer
	// IsNotFound recognises the State's not-found error.
	IsNotFound func(error) bool
	Now        func() time.Time
	Logger     *slog.Logger
	// RecentPerCluster bounds this replica's record per Cluster.
	RecentPerCluster int
	// Drills enables the restore-drill operation; nil leaves it out.
	Drills *DrillOptions
}

// Service is the operation service.
type Service struct {
	opts Options
	mu   sync.Mutex
	// recent is this replica's record of the operations it issued.
	recent map[string][]record
}

type record struct{ id ID }

// maxClustersRecorded bounds the record's keys.
const maxClustersRecorded = 256

// New returns the service.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.RecentPerCluster <= 0 {
		o.RecentPerCluster = 20
	}
	if o.IsNotFound == nil {
		o.IsNotFound = func(error) bool { return false }
	}
	return &Service{opts: o, recent: map[string][]record{}}
}

// descriptor is the fixed text of one operation.
type descriptor struct {
	kind                                  viewmodel.OperationKind
	title, summary, consequence, duration string
	requires                              []string
	observable                            bool
	deadline                              time.Duration
}

var catalog = []descriptor{
	{
		viewmodel.OperationBackup, "Request an on-demand backup", "Creates one Backup of this Cluster with the method it backs up with.",
		"The backup reads from the instance the Cluster's backup target selects and adds I/O load for its duration.",
		"minutes to hours, depending on size",
		[]string{"create backups"},
		true, time.Hour,
	},
	{
		viewmodel.OperationReload, "Reload configuration", "Asks every instance to reload its configuration (annotation cnpg.io/reloadedAt).",
		"PostgreSQL reloads only if a configuration file changed; CloudNativePG reports no outcome for a reload.",
		"seconds",
		[]string{"patch clusters"},
		false, 2 * time.Minute,
	},
	{
		viewmodel.OperationRestart, "Restart the cluster", "Rolls every instance (annotation kubectl.kubernetes.io/restartedAt), replicas first, then the primary.",
		"Each instance restarts once; clients of the primary are disconnected when it restarts or is switched over.",
		"a few minutes per instance",
		[]string{"patch clusters"},
		true, 30 * time.Minute,
	},
	{
		viewmodel.OperationPromote, "Promote a replica (switchover)", "Moves the primary role to the chosen replica.",
		"Clients of the primary are disconnected; the old primary becomes a replica.",
		"under a minute",
		[]string{"patch clusters/status"},
		true, 2 * time.Minute,
	},
}

func describe(k viewmodel.OperationKind) (descriptor, bool) {
	for _, d := range catalog {
		if d.kind == k {
			return d, true
		}
	}
	return descriptor{}, false
}

func blockers(in []cnpgstatus.Blocker) []viewmodel.OperationBlocker {
	out := make([]viewmodel.OperationBlocker, 0, len(in))
	for _, b := range in {
		out = append(out, viewmodel.OperationBlocker{Reason: b.Reason, Text: b.Text, Source: b.Source})
	}
	return out
}

// backupMethod is the method and plugin the Cluster can take an
// on-demand backup with, in the order the operator would pick.
func backupMethod(c *cnpgv1.Cluster) (method, plugin string) {
	if cfg := cnpgstatus.Backups(c); cfg.PluginBased {
		for _, p := range c.Spec.Plugins {
			if cnpgstatus.IsBackupPlugin(p) {
				return cnpgstatus.BackupMethodPlugin, p.Name
			}
		}
	}
	if c.Spec.Backup != nil && c.Spec.Backup.BarmanObjectStore != nil {
		return cnpgstatus.BackupMethodBarmanObjectStore, ""
	}
	if c.Spec.Backup != nil && c.Spec.Backup.VolumeSnapshot != nil {
		return cnpgstatus.BackupMethodVolumeSnapshot, ""
	}
	return cnpgstatus.BackupMethodBarmanObjectStore, ""
}

func (s *Service) notFound(err error) error {
	if s.opts.IsNotFound(err) {
		return Rejection(http.StatusNotFound, "no such Cluster in the console's watched namespaces")
	}
	return err
}

// Offers is the /operations screen of a Cluster for a caller with caps.
func (s *Service) Offers(ctx context.Context, p *auth.Principal, caps authz.Capabilities, namespace, name string) (viewmodel.Operations, error) {
	st, err := s.opts.State.OperationState(ctx, namespace, name)
	if err != nil {
		return viewmodel.Operations{}, s.notFound(err)
	}
	c := st.Cluster
	out := viewmodel.Operations{
		Ref: viewmodel.ClusterRef{Namespace: namespace, Name: name, UID: string(c.UID)}, GeneratedAt: s.opts.Now().UTC(),
		Offers: []viewmodel.OperationOffer{}, Recent: []viewmodel.Operation{},
		RecentScope: "operations issued through this console replica since it started; the API server's audit log is the complete record",
		Revision:    RevisionOf(c),
	}
	allowed := map[viewmodel.OperationKind]bool{
		viewmodel.OperationBackup: caps.Backup, viewmodel.OperationReload: caps.Reload,
		viewmodel.OperationRestart: caps.Restart, viewmodel.OperationPromote: caps.Promote,
	}
	for _, d := range catalog {
		offer := viewmodel.OperationOffer{
			Kind: d.kind, Title: d.title, Summary: d.summary, Consequence: d.consequence, ExpectedDuration: d.duration,
			Allowed: allowed[d.kind], Undecided: slices.Contains(caps.Undecided, string(d.kind)), Requires: d.requires,
			Confirmation: name, Observable: d.observable, Blockers: []viewmodel.OperationBlocker{},
		}
		switch d.kind {
		case viewmodel.OperationBackup:
			method, _ := backupMethod(c)
			offer.Blockers = blockers(cnpgstatus.BackupPreconditions(c, method))
		case viewmodel.OperationRestart:
			offer.Blockers = blockers(cnpgstatus.RestartPreconditions(c))
		case viewmodel.OperationReload:
			offer.Blockers = blockers(cnpgstatus.ReloadPreconditions(c))
		case viewmodel.OperationPromote:
			offer.Confirmation = "the target instance name"
			offer.Blockers = []viewmodel.OperationBlocker{}
			for _, inst := range st.Instances {
				if inst.Name == c.Status.CurrentPrimary {
					continue
				}
				bl := blockers(cnpgstatus.PromotePreconditions(c, st.Pods, inst.Name))
				offer.Targets = append(offer.Targets, viewmodel.PromoteTarget{
					Name: inst.Name, Eligible: len(bl) == 0, Blockers: bl, Health: inst.Health, LagBytes: inst.LagBytes,
				})
			}
			if len(offer.Targets) == 0 {
				offer.Blockers = append(offer.Blockers, viewmodel.OperationBlocker{Reason: "NoReplica", Text: "the Cluster has no replica to promote", Source: "Cluster.status.instanceNames"})
			}
		case viewmodel.OperationRestoreDrill:
			// Not offered from this list: a drill has its own offer.
		}
		out.Offers = append(out.Offers, offer)
	}
	if s.opts.Drills != nil {
		out.Offers = append(out.Offers, s.drillOffer(ctx, p, st, name))
	}
	s.mu.Lock()
	recs := slices.Clone(s.recent[namespace+"/"+name])
	s.mu.Unlock()
	for i := len(recs) - 1; i >= 0; i-- {
		// Who asked is the API server's audit log's to say: Recent lists
		// what was done, not by whom, to every viewer of the Cluster.
		out.Recent = append(out.Recent, s.track(recs[i].id, st))
	}
	return out, nil
}

// Request performs one operation pessimistically: typed confirmation,
// a live read as the caller, the preconditions on that read, then the
// pinned write as the caller.
func (s *Service) Request(ctx context.Context, p *auth.Principal, namespace, name string, req viewmodel.OperationRequest) (viewmodel.OperationAccepted, error) {
	now := s.opts.Now().UTC()
	id := ID{Kind: req.Kind, Namespace: namespace, Cluster: name, AcceptedAt: now.Unix()}
	if err := CheckConfirmation(name, req); err != nil {
		s.audit(ctx, p, id, req, err)
		return viewmodel.OperationAccepted{}, err
	}
	if _, err := s.opts.State.OperationState(ctx, namespace, name); err != nil {
		err = s.notFound(err)
		s.audit(ctx, p, id, req, err)
		return viewmodel.OperationAccepted{}, err
	}
	accepted, err := s.write(p.Context(ctx), &id, req, now)
	s.audit(ctx, p, id, req, err)
	if err != nil {
		return viewmodel.OperationAccepted{}, err
	}
	s.remember(id)
	accepted.OperationID, accepted.Kind, accepted.AcceptedAt = id.Encode(), req.Kind, now
	accepted.Target = viewmodel.ClusterRef{Namespace: namespace, Name: name}
	accepted.Instance, accepted.Created, accepted.CreatedNamespace = id.Instance, id.Created, id.CreatedNamespace
	return accepted, nil
}

func (s *Service) write(ctx context.Context, id *ID, req viewmodel.OperationRequest, now time.Time) (viewmodel.OperationAccepted, error) {
	if req.Kind == viewmodel.OperationRestoreDrill {
		return s.writeDrill(ctx, id, now)
	}
	w := s.opts.Writer
	c, err := w.ReadCluster(ctx, id.Namespace, id.Cluster)
	if err != nil {
		return viewmodel.OperationAccepted{}, err
	}
	var out viewmodel.OperationAccepted
	if err := changedSince(ctx, req.Kind, c); err != nil {
		return out, err
	}
	refuse := func(bl []cnpgstatus.Blocker) error {
		texts := make([]string, 0, len(bl))
		for _, b := range bl {
			texts = append(texts, b.Text)
		}
		return Rejection(http.StatusConflict, string(req.Kind)+" refused: "+strings.Join(texts, "; "))
	}
	switch req.Kind {
	case viewmodel.OperationBackup:
		method, plugin := backupMethod(c)
		if bl := cnpgstatus.BackupPreconditions(c, method); len(bl) > 0 {
			return out, refuse(bl)
		}
		created, gen, err := w.CreateBackup(ctx, ops.BackupObject(id.Namespace, id.Cluster, "ondemand-"+now.Format("20060102150405")+"-", method, plugin))
		if err != nil {
			return out, err
		}
		id.Created, out.ObservedGeneration = created, gen
	case viewmodel.OperationRestart, viewmodel.OperationReload:
		bl := cnpgstatus.RestartPreconditions(c)
		key, value := ops.RestartAnnotation, now.Format(time.RFC3339)
		if req.Kind == viewmodel.OperationReload {
			bl = cnpgstatus.ReloadPreconditions(c)
			key, value = ops.ReloadAnnotation, now.Format(ops.CNPGTimestamp)
		}
		if len(bl) > 0 {
			return out, refuse(bl)
		}
		gen, err := w.AnnotateCluster(ctx, id.Namespace, id.Cluster, key, value, c.ResourceVersion)
		if err != nil {
			return out, err
		}
		id.Marker, out.ObservedGeneration = value, gen
	case viewmodel.OperationPromote:
		pods, err := w.ListPods(ctx, id.Namespace, id.Cluster)
		if err != nil {
			return out, err
		}
		if bl := cnpgstatus.PromotePreconditions(c, pods, req.Target); len(bl) > 0 {
			return out, refuse(bl)
		}
		stamp := now.Format(ops.CNPGTimestamp)
		gen, err := w.Promote(ctx, id.Namespace, id.Cluster, req.Target, stamp, c.ResourceVersion)
		if err != nil {
			return out, err
		}
		id.Instance, id.Marker, out.ObservedGeneration = req.Target, stamp, gen
	default:
		return out, Rejection(http.StatusUnprocessableEntity, "no such operation")
	}
	return out, nil
}

// audit writes the one server audit line per request: who (as the API
// server named them), what, where, and the outcome category.
func (s *Service) audit(ctx context.Context, p *auth.Principal, id ID, req viewmodel.OperationRequest, err error) {
	outcome := "accepted"
	var status interface{ HTTPStatus() int }
	switch {
	case err == nil:
	case errors.As(err, &status):
		outcome = "refused " + http.StatusText(status.HTTPStatus())
	default:
		outcome = "failed"
	}
	attrs := []slog.Attr{
		slog.String("user", p.User.Name), slog.String("session", p.SessionID),
		slog.String("operation", string(req.Kind)), slog.String("namespace", id.Namespace), slog.String("cluster", id.Cluster),
		slog.String("target", req.Target), slog.String("created", id.Created), slog.String("createdNamespace", id.CreatedNamespace),
		slog.String("outcome", outcome),
	}
	// via says which face asked; an MCP client is named by its declared
	// name and the grant it holds (none for a raw API-server token).
	o := OriginFrom(ctx)
	attrs = append(attrs, slog.String("via", o.Via))
	if o.Via == ViaMCP {
		attrs = append(attrs, slog.String("assistant", o.Assistant), slog.String("grant", o.Grant), slog.String("confirmation", o.Confirmation))
	}
	s.opts.Logger.LogAttrs(ctx, slog.LevelInfo, "operation", attrs...)
}

func (s *Service) remember(id ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := id.Namespace + "/" + id.Cluster
	if _, ok := s.recent[key]; !ok && len(s.recent) >= maxClustersRecorded {
		return
	}
	recs := s.recent[key]
	recs = append(recs, record{id: id})
	if len(recs) > s.opts.RecentPerCluster {
		recs = recs[len(recs)-s.opts.RecentPerCluster:]
	}
	s.recent[key] = recs
}

// Lookup resolves a handle against the read model.
func (s *Service) Lookup(ctx context.Context, id ID) (viewmodel.Operation, error) {
	st, err := s.opts.State.OperationState(ctx, id.Namespace, id.Cluster)
	if err != nil {
		return viewmodel.Operation{}, s.notFound(err)
	}
	return s.track(id, st), nil
}

// track derives the phase ladder from the Cluster's current state.
func (s *Service) track(id ID, st ClusterState) viewmodel.Operation {
	d, _ := describe(id.Kind)
	accepted := id.Accepted()
	now := s.opts.Now().UTC()
	op := viewmodel.Operation{
		OperationID: id.Encode(), Kind: id.Kind, Target: id.Ref(), Instance: id.Instance, Created: id.Created,
		AcceptedAt: accepted, Deadline: accepted.Add(d.deadline),
		Steps: []viewmodel.OperationStep{
			{Phase: viewmodel.OpPhaseRequested, At: accepted, Source: "console", Text: "requested"},
			{Phase: viewmodel.OpPhaseAccepted, At: accepted, Source: "Kubernetes API server", Text: acceptedText(id)},
		},
	}
	if st.Cluster != nil {
		op.Target.UID = string(st.Cluster.UID)
	}
	if id.Kind == viewmodel.OperationRestoreDrill {
		op.CreatedNamespace = id.CreatedNamespace
		return s.trackDrill(id, op)
	}
	in := cnpgstatus.OperationInput{Kind: cnpgstatus.OperationKind(id.Kind), Minor: st.Minor, Cluster: st.Cluster, Pods: st.Pods, Value: id.Marker}
	if id.Kind == viewmodel.OperationPromote {
		in.Value, in.Marker = id.Instance, id.Marker
	}
	for _, b := range st.Backups {
		if b.Name == id.Created {
			in.Backup = b
		}
	}
	r := cnpgstatus.OperationProgress(in)
	op.Blocked = r.Blocked
	phase := viewmodel.OpPhaseAccepted
	switch r.Phase {
	case cnpgstatus.OpActing:
		phase = viewmodel.OpPhaseActing
	case cnpgstatus.OpSettled:
		phase = viewmodel.OpPhaseSettled
	case cnpgstatus.OpFailed:
		phase = viewmodel.OpPhaseFailed
	case cnpgstatus.OpNotObservable:
		phase = viewmodel.OpPhaseNotObservable
	}
	if phase != viewmodel.OpPhaseAccepted || (r.Phase == cnpgstatus.OpUnknown && r.Text != "") {
		// The step's time is when the console observed it (any replica
		// derives the ladder anew, so first-seen times are not kept).
		op.Steps = append(op.Steps, viewmodel.OperationStep{Phase: phase, At: now, Source: r.Source, Text: r.Text})
	}
	terminal := phase == viewmodel.OpPhaseSettled || phase == viewmodel.OpPhaseFailed || phase == viewmodel.OpPhaseNotObservable
	if !terminal && now.After(op.Deadline) {
		phase = viewmodel.OpPhaseTimedOut
		op.Steps = append(op.Steps, viewmodel.OperationStep{
			Phase: phase, At: now, Source: "console",
			Text: "no outcome reported before the deadline; this does not mean it failed: look at the Cluster's events and logs",
		})
	}
	op.Phase = phase
	op.Health = viewmodel.OperationHealth(phase, op.Blocked)
	return op
}

func acceptedText(id ID) string {
	switch id.Kind {
	case viewmodel.OperationBackup:
		return "Backup " + id.Created + " created"
	case viewmodel.OperationPromote:
		return "switchover to " + id.Instance + " requested (status.targetPrimary)"
	case viewmodel.OperationReload:
		return "annotation cnpg.io/reloadedAt set to " + id.Marker
	case viewmodel.OperationRestoreDrill:
		return "drill Cluster " + id.CreatedNamespace + "/" + id.Created + " created"
	default:
		return "annotation kubectl.kubernetes.io/restartedAt set to " + id.Marker
	}
}
