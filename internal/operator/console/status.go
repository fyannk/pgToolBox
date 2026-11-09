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

package console

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

// aspectConditions are the conditions Ready is derived from, in the order a
// failing one is reported.
var aspectConditions = []string{
	pgtoolboxv1alpha1.ConditionConfigurationValid,
	pgtoolboxv1alpha1.ConditionWorkloadReady,
	pgtoolboxv1alpha1.ConditionExposureReady,
}

func setCondition(console *pgtoolboxv1alpha1.PgToolBoxConsole, conditionType string, status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason, message string) {
	conditions.Set(&console.Status.Conditions, console.Generation, conditionType, status, reason, message)
}

func initializeConditions(console *pgtoolboxv1alpha1.PgToolBoxConsole) {
	conditions.InitializeUnknown(&console.Status.Conditions, console.Generation, append([]string{
		pgtoolboxv1alpha1.ConditionReady, pgtoolboxv1alpha1.ConditionProgressing, pgtoolboxv1alpha1.ConditionStalled,
	}, aspectConditions...)...)
}

// publish is the one status write of a loop, on every exit path; nothing is
// written when nothing changed, and Events follow a successful write.
func (r *Reconciler) publish(ctx context.Context, console, original *pgtoolboxv1alpha1.PgToolBoxConsole, outcome conditions.Outcome) error {
	outcome.Converged = outcome.Converged && conditions.AllTrue(console.Status.Conditions, console.Generation, aspectConditions...)
	reported := conditions.Summarize(&console.Status.Conditions, console.Generation, outcome, aspectConditions...)
	console.Status.ObservedGeneration = console.Generation
	if equality.Semantic.DeepEqual(original.Status, console.Status) {
		return reported
	}
	statusCtx, cancel := reconcile.StatusContext(ctx)
	defer cancel()
	if err := r.Client.Status().Patch(statusCtx, console, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}),
		client.FieldOwner(reconcile.StatusFieldOwner)); err != nil {
		if apierrors.IsNotFound(err) {
			return reported
		}
		return errors.Join(reported, fmt.Errorf("publish PgToolBoxConsole status: %w", err))
	}
	conditions.RecordTransitions(r.Recorder, console, original.Status.Conditions, console.Status.Conditions, reported)
	return reported
}
