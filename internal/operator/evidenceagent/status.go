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

package evidenceagent

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/conditions"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
)

// Status of the EvidenceAgent controller: the condition model the
// operator's controllers share (internal/operator/conditions); the types and
// reasons are the frozen catalogue of api/v1alpha1.

// aspectConditions are the conditions Ready is derived from, in the order
// a failing one is reported.
var aspectConditions = []string{
	pgtoolboxv1alpha1.ConditionConfigurationValid,
	pgtoolboxv1alpha1.ConditionServingCertificateReady,
	pgtoolboxv1alpha1.ConditionWorkloadReady,
}

// quietReasons are the agent's False reasons of a child on its way: the
// OpenShift service CA has not injected the bundle yet.
var quietReasons = []pgtoolboxv1alpha1.Reason{pgtoolboxv1alpha1.ReasonServiceCAPending}

// setCondition records one condition at the agent's generation.
func setCondition(agent *pgtoolboxv1alpha1.EvidenceAgent, conditionType string, status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason, message string) {
	conditions.Set(&agent.Status.Conditions, agent.Generation, conditionType, status, reason, message)
}

// initializeConditions sets every condition Unknown on first touch.
func initializeConditions(agent *pgtoolboxv1alpha1.EvidenceAgent) {
	conditions.InitializeUnknown(&agent.Status.Conditions, agent.Generation, append([]string{
		pgtoolboxv1alpha1.ConditionReady, pgtoolboxv1alpha1.ConditionProgressing, pgtoolboxv1alpha1.ConditionStalled,
	}, aspectConditions...)...)
}

// publish is the one status write of a loop, on every exit path: Stalled,
// Progressing and Ready are decided from the outcome and the aspects in
// the same write, nothing is written when nothing changed (a no-op
// reconcile writes nothing), and Events follow a successful write, once
// per transition. It returns the error controller-runtime retries with
// backoff: none for a stall or ErrNextLoop.
func (r *Reconciler) publish(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent, original *pgtoolboxv1alpha1.EvidenceAgent, outcome conditions.Outcome) error {
	outcome.Converged = outcome.Converged && conditions.AllTrue(agent.Status.Conditions, agent.Generation, aspectConditions...)
	reported := conditions.Summarize(&agent.Status.Conditions, agent.Generation, outcome, aspectConditions...)
	agent.Status.ObservedGeneration = agent.Generation
	if equality.Semantic.DeepEqual(original.Status, agent.Status) {
		return reported
	}
	// Optimistic lock: a status computed from a stale read is refused
	// (Conflict) and the request retried with backoff, never written.
	if err := r.Client.Status().Patch(ctx, agent, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}),
		client.FieldOwner(reconcile.StatusFieldOwner)); err != nil {
		if apierrors.IsNotFound(err) {
			return reported
		}
		return errors.Join(reported, fmt.Errorf("publish EvidenceAgent status: %w", err))
	}
	conditions.RecordTransitions(r.Recorder, agent, original.Status.Conditions, agent.Status.Conditions, reported, quietReasons...)
	return reported
}
