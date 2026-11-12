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

// Package console is the PgToolBoxConsole controller of the operator: it lets a
// platform team hand out namespaced consoles declaratively, while the
// pgtools chart stays the primary way to install one. It runs
// one console per PgToolBoxConsole, in the CR's namespace, watching that namespace
// only. It renders what the chart renders for a console release and keeps
// it converged; it seeds the lifecycle ConfigMap with the operator's
// end-of-life table and keeps it current until someone edits it.
//
// The console's authority stays what the chart grants: read-only in its
// namespace (a RoleBinding to the shipped pgtoolbox-console ClusterRole,
// which the operator may bind but not write), SubjectAccessReview creation,
// get/watch on its own revocation list, get/watch/update on its own state
// ConfigMap and, with digest delivery, get/update on its digest Lease and
// create on leases.
//
// With persistent history (the default) the console is a StatefulSet with
// one history volume per replica; without, a Deployment. Switching from one
// to the other, the operator deletes the old workload once the new one has
// a replica available, so the console stays reachable.
package console

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/conditions"
	"github.com/fyannk/pgToolBox/internal/operator/exposure"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// RBAC of the PgToolBoxConsole controller. The RoleBindings it writes reference
// ClusterRoles it may bind (the bind verb, by name) and does not hold the
// rules of: RBAC escalation prevention then bounds what any console is
// granted to those two shipped roles. The revocations Role grants get and
// watch on one ConfigMap, the state Role get, watch and update on another,
// the digest Lease Role get and update on one Lease and create on leases:
// all verbs the operator holds (it creates that Lease itself, owned by the
// PgToolBoxConsole; it never reads, lists or watches the others). A console needs
// no cluster-scoped grant: it asks its access questions with the user's own
// token. The Nodes are listed (uncached, bounded) for their podCIDRs only,
// against which an ipBlock peer of the exposure's NetworkPolicy is
// checked; without that grant the check is skipped and said so. The
// Clusters and their server CA Secrets are read
// for the instance CA mirror (instanceca.go): only ca.crt is copied, into
// a ConfigMap the console mounts.
//
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxconsoles,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxconsoles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxconsoles/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=list
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=bind,resourceNames=pgtoolbox-console;pgtoolbox-console-pod-logs;pgtoolbox-console-restore-drills
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes/custom-host,verbs=create;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch

// Features are the optional APIs, probed once at start. Usable() on a
// field means "watched somewhere" (the watches and the cache); a reconcile
// asks In(namespace), because each namespace's RBAC decides for that
// namespace alone.
type Features struct {
	Routes, Gateways discovery.Result
	// OpenShift is true when the platform assigns UIDs and fsGroups.
	OpenShift bool
}

// In is the features in one namespace (discovery.Result.In).
func (f Features) In(namespace string) Features {
	return Features{Routes: f.Routes.In(namespace), Gateways: f.Gateways.In(namespace), OpenShift: f.OpenShift}
}

// Reconciler reconciles PgToolBoxConsoles.
type Reconciler struct {
	// Client reads the label-filtered caches and writes.
	Client client.Client
	// APIReader reads what is never cached: ConfigMaps, Secret metadata,
	// the shipped ClusterRoles, Pods.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  conditions.EventRecorder
	// DefaultImage is the console image when spec.image is omitted.
	DefaultImage string
	// AllowedImageRepositories are the repositories spec.image may name
	// (exact, or a prefix ending in "/"). Empty: the default image's
	// repository only.
	AllowedImageRepositories []string
	// AllowedHostSuffixes are the DNS suffixes spec.exposure.hostname must
	// fall under (EXPOSURE_ALLOWED_HOST_SUFFIXES). Empty: any hostname.
	AllowedHostSuffixes []string
	Features            Features
	// Random fills the generated session key; crypto/rand by default.
	Random func(max *big.Int) (*big.Int, error)
	// Now is the clock of the Node podCIDR memo; time.Now by default.
	Now func() time.Time

	nodes nodeCIDRMemo
}

func (r *Reconciler) applier() reconcile.Applier {
	return reconcile.Applier{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme}
}

// Reconcile publishes status on every exit path.
func (r *Reconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	console := &pgtoolboxv1alpha1.PgToolBoxConsole{}
	if err := r.Client.Get(ctx, request.NamespacedName, console); err != nil {
		if apierrors.IsNotFound(err) {
			// Its children go with the owner references.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !console.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	original := console.DeepCopy()
	initializeConditions(console)
	if console.Annotations[pgtoolboxv1alpha1.AnnotationReconcile] == pgtoolboxv1alpha1.ReconcileSkip {
		// Paused, nothing is applied or observed, with one exception: a
		// hostname outside the allow-list is withdrawn all the same (a
		// pause must not keep a refused name published).
		_, imageProblem := r.imageProblem(console)
		err := r.checkHostname(ctx, console, imageProblem)
		return ctrl.Result{}, r.publish(ctx, console, original, conditions.Outcome{Err: err, Paused: true})
	}
	loop := &loopState{}
	err := r.reconcile(ctx, console, loop)
	result := ctrl.Result{RequeueAfter: loop.requeueAfter}
	var stalled *reconcile.StallError
	if errors.As(err, &stalled) {
		// A stall on something the caches do not hold: look again later
		// rather than wait for a watch.
		result.RequeueAfter = stallRetries[stalled.Reason]
	}
	return resultFor(result, r.publish(ctx, console, original, conditions.Outcome{Err: err, Converged: err == nil}))
}

// resultFor is what Reconcile returns: with an error, the error alone
// (controller-runtime backs off, and warns of a result returned beside an
// error); else the result.
func resultFor(result ctrl.Result, err error) (ctrl.Result, error) {
	if err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

// loopState is what one reconcile asks of the next beyond its status: a
// look again after a while, for what no watch reports.
type loopState struct {
	requeueAfter time.Duration
}

// after asks for a look again within d (the sooner of the asks wins).
func (l *loopState) after(d time.Duration) {
	if l.requeueAfter == 0 || d < l.requeueAfter {
		l.requeueAfter = d
	}
}

// policyRecheck is how often a console whose trust depends on the
// namespace's other NetworkPolicies (it trusts every peer of its own, or
// another one admits more) looks at them again: the operator caches only
// its own (every informer is label-filtered), so a policy someone adds, or
// removes, is seen by this look, not by a watch.
const policyRecheck = 5 * time.Minute

// networkPolicyListLimit bounds the uncached list of the namespace's
// NetworkPolicies; more than that, and the trust is not granted.
const networkPolicyListLimit = 250

// podListLimit bounds the uncached lists of a console's Pods (far more
// than any console runs: a list that returns more, or a continue token,
// counts as Pods that could not be checked, which grants no trust and
// withdraws no policy).
const podListLimit = networkPolicyListLimit

// roleMissingRetry is how often a console stalled on a missing shipped
// ClusterRole looks for it again.
const roleMissingRetry = 2 * time.Minute

// stallRetries are the stalls looked at again, and how often: a shipped
// ClusterRole (the installer's, not in the operator's cache) and a user's
// ConfigMap (the ConfigMap cache is label-filtered), and an ingress
// NetworkPolicy the API server refused. Every other stall
// waits for a watch.
var stallRetries = map[pgtoolboxv1alpha1.Reason]time.Duration{
	pgtoolboxv1alpha1.ReasonConsoleRoleMissing: roleMissingRetry,
	pgtoolboxv1alpha1.ReasonConfigMapNotFound:  configMapMissingRetry,
	// An admission policy, not only the schema, may be what refused it.
	pgtoolboxv1alpha1.ReasonExposurePolicyInvalid: policyRecheck,
}

//nolint:gocyclo,cyclop,funlen // the console's children, in dependency order.
func (r *Reconciler) reconcile(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, loop *loopState) error {
	image := specs.ResolveImage(console.Spec.Image, r.DefaultImage)
	imageReason, imageProblem := r.imageProblem(console)
	if err := r.checkHostname(ctx, console, imageProblem); err != nil {
		return err
	}
	if imageProblem != "" {
		return r.invalid(console, imageReason, imageProblem)
	}
	roles := []string{ConsoleClusterRole}
	if console.PodLogsAllowed() {
		roles = append(roles, ConsolePodLogsClusterRole)
	}
	if console.RestoreDrillsEnabled() {
		roles = append(roles, ConsoleRestoreDrillsClusterRole)
	}
	for _, role := range roles {
		missing, err := r.clusterRoleMissing(ctx, role)
		if err != nil {
			return err
		}
		if missing {
			return r.invalid(console, pgtoolboxv1alpha1.ReasonConsoleRoleMissing, fmt.Sprintf(
				"ClusterRole %s does not exist: it ships with the operator (config/rbac, the chart's operator half, the OLM bundle); reinstall the operator", role))
		}
	}
	if err := r.checkReferences(ctx, console); err != nil {
		return err
	}
	setCondition(console, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
		"image "+image.Reference)
	console.Status.Image = image.Reference

	applier := r.applier()
	// The ServiceAccount first: only an account this console controls is
	// ever bound.
	objects := []client.Object{buildServiceAccount(console), buildService(console)}
	for _, binding := range buildRoleBindings(console) {
		objects = append(objects, binding)
	}
	role, binding := buildRevocationsRBAC(console)
	objects = append(objects, role, binding)
	role, binding = buildStateRBAC(console)
	objects = append(objects, role, binding)
	if console.DigestDelivered() {
		role, binding = buildDigestLeaseRBAC(console)
		objects = append(objects, role, binding)
	}
	for _, object := range objects {
		if err := applier.Apply(ctx, object); err != nil {
			return r.notOwned(console, err)
		}
	}
	if !console.PodLogsAllowed() {
		if err := r.deleteIfOwned(ctx, console, &rbacv1.RoleBinding{}, podLogsBindingName(console)); err != nil {
			return err
		}
	}
	if !console.RestoreDrillsEnabled() {
		if err := r.deleteIfOwned(ctx, console, &rbacv1.RoleBinding{}, restoreDrillsBindingName(console)); err != nil {
			return err
		}
	}
	if console.DigestDelivered() {
		if err := r.createLease(ctx, buildDigestLease(console)); err != nil {
			return err
		}
	} else {
		// The Lease stays (it goes with the PgToolBoxConsole); without the grant
		// no replica takes it.
		if err := r.deleteIfOwned(ctx, console, &rbacv1.RoleBinding{}, console.DigestLeaseName()); err != nil {
			return err
		}
		if err := r.deleteIfOwned(ctx, console, &rbacv1.Role{}, console.DigestLeaseName()); err != nil {
			return err
		}
	}
	if policy := buildDrillNetworkPolicy(console); policy != nil {
		if err := applier.Apply(ctx, policy); err != nil {
			return r.notOwned(console, err)
		}
	} else if err := r.deleteIfOwned(ctx, console, &networkingv1.NetworkPolicy{}, restoreDrillsNetworkPolicyName(console)); err != nil {
		return err
	}
	key, err := r.sessionKey()
	if err != nil {
		return err
	}
	for _, object := range []client.Object{buildSessionSecret(console, key), buildRevocations(console), buildState(console)} {
		if err := r.createOnce(ctx, object); err != nil {
			return r.notOwned(console, err)
		}
	}
	if err := r.reconcileLifecycle(ctx, console); err != nil {
		return err
	}

	if console.EffectiveReplicas() > 1 {
		if err := applier.Apply(ctx, buildPodDisruptionBudget(console)); err != nil {
			return r.notOwned(console, err)
		}
	} else if err := r.deleteIfOwned(ctx, console, &policyv1.PodDisruptionBudget{}, console.WorkloadName()); err != nil {
		return err
	}
	// The ingress policy the exposure will need, checked by the API server
	// (a dry run, admission included) before anything is published: a
	// refused one stalls with nothing new exposed.
	if policy := buildExposureNetworkPolicy(console, r.exposureUsable(console)); policy != nil {
		if err := r.checkExposurePolicy(ctx, console, policy); err != nil {
			return err
		}
	}
	exposed, leftovers, err := r.applyExposure(ctx, console)
	if err != nil {
		return err
	}
	// The ingress policy before the workload that trusts every peer it
	// admits; its removal only once no Pod of the workload trusts them any
	// more (withdrawExposurePolicy), and its widening likewise
	// (heldWidening).
	policy := buildExposureNetworkPolicy(console, exposed)
	var found confinement
	if policy != nil {
		held, stuck, err := r.heldWidening(ctx, console, policy)
		if err != nil {
			return err
		}
		if stuck {
			// A Pod stuck terminating may never go (a lost node): its
			// deletion is watched, and looked for again meanwhile.
			loop.after(policyRecheck)
		}
		if held != "" {
			found.Unverified = held
		} else {
			if err := applier.Apply(ctx, policy); err != nil {
				if policyRefused(err) {
					return r.exposurePolicyRefused(ctx, console, policy.Name, err)
				}
				return r.notOwned(console, err)
			}
			if found, err = r.exposureConfinement(ctx, console, policy.Spec.Ingress[0].From); err != nil {
				return err
			}
		}
	}
	setExposureConfined(console, exposed, found)
	setLeftoverConfinement(console, exposed, leftovers)
	in := deploymentInputs{Image: image, OpenShift: r.Features.OpenShift, Exposed: exposed, Confinement: found}
	if trust := effectiveProxyTrust(console, exposed, found); trust.EveryPeer || found.Bypass != "" {
		loop.after(policyRecheck)
	}

	facts, err := r.reconcileWorkload(ctx, console, in)
	if err != nil {
		return err
	}
	if policy == nil {
		kept, stuck, err := r.withdrawExposurePolicy(ctx, console, facts)
		if err != nil {
			return err
		}
		if kept != "" {
			setCondition(console, pgtoolboxv1alpha1.ConditionExposureConfined, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonExposurePolicyKept, kept)
		}
		if stuck {
			loop.after(policyRecheck)
		}
	}
	r.observeExposure(ctx, console, leftovers)
	console.Status.URL = consoleURL(console)
	// After the workload: the bundle volume is optional, so the console
	// runs (every Cluster "not verifiable") whatever happens here.
	return r.reconcileInstanceCA(ctx, console)
}

// reconcileWorkload applies the console's workload (a StatefulSet with
// persistent history, else a Deployment) and records WorkloadReady. The
// other kind, when this console owns one (an older operator's persistent
// Deployment, or a switch of history.persistent), is deleted once the new
// workload has a replica available: until then both serve, behind the
// same Service, each on its own volumes.
//
// It returns the workload's facts, nil while a StatefulSet is being
// replaced (nothing was applied).
func (r *Reconciler) reconcileWorkload(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, in deploymentInputs) (*workloadFacts, error) {
	desired := buildWorkload(console, in)
	if set, ok := desired.(*appsv1.StatefulSet); ok {
		replacing, err := r.replaceOnClaimChange(ctx, console, set)
		if err != nil || replacing {
			return nil, err
		}
	}
	if err := r.applier().Apply(ctx, desired); err != nil {
		return nil, r.notOwned(console, err)
	}
	facts := workloadFactsOf(desired)
	if err := r.observeWorkload(ctx, console, facts); err != nil {
		return nil, err
	}
	if facts.Available < 1 {
		return &facts, nil
	}
	var previous client.Object = &appsv1.StatefulSet{}
	if facts.Kind == "StatefulSet" {
		previous = &appsv1.Deployment{}
	}
	return &facts, r.deleteIfOwned(ctx, console, previous, console.WorkloadName())
}

// exposureConfinement looks at what the spec does not say: the console
// Pods (read uncached, as for the policy's withdrawal) for an ipBlock that
// holds their addresses, and the namespace's NetworkPolicies, read
// uncached (the operator's informer holds only its own) and bounded: more
// than networkPolicyListLimit, and the rest could not be checked, which
// counts as a bypass. ours are the peers of the exposure's policy; for a
// Route the routers' older namespace label counts as theirs.
func (r *Reconciler) exposureConfinement(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, ours []networkingv1.NetworkPolicyPeer) (confinement, error) {
	pods, complete, err := r.consolePods(ctx, console)
	if err != nil {
		return confinement{}, err
	}
	if !complete {
		return confinement{Unverified: fmt.Sprintf("more than %d console Pods: their addresses and labels could not all be checked", podListLimit)}, nil
	}
	var found confinement
	found.Broad, found.Unverified = podNetworkBlock(ours, pods)
	if found.Broad == "" && strings.HasPrefix(found.Unverified, noPodAddress) {
		verified, err := r.verifiedEarlier(ctx, console, ours)
		if err != nil {
			return confinement{}, err
		}
		if verified {
			found.Unverified = ""
		}
	}
	if found.Broad == "" && slices.ContainsFunc(ours, func(p networkingv1.NetworkPolicyPeer) bool { return p.IPBlock != nil }) {
		nodes, note, err := r.nodePodCIDRs(ctx)
		if err != nil {
			return confinement{}, err
		}
		found.Broad, found.Note = nodeNetworkBlock(ours, nodes), note
		if incoherent := nodeCoherenceNote(pods, nodes); incoherent != "" {
			found.Note = joinNotes(found.Note, incoherent)
		}
	}
	var policies networkingv1.NetworkPolicyList
	if err := r.APIReader.List(ctx, &policies, client.InNamespace(console.Namespace), client.Limit(networkPolicyListLimit)); err != nil {
		return confinement{}, fmt.Errorf("list the NetworkPolicies of namespace %s: %w", console.Namespace, err)
	}
	if policies.Continue != "" || len(policies.Items) > networkPolicyListLimit {
		found.Bypass = fmt.Sprintf("more than %d NetworkPolicies in the namespace: the others could not be checked", networkPolicyListLimit)
		return found, nil
	}
	podLabels := []map[string]string{objectMeta(console, "", pgtoolboxv1alpha1.PropagationTargetPod).Labels}
	for i := range pods {
		podLabels = append(podLabels, pods[i].Labels)
	}
	if console.Spec.Exposure.TypeOrDefault() == pgtoolboxv1alpha1.ExposureTypeRoute {
		ours = append(slices.Clip(ours), legacyRouterPeer())
	}
	found.Bypass = policyBypass(policies.Items, exposurePolicyName(console), podLabels, ours)
	return found, nil
}

// withdrawExposurePolicy deletes the console's ingress NetworkPolicy once
// it confines no Pod that trusts every peer: the workload's rollout is
// complete (facts: the controller saw the generation, every replica is
// updated and available, a StatefulSet's revisions agree) and no console
// Pod, terminating ones included, still carries the operator's any-peer
// grant (annotationTrustsEveryPeer). Until then the policy stays, and kept
// says why (a Pod stuck terminating is named, and stuck says one is: it is
// looked for again besides its watched deletion); the workload's status
// and its Pods are watched, so their next change brings the next look.
func (r *Reconciler) withdrawExposurePolicy(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, facts *workloadFacts) (kept string, stuck bool, err error) {
	name := exposurePolicyName(console)
	existing := &networkingv1.NetworkPolicy{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: console.Namespace, Name: name}, existing); err != nil {
		return "", false, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(existing, console) {
		return "", false, nil
	}
	kept = "NetworkPolicy " + name + " is no longer rendered and is kept until no console Pod trusts every peer it admits"
	if facts == nil || !facts.rolloutComplete() {
		return kept + " (the rollout without the grant is not through)", false, nil
	}
	pods, complete, err := r.consolePods(ctx, console)
	if err != nil {
		return "", false, err
	}
	if !complete {
		// Pods that could not all be listed may trust every peer: the
		// policy stays.
		return kept + fmt.Sprintf(" (more than %d console Pods: they could not all be checked)", podListLimit), false, nil
	}
	if slices.ContainsFunc(pods, func(pod corev1.Pod) bool { return trustsAllPeers(&pod) }) {
		if blockers := stuckGrantPods(pods); blockers != "" {
			return kept + " (" + blockers + ")", true, nil
		}
		return kept + " (a console Pod still carries the grant)", false, nil
	}
	return "", false, r.deleteIfOwned(ctx, console, &networkingv1.NetworkPolicy{}, name)
}

// stuckGrantPods names the console Pods that are terminating and still
// carry the operator's any-peer grant, with when their deletion was asked,
// "" when none: one whose node is lost, or whose finalizer is stuck, holds
// the policy until someone force-deletes it.
func stuckGrantPods(pods []corev1.Pod) string {
	var named []string
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp.IsZero() || !trustsAllPeers(pod) {
			continue
		}
		named = append(named, fmt.Sprintf("Pod %s, terminating since %s", pod.Name, pod.DeletionTimestamp.UTC().Format(time.RFC3339)))
	}
	if len(named) == 0 {
		return ""
	}
	return "still trusting every peer: " + strings.Join(named, "; ") + "; it holds this until it is gone (force-delete it if its node is lost)"
}

// consolePods lists the console's Pods past the cache (a Pod the informer
// has not delivered yet still counts), bounded by podListLimit; complete
// is false when there were more.
func (r *Reconciler) consolePods(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) (pods []corev1.Pod, complete bool, err error) {
	var list corev1.PodList
	if err := r.APIReader.List(ctx, &list, client.InNamespace(console.Namespace), client.MatchingLabels(selectorLabels(console)), client.Limit(podListLimit)); err != nil {
		return nil, false, fmt.Errorf("list console Pods: %w", err)
	}
	return list.Items, list.Continue == "" && len(list.Items) <= podListLimit, nil
}

// heldWidening says why policy is not applied yet, "" when it may be: it
// would admit a source the console's live ingress NetworkPolicy does not
// (peersWiden) while a console Pod may still trust every peer on the
// operator's grant (everyPeerTrustInForce). The workload is then rendered
// without the grant; once its rollout is through and no Pod carries it,
// the wider policy is applied (the workload's status and its Pods are
// watched, so their changes bring the next look). A narrowing change is
// applied at once.
//
// The policy in force is read past the cache, whichever way the cache
// lags: its informer and the workload's are not ordered, so the cache may
// show the narrower policy this operator replaced (no widening is left) or
// an older, wider one that equals policy while the API server already
// holds the narrower one applied last (a widening the cache cannot show).
// One uncached GET per pass of a console with an ingress policy. stuck: a
// Pod stuck terminating holds it (named in the message).
func (r *Reconciler) heldWidening(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, policy *networkingv1.NetworkPolicy) (held string, stuck bool, err error) {
	live := &networkingv1.NetworkPolicy{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(policy), live); err != nil {
		return "", false, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(live, console) {
		return "", false, nil // the apply reports it as not owned
	}
	var livePeers []networkingv1.NetworkPolicyPeer
	if len(live.Spec.Ingress) > 0 {
		livePeers = live.Spec.Ingress[0].From
	}
	if !peersWiden(policy.Spec.Ingress[0].From, livePeers) {
		return "", false, nil
	}
	trusted, blockers, err := r.everyPeerTrustInForce(ctx, console)
	if err != nil || !trusted {
		return "", false, err
	}
	held = "networkPolicy.from admits more than the policy in force, which is kept until no console Pod trusts every peer any more (a rollout without the grant), then widened"
	if blockers != "" {
		return held + "; " + blockers, true, nil
	}
	return held, false, nil
}

// everyPeerTrustInForce: a console Pod may believe X-Forwarded-For from
// every peer on the operator's grant. A workload of the console carries
// the grant in its template, or its controller has not seen its latest
// template yet (a Pod may still be created from an older one), or a
// console Pod, terminating ones included, carries it; Pods that could not
// all be listed count as one that does. blockers names the Pods stuck
// terminating that carry it (stuckGrantPods).
//
// The workloads are read past the cache: the template this operator
// applied a moment ago may carry the grant before its informer delivers
// it. It runs only on a confirmed widening, so the two GETs are rare.
func (r *Reconciler) everyPeerTrustInForce(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) (trusted bool, blockers string, err error) {
	key := client.ObjectKey{Namespace: console.Namespace, Name: console.WorkloadName()}
	for _, workload := range []client.Object{&appsv1.Deployment{}, &appsv1.StatefulSet{}} {
		if err := r.APIReader.Get(ctx, key, workload); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, "", fmt.Errorf("read the console workload: %w", err)
		}
		if !metav1.IsControlledBy(workload, console) {
			continue
		}
		template, observed := workloadTemplate(workload)
		if template.Annotations[annotationTrustsEveryPeer] == "true" || observed < workload.GetGeneration() {
			return true, "", nil
		}
	}
	pods, complete, err := r.consolePods(ctx, console)
	if err != nil {
		return false, "", err
	}
	if !complete {
		return true, "", nil
	}
	return slices.ContainsFunc(pods, func(pod corev1.Pod) bool { return trustsAllPeers(&pod) }), stuckGrantPods(pods), nil
}

// workloadTemplate is a Deployment's or a StatefulSet's Pod template and
// observed generation.
func workloadTemplate(workload client.Object) (*corev1.PodTemplateSpec, int64) {
	switch w := workload.(type) {
	case *appsv1.Deployment:
		return &w.Spec.Template, w.Status.ObservedGeneration
	case *appsv1.StatefulSet:
		return &w.Spec.Template, w.Status.ObservedGeneration
	}
	return &corev1.PodTemplateSpec{}, 0
}

// verifiedEarlier: while no console Pod has an address, the ipBlock peers
// of ours were verified by an earlier one. A workload of the console, read
// past the cache (the template this operator applied last), carries the
// any-peer grant for the same blocks (annotationTrustedBlocks): a Pod
// address showed then that they are not the Pod network, which does not
// change. Without one (creation, or other blocks) they are not trusted.
func (r *Reconciler) verifiedEarlier(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, ours []networkingv1.NetworkPolicyPeer) (bool, error) {
	blocks := trustedBlocksHash(ours)
	if blocks == "" {
		return false, nil
	}
	key := client.ObjectKey{Namespace: console.Namespace, Name: console.WorkloadName()}
	for _, workload := range []client.Object{&appsv1.StatefulSet{}, &appsv1.Deployment{}} {
		if err := r.APIReader.Get(ctx, key, workload); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("read the console workload: %w", err)
		}
		if !metav1.IsControlledBy(workload, console) {
			continue
		}
		template, _ := workloadTemplate(workload)
		if template.Annotations[annotationTrustsEveryPeer] == "true" && template.Annotations[annotationTrustedBlocks] == blocks {
			return true, nil
		}
	}
	return false, nil
}

// replaceOnClaimChange deletes the console's StatefulSet, orphaning its
// Pods and claims, when its claim templates differ from desired's: the API
// server refuses any change to them, so a new size or StorageClass needs a
// new StatefulSet. The Pods keep serving and the new StatefulSet adopts
// them; its deletion event brings the next reconcile, which creates it.
// Existing claims keep their size (a claim can be grown by editing it,
// where its StorageClass allows expansion); claims made later take the new
// template. It reports whether a replacement is under way.
func (r *Reconciler) replaceOnClaimChange(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, desired *appsv1.StatefulSet) (bool, error) {
	live := &appsv1.StatefulSet{}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(desired), live); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(live, console) {
		// Someone else's: the apply reports it as not owned.
		return false, nil
	}
	message := fmt.Sprintf("StatefulSet %s is being replaced for its new history claim template; its Pods keep serving", live.Name)
	if !live.DeletionTimestamp.IsZero() {
		setCondition(console, pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress, message)
		return true, nil
	}
	if live.Annotations[claimTemplateHashAnnotation] == desired.Annotations[claimTemplateHashAnnotation] {
		return false, nil
	}
	if err := r.Client.Delete(ctx, live, client.PropagationPolicy(metav1.DeletePropagationOrphan),
		client.Preconditions{UID: new(live.UID)}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("replace StatefulSet %s: %w", live.Name, err)
	}
	setCondition(console, pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress, message)
	return true, nil
}

// createLease creates the digest Lease when nothing has its name; one that
// exists is the replicas' (whoever created it) and is never written here.
func (r *Reconciler) createLease(ctx context.Context, lease client.Object) error {
	existing := &metav1.PartialObjectMetadata{}
	existing.SetGroupVersionKind(coordinationv1.SchemeGroupVersion.WithKind("Lease"))
	err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(lease), existing)
	switch {
	case err == nil:
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("check Lease %s: %w", lease.GetName(), err)
	}
	if err := r.Client.Create(ctx, lease, client.FieldOwner(reconcile.FieldOwner)); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create Lease %s: %w", lease.GetName(), err)
	}
	return nil
}

// invalid records ConfigurationValid=False and stalls: nothing is rendered
// until the PgToolBoxConsole (or the operator's installation) changes.
// imageProblem is the reason and message of a console image the operator
// refuses (none to resolve, or outside the allow-list); empty when allowed.
func (r *Reconciler) imageProblem(console *pgtoolboxv1alpha1.PgToolBoxConsole) (pgtoolboxv1alpha1.Reason, string) {
	image := specs.ResolveImage(console.Spec.Image, r.DefaultImage)
	switch {
	case image.Reference == "":
		return pgtoolboxv1alpha1.ReasonConsoleImageRequired,
			"spec.image is not set and the operator has no default console image (--default-console-image)"
	case console.Spec.Image != nil && !specs.ImageAllowed(console.Spec.Image.Repository, r.AllowedImageRepositories, r.DefaultImage):
		return pgtoolboxv1alpha1.ReasonImageNotAllowed, fmt.Sprintf(
			"spec.image repository %s is not in the operator's allow-list (--console-image-repositories)", console.Spec.Image.Repository)
	}
	return "", ""
}

// checkHostname is the hostname allow-list, before anything else may stall
// (the image checks included), and while the console is paused too:
// withdraw what this console published, then stall, so that nothing stays
// published under a name the installer did not allow, whatever else is
// wrong. The workload and the ingress policy stay as they were (a template
// that trusts every peer is never left without its policy):
// ExposureConfined says the policy is kept, and goes only when there is
// none. ExposureReady and status.url say that nothing is published any
// more. An allowed hostname clears a HostnameNotAllowed that conditions
// still say (exposure.ClearHostnameRefusal). imageProblem is named in the
// same message.
func (r *Reconciler) checkHostname(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, imageProblem string) error {
	message := exposure.HostnameRefusal(console.Spec.Exposure.ExposureSpec, r.AllowedHostSuffixes, imageProblem)
	if message == "" {
		exposure.ClearHostnameRefusal(&console.Status.Conditions, console.Generation)
		clearPolicyKeptOnStall(console)
		return nil
	}
	leftovers, err := r.withdrawExposure(ctx, console)
	if err != nil {
		return err
	}
	if err := r.policyKeptOnStall(ctx, console); err != nil {
		return err
	}
	console.Status.URL = ""
	setCondition(console, pgtoolboxv1alpha1.ConditionExposureReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed,
		exposure.WithLeftovers(exposure.WithdrawnMessage, leftovers))
	return r.invalid(console, pgtoolboxv1alpha1.ReasonHostnameNotAllowed, message)
}

func (r *Reconciler) invalid(console *pgtoolboxv1alpha1.PgToolBoxConsole, reason pgtoolboxv1alpha1.Reason, message string) error {
	setCondition(console, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, reason, message)
	return reconcile.Stall(reason, "%s", message)
}

// notOwned turns a foreign object under a generated name into the stalled
// ConfigurationValid=False it is; any other error is returned.
func (r *Reconciler) notOwned(console *pgtoolboxv1alpha1.PgToolBoxConsole, err error) error {
	var foreign *reconcile.NotOwnedError
	if errors.As(err, &foreign) {
		return r.invalid(console, pgtoolboxv1alpha1.ReasonResourceNotOwned, foreign.Error())
	}
	return err
}

// clusterRoleMissing reports whether a shipped ClusterRole is absent, from
// a metadata read past the cache (the cache holds only the operator's own
// ClusterRoles).
func (r *Reconciler) clusterRoleMissing(ctx context.Context, name string) (bool, error) {
	role := &metav1.PartialObjectMetadata{}
	role.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("ClusterRole"))
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, role); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("check ClusterRole %s: %w", name, err)
	}
	return false, nil
}

// createOnce creates object when nothing has its name, and otherwise only
// checks that this console controls it: the session key, the revocation
// list and the shared state are written once and then belong to their
// readers and editors.
func (r *Reconciler) createOnce(ctx context.Context, object client.Object) error {
	existing := &metav1.PartialObjectMetadata{}
	existing.SetGroupVersionKind(object.GetObjectKind().GroupVersionKind())
	err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(object), existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Client.Create(ctx, object, client.FieldOwner(reconcile.FieldOwner)); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s %s: %w", object.GetObjectKind().GroupVersionKind().Kind, object.GetName(), err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("check %s %s: %w", object.GetObjectKind().GroupVersionKind().Kind, object.GetName(), err)
	case !reconcile.Controls(existing, object, false):
		return &reconcile.NotOwnedError{Kind: object.GetObjectKind().GroupVersionKind().Kind, Namespace: object.GetNamespace(), Name: object.GetName()}
	}
	return nil
}

// sessionKey draws a fresh 48-character key; it is used only when the
// session Secret does not exist yet.
func (r *Reconciler) sessionKey() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	draw := r.Random
	if draw == nil {
		draw = func(limit *big.Int) (*big.Int, error) { return rand.Int(rand.Reader, limit) }
	}
	key := make([]byte, 48)
	limit := big.NewInt(int64(len(alphabet)))
	for i := range key {
		n, err := draw(limit)
		if err != nil {
			return "", fmt.Errorf("draw the session key: %w", err)
		}
		key[i] = alphabet[n.Int64()]
	}
	return string(key), nil
}

// deleteIfOwned deletes the named object when this console controls it.
func (r *Reconciler) deleteIfOwned(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, object client.Object, name string) error {
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: console.Namespace, Name: name}, object); err != nil {
		return client.IgnoreNotFound(err)
	}
	if owner := metav1.GetControllerOfNoCopy(object); owner == nil || owner.UID != console.UID {
		return nil
	}
	if err := r.Client.Delete(ctx, object, client.Preconditions{UID: new(object.GetUID())}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %T %s: %w", object, name, err)
	}
	return nil
}

// applyExposure writes the exposure object of the requested type and deletes
// the others this console owns. It reports whether an exposure object is
// published: not for clusterIP, nor for a Route or an HTTPRoute whose API
// is unavailable; and, for a kind refused in the console's namespace, what
// it published before and may still be serving (leftoverExposure).
func (r *Reconciler) applyExposure(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) (bool, []exposure.Remnant, error) {
	spec := &console.Spec.Exposure.ExposureSpec
	kind := spec.TypeOrDefault()
	target := exposureTarget(console)
	applier := r.applier()
	exposed := false
	if kind == pgtoolboxv1alpha1.ExposureTypeIngress {
		if err := applier.Apply(ctx, exposure.Ingress(spec, target)); err != nil {
			return false, nil, r.notOwned(console, err)
		}
		exposed = true
	} else if err := r.deleteIfOwned(ctx, console, &networkingv1.Ingress{}, console.WorkloadName()); err != nil {
		return false, nil, err
	}
	if r.Features.In(console.Namespace).Routes.Usable() {
		if kind == pgtoolboxv1alpha1.ExposureTypeRoute {
			// Recorded before the write: one that fails may still have
			// published it.
			exposure.MarkPublished(&console.Status.PublishedExposureKinds, "Route")
			if err := applier.Apply(ctx, exposure.Route(spec, target)); err != nil {
				return false, nil, r.notOwned(console, err)
			}
			exposed = true
		} else {
			if err := r.deleteIfOwned(ctx, console, &routev1.Route{}, console.WorkloadName()); err != nil {
				return false, nil, err
			}
			exposure.MarkGone(&console.Status.PublishedExposureKinds, "Route")
		}
	}
	if r.Features.In(console.Namespace).Gateways.Usable() {
		if kind == pgtoolboxv1alpha1.ExposureTypeGateway {
			// Recorded before the write: one that fails may still have
			// published it.
			exposure.MarkPublished(&console.Status.PublishedExposureKinds, "HTTPRoute")
			if err := applier.Apply(ctx, exposure.HTTPRoute(spec, target)); err != nil {
				return false, nil, r.notOwned(console, err)
			}
			exposed = true
		} else {
			if err := r.deleteIfOwned(ctx, console, &gatewayv1.HTTPRoute{}, console.WorkloadName()); err != nil {
				return false, nil, err
			}
			exposure.MarkGone(&console.Status.PublishedExposureKinds, "HTTPRoute")
		}
	}
	leftovers, err := r.leftoverExposure(ctx, console, func(t pgtoolboxv1alpha1.ExposureType) bool { return t != kind })
	return exposed, leftovers, err
}

// leftoverExposure looks, for each of Route and HTTPRoute refused to the
// operator in the console's namespace (while the cluster serves it), for
// the one the console may have published before the refusal: the cache
// holds none of that kind there, so one uncached Get each (exposure.Leftover),
// deleting it when remove says so for its type. It returns the status
// sentences of those that remain or may remain.
func (r *Reconciler) leftoverExposure(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, remove func(pgtoolboxv1alpha1.ExposureType) bool) ([]exposure.Remnant, error) {
	here := r.Features.In(console.Namespace)
	var leftovers []exposure.Remnant
	for _, k := range []struct {
		api    discovery.Result
		typ    pgtoolboxv1alpha1.ExposureType
		object client.Object
		kind   string
	}{
		{here.Routes, pgtoolboxv1alpha1.ExposureTypeRoute, &routev1.Route{}, "Route"},
		{here.Gateways, pgtoolboxv1alpha1.ExposureTypeGateway, &gatewayv1.HTTPRoute{}, "HTTPRoute"},
	} {
		if k.api.State == discovery.NotInstalled {
			// Not served: nothing of the kind can remain.
			exposure.MarkGone(&console.Status.PublishedExposureKinds, k.kind)
		}
		if k.api.State != discovery.Forbidden {
			continue
		}
		publishedBefore := exposure.MayHavePublished(console.Spec.Exposure.TypeOrDefault() == k.typ, console.Status.PublishedExposureKinds, k.kind)
		leftover, err := exposure.Leftover(ctx, r.APIReader, r.Client, console, k.object, k.kind, console.WorkloadName(), remove(k.typ), publishedBefore)
		if err != nil {
			return nil, err
		}
		if leftover.Gone {
			exposure.MarkGone(&console.Status.PublishedExposureKinds, k.kind)
		}
		if leftover.Sentence != "" {
			leftovers = append(leftovers, leftover)
		}
	}
	return leftovers, nil
}

// setLeftoverConfinement records, for a console that publishes nothing
// itself (exposed unset), a Route or HTTPRoute it published before the
// operator was refused its kind here (leftovers): one found still serves,
// confined by no policy of the operator (ExposureConfined False); one the
// operator may not read while the console's history says it was published
// may (Unknown). Nothing left, ExposureConfined stays as
// setExposureConfined put it.
func setLeftoverConfinement(console *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool, leftovers []exposure.Remnant) {
	if exposed || len(leftovers) == 0 {
		return
	}
	status := metav1.ConditionFalse
	if exposure.AllUnverified(leftovers) {
		status = metav1.ConditionUnknown
	}
	setCondition(console, pgtoolboxv1alpha1.ConditionExposureConfined, status, pgtoolboxv1alpha1.ReasonExposureOpen,
		exposure.WithLeftovers("an exposure object the operator no longer manages may still publish the console, confined by no policy of the operator", leftovers))
}

// exposureUsable: applyExposure would publish an exposure object (its
// type is not clusterIP, and its API is served).
func (r *Reconciler) exposureUsable(console *pgtoolboxv1alpha1.PgToolBoxConsole) bool {
	switch console.Spec.Exposure.TypeOrDefault() {
	case pgtoolboxv1alpha1.ExposureTypeIngress:
		return true
	case pgtoolboxv1alpha1.ExposureTypeRoute:
		return r.Features.In(console.Namespace).Routes.Usable()
	case pgtoolboxv1alpha1.ExposureTypeGateway:
		return r.Features.In(console.Namespace).Gateways.Usable()
	default:
		return false
	}
}

// checkExposurePolicy has the API server check policy (a server-side
// dry run, admission included) before anything is published: a refusal
// stalls (exposurePolicyRefused). The dry run is skipped when the owned
// policy in force is already policy (policyInForce: the API server took
// it, and nothing would change), and a BadRequest (a webhook that cannot
// run on a dry run) gives way to the real apply, which handles a refusal
// the same way.
func (r *Reconciler) checkExposurePolicy(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, policy *networkingv1.NetworkPolicy) error {
	inForce, err := r.policyInForce(ctx, console, policy)
	if err != nil || inForce {
		return err
	}
	err = r.dryRunApply(ctx, policy)
	switch {
	case err == nil, apierrors.IsBadRequest(err):
		return nil
	case policyRefused(err):
		return r.exposurePolicyRefused(ctx, console, policy.Name, err)
	}
	return fmt.Errorf("dry-run NetworkPolicy %s: %w", policy.Name, err)
}

// policyRefused: the API server refused the policy itself, by its schema
// (Invalid) or by an admission policy or webhook (Forbidden, a denial's
// default; the operator holds every verb it uses on NetworkPolicies, so a
// Forbidden is admission's, not RBAC's).
func policyRefused(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err)
}

// policyInForce: the cached NetworkPolicy of policy's name is this
// console's, with policy's spec, labels and annotations (others may add
// more of their own).
func (r *Reconciler) policyInForce(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, policy *networkingv1.NetworkPolicy) (bool, error) {
	live := &networkingv1.NetworkPolicy{}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(policy), live); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return metav1.IsControlledBy(live, console) && equality.Semantic.DeepEqual(live.Spec, policy.Spec) &&
		holds(live.Labels, policy.Labels) && holds(live.Annotations, policy.Annotations), nil
}

// holds: every entry of want is in got.
func holds(got, want map[string]string) bool {
	for k, v := range want {
		if value, ok := got[k]; !ok || value != v {
			return false
		}
	}
	return true
}

// dryRunApply sends desired as Apply would, server-side and dry-run: the
// API server validates it, admission included, and stores nothing.
func (r *Reconciler) dryRunApply(ctx context.Context, desired client.Object) error {
	raw, err := json.Marshal(desired)
	if err != nil {
		return fmt.Errorf("marshal %T apply patch: %w", desired, err)
	}
	probe, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not a client.Object", desired)
	}
	return r.Client.Patch(ctx, probe, client.RawPatch(types.ApplyPatchType, raw),
		client.FieldOwner(reconcile.FieldOwner), client.ForceOwnership, client.DryRunAll)
}

// exposurePolicyRefused stalls on an ingress NetworkPolicy the API server
// refused. The CRD checks what it can; what the API server still refuses
// (or an admission policy) is looked at again on every spec change and
// every policyRecheck (stallRetries). Nothing new is published (the dry
// run comes first), so what is in force is the earlier spec's: with this
// console's own policy in force, ExposureConfined still describes it;
// without one, and an exposure object of this console published,
// ExposureConfined says it is open; with nothing published it goes.
func (r *Reconciler) exposurePolicyRefused(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, name string, refusal error) error {
	stall := r.invalid(console, pgtoolboxv1alpha1.ReasonExposurePolicyInvalid, fmt.Sprintf(
		"the API server refused NetworkPolicy %s rendered from spec.exposure.networkPolicy: %s", name, refusal))
	live := &networkingv1.NetworkPolicy{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: console.Namespace, Name: name}, live)
	switch {
	case err == nil && metav1.IsControlledBy(live, console):
		return stall
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}
	published, unverified, err := r.exposurePublished(ctx, console)
	if err != nil {
		return err
	}
	if !published {
		setExposureConfined(console, false, confinement{})
		return stall
	}
	// Unknown when the only exposure object is one the operator may not
	// read: it may be gone.
	status := metav1.ConditionFalse
	if unverified {
		status = metav1.ConditionUnknown
	}
	setCondition(console, pgtoolboxv1alpha1.ConditionExposureConfined, status, pgtoolboxv1alpha1.ReasonExposureOpen,
		"no ingress NetworkPolicy is in force (the API server refused NetworkPolicy "+name+"): any Pod can reach the console port; the console Pods keep the X-Forwarded-For trust they were rendered with, never every peer (that needs the policy)")
	return stall
}

// exposurePublished: an exposure object (Ingress, Route, HTTPRoute) of
// this console exists, or may exist: one of a kind the operator is refused
// in the namespace, published before the refusal, counts while it remains
// or cannot be read (leftoverExposure). unverified: none is known to exist,
// only one the operator may not read and the console's history says it
// published.
func (r *Reconciler) exposurePublished(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) (published, unverified bool, err error) {
	leftovers, err := r.leftoverExposure(ctx, console, func(pgtoolboxv1alpha1.ExposureType) bool { return false })
	if err != nil {
		return false, false, err
	}
	if len(leftovers) > 0 && !exposure.AllUnverified(leftovers) {
		return true, false, nil
	}
	objects := []client.Object{&networkingv1.Ingress{}}
	if r.Features.In(console.Namespace).Routes.Usable() {
		objects = append(objects, &routev1.Route{})
	}
	if r.Features.In(console.Namespace).Gateways.Usable() {
		objects = append(objects, &gatewayv1.HTTPRoute{})
	}
	key := client.ObjectKey{Namespace: console.Namespace, Name: console.WorkloadName()}
	for _, object := range objects {
		if err := r.Client.Get(ctx, key, object); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, false, err
		}
		if metav1.IsControlledBy(object, console) {
			return true, false, nil
		}
	}
	return len(leftovers) > 0, len(leftovers) > 0, nil
}

// policyKeptOnStall records ExposureConfined while the console is
// stalled with its exposure withdrawn: ExposurePolicyKept when the
// console's own ingress NetworkPolicy remains (read past the cache: one
// applied a moment ago counts), else no condition (nothing is published,
// nothing confines).
func (r *Reconciler) policyKeptOnStall(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) error {
	name := exposurePolicyName(console)
	policy := &networkingv1.NetworkPolicy{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: console.Namespace, Name: name}, policy); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("read the console's ingress NetworkPolicy: %w", err)
		}
		setExposureConfined(console, false, confinement{})
		return nil
	}
	if !metav1.IsControlledBy(policy, console) {
		setExposureConfined(console, false, confinement{})
		return nil
	}
	setCondition(console, pgtoolboxv1alpha1.ConditionExposureConfined, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonExposurePolicyKept,
		policyKeptOnStallMessage(name))
	return nil
}

// policyKeptOnStallMessage is ExposureConfined's message while the console
// is stalled with its exposure withdrawn (policyKeptOnStall).
func policyKeptOnStallMessage(name string) string {
	return "NetworkPolicy " + name + " is kept while the console is stalled with its exposure withdrawn: the workload keeps its last template, which may still trust every peer the policy admits"
}

// clearPolicyKeptOnStall turns ExposureConfined back to Unknown (reason
// Reconciling) when it still says what policyKeptOnStall said and the
// hostname is now allowed: a paused console, which goes no further than
// the hostname check, must not keep saying it is stalled with its exposure
// withdrawn once that no longer holds (exposure.ClearHostnameRefusal does
// the same for ExposureReady and ConfigurationValid). The next loop that
// gets to the exposure says what holds; any other ExposureConfined is left
// as it is, so a no-op loop writes nothing.
func clearPolicyKeptOnStall(console *pgtoolboxv1alpha1.PgToolBoxConsole) {
	c := meta.FindStatusCondition(console.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureConfined)
	if c != nil && c.Reason == pgtoolboxv1alpha1.ReasonExposurePolicyKept.String() && c.Message == policyKeptOnStallMessage(exposurePolicyName(console)) {
		setCondition(console, pgtoolboxv1alpha1.ConditionExposureConfined, metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciling,
			exposure.HostnameAllowedMessage)
	}
}

// withdrawExposure deletes every exposure object this console owns. It
// returns the status sentences of those it could not (leftoverExposure).
func (r *Reconciler) withdrawExposure(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) ([]exposure.Remnant, error) {
	name := console.WorkloadName()
	if err := r.deleteIfOwned(ctx, console, &networkingv1.Ingress{}, name); err != nil {
		return nil, err
	}
	if r.Features.In(console.Namespace).Routes.Usable() {
		if err := r.deleteIfOwned(ctx, console, &routev1.Route{}, name); err != nil {
			return nil, err
		}
		exposure.MarkGone(&console.Status.PublishedExposureKinds, "Route")
	}
	if r.Features.In(console.Namespace).Gateways.Usable() {
		if err := r.deleteIfOwned(ctx, console, &gatewayv1.HTTPRoute{}, name); err != nil {
			return nil, err
		}
		exposure.MarkGone(&console.Status.PublishedExposureKinds, "HTTPRoute")
	}
	return r.leftoverExposure(ctx, console, func(pgtoolboxv1alpha1.ExposureType) bool { return true })
}

// observeExposure reads the exposure object's status into ExposureReady,
// followed by what the console published before a refusal and may still
// be serving (leftovers, from applyExposure): the message never says an
// exposure is gone while one may remain.
func (r *Reconciler) observeExposure(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, leftovers []exposure.Remnant) {
	key := client.ObjectKey{Namespace: console.Namespace, Name: console.WorkloadName()}
	fact := exposure.Fact{Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonNotRequested, Message: "in-cluster Service only (exposure type clusterIP)"}
	switch console.Spec.Exposure.TypeOrDefault() {
	case pgtoolboxv1alpha1.ExposureTypeClusterIP:
	case pgtoolboxv1alpha1.ExposureTypeRoute:
		if !r.Features.In(console.Namespace).Routes.Usable() {
			fact = exposure.Fact{Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonRouteAPIUnavailable, Message: "the Route API is unavailable: " + unavailableWhy(r.Features.In(console.Namespace).Routes)}
			break
		}
		var route routev1.Route
		if err := r.Client.Get(ctx, key, &route); err != nil {
			fact = exposure.Fact{Status: metav1.ConditionUnknown, Reason: pgtoolboxv1alpha1.ReasonReconciling, Message: "the Route is not observed yet"}
		} else {
			fact = exposure.RouteFact(&route)
		}
	case pgtoolboxv1alpha1.ExposureTypeIngress:
		var ingress networkingv1.Ingress
		if err := r.Client.Get(ctx, key, &ingress); err != nil {
			fact = exposure.Fact{Status: metav1.ConditionUnknown, Reason: pgtoolboxv1alpha1.ReasonReconciling, Message: "the Ingress is not observed yet"}
		} else {
			fact = exposure.IngressFact(&ingress)
		}
	case pgtoolboxv1alpha1.ExposureTypeGateway:
		if !r.Features.In(console.Namespace).Gateways.Usable() {
			fact = exposure.Fact{Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonGatewayAPIUnavailable, Message: "the Gateway API is unavailable: " + unavailableWhy(r.Features.In(console.Namespace).Gateways)}
			break
		}
		var route gatewayv1.HTTPRoute
		if err := r.Client.Get(ctx, key, &route); err != nil {
			fact = exposure.Fact{Status: metav1.ConditionUnknown, Reason: pgtoolboxv1alpha1.ReasonReconciling, Message: "the HTTPRoute is not observed yet"}
		} else {
			fact = exposure.HTTPRouteFact(&route)
		}
	}
	setCondition(console, pgtoolboxv1alpha1.ConditionExposureReady, fact.Status, fact.Reason, exposure.WithLeftovers(fact.Message, leftovers))
}

// workloadFacts is what WorkloadReady reads of a Deployment or a
// StatefulSet, from the object an apply returned.
type workloadFacts struct {
	Kind, Name                     string
	Generation, ObservedGeneration int64
	Desired                        int32
	Replicas, Updated, Available   int32
	// Failure and Stuck are a Deployment's ReplicaFailure and
	// Progressing=False messages; a StatefulSet reports neither.
	Failure, Stuck string
	// RevisionsDiffer: a StatefulSet's current and update revisions are
	// not the same yet (a Pod deleted mid-rollout comes back at the old
	// one).
	RevisionsDiffer bool
}

// rolloutComplete: the controller saw the latest generation, and every
// desired replica, and no other, is updated and available.
func (w workloadFacts) rolloutComplete() bool {
	return w.Kind != "" && !w.RevisionsDiffer && w.ObservedGeneration >= w.Generation &&
		w.Replicas == w.Desired && w.Updated == w.Desired && w.Available >= w.Desired
}

func workloadFactsOf(object client.Object) workloadFacts {
	switch w := object.(type) {
	case *appsv1.Deployment:
		facts := workloadFacts{
			Kind: "Deployment", Name: w.Name, Generation: w.Generation, ObservedGeneration: w.Status.ObservedGeneration,
			Desired: ptrValue(w.Spec.Replicas, 1), Replicas: w.Status.Replicas, Updated: w.Status.UpdatedReplicas, Available: w.Status.AvailableReplicas,
		}
		for _, condition := range w.Status.Conditions {
			switch {
			case condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue:
				facts.Failure = condition.Message
			case condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse:
				facts.Stuck = condition.Message
			}
		}
		return facts
	case *appsv1.StatefulSet:
		return workloadFacts{
			Kind: "StatefulSet", Name: w.Name, Generation: w.Generation, ObservedGeneration: w.Status.ObservedGeneration,
			Desired: ptrValue(w.Spec.Replicas, 1), Replicas: w.Status.Replicas, Updated: w.Status.UpdatedReplicas, Available: w.Status.AvailableReplicas,
			RevisionsDiffer: w.Status.UpdateRevision != "" && w.Status.CurrentRevision != w.Status.UpdateRevision,
		}
	}
	return workloadFacts{}
}

// observeWorkload sets WorkloadReady from the live workload and, when it
// is not ready, the blocking fact of its Pods. Ready means every desired
// replica is updated and available, and no ReplicaFailure: a quota that
// refuses the second Pod is not a ready console of two.
func (r *Reconciler) observeWorkload(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, w workloadFacts) error {
	if w.Failure == "" && w.ObservedGeneration >= w.Generation && w.Desired >= 1 &&
		w.Replicas == w.Desired && w.Updated == w.Desired && w.Available >= w.Desired {
		setCondition(console, pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected,
			fmt.Sprintf("%s %s: %d of %d replicas available", w.Kind, w.Name, w.Available, w.Desired))
		return nil
	}
	reason := pgtoolboxv1alpha1.ReasonRolloutInProgress
	message := fmt.Sprintf("%s %s: %d of %d replicas updated and available", w.Kind, w.Name, w.Available, w.Desired)
	switch {
	case w.Failure != "":
		reason, message = pgtoolboxv1alpha1.ReasonReplicaFailure, w.Failure
	case w.Stuck != "":
		reason, message = pgtoolboxv1alpha1.ReasonRolloutStuck, w.Stuck
	}
	podReason, podMessage, err := podFacts(ctx, r.APIReader, console)
	if err != nil {
		return err
	}
	if podReason != "" {
		reason, message = podReason, podMessage
	}
	setCondition(console, pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse, reason, message)
	return nil
}

func ptrValue[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}

// podFacts copies a console Pod's blocking fact into a reason: the
// scheduler's Unschedulable, an image pull failure, a crash loop, a missing
// Secret key.
func podFacts(ctx context.Context, reader client.Reader, console *pgtoolboxv1alpha1.PgToolBoxConsole) (pgtoolboxv1alpha1.Reason, string, error) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(console.Namespace), client.MatchingLabels(selectorLabels(console))); err != nil {
		return "", "", fmt.Errorf("list console Pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
				return pgtoolboxv1alpha1.ReasonPodUnschedulable, fmt.Sprintf("Pod %s: %s", pod.Name, condition.Message), nil
			}
		}
		for _, container := range pod.Status.ContainerStatuses {
			waiting := container.State.Waiting
			if waiting == nil {
				continue
			}
			switch waiting.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				return pgtoolboxv1alpha1.ReasonImagePullFailed, fmt.Sprintf("Pod %s: %s: %s", pod.Name, waiting.Reason, waiting.Message), nil
			case "CrashLoopBackOff":
				return pgtoolboxv1alpha1.ReasonCrashLoopBackOff, fmt.Sprintf("Pod %s: container %s is crash-looping (restarts %d)", pod.Name, container.Name, container.RestartCount), nil
			case "CreateContainerConfigError":
				return pgtoolboxv1alpha1.ReasonSecretKeyMissing, fmt.Sprintf("Pod %s: %s", pod.Name, waiting.Message), nil
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

func unavailableWhy(result discovery.Result) string {
	if result.Reason == "" {
		return "not probed"
	}
	return result.Reason
}

// Node podCIDR lookups: an uncached list of the Nodes, in pages of
// nodeListPage, at most nodeListPages of them (a larger cluster is said
// not to be checked), kept for nodeCIDRTTL and shared by every console:
// a Node's podCIDRs never change once set, and a new Node is seen within
// that time.
//
// The pages are full Nodes: a metadata-only list has no spec, and the
// controller-runtime reader decodes a typed list whole. Pages of 100 keep
// what one page holds in memory to about a hundred Node objects (their
// status lists images and can run to tens of kilobytes each), and only
// the names and podCIDRs outlive the page.
//
// One lookup runs for at most nodeLookupBudget, a third of the reconcile
// timeout, so that the reads after it still have time; a lookup that
// stops keeps the pages it read (nodeScan) and the next resumes from
// them, so a cluster too large for one pass is still read whole over a
// few (each retry comes with the reconcile error's backoff).
const (
	nodeListPage     = 100
	nodeListPages    = 100
	nodeCIDRTTL      = time.Minute
	nodeLookupBudget = reconciliationTimeout / 3
	// nodeScanMaxAge drops a stopped scan nobody resumed: its continue
	// token would have expired by then (the API server compacts every
	// five minutes by default), and a fresh one is cheaper than a 410.
	nodeScanMaxAge = 5 * time.Minute
)

// nodeCIDRMemo is the last successful Node podCIDR lookup, the one under
// way, and what a stopped one had read. The mutex guards the fields only:
// it is never held across a List.
type nodeCIDRMemo struct {
	mu    sync.Mutex
	at    time.Time
	nodes []nodePodCIDRs
	note  string
	// inflight is closed when the lookup under way ends, nil when none is.
	inflight chan struct{}
	// scan is what the last lookup that stopped had read, the zero value
	// when none did.
	scan nodeScan
}

// nodeScan is a Node podCIDR list part read: the Nodes with podCIDRs of
// the pages read so far, how many Nodes and pages those were, and the
// continue token of the next page. A continue token reads the snapshot
// the first page was read at, so pages read by successive lookups add up
// to one list; a token the API server no longer serves (410 Expired)
// starts the list over.
type nodeScan struct {
	started       time.Time
	token         string
	nodes         []nodePodCIDRs
	listed, pages int
}

// nodePodCIDRs returns the Nodes' spec.podCIDRs. When the operator may not
// list Nodes (an installation without the grant), or there are more than
// it lists, or no Node has spec.podCIDRs (a CNI that allocates Pod
// addresses itself), note says that they were not checked and the console
// Pods' own addresses stay the only check; any other error is returned.
//
// One lookup runs at a time; the other callers wait for it, each on its
// own context, and take its result. When it fails (its caller's context
// ended, its budget ran out, or the API server erred) no result is kept,
// only the pages it read, and a waiter whose context is still live runs
// the next lookup itself, from where this one stopped.
func (r *Reconciler) nodePodCIDRs(ctx context.Context) (nodes []nodePodCIDRs, note string, err error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	for {
		r.nodes.mu.Lock()
		if !r.nodes.at.IsZero() && now().Sub(r.nodes.at) < nodeCIDRTTL {
			nodes, note = r.nodes.nodes, r.nodes.note
			r.nodes.mu.Unlock()
			return nodes, note, nil
		}
		if wait := r.nodes.inflight; wait != nil {
			r.nodes.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, "", fmt.Errorf("wait for the Node podCIDR lookup: %w", ctx.Err())
			}
		}
		r.nodes.inflight = make(chan struct{})
		scan := r.nodes.scan
		r.nodes.scan = nodeScan{}
		r.nodes.mu.Unlock()
		if scan.started.IsZero() || now().Sub(scan.started) > nodeScanMaxAge {
			scan = nodeScan{started: now()}
		}
		scan.nodes = slices.Clone(scan.nodes)
		return r.leadNodeLookup(ctx, now, scan)
	}
}

// leadNodeLookup runs the lookup nodePodCIDRs made inflight, from scan,
// within nodeLookupBudget; it keeps a successful result, or the pages a
// failed one read, and releases the waiters on every path (a panic
// included, which keeps nothing).
func (r *Reconciler) leadNodeLookup(ctx context.Context, now func() time.Time, scan nodeScan) (nodes []nodePodCIDRs, note string, err error) {
	returned := false
	defer func() {
		r.nodes.mu.Lock()
		defer r.nodes.mu.Unlock()
		switch {
		case returned && err == nil:
			r.nodes.at, r.nodes.nodes, r.nodes.note = now(), nodes, note
		case returned:
			r.nodes.scan = scan
		}
		close(r.nodes.inflight)
		r.nodes.inflight = nil
	}()
	lookup, cancel := context.WithTimeout(ctx, nodeLookupBudget)
	defer cancel()
	nodes, note, err = r.listNodePodCIDRs(lookup, &scan)
	returned = true
	if err != nil {
		if lookup.Err() != nil && ctx.Err() == nil {
			err = fmt.Errorf("the Node podCIDR lookup did not finish within %s (%d Nodes listed so far; the next attempt resumes where it stopped): %w",
				nodeLookupBudget, scan.listed, err)
		}
		return nil, "", err
	}
	return nodes, note, nil
}

// listNodePodCIDRs reads the Nodes' podCIDRs from where scan stopped,
// recording each page in it as it is read.
func (r *Reconciler) listNodePodCIDRs(ctx context.Context, scan *nodeScan) ([]nodePodCIDRs, string, error) {
	restarted := false
	for scan.pages < nodeListPages {
		var list corev1.NodeList
		options := []client.ListOption{client.Limit(nodeListPage)}
		if scan.token != "" {
			options = append(options, client.Continue(scan.token))
		}
		if err := r.APIReader.List(ctx, &list, options...); err != nil {
			switch {
			case apierrors.IsForbidden(err):
				return nil, "the Nodes' podCIDRs were not checked against the ipBlock peers (the operator may not list Nodes): only the console Pods' own addresses were", nil
			case scan.token != "" && !restarted && (apierrors.IsResourceExpired(err) || apierrors.IsGone(err)):
				// The snapshot is gone: the list starts over, once.
				*scan = nodeScan{started: scan.started}
				restarted = true
				continue
			}
			return nil, "", fmt.Errorf("list the Nodes for their podCIDRs: %w", err)
		}
		scan.pages++
		scan.listed += len(list.Items)
		for i := range list.Items {
			node := nodePodCIDRs{Name: list.Items[i].Name}
			for _, cidr := range list.Items[i].Spec.PodCIDRs {
				if prefix, err := netip.ParsePrefix(cidr); err == nil {
					node.CIDRs = append(node.CIDRs, prefix.Masked())
				}
			}
			if len(node.CIDRs) > 0 {
				scan.nodes = append(scan.nodes, node)
			}
		}
		if scan.token = list.Continue; scan.token == "" {
			return scan.nodes, nodeCoverageNote(scan.listed, len(scan.nodes)), nil
		}
	}
	return nil, fmt.Sprintf("the Nodes' podCIDRs were not checked against the ipBlock peers (more than %d Nodes): only the console Pods' own addresses were", nodeListPage*nodeListPages), nil
}

// nodeCoverageNote says which of the listed Nodes could not be checked
// for want of spec.podCIDRs, "" when every one could (or none is listed):
// a CNI that allocates Pod addresses itself (Calico, Cilium's cluster
// pool) leaves it empty on every Node, so an empty list of ranges is a
// check that could not run, not one that passed.
func nodeCoverageNote(listed, withCIDRs int) string {
	switch {
	case listed == 0 || withCIDRs == listed:
		return ""
	case withCIDRs == 0:
		return fmt.Sprintf("the Node check could not run: none of the %d Nodes has spec.podCIDRs (the CNI allocates Pod addresses itself), so only the console Pods' own addresses were checked against the ipBlock peers", listed)
	default:
		return fmt.Sprintf("%d of the %d Nodes have no spec.podCIDRs: their Pod ranges were not checked against the ipBlock peers", listed-withCIDRs, listed)
	}
}
