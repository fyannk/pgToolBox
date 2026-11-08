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

// Package conditions holds the condition helpers of the v1alpha1 kinds: the
// one condition model that PgAdmin and EvidenceAgent share. The condition types and the reason catalogue live in
// the API package; these helpers are the one write path, stamp
// ObservedGeneration on every condition, bound every message, set Unknown on
// first touch, decide Stalled and Progressing from how the loop ended, derive
// Ready from the aspect conditions in the same write, and emit one Event per
// transition.
package conditions

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
)

// Set writes one condition. The reason is typed, so only catalogue
// constants compile in the controllers; a reason that is nevertheless not
// registered (a conversion from a string) is replaced by ReconcileError
// rather than panicking a running operator — TestReasonCatalogueFrozen and
// the controllers' tests are where an unregistered reason fails.
func Set(conditions *[]metav1.Condition, generation int64, conditionType string,
	status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason, message string,
) {
	if !pgtoolboxv1alpha1.IsKnownReason(reason) {
		message = "unregistered reason " + string(reason) + ": " + message
		reason = pgtoolboxv1alpha1.ReasonReconcileError
	}
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: generation,
		Reason:             reason.String(),
		Message:            reconcile.BoundedMessage(message),
	})
}

// InitializeUnknown sets every listed condition that is absent to Unknown
// (reason Initializing): Unknown on first touch, never a guessed state.
func InitializeUnknown(conditions *[]metav1.Condition, generation int64, conditionTypes ...string) {
	for _, conditionType := range conditionTypes {
		if meta.FindStatusCondition(*conditions, conditionType) == nil {
			Set(conditions, generation, conditionType, metav1.ConditionUnknown,
				pgtoolboxv1alpha1.ReasonInitializing, "not evaluated yet")
		}
	}
}

// DeriveReady sets Ready from aspects: True (AsExpected) when every aspect
// is True at the current generation; otherwise the first aspect that is not
// True (in the order given) decides, False if it is False, Unknown if it is
// Unknown or stale, and Ready copies its reason and message. An error that
// ended the loop (loopErr) makes Ready False with ReconcileError: kubectl
// get never says Ready while the operator is failing.
func DeriveReady(conditions *[]metav1.Condition, generation int64, loopErr error, aspects ...string) {
	if loopErr != nil {
		Set(conditions, generation, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionFalse,
			pgtoolboxv1alpha1.ReasonReconcileError, loopErr.Error())
		return
	}
	for _, aspect := range aspects {
		condition := meta.FindStatusCondition(*conditions, aspect)
		switch {
		case condition == nil:
			Set(conditions, generation, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionUnknown,
				pgtoolboxv1alpha1.ReasonInitializing, aspect+" not evaluated yet")
			return
		case condition.ObservedGeneration != generation:
			Set(conditions, generation, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionUnknown,
				pgtoolboxv1alpha1.ReasonReconciling, aspect+" not evaluated at the current generation")
			return
		case condition.Status != metav1.ConditionTrue:
			Set(conditions, generation, pgtoolboxv1alpha1.ConditionReady, condition.Status,
				pgtoolboxv1alpha1.Reason(condition.Reason), aspect+": "+condition.Message)
			return
		}
	}
	Set(conditions, generation, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionTrue,
		pgtoolboxv1alpha1.ReasonAsExpected, "every aspect is as expected")
}

// Transition is one condition whose status or reason changed in a write.
type Transition struct {
	Type   string
	Before *metav1.Condition
	After  metav1.Condition
}

// Transitions returns the conditions of after whose status or reason
// differs from before (or that are new). Message-only changes are not
// transitions: Events are emitted once per transition, not per message.
func Transitions(before, after []metav1.Condition) []Transition {
	var out []Transition
	for _, condition := range after {
		previous := meta.FindStatusCondition(before, condition.Type)
		if previous != nil && previous.Status == condition.Status && previous.Reason == condition.Reason {
			continue
		}
		var copied *metav1.Condition
		if previous != nil {
			c := *previous
			copied = &c
		}
		out = append(out, Transition{Type: condition.Type, Before: copied, After: condition})
	}
	return out
}

// ErrorClass returns the classification prefix of a ReconcileError message
// ("ReconciliationFailed: ..."), so a repeated error of the same class is
// not a new Event.
func ErrorClass(message string) string {
	if index := strings.Index(message, ": "); index > 0 {
		return message[:index]
	}
	return ""
}

// IsTrue reports whether the condition is present and True.
func IsTrue(conditions []metav1.Condition, conditionType string) bool {
	return meta.IsStatusConditionTrue(conditions, conditionType)
}

// Outcome is how one reconciliation loop ended.
type Outcome struct {
	// Err is what the inner loop returned: nil, ErrNextLoop (waiting for
	// a watched change), a StallError, or a real error.
	Err error
	// Paused is true while the object carries reconcile=skip.
	Paused bool
	// Converged is true when every child is at its desired state.
	Converged bool
}

// Summarize writes Stalled, Progressing and Ready from the loop's outcome,
// in that order and in the same status write as the aspects:
//   - a StallError: Stalled=True and Progressing=False with its reason;
//   - paused: Progressing=False (ReconciliationPaused), and Ready=Unknown
//     with the same reason — the operator is not looking, so it does not
//     vouch for the children;
//   - converged: Progressing=False (AsExpected);
//   - otherwise: Progressing=True (Reconciling).
//
// Ready is DeriveReady over aspects; a real error makes it False
// (ReconcileError) with the error's class as message prefix, so a repeated
// error of the same class is not a new Event. Summarize returns the error
// the controller hands back to controller-runtime: nil for ErrNextLoop and
// for a stall, which a watch (not a backoff) resolves.
func Summarize(conditions *[]metav1.Condition, generation int64, outcome Outcome, aspects ...string) error {
	var stalled *reconcile.StallError
	switch {
	case errors.As(outcome.Err, &stalled):
		Set(conditions, generation, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionTrue, stalled.Reason, stalled.Message)
		Set(conditions, generation, pgtoolboxv1alpha1.ConditionProgressing, metav1.ConditionFalse, stalled.Reason,
			"stalled until the input changes")
	case outcome.Paused:
		Set(conditions, generation, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonAsExpected, "")
		Set(conditions, generation, pgtoolboxv1alpha1.ConditionProgressing, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonReconciliationPaused,
			pausedMessage)
	case outcome.Converged && outcome.Err == nil:
		Set(conditions, generation, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonAsExpected, "")
		Set(conditions, generation, pgtoolboxv1alpha1.ConditionProgressing, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonAsExpected, "converged")
	default:
		Set(conditions, generation, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonAsExpected, "")
		Set(conditions, generation, pgtoolboxv1alpha1.ConditionProgressing, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonReconciling, "converging")
	}

	reported := outcome.Err
	if errors.Is(reported, reconcile.ErrStalled) || errors.Is(reported, reconcile.ErrNextLoop) {
		reported = nil
	}
	var readyErr error
	if reported != nil {
		reason := reconcile.ErrorReason(reported)
		if reason == "" {
			reason = pgtoolboxv1alpha1.ReasonReconciliationFailed
		}
		readyErr = fmt.Errorf("%s: %w", reason, reported)
	}
	if outcome.Paused && reported == nil {
		// Set directly, never derived then overwritten: a status that
		// flips twice in one write moves LastTransitionTime on every loop
		// and turns a no-op reconcile into a write.
		Set(conditions, generation, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciliationPaused,
			pausedMessage)
		return nil
	}
	DeriveReady(conditions, generation, readyErr, aspects...)
	return reported
}

const pausedMessage = pgtoolboxv1alpha1.AnnotationReconcile + "=" + pgtoolboxv1alpha1.ReconcileSkip +
	": reconciliation is paused, nothing is applied or observed"

// EventRecorder is the part of the events.k8s.io recorder the
// controllers use (a consumer-owned interface; the manager's
// events.EventRecorder and the fake recorder satisfy it).
type EventRecorder interface {
	Eventf(regarding runtime.Object, related runtime.Object, eventtype, reason, action, note string, args ...any)
}

// progressReasons are False reasons of a child on its way (a rollout, a
// Pod starting): reported in status, not as Warnings.
var progressReasons = map[pgtoolboxv1alpha1.Reason]bool{
	pgtoolboxv1alpha1.ReasonRolloutInProgress: true,
	pgtoolboxv1alpha1.ReasonPodNotReady:       true,
}

// RecordTransitions emits one Event per transition worth a human's
// attention between the conditions before and after a status write — call
// it once the write succeeded, so a failed write never yields an Event for
// a state nobody can see:
//   - Ready turning True: Normal, reason AsExpected;
//   - Stalled turning True: Warning with the stall's reason, unless an
//     aspect turning False in the same write already said it;
//   - an aspect turning False (any type but Ready, Progressing and
//     Stalled), unless its reason is a progress reason (RolloutInProgress,
//     PodNotReady, or one of quiet): Warning with that reason;
//   - a reconciliation error (reported) of a class Ready did not carry
//     before: Warning with the classified reason (ReconciliationFailed,
//     QuotaExceeded, AdmissionDenied) — once per class, not per retry.
//
// Event reasons are condition reasons; message-only changes are not
// transitions; notes are bounded.
func RecordTransitions(recorder EventRecorder, object runtime.Object, before, after []metav1.Condition, reported error,
	quiet ...pgtoolboxv1alpha1.Reason,
) {
	if recorder == nil {
		return
	}
	isQuiet := func(reason string) bool {
		for _, q := range quiet {
			if q.String() == reason {
				return true
			}
		}
		return progressReasons[pgtoolboxv1alpha1.Reason(reason)]
	}
	event := func(eventType, reason, note string) {
		recorder.Eventf(object, nil, eventType, reason, "Reconcile", "%s", reconcile.BoundedMessage(note))
	}
	warned := map[string]bool{}
	var stalled *metav1.Condition
	for _, t := range Transitions(before, after) {
		c := t.After
		switch {
		case t.Type == pgtoolboxv1alpha1.ConditionReady:
			if c.Status == metav1.ConditionTrue {
				event(corev1.EventTypeNormal, c.Reason, c.Message)
			}
		case t.Type == pgtoolboxv1alpha1.ConditionStalled:
			if c.Status == metav1.ConditionTrue {
				stalled = &c
			}
		case t.Type == pgtoolboxv1alpha1.ConditionProgressing:
		case c.Status == metav1.ConditionFalse && !isQuiet(c.Reason):
			warned[c.Reason] = true
			event(corev1.EventTypeWarning, c.Reason, t.Type+": "+c.Message)
		}
	}
	// A stall whose cause an aspect just reported (ConfigurationValid
	// turning False for the same reason) is the same transition: one
	// Warning, not two.
	if stalled != nil && !warned[stalled.Reason] {
		event(corev1.EventTypeWarning, stalled.Reason, pgtoolboxv1alpha1.ConditionStalled+": "+stalled.Message)
	}
	reason := reconcile.ErrorReason(reported)
	if reason == "" {
		return
	}
	previous := ""
	if ready := meta.FindStatusCondition(before, pgtoolboxv1alpha1.ConditionReady); ready != nil &&
		ready.Reason == pgtoolboxv1alpha1.ReasonReconcileError.String() {
		previous = ErrorClass(ready.Message)
	}
	if previous != reason.String() {
		event(corev1.EventTypeWarning, reason.String(), reported.Error())
	}
}

// AllTrue reports whether every listed condition is True at generation.
func AllTrue(conditions []metav1.Condition, generation int64, conditionTypes ...string) bool {
	for _, conditionType := range conditionTypes {
		c := meta.FindStatusCondition(conditions, conditionType)
		if c == nil || c.Status != metav1.ConditionTrue || c.ObservedGeneration != generation {
			return false
		}
	}
	return true
}
