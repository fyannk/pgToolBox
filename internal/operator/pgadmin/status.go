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

package pgadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/conditions"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// quietReasons are PgAdmin's False reasons of a workload on its way (a
// backup before an upgrade): reported in status, not as Warnings.
var quietReasons = []pgtoolboxv1alpha1.Reason{pgtoolboxv1alpha1.ReasonUpgradeBackupRunning, pgtoolboxv1alpha1.ReasonConfigDatabaseNotReady}

// publish is the one status write of a loop, on every exit path: the
// shared condition model (internal/operator/conditions.Summarize) decides
// Stalled, Progressing and Ready from the aspects in the same write, the
// write is skipped when nothing changed (a no-op reconcile writes
// nothing), and Events are emitted once per transition after the write.
// A real error is returned for controller-runtime's backoff; ErrNextLoop
// and a stall are not errors.
func (r *Reconciler) publish(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, before *pgtoolboxv1alpha1.PgAdminStatus, converged bool, loopErr error) error {
	reported := conditions.Summarize(&p.Status.Conditions, p.Generation, conditions.Outcome{
		Err: loopErr, Paused: p.IsPaused(), Converged: converged,
	}, aspects...)
	p.Status.ObservedGeneration = p.Generation

	if !equality.Semantic.DeepEqual(*before, p.Status) {
		if err := r.writeStatus(ctx, p, before); err != nil {
			return errors.Join(reported, err)
		}
		conditions.RecordTransitions(r.Recorder, p, before.Conditions, p.Status.Conditions, reported, quietReasons...)
	}
	if reported != nil {
		logf.FromContext(ctx).Error(reported, "reconcile failed")
	}
	return reported
}

// writeStatus patches the status subresource with optimistic locking and
// retries a conflict on the latest object, carrying the computed status.
func (r *Reconciler) writeStatus(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, before *pgtoolboxv1alpha1.PgAdminStatus) error {
	desired := *p.Status.DeepCopy()
	original := p.DeepCopy()
	original.Status = *before.DeepCopy()
	first := true
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if !first {
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(p), original); err != nil {
				return err
			}
		}
		first = false
		updated := original.DeepCopy()
		updated.Status = desired
		if err := r.Client.Status().Patch(ctx, updated, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}),
			client.FieldOwner(reconcile.StatusFieldOwner)); err != nil {
			return err
		}
		p.ResourceVersion = updated.ResourceVersion
		return nil
	})
}

// workloadFact reads WorkloadReady from the Deployment's Pods, copying the
// child's fact into the reason (a waiting status says why). Every
// desired replica must run the current template and be Ready (with a
// configDatabase there can be several); a Pod that cannot start is named
// whichever it is, and the observed Deployment's ReplicaFailure (a quota
// refusing Pods) explains a missing one.
func workloadFact(desired, observed *appsv1.Deployment, pods *corev1.PodList) exposureFact {
	hash := specs.HashOf(desired)
	want := int32(1)
	if desired.Spec.Replicas != nil {
		want = *desired.Spec.Replicas
	}
	var current []*corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && pod.Annotations[pgtoolboxv1alpha1.AnnotationSpecHash] == hash {
			current = append(current, pod)
		}
	}
	ready := 0
	var notReady *corev1.Pod
	for _, pod := range current {
		if fact, blocked := podBlocked(pod); blocked {
			return fact
		}
		if podReady(pod) {
			ready++
		} else if notReady == nil {
			notReady = pod
		}
	}
	if failure := replicaFailure(observed); failure != "" && int32(ready) < want {
		return exposureFact{Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonReplicaFailure, Message: failure}
	}
	switch {
	case int32(ready) >= want && want > 0:
		if want == 1 {
			return exposureFact{Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonAsExpected, Message: "Pod " + firstReady(current).Name + " is Ready"}
		}
		return exposureFact{Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonAsExpected, Message: fmt.Sprintf("%d of %d pgAdmin Pods Ready", ready, want)}
	case len(current) == 0:
		return exposureFact{Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonRolloutInProgress, Message: "no pgAdmin Pod runs the current template yet"}
	case notReady != nil:
		return exposureFact{
			Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonPodNotReady,
			Message: fmt.Sprintf("Pod %s is not Ready yet (%d of %d Ready)", notReady.Name, ready, want),
		}
	}
	return exposureFact{
		Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonRolloutInProgress,
		Message: fmt.Sprintf("%d of %d pgAdmin Pods run the current template", len(current), want),
	}
}

// podBlocked is a Pod's fact that it cannot start: unschedulable, an image
// that cannot be pulled, a container that keeps failing.
func podBlocked(pod *corev1.Pod) (exposureFact, bool) {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
			return exposureFact{Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonPodUnschedulable, Message: "Pod " + pod.Name + ": " + condition.Message}, true
		}
	}
	for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		if waiting := status.State.Waiting; waiting != nil {
			switch waiting.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				return exposureFact{
					Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonImagePullFailed,
					Message: fmt.Sprintf("Pod %s: container %s: %s: %s", pod.Name, status.Name, waiting.Reason, waiting.Message),
				}, true
			case "CrashLoopBackOff", "CreateContainerConfigError", "CreateContainerError":
				return exposureFact{
					Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonCrashLoopBackOff,
					Message: fmt.Sprintf("Pod %s: container %s: %s: %s", pod.Name, status.Name, waiting.Reason, lastTermination(status)),
				}, true
			}
		}
	}
	return exposureFact{}, false
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func firstReady(pods []*corev1.Pod) *corev1.Pod {
	for _, pod := range pods {
		if podReady(pod) {
			return pod
		}
	}
	return pods[0]
}

// replicaFailure is the Deployment's ReplicaFailure message, if any.
func replicaFailure(d *appsv1.Deployment) string {
	if d == nil {
		return ""
	}
	for _, condition := range d.Status.Conditions {
		if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
			return "Deployment " + d.Name + ": " + condition.Message
		}
	}
	return ""
}

func lastTermination(status corev1.ContainerStatus) string {
	if t := status.LastTerminationState.Terminated; t != nil {
		return fmt.Sprintf("last exit %d (%s)", t.ExitCode, t.Reason)
	}
	return "starting"
}

// jobOutcome reads a Job's terminal conditions.
func jobOutcome(job *batchv1.Job) (finished, failed bool, message string) {
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			return true, false, ""
		case batchv1.JobFailed:
			return true, true, strings.TrimSpace(condition.Reason + ": " + condition.Message)
		default:
			// Suspended, FailureTarget and SuccessCriteriaMet precede the
			// terminal Complete or Failed condition: not an outcome yet.
		}
	}
	return false, false, ""
}

// registrationReport is what the registration Job writes as its
// termination message.
type registrationReport struct {
	PgAdminVersion string `json:"pgAdminVersion"`
	Servers        int    `json:"servers"`
}

// parseReport reads the registration Job's termination message, which the
// Pod writes and anyone who can exec into it could forge: only a short
// version made of [0-9A-Za-z.+_-] reaches status.pgAdminVersion, and a
// negative server count reads as zero.
func parseReport(message string) registrationReport {
	var report registrationReport
	if err := json.Unmarshal([]byte(message), &report); err != nil {
		return registrationReport{}
	}
	if !validVersion(report.PgAdminVersion) {
		report.PgAdminVersion = ""
	}
	report.Servers = max(report.Servers, 0)
	return report
}

func validVersion(version string) bool {
	if len(version) > 32 {
		return false
	}
	for _, r := range version {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '.', r == '+', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
