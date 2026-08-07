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

// Package pgtoolboxrole reconciles a PgToolBoxRole into the CNPG DatabaseRole
// and password Secret that back it. It never opens a PostgreSQL connection:
// CloudNativePG applies the DatabaseRole and the operator only observes it.
package pgtoolboxrole

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const managedPasswordRandomBytes = 24

// Reconciler reconciles a PgToolBoxRole resource.
type Reconciler struct {
	shared.Runtime
}

// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxroles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxroles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgtoolboxroles/finalizers,verbs=update
// +kubebuilder:rbac:groups=pgtoolbox.fyannk.dev,resources=pgconsoles,verbs=get;list;watch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=databaseroles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile converges one PgToolBoxRole: it validates the referenced PgConsole,
// materializes a managed DatabaseRole + Secret for profile-based roles, or
// validates a referenced DatabaseRole, and reflects the outcome in status.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var role pgtoolboxv1alpha1.PgToolBoxRole
	if err := r.Get(ctx, req.NamespacedName, &role); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !role.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&role, pgtoolboxv1alpha1.PgToolBoxRoleFinalizer) {
			return ctrl.Result{}, nil
		}
		if err := r.cleanupManagedObjects(ctx, &role); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&role, pgtoolboxv1alpha1.PgToolBoxRoleFinalizer)
		return ctrl.Result{}, r.Update(ctx, &role)
	}

	if role.Annotations[pgtoolboxv1alpha1.ReconcileAnnotation] == "skip" {
		log.Info("reconciliation skipped by annotation")
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&role, pgtoolboxv1alpha1.PgToolBoxRoleFinalizer) {
		controllerutil.AddFinalizer(&role, pgtoolboxv1alpha1.PgToolBoxRoleFinalizer)
		return ctrl.Result{}, r.Update(ctx, &role)
	}

	before := role.DeepCopy()
	role.Status.ObservedGeneration = role.GetGeneration()

	console, err := r.resolvePgConsole(ctx, &role)
	if err != nil {
		return ctrl.Result{}, err
	}
	if console == nil {
		conditions.MarkFalse(
			&role,
			pgtoolboxv1alpha1.RoleConditionPgConsoleReady,
			pgtoolboxv1alpha1.ReasonPgConsoleNotFound,
			"referenced PgConsole %s was not found",
			role.Spec.PgConsoleRef.Name,
		)
		return ctrl.Result{}, r.updateStatus(ctx, before, &role)
	}
	conditions.MarkTrue(
		&role,
		pgtoolboxv1alpha1.RoleConditionPgConsoleReady,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"referenced PgConsole %s exists",
		console.Name,
	)

	if role.Spec.PostgresRole.DatabaseRoleRef != nil {
		if err := r.reconcileDatabaseRoleRef(ctx, &role); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		if err := r.reconcileManagedRole(ctx, &role, console); err != nil {
			return ctrl.Result{}, err
		}
	}

	ready := meta.IsStatusConditionTrue(role.Status.Conditions, pgtoolboxv1alpha1.RoleConditionPgConsoleReady) &&
		meta.IsStatusConditionTrue(role.Status.Conditions, pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady) &&
		meta.IsStatusConditionTrue(role.Status.Conditions, pgtoolboxv1alpha1.RoleConditionCredentialReady)
	if ready {
		conditions.MarkTrue(
			&role,
			pgtoolboxv1alpha1.RoleConditionReady,
			pgtoolboxv1alpha1.ReasonAsExpected,
			"PgToolBoxRole is ready",
		)
	} else {
		conditions.MarkFalse(
			&role,
			pgtoolboxv1alpha1.RoleConditionReady,
			pgtoolboxv1alpha1.ReasonReconciling,
			"PgToolBoxRole is not yet ready",
		)
	}

	return ctrl.Result{}, r.updateStatus(ctx, before, &role)
}

// resolvePgConsole fetches the PgConsole referenced by the role. A nil result
// with nil error means the console does not exist.
func (r *Reconciler) resolvePgConsole(ctx context.Context, role *pgtoolboxv1alpha1.PgToolBoxRole) (*pgtoolboxv1alpha1.PgConsole, error) {
	var console pgtoolboxv1alpha1.PgConsole
	key := client.ObjectKey{Namespace: role.Namespace, Name: role.Spec.PgConsoleRef.Name}
	if err := r.Get(ctx, key, &console); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &console, nil
}

// reconcileDatabaseRoleRef validates the user-supplied DatabaseRole and uses
// it directly. The operator does not manage its Secret.
func (r *Reconciler) reconcileDatabaseRoleRef(ctx context.Context, role *pgtoolboxv1alpha1.PgToolBoxRole) error {
	refName := role.Spec.PostgresRole.DatabaseRoleRef.Name
	var databaseRole cnpgv1.DatabaseRole
	key := client.ObjectKey{Namespace: role.Namespace, Name: refName}
	if err := r.APIReader.Get(ctx, key, &databaseRole); err != nil {
		if apierrors.IsNotFound(err) {
			role.Status.DatabaseRoleName = ""
			conditions.MarkFalse(
				role,
				pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady,
				pgtoolboxv1alpha1.ReasonDatabaseRolePending,
				"referenced DatabaseRole %s was not found",
				refName,
			)
			conditions.MarkFalse(
				role,
				pgtoolboxv1alpha1.RoleConditionCredentialReady,
				pgtoolboxv1alpha1.ReasonDatabaseRolePending,
				"referenced DatabaseRole %s was not found",
				refName,
			)
			return nil
		}
		return err
	}
	role.Status.DatabaseRoleName = refName
	conditions.MarkTrue(
		role,
		pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"referenced DatabaseRole %s exists",
		refName,
	)
	if databaseRole.Spec.PasswordSecret == nil || databaseRole.Spec.PasswordSecret.Name == "" {
		conditions.MarkFalse(
			role,
			pgtoolboxv1alpha1.RoleConditionCredentialReady,
			pgtoolboxv1alpha1.ReasonSecretNotFound,
			"referenced DatabaseRole %s has no passwordSecret",
			refName,
		)
		return nil
	}
	conditions.MarkTrue(
		role,
		pgtoolboxv1alpha1.RoleConditionCredentialReady,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"referenced DatabaseRole %s names a password Secret",
		refName,
	)
	return nil
}

// reconcileManagedRole converges the operator-owned Secret and DatabaseRole
// for a profile-based PgToolBoxRole.
func (r *Reconciler) reconcileManagedRole(
	ctx context.Context,
	role *pgtoolboxv1alpha1.PgToolBoxRole,
	console *pgtoolboxv1alpha1.PgConsole,
) error {
	postgresRoleName := managedPostgresRoleName(role)

	credential, err := r.reconcileManagedCredentialSecret(ctx, role, postgresRoleName)
	if err != nil {
		if isSecretFormatError(err) {
			conditions.MarkFalse(
				role,
				pgtoolboxv1alpha1.RoleConditionCredentialReady,
				pgtoolboxv1alpha1.ReasonSecretFormatInvalid,
				"managed credential Secret is not valid basic-auth material",
			)
			return nil
		}
		return err
	}

	databaseRole, changed, err := r.reconcileManagedDatabaseRole(ctx, role, console, postgresRoleName)
	if err != nil {
		return err
	}

	role.Status.DatabaseRoleName = databaseRole.Name

	applied := !changed && databaseRole.Status.Applied != nil && *databaseRole.Status.Applied &&
		databaseRole.Status.ObservedGeneration >= databaseRole.Generation &&
		databaseRole.Status.SecretResourceVersion == credential.ResourceVersion

	if applied {
		conditions.MarkTrue(
			role,
			pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady,
			pgtoolboxv1alpha1.ReasonAsExpected,
			"managed DatabaseRole %s is applied",
			databaseRole.Name,
		)
		conditions.MarkTrue(
			role,
			pgtoolboxv1alpha1.RoleConditionCredentialReady,
			pgtoolboxv1alpha1.ReasonAsExpected,
			"managed credential Secret is current",
		)
		return nil
	}

	if databaseRole.Status.Applied != nil && !*databaseRole.Status.Applied &&
		databaseRole.Status.ObservedGeneration >= databaseRole.Generation {
		conditions.MarkFalse(
			role,
			pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady,
			pgtoolboxv1alpha1.ReasonDatabaseRoleFailed,
			"managed DatabaseRole %s was not applied by CloudNativePG",
			databaseRole.Name,
		)
	} else {
		conditions.MarkFalse(
			role,
			pgtoolboxv1alpha1.RoleConditionDatabaseRoleReady,
			pgtoolboxv1alpha1.ReasonDatabaseRolePending,
			"waiting for CloudNativePG to apply managed DatabaseRole %s",
			databaseRole.Name,
		)
	}
	conditions.MarkFalse(
		role,
		pgtoolboxv1alpha1.RoleConditionCredentialReady,
		pgtoolboxv1alpha1.ReasonDatabaseRolePending,
		"waiting for CloudNativePG to apply the managed DatabaseRole credential",
	)
	return nil
}

func isSecretFormatError(err error) bool {
	var target *shared.SecretFormatError
	return errors.As(err, &target)
}

// reconcileManagedCredentialSecret returns the managed credential, creating
// the basic-auth Secret with a freshly generated password when it does not
// exist. An existing Secret is rejected unless this role controls it and its
// username matches the managed role name.
func (r *Reconciler) reconcileManagedCredentialSecret(
	ctx context.Context,
	role *pgtoolboxv1alpha1.PgToolBoxRole,
	roleName string,
) (*shared.BasicAuthCredential, error) {
	key := client.ObjectKey{
		Namespace: role.Namespace,
		Name:      managedCredentialSecretName(role),
	}
	credential, err := shared.ReadBasicAuthCredential(ctx, r.APIReader, key)
	if err == nil {
		var secret corev1.Secret
		if getErr := r.APIReader.Get(ctx, key, &secret); getErr != nil {
			return nil, getErr
		}
		if !controlledBy(&secret, role) || credential.Username != roleName {
			return nil, fmt.Errorf("managed credential Secret %s/%s does not match its role", key.Namespace, key.Name)
		}
		return credential, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	password, err := generateManagedPassword()
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    managedObjectLabels(role),
		},
		Type: corev1.SecretTypeBasicAuth,
		Data: map[string][]byte{
			corev1.BasicAuthUsernameKey: []byte(roleName),
			corev1.BasicAuthPasswordKey: []byte(password),
		},
	}
	if err := controllerutil.SetControllerReference(role, secret, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, secret); err != nil {
		return nil, err
	}
	return &shared.BasicAuthCredential{
		Username:        roleName,
		Password:        password,
		ResourceVersion: secret.ResourceVersion,
	}, nil
}

// reconcileManagedDatabaseRole converges the CNPG DatabaseRole. The returned
// boolean reports whether a write happened, in which case the applied status
// is stale and must not be trusted until CNPG re-acknowledges it.
func (r *Reconciler) reconcileManagedDatabaseRole(
	ctx context.Context,
	role *pgtoolboxv1alpha1.PgToolBoxRole,
	console *pgtoolboxv1alpha1.PgConsole,
	roleName string,
) (*cnpgv1.DatabaseRole, bool, error) {
	desired, err := r.buildManagedDatabaseRole(role, console, roleName)
	if err != nil {
		return nil, false, err
	}
	var existing cnpgv1.DatabaseRole
	key := client.ObjectKeyFromObject(desired)
	err = r.APIReader.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, false, err
		}
		return desired, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !controlledBy(&existing, role) {
		return nil, false, fmt.Errorf("managed DatabaseRole %s/%s is not owned by its role", key.Namespace, key.Name)
	}
	if reflect.DeepEqual(existing.Spec, desired.Spec) && shared.LabelsContain(existing.Labels, desired.Labels) {
		return &existing, false, nil
	}
	before := existing.DeepCopy()
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	if err := r.Patch(ctx, &existing, client.MergeFrom(before)); err != nil {
		return nil, false, err
	}
	return &existing, true, nil
}

// buildManagedDatabaseRole builds the desired DatabaseRole for the role's
// profile. The password is referenced through the credential Secret, never
// embedded, and the build is deterministic.
func (r *Reconciler) buildManagedDatabaseRole(
	role *pgtoolboxv1alpha1.PgToolBoxRole,
	console *pgtoolboxv1alpha1.PgConsole,
	roleName string,
) (*cnpgv1.DatabaseRole, error) {
	var inRoles []string
	createDB := false
	createRole := false
	switch role.Spec.PostgresRole.Profile {
	case pgtoolboxv1alpha1.PostgresRoleProfileMonitor:
		inRoles = []string{"pg_monitor"}
	case pgtoolboxv1alpha1.PostgresRoleProfileDatabaseReadonly:
		inRoles = []string{"pg_read_all_data"}
	case pgtoolboxv1alpha1.PostgresRoleProfileDatabaseOwner:
		createDB = true
		createRole = true
	default:
		return nil, fmt.Errorf("unknown postgres role profile %q", role.Spec.PostgresRole.Profile)
	}

	inherit := true
	databaseRole := &cnpgv1.DatabaseRole{
		TypeMeta: metav1.TypeMeta{APIVersion: cnpgv1.SchemeGroupVersion.String(), Kind: "DatabaseRole"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      managedDatabaseRoleName(role),
			Namespace: role.Namespace,
			Labels:    managedObjectLabels(role),
		},
		Spec: cnpgv1.DatabaseRoleSpec{
			RoleConfiguration: cnpgv1.RoleConfiguration{
				Name:            roleName,
				Ensure:          cnpgv1.EnsurePresent,
				PasswordSecret:  &cnpgv1.LocalObjectReference{Name: managedCredentialSecretName(role)},
				ConnectionLimit: -1,
				InRoles:         inRoles,
				Inherit:         &inherit,
				Login:           true,
				CreateDB:        createDB,
				CreateRole:      createRole,
			},
			ClusterRef:    corev1.LocalObjectReference{Name: console.Spec.CNPGClusterRef.Name},
			ReclaimPolicy: cnpgv1.DatabaseRoleReclaimRetain,
		},
	}
	if err := controllerutil.SetControllerReference(role, databaseRole, r.Scheme); err != nil {
		return nil, err
	}
	return databaseRole, nil
}

// cleanupManagedObjects deletes the Secret and DatabaseRole owned by this
// profile-based role. Objects not owned by the role are left alone.
func (r *Reconciler) cleanupManagedObjects(ctx context.Context, role *pgtoolboxv1alpha1.PgToolBoxRole) error {
	objects := []client.Object{
		&cnpgv1.DatabaseRole{ObjectMeta: metav1.ObjectMeta{
			Name: managedDatabaseRoleName(role), Namespace: role.Namespace,
		}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: managedCredentialSecretName(role), Namespace: role.Namespace,
		}},
	}
	for _, object := range objects {
		var current client.Object
		switch o := object.(type) {
		case *cnpgv1.DatabaseRole:
			current = &cnpgv1.DatabaseRole{ObjectMeta: o.ObjectMeta}
		case *corev1.Secret:
			current = &corev1.Secret{ObjectMeta: o.ObjectMeta}
		}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !controlledBy(current, role) {
			continue
		}
		if err := r.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// managedPostgresRoleName returns the PostgreSQL role name generated for a
// profile-based role. It is intentionally the same as the DatabaseRole object
// name so the PgConsole controller does not need a second lookup.
func managedPostgresRoleName(role *pgtoolboxv1alpha1.PgToolBoxRole) string {
	return managedDatabaseRoleName(role)
}

// managedDatabaseRoleName names the generated DatabaseRole object.
func managedDatabaseRoleName(role *pgtoolboxv1alpha1.PgToolBoxRole) string {
	return shared.BoundedName(role.Name, "-pgrole")
}

// managedCredentialSecretName returns the generated credential Secret name.
func managedCredentialSecretName(role *pgtoolboxv1alpha1.PgToolBoxRole) string {
	return shared.BoundedName(role.Name, "-pgrole-credentials")
}

// managedObjectLabels ties generated objects to the owning console.
func managedObjectLabels(role *pgtoolboxv1alpha1.PgToolBoxRole) map[string]string {
	return shared.PgConsoleApplication().CommonLabels(role.Spec.PgConsoleRef.Name)
}

// controlledBy reports whether object is controller-owned by role.
func controlledBy(object metav1.Object, role *pgtoolboxv1alpha1.PgToolBoxRole) bool {
	owner := metav1.GetControllerOf(object)
	return owner != nil && owner.UID == role.UID
}

// generateManagedPassword produces a 32-character password from crypto-grade
// randomness.
func generateManagedPassword() (string, error) {
	random := make([]byte, managedPasswordRandomBytes)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate managed credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}

// updateStatus patches status only when it semantically changed.
func (r *Reconciler) updateStatus(ctx context.Context, before, after *pgtoolboxv1alpha1.PgToolBoxRole) error {
	if apiequality.Semantic.DeepEqual(before.Status, after.Status) {
		return nil
	}
	return r.Status().Patch(ctx, after, client.MergeFrom(before))
}

// mapConsoleToRoles enqueues every PgToolBoxRole that references a PgConsole,
// so a role created before its console becomes ready once the console appears.
func (r *Reconciler) mapConsoleToRoles(ctx context.Context, obj client.Object) []reconcile.Request {
	console, ok := obj.(*pgtoolboxv1alpha1.PgConsole)
	if !ok {
		return nil
	}
	var list pgtoolboxv1alpha1.PgToolBoxRoleList
	if err := r.List(ctx, &list, client.InNamespace(console.Namespace)); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.PgConsoleRef.Name == console.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
			})
		}
	}
	return requests
}

// SetupWithManager wires the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pgtoolboxv1alpha1.PgToolBoxRole{}).
		Owns(&corev1.Secret{}).
		Owns(&cnpgv1.DatabaseRole{}).
		Watches(
			&pgtoolboxv1alpha1.PgConsole{},
			handler.EnqueueRequestsFromMapFunc(r.mapConsoleToRoles),
		).
		Named("pgtoolboxrole").
		Complete(r)
}
