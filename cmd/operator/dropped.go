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

package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/conditions"
	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// dropCause is why a watched namespace was left out at start: the label
// values of pgtoolbox_watch_namespaces_dropped{reason}, a fixed set.
type dropCause string

const (
	// dropClustersUnlistable: the operator may not list CloudNativePG
	// Clusters there (the namespace deleted since the install, or the
	// operator not bound in it).
	dropClustersUnlistable dropCause = "clusters-unlistable"
	// dropPermissions: it lacks another namespaced permission of its
	// manager role there (a narrower Role bound in place of the chart's).
	dropPermissions dropCause = "permissions"
	// dropReviewFailed: its permissions there could not be reviewed (an
	// error after the retries, a response too large, or no answer in
	// time).
	dropReviewFailed dropCause = "review-failed"
)

// dropCauses is every dropCause, in the order the gauge exports them.
var dropCauses = []dropCause{dropPermissions, dropReviewFailed, dropClustersUnlistable}

// droppedNamespace is one watched namespace left out at start. For
// dropPermissions, required is what the operator asked for there and
// missing what of it the review refused; for dropReviewFailed, required is
// what to ask again (reReviewFailed); both are nil otherwise. Only
// dropPermissions says what the operator may do there.
type droppedNamespace struct {
	Namespace string
	Cause     dropCause
	// Reason is the cause in words, naming this namespace only.
	Reason   string
	required []managerrole.Permission
	missing  []managerrole.Permission
}

// allows reports whether the review showed the operator may verb resource
// in group there: asked for and not refused. Never for a namespace whose
// permissions are unknown.
func (d droppedNamespace) allows(group, resource, verb string) bool {
	has := func(permissions []managerrole.Permission) bool {
		return slices.ContainsFunc(permissions, func(p managerrole.Permission) bool {
			return p.Group == group && p.Resource == resource && slices.Contains(p.Verbs, verb)
		})
	}
	return d.Cause == dropPermissions && has(d.required) && !has(d.missing)
}

// countDropped is the number of dropped namespaces per cause, every cause
// present (0 when none).
func countDropped(dropped []droppedNamespace) map[dropCause]int {
	counts := make(map[dropCause]int, len(dropCauses))
	for _, cause := range dropCauses {
		counts[cause] = 0
	}
	for _, d := range dropped {
		counts[d.Cause]++
	}
	return counts
}

const (
	// droppedReportTimeout bounds the whole report of the dropped
	// namespaces; it runs beside the start, never in its budget.
	droppedReportTimeout = 30 * time.Second
	// droppedWriteTimeout bounds each read and write of the report.
	droppedWriteTimeout = 5 * time.Second
	// droppedObjectsPerKind bounds the objects of one kind reported in one
	// namespace (one List page).
	droppedObjectsPerKind = 100
	// droppedReportingController is the reportingController of the Events.
	droppedReportingController = "pgtoolbox.fyannk.dev/operator"
)

// droppedAPI is what the report needs of the API: the start-up client
// (uncached), or a fake.
type droppedAPI interface {
	client.Reader
	managerrole.Creator
	Status() client.SubResourceWriter
}

// droppedTarget is one object the report may write about, and its
// conditions.
type droppedTarget struct {
	object     client.Object
	kind       string
	conditions *[]metav1.Condition
}

// droppedKind is one kind whose objects in a dropped namespace keep a
// status nobody updates: its resource (what RBAC names) and how to list it.
type droppedKind struct {
	resource string
	list     func() (client.ObjectList, func() []droppedTarget)
}

// droppedKinds is the kinds the report covers: those whose status the
// operator publishes, and that a DBA reads.
var droppedKinds = []droppedKind{
	{"pgadmins", func() (client.ObjectList, func() []droppedTarget) {
		list := &pgtoolboxv1alpha1.PgAdminList{}
		return list, func() []droppedTarget {
			targets := make([]droppedTarget, len(list.Items))
			for i := range list.Items {
				targets[i] = droppedTarget{&list.Items[i], "PgAdmin", &list.Items[i].Status.Conditions}
			}
			return targets
		}
	}},
	{"pgtoolboxconsoles", func() (client.ObjectList, func() []droppedTarget) {
		list := &pgtoolboxv1alpha1.PgToolBoxConsoleList{}
		return list, func() []droppedTarget {
			targets := make([]droppedTarget, len(list.Items))
			for i := range list.Items {
				targets[i] = droppedTarget{&list.Items[i], "PgToolBoxConsole", &list.Items[i].Status.Conditions}
			}
			return targets
		}
	}},
	{"evidenceagents", func() (client.ObjectList, func() []droppedTarget) {
		list := &pgtoolboxv1alpha1.EvidenceAgentList{}
		return list, func() []droppedTarget {
			targets := make([]droppedTarget, len(list.Items))
			for i := range list.Items {
				targets[i] = droppedTarget{&list.Items[i], "EvidenceAgent", &list.Items[i].Status.Conditions}
			}
			return targets
		}
	}},
}

// droppedMessage is the Ready=Unknown message and the Event note of an
// object in namespace d: fixed for a given cause, naming only d's
// namespace and the permissions missing there.
func droppedMessage(d droppedNamespace) string {
	return fmt.Sprintf("the operator left namespace %s out of its watch at start (%s): this status is no longer updated; restore the chart's manager RoleBinding there, then restart the operator", d.Namespace, d.Reason)
}

// reportDroppedNamespaces tells the DBA, on the objects themselves, that
// their status is no longer updated: for each PgAdmin, PgToolBoxConsole
// and EvidenceAgent of a namespace left out for its permissions (the only
// cause for which the review says what the operator may still do there),
// Ready turns Unknown (reason NamespaceNotWatched) where the review shows
// the operator may list the kind and patch its status, and a Warning Event
// of the same reason is created where it may create events: once, when
// Ready changed, or at every start when Ready cannot be written. Best
// effort, uncached and bounded (each request droppedWriteTimeout, one List
// page per kind, the whole droppedReportTimeout): a namespace is often
// dropped because writes are refused there, so a failure is logged and
// never retried. Nothing is written in a namespace whose permissions are
// unknown (Clusters unlistable, review failed), nor in any watched one.
func reportDroppedNamespaces(ctx context.Context, api droppedAPI, dropped []droppedNamespace, now func() time.Time) {
	ctx, cancel := context.WithTimeout(ctx, droppedReportTimeout)
	defer cancel()
	instance, _ := os.Hostname()
	if instance == "" {
		instance = "pgtoolbox-operator"
	}
	for _, d := range dropped {
		if d.Cause != dropPermissions {
			setupLog.Info("no status or Event written in a dropped namespace: the operator's permissions there are unknown",
				"namespace", d.Namespace, "reason", string(d.Cause))
			continue
		}
		for _, kind := range droppedKinds {
			reportDroppedKind(ctx, api, d, kind, instance, now)
		}
	}
}

// reportDroppedKind is reportDroppedNamespaces for one kind in one namespace.
func reportDroppedKind(ctx context.Context, api droppedAPI, d droppedNamespace, kind droppedKind, instance string, now func() time.Time) {
	group := pgtoolboxv1alpha1.GroupVersion.Group
	canEvent := d.allows("events.k8s.io", "events", "create")
	canStatus := d.allows(group, kind.resource+"/status", "patch")
	if !d.allows(group, kind.resource, "list") || (!canEvent && !canStatus) {
		setupLog.Info("no status or Event written in a dropped namespace: the operator may not list the kind and write its status or events there",
			"namespace", d.Namespace, "resource", kind.resource)
		return
	}
	list, targets := kind.list()
	listCtx, cancel := context.WithTimeout(ctx, droppedWriteTimeout)
	err := api.List(listCtx, list, client.InNamespace(d.Namespace), client.Limit(droppedObjectsPerKind))
	cancel()
	if err != nil {
		setupLog.Error(err, "could not list objects of a dropped namespace to report it", "namespace", d.Namespace, "resource", kind.resource)
		return
	}
	message := droppedMessage(d)
	for _, target := range targets() {
		// The Event marks the transition: Ready changed, or Ready could
		// not be written (nothing then remembers it was said).
		event := true
		if canStatus {
			transition, err := markNotWatched(ctx, api, target, message)
			if apierrors.IsConflict(err) {
				// Written meanwhile (another replica's report, before
				// this one lost the election): read it again and
				// decide on what it says now, so one transition makes
				// one Event; where it may not be read, what was written
				// is taken as said.
				if d.allows(group, kind.resource, "get") {
					transition, err = remarkNotWatched(ctx, api, target, message)
				} else {
					transition, err = false, nil
				}
			}
			if err != nil {
				setupLog.Error(err, "could not mark an object of a dropped namespace Ready=Unknown",
					"namespace", d.Namespace, "kind", target.kind, "name", target.object.GetName())
			}
			event = transition || err != nil
		}
		if !canEvent || !event {
			continue
		}
		if err := createDroppedEvent(ctx, api, target, message, instance, now()); err != nil {
			setupLog.Error(err, "could not record the dropped-namespace Event",
				"namespace", d.Namespace, "kind", target.kind, "name", target.object.GetName())
		}
	}
}

// markNotWatched sets Ready=Unknown (NamespaceNotWatched, message) on
// target and patches its status, locked to the resourceVersion listed
// (a status written meanwhile wins) and as the status field owner; nothing
// is written when Ready already says so. It reports whether Ready's
// status, reason or message changed (a transition, worth an Event).
func markNotWatched(ctx context.Context, api droppedAPI, target droppedTarget, message string) (bool, error) {
	original, ok := target.object.DeepCopyObject().(client.Object)
	if !ok {
		return false, fmt.Errorf("%T is not a client.Object", target.object)
	}
	before := slices.Clone(*target.conditions)
	conditions.Set(target.conditions, target.object.GetGeneration(), pgtoolboxv1alpha1.ConditionReady,
		metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonNamespaceNotWatched, message)
	if equality.Semantic.DeepEqual(before, *target.conditions) {
		return false, nil
	}
	previous := meta.FindStatusCondition(before, pgtoolboxv1alpha1.ConditionReady)
	ready := meta.FindStatusCondition(*target.conditions, pgtoolboxv1alpha1.ConditionReady)
	transition := previous == nil || previous.Status != ready.Status || previous.Reason != ready.Reason || previous.Message != ready.Message
	writeCtx, cancel := context.WithTimeout(ctx, droppedWriteTimeout)
	defer cancel()
	if err := api.Status().Patch(writeCtx, target.object, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}),
		client.FieldOwner(reconcile.StatusFieldOwner)); err != nil {
		return false, fmt.Errorf("patch the status of %s %s/%s: %w", target.kind, target.object.GetNamespace(), target.object.GetName(), err)
	}
	return transition, nil
}

// remarkNotWatched is markNotWatched on target read again.
func remarkNotWatched(ctx context.Context, api droppedAPI, target droppedTarget, message string) (bool, error) {
	readCtx, cancel := context.WithTimeout(ctx, droppedWriteTimeout)
	err := api.Get(readCtx, client.ObjectKeyFromObject(target.object), target.object)
	cancel()
	if err != nil {
		return false, fmt.Errorf("read %s %s/%s again: %w", target.kind, target.object.GetNamespace(), target.object.GetName(), err)
	}
	return markNotWatched(ctx, api, target, message)
}

// droppedReport is reportDroppedNamespaces as a manager Runnable that runs
// on the elected leader only: two replicas starting together would each
// report, and the second would emit a second Event for one transition.
type droppedReport struct {
	api     droppedAPI
	dropped []droppedNamespace
	now     func() time.Time
}

// NeedLeaderElection makes the manager start the report once this
// replica leads.
func (droppedReport) NeedLeaderElection() bool { return true }

// Start runs the report, bounded by droppedReportTimeout, and returns: an
// error is logged per object, never stops the manager.
func (r droppedReport) Start(ctx context.Context) error {
	reportDroppedNamespaces(ctx, r.api, r.dropped, r.now)
	return nil
}

// createDroppedEvent creates the Warning Event (events.k8s.io/v1, reason
// NamespaceNotWatched) regarding target.
func createDroppedEvent(ctx context.Context, api droppedAPI, target droppedTarget, message, instance string, at time.Time) error {
	object := target.object
	event := &eventsv1.Event{
		// Generated (a PgAdmin and an EvidenceAgent may share a name); the
		// API server accepts a prefix ending in "-", not ".", and
		// truncates a long one.
		ObjectMeta:          metav1.ObjectMeta{GenerateName: object.GetName() + "-", Namespace: object.GetNamespace()},
		EventTime:           metav1.NewMicroTime(at),
		ReportingController: droppedReportingController,
		ReportingInstance:   instance,
		Action:              "Watch",
		Reason:              pgtoolboxv1alpha1.ReasonNamespaceNotWatched.String(),
		Type:                corev1.EventTypeWarning,
		Note:                reconcile.BoundedMessage(message),
		Regarding: corev1.ObjectReference{
			APIVersion: pgtoolboxv1alpha1.GroupVersion.String(), Kind: target.kind,
			Namespace: object.GetNamespace(), Name: object.GetName(), UID: object.GetUID(),
			ResourceVersion: object.GetResourceVersion(),
		},
	}
	writeCtx, cancel := context.WithTimeout(ctx, droppedWriteTimeout)
	defer cancel()
	if err := api.Create(writeCtx, event); err != nil {
		return fmt.Errorf("create the Event of %s %s/%s: %w", target.kind, object.GetNamespace(), object.GetName(), err)
	}
	return nil
}

// droppedFromReachable is the dropClustersUnlistable entries of refused.
func droppedFromReachable(refused []discovery.Result) []droppedNamespace {
	dropped := make([]droppedNamespace, len(refused))
	for i, r := range refused {
		dropped[i] = droppedNamespace{Namespace: r.Namespace, Cause: dropClustersUnlistable, Reason: r.Reason}
	}
	return dropped
}
