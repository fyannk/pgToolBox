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

// Package pgconsole reconciles PgConsole resources into a running console
// pod: the pgtoolbox-proxy, the pgConsole container, the embedded pgAdmin
// and the optional evidence sidecar, plus their Service, ServiceAccount,
// the namespaced Roles that are the pod's entire authority, exposure and
// NetworkPolicy.
package pgconsole

import (
	"context"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	"github.com/fyannk/pgtoolbox/internal/render"
	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// clusterNotFoundRequeue is how long a console whose CNPG Cluster does not
// exist yet waits before retrying. A missing Cluster is an expected state,
// not an error: the console object may legitimately be applied first.
const clusterNotFoundRequeue = 30 * time.Second

// application is PgConsole's frozen identity in the pgtoolbox family.
var application = shared.PgConsoleApplication()

// DefaultImages carries the operator's configured fallback images
// (--default-*-image flags). A spec image always wins; a container with
// neither cannot be composed.
type DefaultImages struct {
	PgConsole         string
	Proxy             string
	PgAdmin           string
	ObjectStoreViewer string
}

// Reconciler reconciles a PgConsole resource.
type Reconciler struct {
	// Runtime carries everything shared with the other applications in the
	// family.
	shared.Runtime

	// DefaultImages are the operator-configured fallback images.
	DefaultImages DefaultImages

	// OperatorImage is this operator's own container image reference, used
	// as the admin-sync init container image.
	OperatorImage string

	// AdminSync applies pgAdmin user/server state through the in-pod
	// sidecar API. A nil or Disabled Syncer skips pgAdmin provisioning.
	AdminSync adminsync.Syncer

	// BarmanObjectStoreAvailable reports whether the Barman Cloud Plugin
	// ObjectStore API is served, discovered at startup. It gates one read
	// rule in the generated Role and the evidence composition.
	BarmanObjectStoreAvailable bool
}

// Reconciliation of the PgConsole CRD and everything the instance owns.
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgconsoles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgconsoles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgconsoles/finalizers,verbs=update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies;ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=databaseroles,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxusers,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxusers/status,verbs=update;patch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxroles,verbs=get;list;watch

// The rules below are never exercised by this operator. They exist only
// because Kubernetes escalation prevention refuses to let a controller create
// a Role granting permissions it does not itself hold, and this controller
// generates the Roles that are a PgConsole's entire authority. Each is
// granted onward and never used here.
//
// The clusters and clusters/status patches are the CloudNativePG reload,
// restart and switchover triggers. Nothing in this repository may use them:
// the console pod holds them, the operator merely writes them down.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=list;watch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=backups;scheduledbackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=backups,verbs=create
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=patch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters/status,verbs=patch
// +kubebuilder:rbac:groups=barmancloud.cnpg.io,resources=objectstores,verbs=get
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxaccessrequests,verbs=create
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxaccessrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxaccessrequests/status,verbs=update;patch

// Reconcile converges one PgConsole: workload RBAC, proxy configuration
// Secret, ServiceAccount, Deployment, Service, exposure and NetworkPolicy,
// then publishes status conditions.
func (r *Reconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var console pgtoolboxv1alpha1.PgConsole
	if err := r.Get(ctx, req.NamespacedName, &console); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !console.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&console, application.Finalizer) {
			return ctrl.Result{}, nil
		}
		controllerutil.RemoveFinalizer(&console, application.Finalizer)
		return ctrl.Result{}, r.Update(ctx, &console)
	}
	if !controllerutil.ContainsFinalizer(&console, application.Finalizer) {
		controllerutil.AddFinalizer(&console, application.Finalizer)
		return ctrl.Result{}, r.Update(ctx, &console)
	}

	statusBefore := console.DeepCopy()

	if console.Annotations[pgtoolboxv1alpha1.ReconcileAnnotation] == "skip" {
		conditions.MarkFalse(
			&console,
			pgtoolboxv1alpha1.PgConsoleConditionProgressing,
			pgtoolboxv1alpha1.ReasonReconciliationSkipped,
			"reconciliation skipped by annotation",
		)
		if err := r.updateStatus(ctx, statusBefore, &console); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("reconciliation skipped by annotation")
		return ctrl.Result{}, nil
	}

	console.Status.ObservedGeneration = console.GetGeneration()

	// The webhook and CEL rules already refuse most of these; re-checking
	// covers objects that predate them or bypassed admission, reporting
	// instead of deploying a console that cannot work.
	if err := validateExposure(&console); err != nil {
		conditions.MarkFalse(
			&console,
			pgtoolboxv1alpha1.PgConsoleConditionConfigurationValid,
			pgtoolboxv1alpha1.ReasonConfigurationInvalid,
			"invalid exposure configuration: %v",
			err,
		)
		return ctrl.Result{}, r.updateStatus(ctx, statusBefore, &console)
	}

	// The console serves exactly one CNPG Cluster. A missing Cluster is an
	// expected state — the console may have been applied first — so it is
	// reported on ClusterReady and retried, never failed hard. The Cluster
	// object itself is read-only here: nothing in this operator ever writes
	// to a CNPG Cluster.
	var cluster cnpgv1.Cluster
	clusterKey := client.ObjectKey{Namespace: console.Namespace, Name: console.Spec.CNPGClusterRef.Name}
	if err := r.APIReader.Get(ctx, clusterKey, &cluster); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		conditions.MarkFalse(
			&console,
			pgtoolboxv1alpha1.PgConsoleConditionClusterReady,
			pgtoolboxv1alpha1.ReasonClusterNotFound,
			"CNPG Cluster %s was not found in namespace %s",
			clusterKey.Name,
			clusterKey.Namespace,
		)
		if err := r.updateStatus(ctx, statusBefore, &console); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: clusterNotFoundRequeue}, nil
	}
	conditions.MarkTrue(
		&console,
		pgtoolboxv1alpha1.PgConsoleConditionClusterReady,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"CNPG Cluster %s exists",
		cluster.Name,
	)

	// Resolve the console's users once per reconcile: the same role, password
	// and credential information feeds the proxy config, pgAdmin sync, and the
	// per-user status conditions.
	resolvedUsers, err := r.resolveConsoleUsers(ctx, &console)
	if err != nil {
		return ctrl.Result{}, err
	}

	// The proxy configuration Secret is rendered first: its checksum feeds
	// the Pod template, and an unrenderable configuration stops the
	// reconcile before any workload object moves.
	checksum, proxyIssue, err := r.reconcileProxyConfigSecret(ctx, &console, proxyUsers(resolvedUsers))
	if err != nil {
		return ctrl.Result{}, err
	}
	if proxyIssue != nil {
		if proxyIssue.reason == pgtoolboxv1alpha1.ReasonUnsupportedAuthMode {
			r.Recorder.Event(&console, corev1.EventTypeWarning, proxyIssue.reason, proxyIssue.message)
		}
		conditions.MarkFalse(
			&console,
			pgtoolboxv1alpha1.PgConsoleConditionProxyConfigReady,
			proxyIssue.reason,
			"%s",
			proxyIssue.message,
		)
		return ctrl.Result{}, r.updateStatus(ctx, statusBefore, &console)
	}
	conditions.MarkTrue(
		&console,
		pgtoolboxv1alpha1.PgConsoleConditionProxyConfigReady,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"proxy configuration rendered",
	)
	console.Status.ConfigRevision = render.Revision(checksum)

	deployment, err := r.reconcileResources(ctx, &console, checksum)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcilePgAdminSync(ctx, &console, deployment, checksum, resolvedUsers); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.applyUserStatuses(ctx, resolvedUsers); err != nil {
		return ctrl.Result{}, err
	}

	conditions.MarkTrue(
		&console,
		pgtoolboxv1alpha1.PgConsoleConditionConfigurationValid,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"configuration is valid",
	)
	r.publishWorkloadStatus(&console, deployment)
	return ctrl.Result{}, r.updateStatus(ctx, statusBefore, &console)
}

// reconcileResources converges every owned object, ordered so authority and
// secrets exist before the workload that mounts them.
func (r *Reconciler) reconcileResources(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	checksum string,
) (*appsv1.Deployment, error) {
	inputs := workloadInputs{ConfigChecksum: checksum}

	var err error
	if inputs.ProxyImage, err = resolveImage(console.Spec.Proxy.Image, r.DefaultImages.Proxy); err != nil {
		return nil, err
	}
	if inputs.ConsoleImage, err = resolveImage(console.Spec.Image, r.DefaultImages.PgConsole); err != nil {
		return nil, err
	}
	if pgAdminEnabled(console) {
		if inputs.PgAdminImage, err = resolveImage(console.Spec.PgAdmin.Image, r.DefaultImages.PgAdmin); err != nil {
			return nil, err
		}
	}

	if err := r.reconcileRBAC(ctx, console); err != nil {
		return nil, err
	}

	serviceAccount, err := r.serviceAccount(console)
	if err != nil {
		return nil, err
	}
	if err := r.applyServiceAccount(ctx, serviceAccount); err != nil {
		return nil, err
	}

	if err := r.reconcilePgAdminSettingsPVC(ctx, console); err != nil {
		return nil, err
	}

	if pgAdminEnabled(console) && r.OperatorImage != "" {
		adminSyncSecretVersion, err := r.reconcileAdminSyncSecret(ctx, console)
		if err != nil {
			return nil, err
		}
		inputs.AdminSyncSecretVersion = adminSyncSecretVersion
	}

	composition, tokenSecretName, err := r.reconcileEvidence(ctx, console)
	if err != nil {
		return nil, err
	}
	inputs.Composition = composition
	inputs.TokenSecretName = tokenSecretName
	if composition != nil {
		inputs.ViewerImage = r.viewerImage(console)
	}

	desired, err := r.deployment(console, inputs)
	if err != nil {
		return nil, err
	}
	deployment, err := r.reconcileDeployment(ctx, desired)
	if err != nil {
		return nil, err
	}
	if err := r.collectSupersededTokens(ctx, console, tokenSecretName, deployment); err != nil {
		return nil, err
	}

	service, err := r.service(console)
	if err != nil {
		return nil, err
	}
	if err := r.applyService(ctx, service); err != nil {
		return nil, err
	}

	if err := r.reconcileExposure(ctx, console); err != nil {
		return nil, err
	}
	return deployment, r.reconcileNetworkPolicy(ctx, console)
}

// publishWorkloadStatus derives the workload conditions from the observed
// Deployment. The console is a singleton: one ready, up-to-date replica is
// the whole rollout.
func (r *Reconciler) publishWorkloadStatus(
	console *pgtoolboxv1alpha1.PgConsole,
	deployment *appsv1.Deployment,
) {
	if rolloutComplete(deployment) {
		conditions.MarkTrue(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionReady,
			pgtoolboxv1alpha1.ReasonAsExpected,
			"console is available",
		)
		conditions.MarkFalse(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionProgressing,
			pgtoolboxv1alpha1.ReasonAsExpected,
			"rollout complete",
		)
		return
	}
	conditions.MarkFalse(
		console,
		pgtoolboxv1alpha1.PgConsoleConditionReady,
		pgtoolboxv1alpha1.ReasonRolloutInProgress,
		"console rollout in progress",
	)
	conditions.MarkTrue(
		console,
		pgtoolboxv1alpha1.PgConsoleConditionProgressing,
		pgtoolboxv1alpha1.ReasonRolloutInProgress,
		"console rollout in progress",
	)
}

// updateStatus patches the status only when it semantically changed, so
// steady-state reconciles issue no writes.
func (r *Reconciler) updateStatus(ctx context.Context, before, after *pgtoolboxv1alpha1.PgConsole) error {
	if apiequality.Semantic.DeepEqual(before.Status, after.Status) {
		return nil
	}
	return r.Status().Patch(ctx, after, client.MergeFrom(before))
}

// mapToolBoxResourceToConsole turns a PgToolBoxUser or PgToolBoxRole event
// into a reconcile request for the PgConsole named by its spec.pgConsoleRef.
func (r *Reconciler) mapToolBoxResourceToConsole(_ context.Context, obj client.Object) []reconcile.Request {
	switch resource := obj.(type) {
	case *pgtoolboxv1alpha1.PgToolBoxUser:
		return []reconcile.Request{{NamespacedName: client.ObjectKey{
			Namespace: resource.Namespace,
			Name:      resource.Spec.PgConsoleRef.Name,
		}}}
	case *pgtoolboxv1alpha1.PgToolBoxRole:
		return []reconcile.Request{{NamespacedName: client.ObjectKey{
			Namespace: resource.Namespace,
			Name:      resource.Spec.PgConsoleRef.Name,
		}}}
	default:
		return nil
	}
}

// SetupWithManager wires the controller: the PgConsole itself, its owned
// workload objects, and metadata-only Secret ownership so configuration and
// token content never enters the informer cache.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	controllerBuilder := ctrl.NewControllerManagedBy(mgr).
		For(&pgtoolboxv1alpha1.PgConsole{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&corev1.Secret{}, builder.OnlyMetadata)
	if r.RouteAPIAvailable {
		controllerBuilder = controllerBuilder.Owns(&routev1.Route{})
	}
	if r.GatewayAPIAvailable {
		controllerBuilder = controllerBuilder.Owns(&gatewayv1.HTTPRoute{})
	}
	controllerBuilder = controllerBuilder.
		Watches(
			&pgtoolboxv1alpha1.PgToolBoxUser{},
			handler.EnqueueRequestsFromMapFunc(r.mapToolBoxResourceToConsole),
		).
		Watches(
			&pgtoolboxv1alpha1.PgToolBoxRole{},
			handler.EnqueueRequestsFromMapFunc(r.mapToolBoxResourceToConsole),
		)
	return controllerBuilder.
		Named("pgconsole").
		Complete(r)
}
