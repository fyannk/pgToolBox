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

// Package accessrequest is the PgAccessRequest controller of the
// operator: the small half of access requests that
// must run without a person. The console creates the RoleBinding of an
// approved request with the approver's own token; this controller only
// takes grants away — at spec.expiresAt, on revocation, on drift, on a
// self-approval, and for the RoleBinding of an approval whose status write
// never landed — and publishes what the namespace's RBAC actually says.
// It never creates or updates a RoleBinding, so it needs no bind or
// escalate permission and cannot be used to grant anything.
package accessrequest

import (
	"context"
	"fmt"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/conditions"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
)

// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgaccessrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgaccessrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// OrphanGrace is how long a RoleBinding of a request that is not Approved
// may live: the console creates the binding first and records the
// approval second, so a binding younger than this is an approval in
// flight, an older one an approval whose status write failed.
const OrphanGrace = time.Minute

// Reconciler reconciles PgAccessRequests.
type Reconciler struct {
	// Client reads the label-filtered caches (RoleBindings carrying
	// managed-by=pgtoolbox, Clusters, PgAccessRequests) and writes.
	Client client.Client
	// APIReader reads past the cache. After a delete that failed (an
	// etcd timeout, a dropped connection), it says whether the binding is
	// in fact gone; the cache may already agree, and the next loop would
	// then find nothing to delete and record no RoleBindingDeleted Event.
	// Nil: a failed delete is retried and its Event may be lost.
	APIReader client.Reader
	Recorder  events.EventRecorder
	// Now is injectable for expiry tests.
	Now func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile publishes status on every exit path.
func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	access := &pgtoolboxv1alpha1.PgAccessRequest{}
	if err := r.Client.Get(ctx, request.NamespacedName, access); err != nil {
		// A deleted request takes its RoleBinding with it: the binding's
		// controller reference hands it to the garbage collector.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !access.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	original := access.DeepCopy()
	initializeConditions(access)
	loop := &loopState{}
	if access.Annotations[pgtoolboxv1alpha1.AnnotationReconcile] == pgtoolboxv1alpha1.ReconcileSkip {
		return ctrl.Result{}, r.publish(ctx, access, original, loop, conditions.Outcome{Paused: true})
	}
	result, err := r.reconcile(ctx, access, loop)
	return result, r.publish(ctx, access, original, loop, conditions.Outcome{Err: err, Converged: err == nil})
}

// loopState collects the Events of operator actions, emitted once the
// status that reports them is written.
type loopState struct {
	events []pendingEvent
}

type pendingEvent struct {
	eventType string
	reason    pgtoolboxv1alpha1.Reason
	note      string
}

func (l *loopState) record(eventType string, reason pgtoolboxv1alpha1.Reason, format string, args ...any) {
	l.events = append(l.events, pendingEvent{eventType: eventType, reason: reason, note: fmt.Sprintf(format, args...)})
}

func (r *Reconciler) reconcile(ctx context.Context, access *pgtoolboxv1alpha1.PgAccessRequest, loop *loopState) (ctrl.Result, error) {
	if err := r.checkCluster(ctx, access); err != nil {
		return ctrl.Result{}, err
	}
	bindings, err := r.bindings(ctx, access)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := r.now()
	d := Decide(access, bindings, now)
	if d.Phase != access.Status.Phase {
		switch d.Phase {
		case pgtoolboxv1alpha1.AccessPhaseExpired:
			loop.record(corev1.EventTypeNormal, pgtoolboxv1alpha1.ReasonAccessExpired, "expiresAt %s passed", access.Spec.ExpiresAt.UTC().Format(time.RFC3339))
		case pgtoolboxv1alpha1.AccessPhaseRevoked:
			access.Status.RevokedBy = pgtoolboxv1alpha1.OperatorActor
			access.Status.RevokedAt = &metav1.Time{Time: now}
			access.Status.Note = d.Note
			loop.record(corev1.EventTypeWarning, d.Reason, "%s", d.Note)
		case pgtoolboxv1alpha1.AccessPhasePending, pgtoolboxv1alpha1.AccessPhaseApproved, pgtoolboxv1alpha1.AccessPhaseDenied:
			// Decided by people through the console; the operator records no event.
		}
		access.Status.Phase = d.Phase
	}
	for _, binding := range d.Delete {
		if err := r.deleteBinding(ctx, access, binding); err != nil {
			return ctrl.Result{}, err
		}
		// The deletion happened whatever the status write does next: its
		// Event is not deferred, and the next loop finds nothing to delete.
		r.event(access, pendingEvent{
			corev1.EventTypeNormal, pgtoolboxv1alpha1.ReasonRoleBindingDeleted,
			fmt.Sprintf("RoleBinding %s deleted: %s", binding.Name, d.Message),
		})
	}
	if access.Status.Phase == pgtoolboxv1alpha1.AccessPhaseApproved {
		access.Status.GrantedScope = pgtoolboxv1alpha1.AccessGrantScopeNamespace
	}
	setCondition(access, pgtoolboxv1alpha1.ConditionRoleBindingSynced, d.Status, d.Reason, d.Message)
	if d.RequeueAt.IsZero() {
		return ctrl.Result{}, nil
	}
	// A timer for the next fact that changes without an event (expiry,
	// the end of an orphan's grace), not a poll.
	return ctrl.Result{RequeueAfter: max(time.Second, d.RequeueAt.Sub(now))}, nil
}

// checkCluster sets ConfigurationValid: a request naming a Cluster that
// does not exist in the namespace is reported, never refused — the grant
// is namespace-wide whatever spec.cluster says, and expiry and revocation
// must keep working.
func (r *Reconciler) checkCluster(ctx context.Context, access *pgtoolboxv1alpha1.PgAccessRequest) error {
	scope := fmt.Sprintf("an approval grants %s on every Cluster of namespace %s", pgtoolboxv1alpha1.ClusterRoleFor(access.Spec.Capability), access.Namespace)
	if access.Spec.Cluster == "" {
		setCondition(access, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, scope)
		return nil
	}
	cluster := &cnpgv1.Cluster{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: access.Namespace, Name: access.Spec.Cluster}, cluster)
	switch {
	case apierrors.IsNotFound(err):
		setCondition(access, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonClusterNotFound,
			fmt.Sprintf("Cluster %s does not exist in namespace %s; %s", access.Spec.Cluster, access.Namespace, scope))
		return nil
	case err != nil:
		return fmt.Errorf("read Cluster %s: %w", access.Spec.Cluster, err)
	}
	setCondition(access, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
		fmt.Sprintf("spec.cluster names %s, but a RoleBinding cannot be narrowed to one Cluster: %s", access.Spec.Cluster, scope))
	return nil
}

// bindings returns the RoleBindings this request controls, from the
// operator's label-filtered cache. A binding that carries the request's
// label without its controller reference is not this request's and is
// never touched.
func (r *Reconciler) bindings(ctx context.Context, access *pgtoolboxv1alpha1.PgAccessRequest) ([]rbacv1.RoleBinding, error) {
	var list rbacv1.RoleBindingList
	if err := r.Client.List(ctx, &list, client.InNamespace(access.Namespace),
		client.MatchingLabels{pgtoolboxv1alpha1.LabelAccessRequest: access.Name}); err != nil {
		return nil, fmt.Errorf("list RoleBindings of %s: %w", access.Name, err)
	}
	out := make([]rbacv1.RoleBinding, 0, len(list.Items))
	for i := range list.Items {
		if owner := metav1.GetControllerOfNoCopy(&list.Items[i]); owner != nil && owner.UID == access.UID && owner.Kind == "PgAccessRequest" {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// deleteBinding deletes one RoleBinding the request controls, preconditioned
// on its UID so a binding recreated under the same name in between is not
// the one deleted.
func (r *Reconciler) deleteBinding(ctx context.Context, access *pgtoolboxv1alpha1.PgAccessRequest, binding rbacv1.RoleBinding) error {
	uid := binding.UID
	err := r.Client.Delete(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: access.Namespace, Name: binding.Name}},
		client.Preconditions{UID: &uid})
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	if r.APIReader != nil {
		// The API server may have applied the delete and still answered an
		// error: what the binding is now settles it.
		var live rbacv1.RoleBinding
		readErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: access.Namespace, Name: binding.Name}, &live)
		if apierrors.IsNotFound(readErr) || (readErr == nil && live.UID != uid) {
			return nil
		}
	}
	return fmt.Errorf("delete RoleBinding %s: %w", binding.Name, err)
}

// Decision is what the RBAC of the namespace must become for a request:
// the phase to record, the RoleBindings to delete, the RoleBindingSynced
// condition and when to look again.
type Decision struct {
	Phase     pgtoolboxv1alpha1.PgAccessRequestPhase
	Delete    []rbacv1.RoleBinding
	Status    metav1.ConditionStatus
	Reason    pgtoolboxv1alpha1.Reason
	Message   string
	Note      string
	RequeueAt time.Time
}

// Decide is the controller's pure core: from the request, the RoleBindings
// it controls and the time, what to record and what to delete. It never
// asks for a binding to be created.
func Decide(access *pgtoolboxv1alpha1.PgAccessRequest, bindings []rbacv1.RoleBinding, now time.Time) Decision {
	phase := access.Phase()
	d := Decision{Phase: phase, Status: metav1.ConditionTrue}
	expiry := time.Time{}
	if access.Spec.ExpiresAt != nil {
		expiry = access.Spec.ExpiresAt.Time
	}

	switch {
	case phase == pgtoolboxv1alpha1.AccessPhasePending && access.ExpiredAt(now):
		d.Phase = pgtoolboxv1alpha1.AccessPhaseExpired
	case phase == pgtoolboxv1alpha1.AccessPhaseApproved && selfApproved(access):
		d.Phase = pgtoolboxv1alpha1.AccessPhaseRevoked
		d.Reason = pgtoolboxv1alpha1.ReasonSelfApprovalRefused
		d.Note = fmt.Sprintf("revoked by the operator: the request was approved by its own subject (%s); ask someone else to decide", access.Status.DecidedBy)
		d.Message = d.Note
		d.Delete = bindings
		return d
	case phase == pgtoolboxv1alpha1.AccessPhaseApproved && access.ExpiredAt(now):
		d.Phase = pgtoolboxv1alpha1.AccessPhaseExpired
	}

	switch d.Phase {
	case pgtoolboxv1alpha1.AccessPhasePending:
		d.Reason, d.Message = pgtoolboxv1alpha1.ReasonAwaitingDecision, "waiting for a decision; nothing is bound"
		d.RequeueAt = expiry
		for i := range bindings {
			deadline := bindings[i].CreationTimestamp.Add(OrphanGrace)
			if now.Before(deadline) {
				// An approval in flight: its status write follows the
				// binding's create.
				d.RequeueAt = earliest(d.RequeueAt, deadline)
				continue
			}
			d.Delete = append(d.Delete, bindings[i])
			d.Reason, d.Message = pgtoolboxv1alpha1.ReasonRoleBindingOrphaned,
				"a RoleBinding outlived its approval's failed status write and was deleted; the request is still waiting for a decision"
		}
	case pgtoolboxv1alpha1.AccessPhaseApproved:
		d.RequeueAt = expiry
		name := access.Status.RoleBinding
		if name == "" {
			name = access.RoleBindingName()
		}
		var granted *rbacv1.RoleBinding
		for i := range bindings {
			switch {
			case bindings[i].Name == name && access.GrantMatches(&bindings[i]):
				granted = &bindings[i]
			default:
				d.Delete = append(d.Delete, bindings[i])
			}
		}
		switch {
		case len(d.Delete) > 0:
			d.Status, d.Reason = metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRoleBindingDrifted
			d.Message = "a RoleBinding of this request bound another subject or ClusterRole than the request names and was deleted"
			if granted != nil {
				d.Message += "; RoleBinding " + granted.Name + " still grants the request"
			}
		case granted == nil:
			d.Status, d.Reason = metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRoleBindingMissing
			d.Message = fmt.Sprintf("RoleBinding %s does not exist; the operator never recreates a grant: revoke the request and ask again", name)
		default:
			d.Reason = pgtoolboxv1alpha1.ReasonAccessGranted
			d.Message = fmt.Sprintf("RoleBinding %s binds %s %s to ClusterRole %s in namespace %s: every Cluster of the namespace",
				granted.Name, access.Spec.Subject.Kind, access.Spec.Subject.Name, granted.RoleRef.Name, access.Namespace)
		}
	default:
		d.Delete = bindings
		d.Reason = map[pgtoolboxv1alpha1.PgAccessRequestPhase]pgtoolboxv1alpha1.Reason{
			pgtoolboxv1alpha1.AccessPhaseDenied:  pgtoolboxv1alpha1.ReasonAccessDenied,
			pgtoolboxv1alpha1.AccessPhaseExpired: pgtoolboxv1alpha1.ReasonAccessExpired,
			pgtoolboxv1alpha1.AccessPhaseRevoked: pgtoolboxv1alpha1.ReasonAccessRevoked,
		}[d.Phase]
		d.Message = fmt.Sprintf("the request is %s; nothing is bound", d.Phase)
	}
	return d
}

// selfApproved reports an approval recorded by the request's own subject.
// decidedBy is written by the approver, so this catches an honest record
// of a self-approval (a client other than the console, which refuses
// it); it is a four-eyes check, not an authentication.
func selfApproved(access *pgtoolboxv1alpha1.PgAccessRequest) bool {
	username := access.SubjectUsername()
	return username != "" && access.Status.DecidedBy == username
}

func earliest(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

// emit records the loop's phase Events after a successful status write: a
// phase nobody can see in status yields no Event, and a failed write is
// retried by a loop that records it again.
func (r *Reconciler) emit(access *pgtoolboxv1alpha1.PgAccessRequest, loop *loopState) {
	for _, e := range loop.events {
		r.event(access, e)
	}
}

func (r *Reconciler) event(access *pgtoolboxv1alpha1.PgAccessRequest, e pendingEvent) {
	if r.Recorder != nil {
		r.Recorder.Eventf(access, nil, e.eventType, e.reason.String(), "Reconcile", "%s", reconcile.BoundedMessage(e.note))
	}
}
