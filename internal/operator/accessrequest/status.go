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

package accessrequest

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

// Status of the PgAccessRequest controller: the condition model the
// operator's controllers share (internal/operator/conditions).

// aspectConditions are the conditions Ready is derived from, in the order
// a failing one is reported.
var aspectConditions = []string{
	pgtoolboxv1alpha1.ConditionConfigurationValid,
	pgtoolboxv1alpha1.ConditionRoleBindingSynced,
}

func setCondition(access *pgtoolboxv1alpha1.PgAccessRequest, conditionType string, status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason, message string) {
	conditions.Set(&access.Status.Conditions, access.Generation, conditionType, status, reason, message)
}

func initializeConditions(access *pgtoolboxv1alpha1.PgAccessRequest) {
	conditions.InitializeUnknown(&access.Status.Conditions, access.Generation, append([]string{
		pgtoolboxv1alpha1.ConditionReady, pgtoolboxv1alpha1.ConditionProgressing, pgtoolboxv1alpha1.ConditionStalled,
	}, aspectConditions...)...)
}

// publish is the one status write of a loop, on every exit path: Stalled,
// Progressing and Ready from the outcome and the aspects in the same write;
// nothing written when nothing changed; Events after a successful write.
// The patch is a merge patch locked to the resourceVersion read: the
// decision fields are written by people through the console, and a status
// computed from a read older than their write is refused (Conflict) and
// recomputed, never written over theirs.
func (r *Reconciler) publish(ctx context.Context, access, original *pgtoolboxv1alpha1.PgAccessRequest, loop *loopState, outcome conditions.Outcome) error {
	outcome.Converged = outcome.Converged && conditions.AllTrue(access.Status.Conditions, access.Generation, aspectConditions...)
	reported := conditions.Summarize(&access.Status.Conditions, access.Generation, outcome, aspectConditions...)
	access.Status.ObservedGeneration = access.Generation
	if equality.Semantic.DeepEqual(original.Status, access.Status) {
		return reported
	}
	statusCtx, cancel := reconcile.StatusContext(ctx)
	defer cancel()
	if err := r.Client.Status().Patch(statusCtx, access, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}),
		client.FieldOwner(reconcile.StatusFieldOwner)); err != nil {
		if apierrors.IsNotFound(err) {
			return reported
		}
		return errors.Join(reported, fmt.Errorf("publish PgAccessRequest status: %w", err))
	}
	r.emit(access, loop)
	conditions.RecordTransitions(r.Recorder, access, original.Status.Conditions, access.Status.Conditions, reported)
	return reported
}
