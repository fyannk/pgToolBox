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

package conditions

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
)

func TestDeriveReady(t *testing.T) {
	aspects := []string{"A", "B"}
	cases := []struct {
		name       string
		conditions []metav1.Condition
		loopErr    error
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "all true",
			conditions: []metav1.Condition{cond("A", metav1.ConditionTrue, 2), cond("B", metav1.ConditionTrue, 2)},
			wantStatus: metav1.ConditionTrue, wantReason: "AsExpected",
		},
		{
			name:       "one false copies its reason",
			conditions: []metav1.Condition{cond("A", metav1.ConditionTrue, 2), cond("B", metav1.ConditionFalse, 2)},
			wantStatus: metav1.ConditionFalse, wantReason: "PodUnschedulable",
		},
		{
			name:       "missing aspect is unknown",
			conditions: []metav1.Condition{cond("A", metav1.ConditionTrue, 2)},
			wantStatus: metav1.ConditionUnknown, wantReason: "Initializing",
		},
		{
			name:       "stale aspect is unknown",
			conditions: []metav1.Condition{cond("A", metav1.ConditionTrue, 1), cond("B", metav1.ConditionTrue, 2)},
			wantStatus: metav1.ConditionUnknown, wantReason: "Reconciling",
		},
		{
			name:       "loop error wins over true aspects",
			conditions: []metav1.Condition{cond("A", metav1.ConditionTrue, 2), cond("B", metav1.ConditionTrue, 2)},
			loopErr:    errors.New("ReconciliationFailed: boom"),
			wantStatus: metav1.ConditionFalse, wantReason: "ReconcileError",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conditions := append([]metav1.Condition(nil), tc.conditions...)
			DeriveReady(&conditions, 2, tc.loopErr, aspects...)
			ready := meta.FindStatusCondition(conditions, "Ready")
			if ready == nil || ready.Status != tc.wantStatus || ready.Reason != tc.wantReason || ready.ObservedGeneration != 2 {
				t.Fatalf("Ready = %+v, want %s/%s", ready, tc.wantStatus, tc.wantReason)
			}
		})
	}
}

func TestSetUnregisteredReasonDoesNotPanic(t *testing.T) {
	var conditions []metav1.Condition
	Set(&conditions, 1, "A", metav1.ConditionFalse, pgtoolboxv1alpha1.Reason("Typo"), "m")
	if conditions[0].Reason != "ReconcileError" {
		t.Fatalf("reason = %s", conditions[0].Reason)
	}
}

func TestTransitionsIgnoreMessageOnlyChanges(t *testing.T) {
	before := []metav1.Condition{cond("A", metav1.ConditionTrue, 1)}
	after := []metav1.Condition{cond("A", metav1.ConditionTrue, 1), cond("B", metav1.ConditionFalse, 1)}
	after[0].Message = "different"
	got := Transitions(before, after)
	if len(got) != 1 || got[0].Type != "B" || got[0].Before != nil {
		t.Fatalf("transitions = %+v", got)
	}
}

func TestInitializeUnknown(t *testing.T) {
	conditions := []metav1.Condition{cond("A", metav1.ConditionTrue, 1)}
	InitializeUnknown(&conditions, 1, "A", "B")
	if meta.FindStatusCondition(conditions, "A").Status != metav1.ConditionTrue {
		t.Fatal("existing condition overwritten")
	}
	if b := meta.FindStatusCondition(conditions, "B"); b == nil || b.Status != metav1.ConditionUnknown || b.Reason != "Initializing" {
		t.Fatalf("B = %+v", b)
	}
}

func cond(conditionType string, status metav1.ConditionStatus, generation int64) metav1.Condition {
	reason := "AsExpected"
	if status == metav1.ConditionFalse {
		reason = "PodUnschedulable"
	}
	return metav1.Condition{Type: conditionType, Status: status, Reason: reason, ObservedGeneration: generation, Message: "m"}
}

// TestSummarize is the one condition model both kinds publish: how a
// loop ended decides Stalled, Progressing and Ready, and what the
// controller returns.
func TestSummarize(t *testing.T) {
	const generation = 4
	aspects := []string{"ConfigurationValid", "WorkloadReady"}
	allTrue := []metav1.Condition{cond("ConfigurationValid", metav1.ConditionTrue, generation), cond("WorkloadReady", metav1.ConditionTrue, generation)}
	workloadFalse := []metav1.Condition{cond("ConfigurationValid", metav1.ConditionTrue, generation), cond("WorkloadReady", metav1.ConditionFalse, generation)}
	type want struct{ status, reason string }
	cases := []struct {
		name                      string
		start                     []metav1.Condition
		outcome                   Outcome
		ready, progressing, stall want
		returns                   bool
	}{
		{
			name: "converged", start: allTrue, outcome: Outcome{Converged: true},
			ready: want{"True", "AsExpected"}, progressing: want{"False", "AsExpected"}, stall: want{"False", "AsExpected"},
		},
		{
			name: "converging copies the child's fact", start: workloadFalse, outcome: Outcome{},
			ready: want{"False", "PodUnschedulable"}, progressing: want{"True", "Reconciling"}, stall: want{"False", "AsExpected"},
		},
		{
			name: "waiting for a watch is not an error", start: workloadFalse, outcome: Outcome{Err: reconcile.NextLoop("pod")},
			ready: want{"False", "PodUnschedulable"}, progressing: want{"True", "Reconciling"}, stall: want{"False", "AsExpected"},
		},
		{
			name: "stalled", start: []metav1.Condition{cond("ConfigurationValid", metav1.ConditionFalse, generation)},
			outcome: Outcome{Err: fmt.Errorf("resolve: %w", reconcile.Stall(pgtoolboxv1alpha1.ReasonSecretNotFound, "no Secret x"))},
			ready:   want{"False", "PodUnschedulable"}, progressing: want{"False", "SecretNotFound"}, stall: want{"True", "SecretNotFound"},
		},
		{
			name: "a real error makes Ready False and is returned", start: allTrue, outcome: Outcome{Err: errors.New("boom"), Converged: true},
			ready: want{"False", "ReconcileError"}, progressing: want{"True", "Reconciling"}, stall: want{"False", "AsExpected"},
			returns: true,
		},
		{
			name: "paused does not vouch for the children", start: allTrue, outcome: Outcome{Paused: true},
			ready: want{"Unknown", "ReconciliationPaused"}, progressing: want{"False", "ReconciliationPaused"}, stall: want{"False", "AsExpected"},
		},
		{
			name: "first touch", start: nil, outcome: Outcome{},
			ready: want{"Unknown", "Initializing"}, progressing: want{"True", "Reconciling"}, stall: want{"False", "AsExpected"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conditions := append([]metav1.Condition(nil), tc.start...)
			err := Summarize(&conditions, generation, tc.outcome, aspects...)
			if (err != nil) != tc.returns {
				t.Fatalf("returned %v, want an error: %v", err, tc.returns)
			}
			for conditionType, w := range map[string]want{"Ready": tc.ready, "Progressing": tc.progressing, "Stalled": tc.stall} {
				c := meta.FindStatusCondition(conditions, conditionType)
				if c == nil || string(c.Status) != w.status || c.Reason != w.reason || c.ObservedGeneration != generation {
					t.Errorf("%s = %+v, want %s/%s at generation %d", conditionType, c, w.status, w.reason, generation)
				}
			}
			if tc.returns {
				if msg := meta.FindStatusCondition(conditions, "Ready").Message; !strings.HasPrefix(msg, "ReconciliationFailed: ") {
					t.Errorf("Ready message %q does not carry the error class", msg)
				}
			}
		})
	}
}

func drain(recorder *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestRecordTransitions: one Event per transition worth a human's
// attention, none for a message-only change, a progress reason or a
// repeated error class.
func TestRecordTransitions(t *testing.T) {
	gr := schema.GroupResource{Resource: "deployments"}
	quota := apierrors.NewForbidden(gr, "x", errors.New("exceeded quota: q"))
	at := func(conditionType string, status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason) metav1.Condition {
		return metav1.Condition{Type: conditionType, Status: status, Reason: reason.String(), Message: "m", ObservedGeneration: 1}
	}
	readyErr := func(class pgtoolboxv1alpha1.Reason) metav1.Condition {
		c := at("Ready", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonReconcileError)
		c.Message = class.String() + ": boom"
		return c
	}
	cases := []struct {
		name          string
		before, after []metav1.Condition
		reported      error
		quiet         []pgtoolboxv1alpha1.Reason
		want          []string
	}{
		{
			name:   "first touch is silent",
			before: nil, after: []metav1.Condition{at("Ready", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonInitializing)},
		},
		{
			name:   "Ready turning True is Normal",
			before: []metav1.Condition{at("Ready", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonPodNotReady)},
			after:  []metav1.Condition{at("Ready", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)},
			want:   []string{"Normal AsExpected"},
		},
		{
			name:   "an aspect turning False is Warning; Ready following it is not another",
			before: []metav1.Condition{at("WorkloadReady", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)},
			after: []metav1.Condition{
				at("WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonImagePullFailed),
				at("Ready", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonImagePullFailed),
			},
			want: []string{"Warning ImagePullFailed"},
		},
		{
			name:   "a progress reason is quiet",
			before: nil,
			after: []metav1.Condition{
				at("WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress),
				at("ServingCertificateReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonServiceCAPending),
			},
			quiet: []pgtoolboxv1alpha1.Reason{pgtoolboxv1alpha1.ReasonServiceCAPending},
		},
		{
			name:   "a stall reported by its aspect is one Warning",
			before: nil,
			after: []metav1.Condition{
				at("ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonClusterNotFound),
				at("Stalled", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonClusterNotFound),
			},
			want: []string{"Warning ClusterNotFound"},
		},
		{
			name:   "two aspects turning False for one reason are one Warning",
			before: nil,
			after: []metav1.Condition{
				at("ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed),
				at("ExposureReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed),
				at("WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonImagePullFailed),
				at("Stalled", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonHostnameNotAllowed),
			},
			want: []string{"Warning HostnameNotAllowed", "Warning ImagePullFailed"},
		},
		{
			name:   "a stall of its own is a Warning",
			before: nil,
			after:  []metav1.Condition{at("Stalled", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonUpgradeBackupFailed)},
			want:   []string{"Warning UpgradeBackupFailed"},
		},
		{
			name:   "message-only change is no transition",
			before: []metav1.Condition{at("WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonCrashLoopBackOff)},
			after: []metav1.Condition{func() metav1.Condition {
				c := at("WorkloadReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonCrashLoopBackOff)
				c.Message = "restarts 7"
				return c
			}()},
		},
		{
			name:     "a new error class is one classified Warning",
			before:   []metav1.Condition{at("Ready", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)},
			after:    []metav1.Condition{readyErr(pgtoolboxv1alpha1.ReasonQuotaExceeded)},
			reported: quota,
			want:     []string{"Warning QuotaExceeded"},
		},
		{
			name:     "the same error class again is silent",
			before:   []metav1.Condition{readyErr(pgtoolboxv1alpha1.ReasonQuotaExceeded)},
			after:    []metav1.Condition{readyErr(pgtoolboxv1alpha1.ReasonQuotaExceeded)},
			reported: quota,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := events.NewFakeRecorder(16)
			RecordTransitions(recorder, &corev1.ConfigMap{}, tc.before, tc.after, tc.reported, tc.quiet...)
			got := drain(recorder)
			if len(got) != len(tc.want) {
				t.Fatalf("events %q, want %q", got, tc.want)
			}
			for i := range got {
				if !strings.HasPrefix(got[i], tc.want[i]) {
					t.Errorf("event %q, want prefix %q", got[i], tc.want[i])
				}
			}
		})
	}
}

func TestAllTrue(t *testing.T) {
	conditions := []metav1.Condition{cond("A", metav1.ConditionTrue, 2), cond("B", metav1.ConditionTrue, 1)}
	if !AllTrue(conditions, 2, "A") || AllTrue(conditions, 2, "A", "B") || AllTrue(conditions, 2, "C") {
		t.Fatal("AllTrue must need every condition True at the current generation")
	}
}

// TestSummarizeIsStable: summarizing the same outcome again changes
// nothing, LastTransitionTime included, whatever the outcome — the
// precondition of a no-op reconcile that writes nothing.
func TestSummarizeIsStable(t *testing.T) {
	aspects := []string{"A"}
	for _, outcome := range []Outcome{
		{Converged: true},
		{},
		{Paused: true},
		{Err: errors.New("boom")},
		{Err: reconcile.Stall(pgtoolboxv1alpha1.ReasonSecretNotFound, "x")},
	} {
		conditions := []metav1.Condition{cond("A", metav1.ConditionTrue, 1)}
		_ = Summarize(&conditions, 1, outcome, aspects...)
		for i := range conditions {
			conditions[i].LastTransitionTime = metav1.Unix(1, 0)
		}
		first := append([]metav1.Condition(nil), conditions...)
		_ = Summarize(&conditions, 1, outcome, aspects...)
		for i := range first {
			if first[i] != conditions[i] {
				t.Errorf("%+v: %s changed on a repeat: %+v -> %+v", outcome, first[i].Type, first[i], conditions[i])
			}
		}
	}
}
