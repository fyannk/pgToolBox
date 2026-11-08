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

package v1alpha1

import (
	"fmt"
	"regexp"
	"sort"
)

// Condition types every v1alpha1 kind uses. Ready is the
// summary and is derived from the aspect conditions in the same status
// write; Progressing is positive (True while converging); Stalled is
// abnormal-true (True only when the operator cannot make progress without a
// change of input). Kind-specific aspect conditions (ServersSynced, ...)
// live in the kind's own types file. Condition types are frozen API
// surface: extend, never rename.
const (
	ConditionReady              = "Ready"
	ConditionProgressing        = "Progressing"
	ConditionStalled            = "Stalled"
	ConditionConfigurationValid = "ConfigurationValid"
	ConditionWorkloadReady      = "WorkloadReady"
	ConditionExposureReady      = "ExposureReady"
)

// Reason is a condition or Event reason from the frozen catalogue. Reasons
// are API surface — alerting and automation key on them — so they are
// typed, registered once, checked against a golden file by a test, and only
// ever extended. A reason that copies a child's fact (a Pod's
// Unschedulable, a Job's failure) names the fact, never "waiting".
type Reason string

// String returns the reason as metav1.Condition and Events expect it.
func (r Reason) String() string { return string(r) }

// Reasons shared by every kind.
const (
	// ReasonAsExpected is the healthy reason of every aspect condition and
	// of Ready.
	ReasonAsExpected Reason = "AsExpected"
	// ReasonInitializing is the reason of a condition the controller has
	// not evaluated yet: Unknown on first touch.
	ReasonInitializing Reason = "Initializing"
	// ReasonReconciling is Progressing=True while children converge.
	ReasonReconciling Reason = "Reconciling"
	// ReasonReconcileError is Ready=False when the last reconciliation
	// ended with an error; the message carries the bounded error text.
	ReasonReconcileError Reason = "ReconcileError"
	// ReasonReconciliationPaused is reported while the object carries the
	// reconcile=skip annotation.
	ReasonReconciliationPaused Reason = "ReconciliationPaused"
	// ReasonConfigurationInvalid is ConfigurationValid=False (and
	// Stalled=True): a cross-object check the CRD cannot express failed.
	ReasonConfigurationInvalid Reason = "ConfigurationInvalid"
	// ReasonResourceNotOwned is ConfigurationValid=False when an object the
	// operator must manage exists under the generated name and is
	// controlled by someone else.
	ReasonResourceNotOwned Reason = "ResourceNotOwned"
	// ReasonSecretNotFound names a referenced Secret that does not exist.
	ReasonSecretNotFound Reason = "SecretNotFound"
	// ReasonSecretKeyMissing names a referenced Secret key that is absent.
	ReasonSecretKeyMissing Reason = "SecretKeyMissing"
	// ReasonConfigMapNotFound names a referenced ConfigMap that does not
	// exist.
	ReasonConfigMapNotFound Reason = "ConfigMapNotFound"
	// ReasonClusterNotFound names a referenced CloudNativePG Cluster that
	// does not exist in the namespace (an EvidenceAgent's
	// spec.clusterRef, a PgAdmin's spec.clusterRefs).
	ReasonClusterNotFound Reason = "ClusterNotFound"
	// ReasonImageNotAllowed: the image a CR names (spec.image, an
	// oauth-proxy image) is outside the operator's allow-list of
	// repositories (--evidence-agent-image-repositories,
	// --pgadmin-image-repositories).
	ReasonImageNotAllowed Reason = "ImageNotAllowed"

	// Workload facts, copied from the child.
	ReasonRolloutInProgress Reason = "RolloutInProgress"
	ReasonRolloutStuck      Reason = "RolloutStuck"
	ReasonPodUnschedulable  Reason = "PodUnschedulable"
	ReasonImagePullFailed   Reason = "ImagePullFailed"
	ReasonCrashLoopBackOff  Reason = "CrashLoopBackOff"
	ReasonReplicaFailure    Reason = "ReplicaFailure"
	ReasonPodNotReady       Reason = "PodNotReady"
	ReasonScaledToZero      Reason = "ScaledToZero"

	// Exposure facts.
	ReasonNotRequested     Reason = "NotRequested"
	ReasonNotAdmitted      Reason = "NotAdmitted"
	ReasonNotAccepted      Reason = "NotAccepted"
	ReasonGatewayNotFound  Reason = "GatewayNotFound"
	ReasonHostnameConflict Reason = "HostnameConflict"
	// ReasonRouteAPIUnavailable, ReasonGatewayAPIUnavailable: the optional
	// API the exposure needs is not served or not readable (ProbeListable
	// returned NotFound/NoMatch/Forbidden at start).
	ReasonRouteAPIUnavailable   Reason = "RouteAPIUnavailable"
	ReasonGatewayAPIUnavailable Reason = "GatewayAPIUnavailable"

	// Event reasons of the classified Warning recorded when an error ends
	// a reconciliation.
	ReasonReconciliationFailed Reason = "ReconciliationFailed"
	ReasonQuotaExceeded        Reason = "QuotaExceeded"
	ReasonAdmissionDenied      Reason = "AdmissionDenied"
)

// commonReasons is the shared part of the catalogue.
var commonReasons = []Reason{
	ReasonAsExpected,
	ReasonInitializing,
	ReasonReconciling,
	ReasonReconcileError,
	ReasonReconciliationPaused,
	ReasonConfigurationInvalid,
	ReasonResourceNotOwned,
	ReasonSecretNotFound,
	ReasonSecretKeyMissing,
	ReasonConfigMapNotFound,
	ReasonClusterNotFound,
	ReasonImageNotAllowed,
	ReasonRolloutInProgress,
	ReasonRolloutStuck,
	ReasonPodUnschedulable,
	ReasonImagePullFailed,
	ReasonCrashLoopBackOff,
	ReasonReplicaFailure,
	ReasonPodNotReady,
	ReasonScaledToZero,
	ReasonNotRequested,
	ReasonNotAdmitted,
	ReasonNotAccepted,
	ReasonGatewayNotFound,
	ReasonHostnameConflict,
	ReasonRouteAPIUnavailable,
	ReasonGatewayAPIUnavailable,
	ReasonReconciliationFailed,
	ReasonQuotaExceeded,
	ReasonAdmissionDenied,
}

// CatalogueCommon is the owner name of the shared reasons in Catalogue.
const CatalogueCommon = "common"

// reasonPattern is the metav1.Condition reason format.
var reasonPattern = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

// catalogue maps every registered reason to the owner that registered it.
var catalogue = map[Reason]string{}

func init() {
	registerReasons(CatalogueCommon, commonReasons...)
}

// registerReasons adds owner's reasons to the catalogue. It is called from
// package init() only (conditions.go for the shared reasons, each kind's
// funcs file for its own), so a malformed or doubly-owned reason fails
// every test binary that imports the package, not a running operator.
func registerReasons(owner string, reasons ...Reason) {
	for _, reason := range reasons {
		if !reasonPattern.MatchString(string(reason)) || len(reason) > 1024 {
			panic(fmt.Sprintf("v1alpha1: reason %q of %s is not a valid condition reason", reason, owner))
		}
		if previous, ok := catalogue[reason]; ok && previous != owner {
			panic(fmt.Sprintf("v1alpha1: reason %q registered by both %s and %s", reason, previous, owner))
		}
		catalogue[reason] = owner
	}
}

// IsKnownReason reports whether reason is in the frozen catalogue.
func IsKnownReason(reason Reason) bool {
	_, ok := catalogue[reason]
	return ok
}

// Catalogue returns the registered reasons of owner, sorted; with an empty
// owner, every registered reason.
func Catalogue(owner string) []Reason {
	var reasons []Reason
	for reason, registeredBy := range catalogue {
		if owner == "" || owner == registeredBy {
			reasons = append(reasons, reason)
		}
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
	return reasons
}

// CatalogueOwners returns the owners that registered reasons, sorted.
func CatalogueOwners() []string {
	seen := map[string]bool{}
	var owners []string
	for _, owner := range catalogue {
		if !seen[owner] {
			seen[owner] = true
			owners = append(owners, owner)
		}
	}
	sort.Strings(owners)
	return owners
}
