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
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
// token. The
// Clusters and their server CA Secrets are read
// for the instance CA mirror (instanceca.go): only ca.crt is copied, into
// a ConfigMap the console mounts.
//
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxconsoles,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxconsoles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxconsoles/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;create;update
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
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
	err := r.reconcile(ctx, console)
	var result ctrl.Result
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

// policyRecheck is how often a console stalled on an ingress
// NetworkPolicy the API server refused (an admission policy may change)
// tries it again.
const policyRecheck = 5 * time.Minute

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
func (r *Reconciler) reconcile(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) error {
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
	mcpService, mcpCreated := buildMCPLeader(console)
	if console.MCPEnabled() {
		role, binding = buildMCPLeaderRBAC(console)
		objects = append(objects, mcpService, role, binding)
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
	if console.MCPEnabled() {
		if err := r.createLease(ctx, mcpCreated[0]); err != nil {
			return err
		}
		for _, slice := range mcpCreated[1:] {
			if err := r.createOnce(ctx, slice); err != nil {
				return r.notOwned(console, err)
			}
		}
	} else {
		// The Lease and the slices stay (they go with the PgToolBoxConsole);
		// without the Service and the grant nothing reaches or writes them.
		for _, object := range []client.Object{&corev1.Service{}, &rbacv1.RoleBinding{}, &rbacv1.Role{}} {
			if err := r.deleteIfOwned(ctx, console, object, console.MCPLeaseName()); err != nil {
				return err
			}
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
	// The ingress policy before the exposure it confines: a policy the API
	// server refuses stalls with nothing new published.
	if policy := buildExposureNetworkPolicy(console, r.exposureUsable(console)); policy != nil {
		if err := applier.Apply(ctx, policy); err != nil {
			if policyRefused(err) {
				return r.exposurePolicyRefused(console, policy.Name, err)
			}
			return r.notOwned(console, err)
		}
	} else if err := r.deleteIfOwned(ctx, console, &networkingv1.NetworkPolicy{}, exposurePolicyName(console)); err != nil {
		return err
	}
	exposed, err := r.applyExposure(ctx, console)
	if err != nil {
		return err
	}
	setExposureConfined(console, exposed)
	in := deploymentInputs{Image: image, OpenShift: r.Features.OpenShift, Exposed: exposed}
	if err := r.reconcileWorkload(ctx, console, in); err != nil {
		return err
	}
	r.observeExposure(ctx, console)
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
func (r *Reconciler) reconcileWorkload(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, in deploymentInputs) error {
	desired := buildWorkload(console, in)
	if set, ok := desired.(*appsv1.StatefulSet); ok {
		replacing, err := r.replaceOnClaimChange(ctx, console, set)
		if err != nil || replacing {
			return err
		}
	}
	if err := r.applier().Apply(ctx, desired); err != nil {
		return r.notOwned(console, err)
	}
	facts := workloadFactsOf(desired)
	if err := r.observeWorkload(ctx, console, facts); err != nil {
		return err
	}
	if facts.Available < 1 {
		return nil
	}
	var previous client.Object = &appsv1.StatefulSet{}
	if facts.Kind == "StatefulSet" {
		previous = &appsv1.Deployment{}
	}
	return r.deleteIfOwned(ctx, console, previous, console.WorkloadName())
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
// wrong. The workload and the ingress policy stay as they were;
// ExposureConfined goes (nothing is published). ExposureReady and status.url say that nothing is published any
// more. An allowed hostname clears a HostnameNotAllowed that conditions
// still say (exposure.ClearHostnameRefusal). imageProblem is named in the
// same message.
func (r *Reconciler) checkHostname(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole, imageProblem string) error {
	message := exposure.HostnameRefusal(console.Spec.Exposure.ExposureSpec, r.AllowedHostSuffixes, imageProblem)
	if message == "" {
		exposure.ClearHostnameRefusal(&console.Status.Conditions, console.Generation)
		return nil
	}
	if err := r.withdrawExposure(ctx, console); err != nil {
		return err
	}
	setExposureConfined(console, false)
	console.Status.URL = ""
	setCondition(console, pgtoolboxv1alpha1.ConditionExposureReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed,
		exposure.WithdrawnMessage)
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
// is unavailable. One published before the operator was refused its kind
// in the namespace is no longer seen: it is deleted by hand.
func (r *Reconciler) applyExposure(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) (bool, error) {
	spec := &console.Spec.Exposure.ExposureSpec
	kind := spec.TypeOrDefault()
	target := exposureTarget(console)
	applier := r.applier()
	exposed := false
	if kind == pgtoolboxv1alpha1.ExposureTypeIngress {
		if err := applier.Apply(ctx, exposure.Ingress(spec, target)); err != nil {
			return false, r.notOwned(console, err)
		}
		exposed = true
	} else if err := r.deleteIfOwned(ctx, console, &networkingv1.Ingress{}, console.WorkloadName()); err != nil {
		return false, err
	}
	if r.Features.In(console.Namespace).Routes.Usable() {
		// The console's Route, then one per MCP path (none with MCP off).
		routes := []*routev1.Route{}
		if kind == pgtoolboxv1alpha1.ExposureTypeRoute {
			routes = append([]*routev1.Route{exposure.Route(spec, target)}, exposure.ExtraRoutes(spec, target)...)
		}
		names := []string{console.WorkloadName()}
		for _, p := range mcpPaths(console) {
			names = append(names, p.Service+"-"+p.Name)
		}
		for _, name := range names {
			if i := slices.IndexFunc(routes, func(route *routev1.Route) bool { return route.Name == name }); i >= 0 {
				if err := applier.Apply(ctx, routes[i]); err != nil {
					return false, r.notOwned(console, err)
				}
				exposed = true
			} else if err := r.deleteIfOwned(ctx, console, &routev1.Route{}, name); err != nil {
				return false, err
			}
		}
	}
	if r.Features.In(console.Namespace).Gateways.Usable() {
		if kind == pgtoolboxv1alpha1.ExposureTypeGateway {
			if err := applier.Apply(ctx, exposure.HTTPRoute(spec, target)); err != nil {
				return false, r.notOwned(console, err)
			}
			exposed = true
		} else if err := r.deleteIfOwned(ctx, console, &gatewayv1.HTTPRoute{}, console.WorkloadName()); err != nil {
			return false, err
		}
	}
	return exposed, nil
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

// policyRefused: the API server refused the policy itself, by its schema
// (Invalid) or by an admission policy or webhook (Forbidden, a denial's
// default; the operator holds every verb it uses on NetworkPolicies, so a
// Forbidden is admission's, not RBAC's).
func policyRefused(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err)
}

// exposurePolicyRefused stalls on an ingress NetworkPolicy the API server
// refused (its apply comes before the exposure's, so nothing new is
// published). The CRD checks what it can; what the API server still refuses
// (or an admission policy) is looked at again on every spec change and
// every policyRecheck (stallRetries). ExposureConfined goes False with the
// stall's reason: whatever the earlier spec published is not known to be
// confined.
func (r *Reconciler) exposurePolicyRefused(console *pgtoolboxv1alpha1.PgToolBoxConsole, name string, refusal error) error {
	message := fmt.Sprintf("the API server refused NetworkPolicy %s rendered from spec.exposure.networkPolicy: %s", name, refusal)
	setCondition(console, pgtoolboxv1alpha1.ConditionExposureConfined, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonExposurePolicyInvalid, message)
	return r.invalid(console, pgtoolboxv1alpha1.ReasonExposurePolicyInvalid, message)
}

// withdrawExposure deletes every exposure object this console owns.
func (r *Reconciler) withdrawExposure(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) error {
	name := console.WorkloadName()
	if err := r.deleteIfOwned(ctx, console, &networkingv1.Ingress{}, name); err != nil {
		return err
	}
	if r.Features.In(console.Namespace).Routes.Usable() {
		if err := r.deleteIfOwned(ctx, console, &routev1.Route{}, name); err != nil {
			return err
		}
	}
	if r.Features.In(console.Namespace).Gateways.Usable() {
		return r.deleteIfOwned(ctx, console, &gatewayv1.HTTPRoute{}, name)
	}
	return nil
}

// observeExposure reads the exposure object's status into ExposureReady.
func (r *Reconciler) observeExposure(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) {
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
	setCondition(console, pgtoolboxv1alpha1.ConditionExposureReady, fact.Status, fact.Reason, fact.Message)
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
