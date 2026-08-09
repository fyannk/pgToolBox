/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

// Package conditions centralizes status-condition updates for API objects.
package conditions

import (
	"fmt"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MarkTrue sets a condition to True with a validated reason and the object's
// current observed generation.
func MarkTrue(obj any, conditionType, reason, messagef string, args ...any) {
	mark(obj, conditionType, metav1.ConditionTrue, reason, messagef, args...)
}

// MarkFalse sets a condition to False with a validated reason and the object's
// current observed generation.
func MarkFalse(obj any, conditionType, reason, messagef string, args ...any) {
	mark(obj, conditionType, metav1.ConditionFalse, reason, messagef, args...)
}

// MarkUnknown sets a condition to Unknown with a validated reason and the
// object's current observed generation.
func MarkUnknown(obj any, conditionType, reason, messagef string, args ...any) {
	mark(obj, conditionType, metav1.ConditionUnknown, reason, messagef, args...)
}

// mark is the single write path behind the Mark helpers. It panics on
// unknown reasons so a typo fails loudly in tests instead of surfacing in
// status, and stamps the condition with the object's current generation so
// consumers can detect stale conditions.
func mark(obj any, conditionType string, status metav1.ConditionStatus, reason, messagef string, args ...any) {
	assertKnownReason(reason)

	conditions, generation := statusConditions(obj)
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            fmt.Sprintf(messagef, args...),
	})
}

// statusConditions maps a supported API object to its mutable condition
// slice and current generation. It panics on any other type: adding a
// conditioned kind requires an explicit case here rather than silently
// dropping its conditions.
func statusConditions(obj any) (*[]metav1.Condition, int64) {
	switch o := obj.(type) {
	case *pgtoolboxv1alpha1.PgConsole:
		return &o.Status.Conditions, o.GetGeneration()
	case *pgtoolboxv1alpha1.PgToolBoxUser:
		return &o.Status.Conditions, o.GetGeneration()
	case *pgtoolboxv1alpha1.PgToolBoxAccessRequest:
		return &o.Status.Conditions, o.GetGeneration()
	default:
		panic(fmt.Sprintf("unsupported conditioned object type %T", obj))
	}
}

// assertKnownReason panics on reasons absent from knownReasons, keeping the
// vocabulary surfaced in status a closed, documented set.
func assertKnownReason(reason string) {
	if _, ok := knownReasons[reason]; ok {
		return
	}
	panic(fmt.Sprintf("unknown condition reason %q", reason))
}

var knownReasons = map[string]struct{}{
	pgtoolboxv1alpha1.ReasonAsExpected:                {},
	pgtoolboxv1alpha1.ReasonReconciling:               {},
	pgtoolboxv1alpha1.ReasonReconciliationSkipped:     {},
	pgtoolboxv1alpha1.ReasonRolloutInProgress:         {},
	pgtoolboxv1alpha1.ReasonRolloutStuck:              {},
	pgtoolboxv1alpha1.ReasonConfigurationInvalid:      {},
	pgtoolboxv1alpha1.ReasonSecretNotFound:            {},
	pgtoolboxv1alpha1.ReasonSecretKeyMissing:          {},
	pgtoolboxv1alpha1.ReasonSecretFormatInvalid:       {},
	pgtoolboxv1alpha1.ReasonRenderFailed:              {},
	pgtoolboxv1alpha1.ReasonNotAdmitted:               {},
	pgtoolboxv1alpha1.ReasonNotAccepted:               {},
	pgtoolboxv1alpha1.ReasonGatewayNotFound:           {},
	pgtoolboxv1alpha1.ReasonHostnameConflict:          {},
	pgtoolboxv1alpha1.ReasonNotRequested:              {},
	pgtoolboxv1alpha1.ReasonAllApplied:                {},
	pgtoolboxv1alpha1.ReasonSomeDegraded:              {},
	pgtoolboxv1alpha1.ReasonNoneConfigured:            {},
	pgtoolboxv1alpha1.ReasonPgConsoleNotFound:         {},
	pgtoolboxv1alpha1.ReasonClusterNotFound:           {},
	pgtoolboxv1alpha1.ReasonClusterNotReady:           {},
	pgtoolboxv1alpha1.ReasonRoleNotFound:              {},
	pgtoolboxv1alpha1.ReasonCredentialLost:            {},
	pgtoolboxv1alpha1.ReasonDatabaseRolePending:       {},
	pgtoolboxv1alpha1.ReasonDatabaseRoleFailed:        {},
	pgtoolboxv1alpha1.ReasonPendingRollout:            {},
	pgtoolboxv1alpha1.ReasonSyncFailed:                {},
	pgtoolboxv1alpha1.ReasonCNPGAPIMissing:            {},
	pgtoolboxv1alpha1.ReasonDatabaseRoleAPIMissing:    {},
	pgtoolboxv1alpha1.ReasonPending:                   {},
	pgtoolboxv1alpha1.ReasonApproved:                  {},
	pgtoolboxv1alpha1.ReasonDenied:                    {},
	pgtoolboxv1alpha1.ReasonEvidenceDisabled:          {},
	pgtoolboxv1alpha1.ReasonImageRequired:             {},
	pgtoolboxv1alpha1.ReasonObjectStoreNotFound:       {},
	pgtoolboxv1alpha1.ReasonUnsupportedCredentialMode: {},
	pgtoolboxv1alpha1.ReasonUnsupportedAuthMode:       {},
}
