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
	"fmt"
	"net/http"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/opstate"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// DrillOptions enable the restore-drill operation. The
// drill is started like any other operation: offered with the API
// server's answer for the caller, re-checked on a live read, and created
// with the caller's own token. The console's own identity only seals it
// (Keys) so that its supervisor may later delete it.
type DrillOptions struct {
	// Namespace is RESTORE_DRILL_NAMESPACE; empty means the source's.
	Namespace string
	Prefix    string
	TimeLimit time.Duration
	Resources ops.DrillResources
	// ServerCA is RESTORE_DRILL_SERVER_CA: which CA signs the drill's
	// server certificate, so that the console can verify its /pg/status.
	ServerCA ops.DrillServerCA
	// Owner is the console identity the drill is labelled with.
	Owner string
	// Keys seal the drill (auth.PurposeKeys with ops.DrillKeyLabel).
	Keys [][]byte
	// Selector is CLUSTER_SELECTOR: a drill outside it would be invisible
	// to the console that must check and delete it.
	Selector labels.Selector
	State    DrillState
	Records  *drills.Store
	// MaxConcurrent caps the drills of this console running at once
	// (RESTORE_DRILL_MAX_CONCURRENT), counted on the read model so that
	// every replica sees the same number.
	MaxConcurrent int
	// CanCreate asks the API server whether the caller may create
	// Clusters in namespace (a SelfSubjectAccessReview with their token).
	CanCreate func(ctx context.Context, p *auth.Principal, namespace string) (allowed, undecided bool)
}

// DrillState is what the offer and the request read beyond the Cluster.
type DrillState interface {
	// DatabaseBytes is the Cluster's primary's database size from its
	// exporter (sum over databases), nil when not reported.
	DatabaseBytes(namespace, name, primary string) *int64
	// ObjectStore reads a barman-cloud ObjectStore's destination and
	// credential kind: whether it exists, and whether the console can
	// tell (it watches ObjectStores there). A store whose configuration
	// does not parse exists with empty facts.
	ObjectStore(ctx context.Context, namespace, name string) (facts ops.StoreFacts, exists, readable bool)
	// DrillClusters lists the Clusters labelled as drills of owner (the
	// read model, every replica's view).
	DrillClusters(ctx context.Context, owner string) ([]*cnpgv1.Cluster, error)
	// PgBackRestTarget is the backup a drill of a pgBackRest Cluster
	// restores: the newest backup of the live evidence agent's recovery
	// window and its stop time, or why there is none.
	PgBackRestTarget(namespace, name string) (target PgBackRestTarget, why string)
}

// PgBackRestTarget is the read model's newest backup of an evidence
// agent's recovery window (opstate).
type PgBackRestTarget = opstate.PgBackRestTarget

func (d *DrillOptions) namespaceFor(source string) string {
	if d.Namespace != "" {
		return d.Namespace
	}
	return source
}

// latestCompleted is the newest completed Backup of the Cluster.
func latestCompleted(backups []*cnpgv1.Backup) *cnpgv1.Backup {
	var best *cnpgv1.Backup
	at := func(b *cnpgv1.Backup) time.Time {
		if b.Status.StoppedAt != nil {
			return b.Status.StoppedAt.Time
		}
		return b.CreationTimestamp.Time
	}
	for _, b := range backups {
		if b.Status.Phase != cnpgv1.BackupPhaseCompleted {
			continue
		}
		if best == nil || at(b).After(at(best)) {
			best = b
		}
	}
	return best
}

// drillLabels are the labels a drill of c gets beyond its own: the
// source's values for the keys CLUSTER_SELECTOR requires, so the drill
// stays inside the selector.
func (d *DrillOptions) drillLabels(c *cnpgv1.Cluster) map[string]string {
	out := map[string]string{}
	if d.Selector == nil {
		return out
	}
	reqs, _ := d.Selector.Requirements()
	for _, r := range reqs {
		if v, ok := c.Labels[r.Key()]; ok && r.Operator() != selection.DoesNotExist && r.Operator() != selection.NotIn && r.Operator() != selection.NotEquals {
			out[r.Key()] = v
		}
	}
	return out
}

// drillBlockers are the preconditions of a drill of c, each saying what
// is missing.
func (s *Service) drillBlockers(ctx context.Context, c *cnpgv1.Cluster, backups []*cnpgv1.Backup) ([]viewmodel.OperationBlocker, ops.RecoverySource, drillBackup) {
	d := s.opts.Drills
	ns := d.namespaceFor(c.Namespace)
	var out []viewmodel.OperationBlocker
	if _, ok := c.Labels[ops.LabelRestoreDrillOf]; ok {
		out = append(out, viewmodel.OperationBlocker{Reason: "DrillOfDrill", Text: "this Cluster is itself a restore drill", Source: "Cluster.metadata.labels[" + ops.LabelRestoreDrillOf + "]"})
	}
	rs, err := ops.DrillRecoverySource(c)
	var backup drillBackup
	if rs.PgBackRest == nil {
		backup = barmanDrillBackup(latestCompleted(backups))
		if backup.object == nil {
			out = append(out, viewmodel.OperationBlocker{Reason: "NoCompletedBackup", Text: "the Cluster has no completed Backup to restore", Source: "Backup.status.phase"})
		}
	}
	switch {
	case err != nil:
		out = append(out, viewmodel.OperationBlocker{
			Reason: "NoRecoverySource", Source: "Cluster.spec.plugins, Cluster.spec.backup",
			Text: "the Cluster backs up neither with the barman-cloud or a pgBackRest plugin nor with barmanObjectStore; volume snapshot backups are not drilled",
		})
	case rs.PgBackRest != nil:
		var bl []viewmodel.OperationBlocker
		backup, bl = d.pgbackrestBlockers(c, ns, *rs.PgBackRest, backups)
		out = append(out, bl...)
		if backup.label != "" {
			// The drill restores the label the blockers checked.
			source := *rs.PgBackRest
			source.Target = backup.label
			rs.PgBackRest = &source
		}
	case rs.Plugin:
		out = append(out, d.pluginBlockers(ctx, c, ns, rs)...)
	case ns != c.Namespace:
		out = append(out, viewmodel.OperationBlocker{
			Reason: "InTreeOtherNamespace", Source: "Cluster.spec.backup.barmanObjectStore",
			Text: "barmanObjectStore credentials are Secrets of " + c.Namespace + "; a drill in " + ns +
				" needs the barman-cloud plugin and an ObjectStore there (or leave RESTORE_DRILL_NAMESPACE unset)",
		})
	default:
		if wi := ops.FactsOf(rs.InTree).WorkloadIdentity; wi != "" {
			out = append(out, workloadIdentityBlocker(wi, "Cluster.spec.backup.barmanObjectStore"))
		}
	}
	if _, why := ops.DrillServerCASecretFor(c, ns, d.ServerCA); why != "" {
		// The drill would restore, then stay at "promoted: pending" until
		// it times out: the console could not verify its /pg/status.
		out = append(out, viewmodel.OperationBlocker{
			Reason: "DrillNotVerifiable", Source: "RESTORE_DRILL_SERVER_CA, Cluster.spec.certificates",
			Text: "the console could not verify the drill's /pg/status: " + why +
				"; give drills a CA Secret of their own (RESTORE_DRILL_SERVER_CA=secret) whose ca.crt is in the console's instance CA bundle",
		})
	}
	if d.Selector != nil && !d.Selector.Empty() {
		l := d.drillLabels(c)
		l[ops.LabelRestoreDrillOf], l[ops.LabelRestoreDrillSourceNamespace], l[ops.LabelDrillOwner] = c.Name, c.Namespace, d.Owner
		if !d.Selector.Matches(labels.Set(l)) {
			out = append(out, viewmodel.OperationBlocker{
				Reason: "OutsideClusterSelector", Source: "CLUSTER_SELECTOR",
				Text: "a drill Cluster would not match this console's CLUSTER_SELECTOR, so the console could neither check nor delete it",
			})
		}
	}
	out = append(out, d.runningBlockers(ctx, c)...)
	if out == nil {
		out = []viewmodel.OperationBlocker{}
	}
	return out, rs, backup
}

// drillBackup is what a drill restores: the Backup object (Barman: the
// newest completed one; pgBackRest: the one that took the label, when
// any), the pgBackRest label, and the backup's stop time.
type drillBackup struct {
	object    *cnpgv1.Backup
	label     string
	stoppedAt *time.Time
}

func barmanDrillBackup(b *cnpgv1.Backup) drillBackup {
	out := drillBackup{object: b}
	if b != nil && b.Status.StoppedAt != nil {
		t := b.Status.StoppedAt.UTC()
		out.stoppedAt = &t
	}
	return out
}

// name is the Backup object's name, else the label.
func (b drillBackup) name() string {
	if b.object != nil {
		return b.object.Name
	}
	return b.label
}

// pgbackrestBlockers check a pgBackRest drill: it runs in the source's
// namespace (the Stanza or Archive resolves in the recovering Cluster's
// namespace, and the console does not read those objects to compare one
// elsewhere), and the live evidence agent's recovery window names the
// backup to restore, from the plugin object the Cluster names. The label,
// returned in the drillBackup, is restored with recoveryTarget.backupID.
func (d *DrillOptions) pgbackrestBlockers(c *cnpgv1.Cluster, ns string, pb ops.PgBackRestSource, backups []*cnpgv1.Backup) (drillBackup, []viewmodel.OperationBlocker) {
	var out []viewmodel.OperationBlocker
	if ns != c.Namespace {
		out = append(out, viewmodel.OperationBlocker{
			Reason: "PgBackRestOtherNamespace", Source: "RESTORE_DRILL_NAMESPACE, Cluster.spec.plugins",
			Text: "a pgBackRest plugin reads its Stanza or Archive in the recovering Cluster's namespace and the console cannot check that one in " + ns +
				" points at the source's repository; leave RESTORE_DRILL_NAMESPACE unset to drill pgBackRest Clusters",
		})
	}
	target, why := d.State.PgBackRestTarget(c.Namespace, c.Name)
	if target.Label == "" {
		out = append(out, viewmodel.OperationBlocker{
			Reason: "NoRecoveryWindow", Source: "EvidenceAgent recovery_window",
			Text: "the evidence agent reports no recovery window for this Cluster's stanza (" + why + "): a pgBackRest drill restores the newest backup of that window",
		})
		return drillBackup{}, out
	}
	if target.Object != "" && (target.Plugin != pb.Plugin || target.Object != pb.Object) {
		// The agent resolved another plugin object than the one the
		// Cluster names now (the Cluster changed since): its label belongs
		// to another stanza, or another repository. Dalibo names its
		// stanza in the Stanza object only, so this is its one check.
		out = append(out, viewmodel.OperationBlocker{
			Reason: "StanzaObjectMismatch", Source: "Cluster.spec.plugins[" + pb.Plugin + "], EvidenceAgent status.pgbackrest",
			Text: "the evidence agent read " + target.Object + " (" + target.Plugin + "), the Cluster names " + pb.Object + " (" + pb.Plugin +
				"): wait for the agent to resolve the Cluster's current object",
		})
		return drillBackup{}, out
	}
	if pb.Stanza != "" && target.Stanza != pb.Stanza {
		// Opera names its stanza on the Cluster: the agent must have read
		// that one, or its label means nothing to the restore.
		out = append(out, viewmodel.OperationBlocker{
			Reason: "StanzaMismatch", Source: "Cluster.spec.plugins[" + ops.PgBackRestOperaPlugin + "].parameters.stanza, EvidenceAgent stanza",
			Text: "the evidence agent read stanza " + target.Stanza + ", the Cluster backs up into " + pb.Stanza,
		})
		return drillBackup{}, out
	}
	label := target.Label
	out2 := drillBackup{label: label, stoppedAt: target.StoppedAt}
	for _, b := range backups {
		if b.Status.Phase != cnpgv1.BackupPhaseCompleted {
			continue
		}
		if cnpgstatus.PgBackRestBackupKey(pb.Plugin, b.Status.BackupID, b.Status.BackupName) == label {
			out2.object = b
			break
		}
	}
	return out2, out
}

// pluginBlockers check the ObjectStore the drill would read: it exists in
// the drill namespace, lends no cloud role, and, in another namespace than
// the source's, points where the source's does.
func (d *DrillOptions) pluginBlockers(ctx context.Context, c *cnpgv1.Cluster, ns string, rs ops.RecoverySource) []viewmodel.OperationBlocker {
	var out []viewmodel.OperationBlocker
	where := "ObjectStore " + ns + "/" + rs.ObjectStore
	facts, exists, readable := d.State.ObjectStore(ctx, ns, rs.ObjectStore)
	switch {
	case !readable:
		return append(out, viewmodel.OperationBlocker{
			Reason: "ObjectStoreNotReadable", Source: "ObjectStore",
			Text: "the console cannot read ObjectStores in " + ns + ", so it cannot tell whether the drill could recover",
		})
	case !exists:
		text := "ObjectStore " + rs.ObjectStore + " does not exist in " + ns
		if ns != c.Namespace {
			text += ": with the barman-cloud plugin a recovering Cluster names an ObjectStore in its own namespace; create one there " +
				"pointing at the same bucket and path, with read-only credentials"
		}
		return append(out, viewmodel.OperationBlocker{Reason: "ObjectStoreMissing", Text: text, Source: where})
	}
	if facts.WorkloadIdentity != "" {
		out = append(out, workloadIdentityBlocker(facts.WorkloadIdentity, where+" spec.configuration"))
	}
	if ns == c.Namespace {
		return out
	}
	src, srcExists, srcReadable := d.State.ObjectStore(ctx, c.Namespace, rs.ObjectStore)
	if !srcReadable || !srcExists {
		return append(out, viewmodel.OperationBlocker{
			Reason: "SourceObjectStoreUnknown", Source: "ObjectStore " + c.Namespace + "/" + rs.ObjectStore,
			Text: "the console cannot read the source's ObjectStore " + c.Namespace + "/" + rs.ObjectStore +
				", so it cannot check that the drill namespace's one points at the same repository",
		})
	}
	var differ []string
	if facts.DestinationPath == "" || facts.DestinationPath != src.DestinationPath {
		differ = append(differ, "destinationPath")
	}
	if facts.EndpointURL != src.EndpointURL {
		differ = append(differ, "endpointURL")
	}
	if facts.ServerName != src.ServerName {
		differ = append(differ, "serverName")
	}
	if len(differ) > 0 {
		out = append(out, viewmodel.OperationBlocker{
			Reason: "ObjectStoreMismatch", Source: where + ", ObjectStore " + c.Namespace + "/" + rs.ObjectStore,
			Text: "ObjectStore " + rs.ObjectStore + " in " + ns + " does not point at the source's repository (" + strings.Join(differ, ", ") +
				" differ): the drill would restore something else",
		})
	}
	return out
}

// workloadIdentityBlocker refuses a recovery source that authenticates
// with the pod's cloud identity: the drill would need the source's role
// (spec.serviceAccountTemplate), which can usually write to the
// repository, and the console never copies it.
func workloadIdentityBlocker(option, source string) viewmodel.OperationBlocker {
	return viewmodel.OperationBlocker{
		Reason: "WorkloadIdentity", Source: source,
		Text: "the repository credentials are the pod's cloud identity (" + option + "): a drill would need the source's role, which can write, " +
			"and the console does not lend it; give the drill an ObjectStore with read-only key credentials in its own namespace",
	}
}

// runningBlockers enforce one running drill per source and the console's
// cap, on the read model (every replica's view) and this replica's record.
func (d *DrillOptions) runningBlockers(ctx context.Context, c *cnpgv1.Cluster) []viewmodel.OperationBlocker {
	var out []viewmodel.OperationBlocker
	running := func(name string) viewmodel.OperationBlocker {
		return viewmodel.OperationBlocker{
			Reason: "DrillRunning", Source: "Cluster.metadata.labels[" + ops.LabelRestoreDrillOf + "]",
			Text: "drill " + name + " of this Cluster is still running",
		}
	}
	list, err := d.State.DrillClusters(ctx, d.Owner)
	if err != nil {
		return append(out, viewmodel.OperationBlocker{
			Reason: "DrillsUnknown", Source: "read model",
			Text: "the console cannot list its running drills now, so it cannot tell whether another may start",
		})
	}
	live := 0
	mine := ""
	for _, dc := range list {
		if !dc.DeletionTimestamp.IsZero() {
			continue
		}
		live++
		if dc.Labels[ops.LabelRestoreDrillOf] == c.Name && dc.Labels[ops.LabelRestoreDrillSourceNamespace] == c.Namespace && mine == "" {
			mine = dc.Namespace + "/" + dc.Name
		}
	}
	if mine == "" && d.Records != nil {
		for _, r := range d.Records.Source(drills.SourceKey{Namespace: c.Namespace, Name: c.Name}) {
			if !r.Finished() {
				mine = r.Drill.Namespace + "/" + r.Drill.Name
				break
			}
		}
	}
	if mine != "" {
		out = append(out, running(mine))
	}
	if limit := d.MaxConcurrent; limit > 0 && live >= limit {
		out = append(out, viewmodel.OperationBlocker{
			Reason: "TooManyDrills", Source: "RESTORE_DRILL_MAX_CONCURRENT",
			Text: fmt.Sprintf("%d restore drills of this console are running, its limit; wait for one to finish", live),
		})
	}
	return out
}

func recoveryText(rs ops.RecoverySource) string {
	if pb := rs.PgBackRest; pb != nil {
		if pb.Plugin == ops.PgBackRestOperaPlugin {
			return "Archive " + pb.Object + " (pgBackRest, Opera), stanza " + pb.Stanza
		}
		return "Stanza " + pb.Object + " (pgBackRest, Dalibo)"
	}
	if rs.Plugin {
		return "ObjectStore " + rs.ObjectStore + " (barman-cloud plugin), server " + rs.ServerName
	}
	if rs.InTree != nil {
		return "barmanObjectStore " + rs.InTree.DestinationPath + ", server " + rs.ServerName
	}
	return ""
}

// drillOffer is the restore-drill offer for p on st.
func (s *Service) drillOffer(ctx context.Context, p *auth.Principal, st ClusterState, name string) viewmodel.OperationOffer {
	d := s.opts.Drills
	c := st.Cluster
	ns := d.namespaceFor(c.Namespace)
	bl, rs, backup := s.drillBlockers(ctx, c, st.Backups)
	offer := viewmodel.OperationOffer{
		Kind: viewmodel.OperationRestoreDrill, Title: "Run a restore drill",
		Summary: "Restores the latest backup into a throwaway Cluster in " + ns + ", checks it without SQL, then deletes it.",
		Consequence: "The drill Cluster uses storage and compute like one instance of this Cluster until it is deleted (after its check, at the latest after " +
			drills.HumanDuration(d.TimeLimit) + "). It has no WAL archiver and no backup configuration, and starts with logical replication workers, pg_cron jobs and TimescaleDB jobs off; " +
			"other extensions that reach the network are not neutralised (see the drill NetworkPolicy).",
		ExpectedDuration: "about as long as a restore of this Cluster",
		Requires:         []string{"create clusters in " + ns}, Blockers: bl, Confirmation: name, Observable: true,
		Drill: &viewmodel.DrillOffer{Namespace: ns, RecoverySource: recoveryText(rs), TimeLimitSeconds: int64(d.TimeLimit / time.Second)},
	}
	offer.Drill.Backup, offer.Drill.BackupLabel, offer.Drill.BackupStoppedAt = backup.name(), backup.label, backup.stoppedAt
	if d.CanCreate != nil && p != nil {
		offer.Allowed, offer.Undecided = d.CanCreate(ctx, p, ns)
	}
	return offer
}

// writeDrill re-checks the drill's preconditions on the live Cluster and
// creates the drill as the caller.
func (s *Service) writeDrill(ctx context.Context, id *ID, now time.Time) (viewmodel.OperationAccepted, error) {
	var out viewmodel.OperationAccepted
	d := s.opts.Drills
	if d == nil {
		return out, Rejection(http.StatusUnprocessableEntity, "restore drills are not enabled on this console")
	}
	c, err := s.opts.Writer.ReadCluster(ctx, id.Namespace, id.Cluster)
	if err != nil {
		return out, err
	}
	st, err := s.opts.State.OperationState(ctx, id.Namespace, id.Cluster)
	if err != nil {
		return out, s.notFound(err)
	}
	bl, rs, backup := s.drillBlockers(ctx, c, st.Backups)
	if len(bl) > 0 {
		texts := make([]string, 0, len(bl))
		for _, b := range bl {
			texts = append(texts, b.Text)
		}
		return out, Rejection(http.StatusConflict, "restoreDrill refused: "+strings.Join(texts, "; "))
	}
	ns := d.namespaceFor(c.Namespace)
	meta := ops.DrillMeta{
		Backup: backup.name(), BackupLabel: backup.label, StartedAt: now, Deadline: now.Add(d.TimeLimit), Owner: d.Owner,
		SourceBytes: d.State.DatabaseBytes(c.Namespace, c.Name, c.Status.CurrentPrimary),
	}
	if backup.stoppedAt != nil {
		meta.BackupStoppedAt = *backup.stoppedAt
	}
	obj, err := ops.DrillCluster(ops.DrillRequest{
		Source: c, Recovery: rs, Namespace: ns, Name: ops.DrillName(d.Prefix, c.Name, now),
		Meta: meta, Resources: d.Resources, ExtraLabels: d.drillLabels(c), ServerCA: d.ServerCA,
	}, d.Keys)
	if err != nil {
		return out, err
	}
	uid, err := s.opts.Writer.CreateDrillCluster(ctx, obj)
	if err != nil {
		return out, err
	}
	if d.Records != nil {
		rec := drills.FromMeta(drills.Key{Namespace: ns, Name: obj.Name}, meta)
		rec.Source = drills.SourceKey{Namespace: c.Namespace, Name: c.Name}
		rec.SourceUID = string(c.UID)
		rec.UID = string(uid)
		rec.Result = viewmodel.DrillRunning
		d.Records.Put(rec)
	}
	id.Created, id.CreatedNamespace = obj.Name, ns
	return out, nil
}

// trackDrill derives a drill operation's ladder from the console's
// record of it (any replica's supervisor rebuilds the record from the
// drill's sealed annotation within one interval).
func (s *Service) trackDrill(id ID, op viewmodel.Operation) viewmodel.Operation {
	now := s.opts.Now().UTC()
	limit := 2 * time.Hour
	if s.opts.Drills != nil {
		limit = s.opts.Drills.TimeLimit
	}
	op.Deadline = id.Accepted().Add(limit + 2*drills.Interval)
	phase := viewmodel.OpPhaseAccepted
	var rec drills.Record
	ok := false
	if s.opts.Drills != nil && s.opts.Drills.Records != nil {
		rec, ok = s.opts.Drills.Records.Get(drills.Key{Namespace: id.CreatedNamespace, Name: id.Created})
		// The handle is not signed: a record answers only for the Cluster
		// the handle names, never for a drill of another source.
		ok = ok && rec.Source == drills.SourceKey{Namespace: id.Namespace, Name: id.Cluster}
	}
	const source = "console restore-drill supervisor"
	if ok {
		switch rec.Result {
		case viewmodel.DrillPassed:
			phase = viewmodel.OpPhaseSettled
			op.Steps = append(op.Steps, viewmodel.OperationStep{Phase: phase, At: rec.FinishedAt.UTC(), Source: source, Text: "check passed" + teardownNote(rec)})
		case viewmodel.DrillFailed, viewmodel.DrillTimedOut, viewmodel.DrillCancelled:
			phase = viewmodel.OpPhaseFailed
			op.Steps = append(op.Steps, viewmodel.OperationStep{Phase: phase, At: rec.FinishedAt.UTC(), Source: source, Text: string(rec.Result) + ": " + rec.Reason + teardownNote(rec)})
		default:
			phase = viewmodel.OpPhaseActing
			text := "restoring"
			for _, c := range rec.Checks {
				if c.Outcome == viewmodel.DrillCheckPending {
					text = string(c.Name) + ": " + c.Text
					break
				}
			}
			op.Steps = append(op.Steps, viewmodel.OperationStep{Phase: phase, At: now, Source: source, Text: text})
		}
	}
	terminal := phase == viewmodel.OpPhaseSettled || phase == viewmodel.OpPhaseFailed
	if !terminal && now.After(op.Deadline) {
		phase = viewmodel.OpPhaseTimedOut
		op.Steps = append(op.Steps, viewmodel.OperationStep{
			Phase: phase, At: now, Source: "console",
			Text: "no result recorded by this console replica before the deadline; look at the Backups screen and the drill Cluster's events",
		})
	}
	op.Phase = phase
	op.Health = viewmodel.OperationHealth(phase, false)
	return op
}

func teardownNote(r drills.Record) string {
	switch {
	case !r.TornDownAt.IsZero():
		return "; drill Cluster deleted"
	case r.TeardownError != "":
		return "; drill Cluster not deleted: " + r.TeardownError
	default:
		return "; drill Cluster being deleted"
	}
}
