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

// Package evidenceagent is the EvidenceAgent controller of the operator: it
// deploys one read-only evidence agent per CR in the CR's namespace, with
// the object-store credentials the barman-cloud plugin uses (referenced, never read), a
// serving certificate whose CA it publishes in status.caBundle, and the
// shared ClusterRoleBinding that lets each agent create TokenReviews and
// SubjectAccessReviews.
package evidenceagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/backupresolve"
	"github.com/fyannk/pgToolBox/internal/operator/conditions"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
	"github.com/fyannk/pgToolBox/pkg/discovery"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=evidenceagents,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=evidenceagents/status,verbs=get;update;patch
// Controller owner references set blockOwnerDeletion, which an API server
// enforcing OwnerReferencesPermissionEnforcement (OpenShift) allows only to
// whoever may update the owner's finalizers.
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=evidenceagents/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=update;patch,resourceNames=pgtoolbox-evidence-agent
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=barmancloud.cnpg.io,resources=objectstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgbackrest.dalibo.com,resources=stanzas,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgbackrest.cnpg.opera.com,resources=archives,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconciler reconciles EvidenceAgents.
type Reconciler struct {
	// Client reads the label-filtered caches and writes.
	Client client.Client
	// APIReader reads what is never cached: Secrets and ConfigMaps (content
	// or metadata), Clusters, ObjectStores, Pods.
	APIReader client.Reader
	Recorder  events.EventRecorder
	// DefaultImage is the agent image when spec.image is omitted.
	DefaultImage string
	// AllowedImageRepositories are the repositories spec.image may name
	// (exact, or a prefix ending in "/"). Empty: the default image's
	// repository only. Whoever may create an EvidenceAgent chooses which
	// image receives the namespace's object-store credentials, so the
	// choice is bounded here, by the operator's installer.
	AllowedImageRepositories []string
	// ObjectStoreAPI is the start-time probe of barmancloud.cnpg.io
	// (discovery.ProbeObjectStoreAPI); the zero value is "unavailable".
	ObjectStoreAPI discovery.Result
	// StanzaAPI and ArchiveAPI are the start-time probes of the two
	// pgBackRest plugins' kinds (discovery.ProbeStanzaAPI,
	// ProbeArchiveAPI), with the same zero value.
	StanzaAPI  discovery.Result
	ArchiveAPI discovery.Result
	// Now is injectable for certificate tests.
	Now func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile publishes status on every exit path: whatever the inner loop
// returned, publish derives Ready from the aspect conditions in the same
// write and emits one Event per transition.
func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	agent := &pgtoolboxv1alpha1.EvidenceAgent{}
	if getErr := r.Client.Get(ctx, request.NamespacedName, agent); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			// A deleted agent leaves the shared binding here: its children
			// go with the owner references, and nothing else holds it.
			return ctrl.Result{}, r.reconcileClusterRBAC(ctx)
		}
		return ctrl.Result{}, getErr
	}
	if !agent.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	original := agent.DeepCopy()
	initializeConditions(agent)

	if agent.Annotations[pgtoolboxv1alpha1.AnnotationReconcile] == pgtoolboxv1alpha1.ReconcileSkip {
		return ctrl.Result{}, r.publish(ctx, agent, original, conditions.Outcome{Paused: true})
	}
	result, err := r.reconcile(ctx, agent)
	return result, r.publish(ctx, agent, original, conditions.Outcome{Err: err, Converged: err == nil})
}

func (r *Reconciler) reconcile(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent) (ctrl.Result, error) {
	resolved, whyNot, err := r.resolve(ctx, agent)
	if err != nil {
		return ctrl.Result{}, err
	}
	image := resolveImage(agent.Spec.Image, r.DefaultImage)
	switch {
	case whyNot != nil:
	case image.Reference == "":
		whyNot = problem(pgtoolboxv1alpha1.ReasonImageRequired, "spec.image is not set and the operator has no default evidence agent image (--default-evidence-agent-image)")
	case agent.Spec.Image != nil && !specs.ImageAllowed(agent.Spec.Image.Repository, r.AllowedImageRepositories, r.DefaultImage):
		whyNot = problem(pgtoolboxv1alpha1.ReasonImageNotAllowed,
			"spec.image repository %s is not in the operator's allow-list (--evidence-agent-image-repositories)", agent.Spec.Image.Repository)
	}
	if whyNot != nil {
		// Nothing is rendered for an invalid configuration: stalled until
		// the agent or the object it names changes (both are watched).
		setCondition(agent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, whyNot.reason, whyNot.message)
		return ctrl.Result{}, reconcile.Stall(whyNot.reason, "%s", whyNot.message)
	}
	setCondition(agent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, resolvedMessage(resolved))
	agent.Status.ObjectStoreName = resolved.ObjectStoreName
	agent.Status.ServerName = resolved.ServerName
	if resolved.Format == backupresolve.FormatPgBackRest {
		agent.Status.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestStatus{
			Plugin: resolved.PgBackRestPlugin, Kind: resolved.PgBackRestKind, ObjectName: resolved.PgBackRestObject,
			Stanza: resolved.Stanza, ArchiveStanza: resolved.ArchiveStanza, Repository: int32(resolved.Repository), // #nosec G115 -- at most 256
		}
	}
	agent.Status.Fingerprint = resolved.Fingerprint
	agent.Status.Image = image.Reference

	servingSecret := agent.EffectiveServingSecretName()
	for _, object := range []client.Object{buildServiceAccount(agent), buildService(agent, servingSecret)} {
		if err := r.apply(ctx, agent, object); err != nil {
			return r.notOwned(agent, err)
		}
	}
	agent.Status.Endpoint = evidencev1alpha1.Endpoint(agent.WorkloadName(), agent.Namespace)
	// After the ServiceAccount: only an account this agent controls is
	// ever bound (reconcileClusterRBAC checks the controller reference).
	if err := r.reconcileClusterRBAC(ctx); err != nil {
		return ctrl.Result{}, err
	}

	certificate, err := r.reconcileCertificate(ctx, agent)
	if err != nil {
		return r.notOwned(agent, err)
	}

	deployment := buildDeployment(renderInputs{Agent: agent, Image: image, Composition: *resolved, ServingSecretName: servingSecret})
	if err := r.apply(ctx, agent, deployment); err != nil {
		return r.notOwned(agent, err)
	}
	if err := r.observeWorkload(ctx, agent, deployment, resolved); err != nil {
		return ctrl.Result{}, err
	}

	result := ctrl.Result{}
	if !certificate.renewAt.IsZero() {
		// A timer for the next certificate renewal, not a poll: nothing
		// else will change before then.
		result.RequeueAfter = max(time.Second, certificate.renewAt.Sub(r.now()))
	}
	return result, nil
}

// resolvedMessage is ConfigurationValid's message once resolved: names and
// numbers, never a value read from a Secret.
func resolvedMessage(resolved *composition) string {
	if resolved.Format != backupresolve.FormatPgBackRest {
		return fmt.Sprintf("Cluster %s, %s, server %s", resolved.ClusterName, resolved.Origin, resolved.ServerName)
	}
	message := fmt.Sprintf("Cluster %s, %s, repo%d, stanza %s", resolved.ClusterName, resolved.Origin, resolved.Repository, resolved.Stanza)
	if resolved.ArchiveStanza != "" {
		message += ", WAL under stanza " + resolved.ArchiveStanza
	}
	return message
}

// notOwned turns a foreign object under a generated name into the
// stalled ConfigurationValid=False it is; any other error is returned.
func (r *Reconciler) notOwned(agent *pgtoolboxv1alpha1.EvidenceAgent, err error) (ctrl.Result, error) {
	var foreign *reconcile.NotOwnedError
	if errors.As(err, &foreign) {
		setCondition(agent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonResourceNotOwned, foreign.Error())
		return ctrl.Result{}, reconcile.Stall(pgtoolboxv1alpha1.ReasonResourceNotOwned, "%s", foreign.Error())
	}
	return ctrl.Result{}, err
}

// apply checks control, then server-side-applies desired with
// ForceOwnership. On success desired holds the live object (status
// included).
func (r *Reconciler) apply(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent, desired client.Object) error {
	if err := r.checkControl(ctx, agent, desired); err != nil {
		return err
	}
	data, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("encode %T apply: %w", desired, err)
	}
	if err := r.Client.Patch(ctx, desired, client.RawPatch(types.ApplyPatchType, data), client.FieldOwner(reconcile.FieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s: %w", desired.GetObjectKind().GroupVersionKind().Kind, desired.GetName(), err)
	}
	return nil
}

// checkControl refuses to apply over an object of the generated name that
// someone else controls. The cache answers for the operator's own labelled
// objects; a cache miss is confirmed with a metadata-only read, because a
// foreign object is invisible to a label-filtered cache.
func (r *Reconciler) checkControl(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent, desired client.Object) error {
	gvk := desired.GetObjectKind().GroupVersionKind()
	key := client.ObjectKeyFromObject(desired)
	var existing metav1.Object
	cached, ok := desired.DeepCopyObject().(client.Object)
	if ok && r.Client.Get(ctx, key, cached) == nil {
		existing = cached
	} else {
		partial := &metav1.PartialObjectMetadata{}
		partial.SetGroupVersionKind(gvk)
		if err := r.APIReader.Get(ctx, key, partial); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("check control of %s %s: %w", gvk.Kind, key.Name, err)
		}
		existing = partial
	}
	if desired.GetNamespace() == "" {
		if existing.GetLabels()[pgtoolboxv1alpha1.LabelManagedBy] != pgtoolboxv1alpha1.ManagedByValue {
			return &reconcile.NotOwnedError{Kind: gvk.Kind, Name: key.Name}
		}
		return nil
	}
	if owner := metav1.GetControllerOfNoCopy(existing); owner == nil || owner.UID != agent.UID {
		return &reconcile.NotOwnedError{Kind: gvk.Kind, Namespace: key.Namespace, Name: key.Name}
	}
	return nil
}

// reconcileClusterRBAC applies the agents' ClusterRole and the binding of
// every live agent's ServiceAccount to it, recomputed from the cache.
func (r *Reconciler) reconcileClusterRBAC(ctx context.Context) error {
	var agents pgtoolboxv1alpha1.EvidenceAgentList
	if err := r.Client.List(ctx, &agents); err != nil {
		return fmt.Errorf("list EvidenceAgents: %w", err)
	}
	// Bind only ServiceAccounts their EvidenceAgent controls: an agent
	// whose name collides with someone else's ServiceAccount (it stalls
	// with ResourceNotOwned) must not grant that account the reviews.
	var accounts corev1.ServiceAccountList
	if err := r.Client.List(ctx, &accounts, client.HasLabels{pgtoolboxv1alpha1.LabelEvidenceAgent}); err != nil {
		return fmt.Errorf("list agent ServiceAccounts: %w", err)
	}
	controlled := map[types.UID]string{}
	for i := range accounts.Items {
		if owner := metav1.GetControllerOfNoCopy(&accounts.Items[i]); owner != nil && owner.Kind == "EvidenceAgent" {
			controlled[owner.UID] = accounts.Items[i].Namespace + "/" + accounts.Items[i].Name
		}
	}
	subjects := make([]agentSubject, 0, len(agents.Items))
	for i := range agents.Items {
		agent := &agents.Items[i]
		if agent.DeletionTimestamp.IsZero() && controlled[agent.UID] == agent.Namespace+"/"+agent.WorkloadName() {
			subjects = append(subjects, agentSubject{Namespace: agent.Namespace, Name: agent.WorkloadName()})
		}
	}
	for _, object := range []client.Object{buildClusterRole(), buildClusterRoleBinding(subjects)} {
		if err := r.apply(ctx, nil, object); err != nil {
			return err
		}
	}
	return nil
}

// observeWorkload sets WorkloadReady from the live Deployment (and, when it
// is not ready, the facts of its Pod), and publishes producerEpoch once the
// rollout of an epoch completed.
func (r *Reconciler) observeWorkload(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent, deployment *appsv1.Deployment, resolved *composition) error {
	status := deployment.Status
	complete := status.ObservedGeneration >= deployment.Generation &&
		status.UpdatedReplicas == status.Replicas && status.AvailableReplicas >= 1 && status.Replicas >= 1
	if complete {
		if epoch, err := strconv.ParseInt(deployment.Spec.Template.Annotations[AnnotationProducerEpoch], 10, 64); err == nil {
			agent.Status.ProducerEpoch = epoch
		}
		setCondition(agent, pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
			fmt.Sprintf("Deployment %s is available at epoch %d", deployment.Name, agent.Status.ProducerEpoch))
		return nil
	}

	reason, message := pgtoolboxv1alpha1.ReasonRolloutInProgress, fmt.Sprintf("Deployment %s: %d of %d replicas updated and available", deployment.Name, status.AvailableReplicas, max(status.Replicas, 1))
	for _, condition := range status.Conditions {
		switch {
		case condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue:
			reason, message = pgtoolboxv1alpha1.ReasonReplicaFailure, condition.Message
		case condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse:
			reason, message = pgtoolboxv1alpha1.ReasonRolloutStuck, condition.Message
		}
	}
	missing, err := r.missingSecrets(ctx, agent, resolved)
	if err != nil {
		return err
	}
	if missing != "" {
		reason, message = pgtoolboxv1alpha1.ReasonSecretNotFound, fmt.Sprintf("Secret %s referenced by %s does not exist", missing, resolved.Origin)
	} else if podReason, podMessage, err := r.podFacts(ctx, agent); err != nil {
		return err
	} else if podReason != "" {
		reason, message = podReason, podMessage
	}
	setCondition(agent, pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse, reason, message)
	return nil
}

// missingSecrets returns the first referenced Secret that does not exist,
// from a metadata-only read: the operator never reads their content.
func (r *Reconciler) missingSecrets(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent, resolved *composition) (string, error) {
	for _, name := range resolved.ReferencedSecrets() {
		secret := &metav1.PartialObjectMetadata{}
		secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return name, nil
			}
			return "", fmt.Errorf("check Secret %s: %w", name, err)
		}
	}
	return "", nil
}

// podFacts copies the agent Pod's blocking fact into a reason: the
// scheduler's Unschedulable, an image pull failure, a crash loop. The Pods
// are read from the cache, whose Pod informer the controller watches.
func (r *Reconciler) podFacts(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent) (pgtoolboxv1alpha1.Reason, string, error) {
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(agent.Namespace), client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(selectorLabels(agent))}); err != nil {
		return "", "", fmt.Errorf("list agent Pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
				return pgtoolboxv1alpha1.ReasonPodUnschedulable, fmt.Sprintf("Pod %s: %s", pod.Name, condition.Message), nil
			}
		}
		for _, container := range pod.Status.ContainerStatuses {
			if waiting := container.State.Waiting; waiting != nil {
				switch waiting.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
					return pgtoolboxv1alpha1.ReasonImagePullFailed, fmt.Sprintf("Pod %s: %s: %s", pod.Name, waiting.Reason, waiting.Message), nil
				case "CrashLoopBackOff":
					return pgtoolboxv1alpha1.ReasonCrashLoopBackOff, fmt.Sprintf("Pod %s: container %s is crash-looping (restarts %d)", pod.Name, container.Name, container.RestartCount), nil
				case "CreateContainerConfigError":
					return pgtoolboxv1alpha1.ReasonSecretKeyMissing, fmt.Sprintf("Pod %s: %s", pod.Name, waiting.Message), nil
				}
			}
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status != corev1.ConditionTrue && pod.Status.Phase == corev1.PodRunning {
				return pgtoolboxv1alpha1.ReasonPodNotReady, fmt.Sprintf("Pod %s is running and not ready", pod.Name), nil
			}
		}
	}
	return "", "", nil
}
