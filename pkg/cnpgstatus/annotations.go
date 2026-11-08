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

package cnpgstatus

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// Annotation keys and values, identical at v1.30.0 and main
// (pkg/utils/labels_annotations.go; line numbers at v1.30.0).
const (
	// AnnotationFencedInstances: FencedInstanceAnnotation, :238. A JSON
	// array of instance names; "*" fences every instance
	// (pkg/utils/fencing.go FenceAllInstances :48, checked first at :100).
	AnnotationFencedInstances = "cnpg.io/fencedInstances"
	// AnnotationHibernation: HibernationAnnotationName, :225; values
	// "on"/"off" (:372, :376), anything else rejected by the webhook
	// (internal/webhook/v1/cluster_webhook.go validateHibernationAnnotation
	// :2830) and treated as off by the reconciler
	// (pkg/reconciler/hibernation/status.go isHibernationEnabled :136).
	AnnotationHibernation = "cnpg.io/hibernation"
	// AnnotationReconciliationLoop: ReconciliationLoopAnnotationName,
	// :167; effective when "disabled" (IsReconciliationDisabled :528).
	AnnotationReconciliationLoop = "cnpg.io/reconciliationLoop"
	// AnnotationValidation: WebhookValidationAnnotationName, :309;
	// effective when "disabled" (internal/webhook/v1/common.go
	// isValidationEnabled :44-58).
	AnnotationValidation = "cnpg.io/validation"
	// AnnotationPodPatch: PodPatchAnnotationName, :305; any non-empty
	// value is applied as a JSON patch to every instance Pod
	// (pkg/specs/pods.go :578).
	AnnotationPodPatch = "cnpg.io/podPatch"
	// AnnotationReconcilePodSpec: ReconcilePodSpecAnnotationName, :170;
	// effective when "disabled" (IsPodSpecReconciliationDisabled :533).
	AnnotationReconcilePodSpec = "cnpg.io/reconcilePodSpec"
	// AnnotationSkipWalArchiving: SkipWalArchiving, :201; effective when
	// "enabled" (IsWalArchivingDisabled :548).
	AnnotationSkipWalArchiving = "cnpg.io/skipWalArchiving" // #nosec G101 -- an annotation key, not a credential
	// AnnotationSkipEmptyWalArchiveCheck: skipEmptyWalArchiveCheck, :205;
	// effective when "enabled" (IsEmptyWalArchiveCheckEnabled :542).
	AnnotationSkipEmptyWalArchiveCheck = "cnpg.io/skipEmptyWalArchiveCheck"
	// AnnotationUnrecoverable: UnrecoverableInstanceAnnotationName, :325.
	// Read on the instance Pod, not the Cluster, as a strconv.ParseBool
	// value (internal/controller/cluster_unrecoverable.go isPodUnrecoverable
	// :98-116).
	AnnotationUnrecoverable = "alpha.cnpg.io/unrecoverable"
)

const (
	valueDisabled = "disabled"
	valueEnabled  = "enabled"
	fenceAll      = "*"
)

// Bounds on annotation-derived output. An annotation is user input of
// up to 256 KiB; the console never echoes it unbounded.
const (
	maxAnnotationValue = 256
	maxFencedInstances = 64
	maxMessage         = 1024
)

// bound cuts s to at most limit bytes on a rune boundary, and replaces
// invalid UTF-8.
func bound(s string, limit int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Fencing states.
const (
	FencingNone       = "none"
	FencingSome       = "some"
	FencingAll        = "all"
	FencingUnreadable = "unreadable"
)

// FencingReading is the fencing intent of a Cluster (operator claim).
type FencingReading struct {
	// State is none, some, all or unreadable.
	State string `json:"state"`
	// Instances are the fenced instance names, sorted and bounded; ["*"]
	// when every instance is fenced.
	Instances []string `json:"instances,omitempty"`
}

// Fencing reads cnpg.io/fencedInstances with the semantics of
// internal/console/kube/convert.go parseFencedInstances and CNPG's
// GetFencedInstances (pkg/utils/fencing.go v1.30.0:52-64): absent or
// empty is no fencing; a value that is not a JSON array of strings is
// unreadable (the operator then fails the reconcile, so the state is
// unknown, not "none"); "*" anywhere in the list fences every instance.
// A nil Cluster is unreadable.
func Fencing(c *cnpgv1.Cluster) FencingReading {
	if c == nil {
		return FencingReading{State: FencingUnreadable}
	}
	raw, ok := c.Annotations[AnnotationFencedInstances]
	if !ok || raw == "" {
		return FencingReading{State: FencingNone}
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return FencingReading{State: FencingUnreadable}
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == fenceAll {
			return FencingReading{State: FencingAll, Instances: []string{fenceAll}}
		}
		if name != "" {
			set[bound(name, maxAnnotationValue)] = struct{}{}
		}
	}
	if len(set) == 0 {
		return FencingReading{State: FencingNone}
	}
	instances := make([]string, 0, len(set))
	for name := range set {
		instances = append(instances, name)
	}
	sort.Strings(instances)
	if len(instances) > maxFencedInstances {
		instances = instances[:maxFencedInstances]
	}
	return FencingReading{State: FencingSome, Instances: instances}
}

// Hibernation states.
const (
	HibernationOff        = "off"
	HibernationInProgress = "inProgress"
	HibernationHibernated = "hibernated"
	HibernationUnreadable = "unreadable"
)

// HibernationReading combines the hibernation intent (annotation) with
// the operator's report (condition cnpg.io/hibernation).
type HibernationReading struct {
	// State is off, inProgress, hibernated or unreadable.
	State string `json:"state"`
	// Reason and Message come from the condition when there is one, or
	// name the console's inference ("Requested", "Resuming",
	// "InvalidAnnotationValue") when there is not.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Hibernation reads the cnpg.io/hibernation annotation and condition
// (pkg/reconciler/hibernation/status.go v1.30.0, same at main). The
// operator keeps the condition only while the annotation is
// "on" (EnrichStatus :83-134): True/Hibernated once no Pod is left,
// False/WaitingForHealthy, DeletingPods or WaitingPodsDeletion on the
// way. WrongAnnotationValue is declared (:49) but never set at v1.30.0.
// A nil Cluster is unreadable.
func Hibernation(c *cnpgv1.Cluster) HibernationReading {
	if c == nil {
		return HibernationReading{State: HibernationUnreadable}
	}
	value, annotated := c.Annotations[AnnotationHibernation]
	condition := meta.FindStatusCondition(c.Status.Conditions, ConditionHibernation)
	fromCondition := func(state string) HibernationReading {
		return HibernationReading{
			State:   state,
			Reason:  bound(condition.Reason, maxAnnotationValue),
			Message: bound(condition.Message, maxMessage),
		}
	}
	switch {
	case annotated && value == "on":
		if condition == nil {
			return HibernationReading{
				State:   HibernationInProgress,
				Reason:  "Requested",
				Message: "hibernation is requested; the operator has not reported progress yet",
			}
		}
		if condition.Status == "True" {
			return fromCondition(HibernationHibernated)
		}
		if condition.Reason == "WrongAnnotationValue" {
			return fromCondition(HibernationUnreadable)
		}
		return fromCondition(HibernationInProgress)
	case !annotated || value == "off":
		if condition != nil {
			// The operator removes the condition on its next status pass
			// once the annotation is no longer "on": until then the
			// cluster is coming back.
			return HibernationReading{
				State:   HibernationInProgress,
				Reason:  "Resuming",
				Message: "hibernation is no longer requested; the operator still reports " + bound(condition.Reason, maxAnnotationValue),
			}
		}
		return HibernationReading{State: HibernationOff}
	default:
		return HibernationReading{
			State:   HibernationUnreadable,
			Reason:  "InvalidAnnotationValue",
			Message: "cnpg.io/hibernation is " + quoteBounded(value) + `; the operator acts only on "on" and treats any other value as off`,
		}
	}
}

func quoteBounded(s string) string {
	b, err := json.Marshal(bound(s, maxAnnotationValue))
	if err != nil {
		return `""`
	}
	return string(b)
}

// EscapeHatch is an annotation that makes the operator skip something it
// would otherwise do: each one explains "why nothing happens".
type EscapeHatch struct {
	Annotation string `json:"annotation"`
	// Value is the annotation value, bounded to 256 bytes.
	Value string `json:"value"`
	// Effect says what the operator does not do while it is set.
	Effect string `json:"effect"`
	Class  Class  `json:"class"`
}

// escapeHatchRule decides whether one annotation is active.
type escapeHatchRule struct {
	annotation string
	active     func(value string) bool
	effect     string
	class      Class
}

// clusterEscapeHatches is in rendering order (most consequential first);
// keys and values verified at v1.30.0 and main
// (pkg/utils/labels_annotations.go).
var clusterEscapeHatches = []escapeHatchRule{
	// No reconciliation means no failover and no self-healing.
	{
		annotation: AnnotationReconciliationLoop,
		active:     func(v string) bool { return v == valueDisabled },
		effect:     "the operator does not reconcile this cluster: no failover, no switchover, no self-healing",
		class:      ClassCritical,
	},
	// No WAL archiving means no point-in-time recovery past the last base backup.
	{
		annotation: AnnotationSkipWalArchiving,
		active:     func(v string) bool { return v == valueEnabled },
		effect:     "PostgreSQL does not archive WAL: no point-in-time recovery beyond the last backup",
		class:      ClassCritical,
	},
	{
		annotation: AnnotationValidation,
		active:     func(v string) bool { return v == valueDisabled },
		effect:     "the admission webhook does not validate changes to this cluster",
		class:      ClassWarning,
	},
	{
		annotation: AnnotationReconcilePodSpec,
		active:     func(v string) bool { return v == valueDisabled },
		effect:     "instance Pods are not rolled when the desired Pod spec changes",
		class:      ClassWarning,
	},
	{
		annotation: AnnotationPodPatch,
		active:     func(v string) bool { return v != "" },
		effect:     "a JSON patch is applied to every instance Pod the operator creates",
		class:      ClassWarning,
	},
	{
		annotation: AnnotationSkipEmptyWalArchiveCheck,
		active:     func(v string) bool { return v == valueEnabled },
		effect:     "bootstrap does not check that the WAL archive is empty before writing to it",
		class:      ClassInfo,
	},
}

// EscapeHatches lists the active escape-hatch annotations of a Cluster,
// in a fixed order, values bounded to 256 bytes. A nil Cluster has
// none. alpha.cnpg.io/unrecoverable lives on Pods: see PodEscapeHatches.
func EscapeHatches(c *cnpgv1.Cluster) []EscapeHatch {
	if c == nil {
		return nil
	}
	var hatches []EscapeHatch
	for _, rule := range clusterEscapeHatches {
		value, ok := c.Annotations[rule.annotation]
		if !ok || !rule.active(value) {
			continue
		}
		hatches = append(hatches, EscapeHatch{
			Annotation: rule.annotation,
			Value:      bound(value, maxAnnotationValue),
			Effect:     rule.effect,
			Class:      rule.class,
		})
	}
	return hatches
}

// PodEscapeHatch is an escape hatch set on one instance Pod.
type PodEscapeHatch struct {
	Pod string `json:"pod"`
	EscapeHatch
}

// PodEscapeHatches lists the instance Pods carrying
// alpha.cnpg.io/unrecoverable with a true value (strconv.ParseBool
// semantics, as isPodUnrecoverable reads it): the operator deletes that
// instance together with its PVCs and recreates it. Sorted by Pod name;
// nil Pods are skipped.
func PodEscapeHatches(pods []*corev1.Pod) []PodEscapeHatch {
	var hatches []PodEscapeHatch
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		value, ok := pod.Annotations[AnnotationUnrecoverable]
		if !ok || !parseBoolLikeStrconv(value) {
			continue
		}
		hatches = append(hatches, PodEscapeHatch{
			Pod: bound(pod.Name, maxAnnotationValue),
			EscapeHatch: EscapeHatch{
				Annotation: AnnotationUnrecoverable,
				Value:      bound(value, maxAnnotationValue),
				Effect:     "the operator deletes this instance and its PVCs, then recreates it from the others",
				Class:      ClassCritical,
			},
		})
	}
	sort.Slice(hatches, func(i, j int) bool { return hatches[i].Pod < hatches[j].Pod })
	return hatches
}

// parseBoolLikeStrconv is strconv.ParseBool's true set.
func parseBoolLikeStrconv(s string) bool {
	switch s {
	case "1", "t", "T", "TRUE", "true", "True":
		return true
	default:
		return false
	}
}
