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
	"net/url"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/cnpgcred"
	"github.com/fyannk/pgToolBox/internal/operator/metadata"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// configDatabase: pgAdmin's settings database
// on a CloudNativePG Cluster of the PgAdmin's namespace, through pgAdmin's
// own CONFIG_DATABASE_URI. The operator owns two CloudNativePG objects: a
// DatabaseRole whose password CloudNativePG generates (spec.password mode
// generate, CloudNativePG 1.31) into a Secret it owns, and a Database owned
// by that role. The operator never reads the password: the Secret is named
// in the offer like any other credential, the passfile-sync sidecar writes
// it into the Pod's pgpass, and libpq reads it there when pgAdmin opens
// the URI, which carries no password. Moving an existing SQLite database
// into PostgreSQL is a manual step with pgAdmin's own tooling; the CRD
// refuses adding or removing configDatabase on an existing PgAdmin.

const (
	componentConfigDatabase = "config-database"
	// configDatabaseService is the libpq service of the settings database
	// in the credential files (offer services match ^pgtoolbox-).
	configDatabaseService = "pgtoolbox-config-database"
	// configDatabaseServerName is its display name (never registered).
	configDatabaseServerName = "pgAdmin settings database"
)

// configDatabaseState is the outcome of the settings database step.
type configDatabaseState struct {
	// Server is the offer entry once the role, its generated password and
	// the database are applied; nil before.
	Server *offer.Server
	// URI is pgAdmin's CONFIG_DATABASE_URI (no password in it).
	URI string
	// UID identifies the database (a new one is an empty settings
	// database: registration starts over).
	UID types.UID
	// Waiting says why the settings database is not usable yet.
	Waiting string
	// Problem is a configuration problem (ConfigurationValid=False with
	// ProblemReason), not a stall: a watch brings the fix.
	Problem       string
	ProblemReason pgtoolboxv1alpha1.Reason
}

// configRoleName is the name of the DatabaseRole object.
func configRoleName(p *pgtoolboxv1alpha1.PgAdmin) string {
	return metadata.BoundedName(p.Name, "-pgadmin-config")
}

// configPasswordSecretName is the Secret CloudNativePG generates the
// role's password into: named explicitly rather than left to its default,
// so the offer never guesses.
func configPasswordSecretName(p *pgtoolboxv1alpha1.PgAdmin) string {
	return metadata.BoundedName(p.Name, "-pgadmin-config-password")
}

// buildConfigRole is the DatabaseRole, unstructured: spec.password is newer
// than the vendored CloudNativePG API (1.30).
func buildConfigRole(p *pgtoolboxv1alpha1.PgAdmin) *unstructured.Unstructured {
	spec := p.Spec.ConfigDatabase
	role := spec.DatabaseOrDefault(p.Name)
	meta := objectMeta(p, configRoleName(p), componentConfigDatabase, pgtoolboxv1alpha1.PropagationTargetAll)
	object := map[string]any{
		"apiVersion": cnpgv1.SchemeGroupVersion.String(),
		"kind":       "DatabaseRole",
		"metadata":   metaMap(meta),
		"spec": map[string]any{
			"cluster": map[string]any{"name": spec.ClusterRef.Name},
			"name":    role,
			"ensure":  "present",
			"login":   true,
			"comment": "pgAdmin settings database of PgAdmin " + p.Name + " (pgtoolbox)",
			// Kept in PostgreSQL when the PgAdmin goes: its database holds
			// the settings, and dropping a role is the DBA's call.
			"databaseRoleReclaimPolicy": "retain",
			"password": map[string]any{
				"mode":   "generate",
				"secret": configPasswordSecretName(p),
			},
		},
	}
	u := &unstructured.Unstructured{Object: object}
	specs.SetHash(u, specs.Hash(object["spec"]))
	return u
}

// buildConfigDatabase is the Database, owned by the role.
func buildConfigDatabase(p *pgtoolboxv1alpha1.PgAdmin) *cnpgv1.Database {
	spec := p.Spec.ConfigDatabase
	name := spec.DatabaseOrDefault(p.Name)
	database := &cnpgv1.Database{
		TypeMeta:   metav1.TypeMeta{APIVersion: cnpgv1.SchemeGroupVersion.String(), Kind: "Database"},
		ObjectMeta: objectMeta(p, configRoleName(p), componentConfigDatabase, pgtoolboxv1alpha1.PropagationTargetAll),
		Spec: cnpgv1.DatabaseSpec{
			ClusterRef:    corev1.LocalObjectReference{Name: spec.ClusterRef.Name},
			Name:          name,
			Owner:         name,
			Ensure:        cnpgv1.EnsurePresent,
			ReclaimPolicy: cnpgv1.DatabaseReclaimRetain,
		},
	}
	specs.SetHash(database, specs.Hash(database.Spec))
	return database
}

// metaMap renders ObjectMeta for an unstructured object.
func metaMap(meta metav1.ObjectMeta) map[string]any {
	raw, err := json.Marshal(meta)
	if err != nil {
		panic(fmt.Sprintf("marshal ObjectMeta: %v", err))
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(fmt.Sprintf("unmarshal ObjectMeta: %v", err))
	}
	delete(out, "creationTimestamp")
	return out
}

// configDatabaseURI is CONFIG_DATABASE_URI: host, port, database and user,
// verify-full against the Cluster's CA file the sidecar writes, and no
// password (libpq finds it in PGPASSFILE).
func configDatabaseURI(server *offer.Server) string {
	query := url.Values{}
	query.Set("sslmode", offer.SSLMode)
	query.Set("sslrootcert", offer.CAFile(credentialsMountPath, server.CAKey()))
	u := url.URL{
		Scheme:   "postgresql",
		User:     url.User(server.User),
		Host:     fmt.Sprintf("%s:%d", server.Host, server.Port),
		Path:     "/" + server.DBName,
		RawQuery: query.Encode(),
	}
	return u.String()
}

// reconcileConfigDatabase applies the role and the database and reports
// whether CloudNativePG applied both, with the password generated.
func (r *Reconciler) reconcileConfigDatabase(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) (configDatabaseState, error) {
	spec := p.Spec.ConfigDatabase
	var state configDatabaseState
	stall := func(reason pgtoolboxv1alpha1.Reason, format string, args ...any) error {
		message := fmt.Sprintf(format, args...)
		set(p, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, reason, "%s", message)
		return reconcile.Stall(reason, "%s", message)
	}
	switch {
	case !r.Features.In(p.Namespace).Databases.Usable():
		return state, stall(pgtoolboxv1alpha1.ReasonDatabaseAPIUnavailable,
			"spec.configDatabase needs the CloudNativePG Database API: %s", unavailableWhy(r.Features.In(p.Namespace).Databases))
	case !r.Features.In(p.Namespace).DatabaseRoles.Usable():
		return state, stall(pgtoolboxv1alpha1.ReasonDatabaseRoleAPIUnavailable,
			"spec.configDatabase needs the CloudNativePG DatabaseRole API: %s", unavailableWhy(r.Features.In(p.Namespace).DatabaseRoles))
	}
	var cluster cnpgv1.Cluster
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: spec.ClusterRef.Name}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			// Not stalled: the Cluster watch brings the next loop.
			state.Problem = fmt.Sprintf("spec.configDatabase.clusterRef: no Cluster %s in namespace %s", spec.ClusterRef.Name, p.Namespace)
			state.ProblemReason = pgtoolboxv1alpha1.ReasonClusterNotFound
			state.Waiting = "the configDatabase Cluster " + spec.ClusterRef.Name + " does not exist"
			return state, nil
		}
		return state, err
	}

	if conflict, err := r.configRoleConflict(ctx, p, &cluster); err != nil {
		return state, err
	} else if conflict != "" {
		// Applying would take over a role someone else manages (its
		// password regenerated, its database shared): never.
		return state, stall(pgtoolboxv1alpha1.ReasonConfigurationInvalid, "%s", conflict)
	}
	if err := r.applyConfigRole(ctx, p); err != nil {
		switch {
		case passwordFieldUnknown(err):
			// The API server refuses spec.password (server-side apply finds
			// it "not declared in schema", strict field validation an
			// "unknown field"): this CloudNativePG cannot generate the
			// password, and the operator never holds one.
			return state, stall(pgtoolboxv1alpha1.ReasonPasswordGenerationUnavailable,
				"spec.configDatabase needs CloudNativePG to generate the role's password (DatabaseRole spec.password, CloudNativePG 1.31 and later): %s",
				reconcile.BoundedMessage(err.Error()))
		case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
			return state, stall(pgtoolboxv1alpha1.ReasonConfigurationInvalid,
				"the settings database role was refused: %s", reconcile.BoundedMessage(err.Error()))
		}
		return state, r.notOwned(p, err)
	}
	database := buildConfigDatabase(p)
	if err := r.applier().Apply(ctx, database); err != nil {
		return state, r.notOwned(p, err)
	}

	var role cnpgv1.DatabaseRole
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: configRoleName(p)}, &role); err != nil {
		if apierrors.IsNotFound(err) {
			state.Waiting = "the settings database role is not observed yet"
			return state, nil
		}
		return state, err
	}
	var applied cnpgv1.Database
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(database), &applied); err != nil {
		if apierrors.IsNotFound(err) {
			state.Waiting = "the settings database is not observed yet"
			return state, nil
		}
		return state, err
	}
	state.UID = applied.UID
	switch {
	case role.Status.Applied == nil || !*role.Status.Applied || role.Status.ObservedGeneration < role.Generation:
		state.Waiting = "CloudNativePG has not applied the settings database role yet" + statusMessage(role.Status.Message)
	case role.Status.SecretResourceVersion == "":
		state.Waiting = "CloudNativePG has not generated the settings database role's password yet"
	case applied.Status.Applied == nil || !*applied.Status.Applied || applied.Status.ObservedGeneration < applied.Generation:
		state.Waiting = "CloudNativePG has not created the settings database yet" + statusMessage(applied.Status.Message)
	}
	if state.Waiting != "" {
		return state, nil
	}
	name := spec.DatabaseOrDefault(p.Name)
	host, port, _ := endpointHost(&cluster, pgtoolboxv1alpha1.EndpointRW, nil, false)
	state.Server = &offer.Server{
		Name:                  configDatabaseServerName,
		Cluster:               cluster.Name,
		Endpoint:              string(pgtoolboxv1alpha1.EndpointRW),
		Host:                  host,
		Port:                  port,
		DBName:                name,
		User:                  name,
		Service:               configDatabaseService,
		SecretName:            configPasswordSecretName(p),
		SecretResourceVersion: role.Status.SecretResourceVersion,
		CASecretName:          cnpgcred.CASecretName(&cluster),
	}
	state.URI = configDatabaseURI(state.Server)
	return state, nil
}

// configRoleConflict says why the settings database role may not be this
// PgAdmin's: it is the Cluster's application owner or superuser, a role the
// Cluster declares in spec.managed.roles, or the role of a DatabaseRole
// this PgAdmin does not control (another PgAdmin's, a person's, a DBA's).
func (r *Reconciler) configRoleConflict(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin, cluster *cnpgv1.Cluster) (string, error) {
	name := p.Spec.ConfigDatabase.DatabaseOrDefault(p.Name)
	field := "spec.configDatabase.database " + name
	switch {
	case name == cnpgcred.ApplicationOwner(cluster) || name == cnpgcred.ApplicationDatabase(cluster):
		return field + " is the Cluster's application owner or database", nil
	case name == superuserRoleName:
		return field + " is the superuser", nil
	}
	if cluster.Spec.Managed != nil {
		for _, role := range cluster.Spec.Managed.Roles {
			if role.Name == name {
				return field + " is a role the Cluster declares in spec.managed.roles", nil
			}
		}
	}
	var roles cnpgv1.DatabaseRoleList
	if err := r.Client.List(ctx, &roles, client.InNamespace(p.Namespace)); err != nil {
		return "", fmt.Errorf("list DatabaseRoles: %w", err)
	}
	for i := range roles.Items {
		role := &roles.Items[i]
		if role.Spec.ClusterRef.Name != cluster.Name || cnpgcred.DatabaseRoleName(role) != name {
			continue
		}
		if owner := metav1.GetControllerOf(role); owner == nil || owner.UID != p.UID {
			return field + " is already managed by DatabaseRole " + role.Name, nil
		}
	}
	return "", nil
}

// applyConfigRole applies the unstructured DatabaseRole with strict field
// validation, after the same control check every child gets.
func (r *Reconciler) applyConfigRole(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) error {
	desired := buildConfigRole(p)
	var existing cnpgv1.DatabaseRole
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: desired.GetName()}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		live := &metav1.PartialObjectMetadata{}
		live.SetGroupVersionKind(cnpgv1.SchemeGroupVersion.WithKind("DatabaseRole"))
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), live); err == nil {
			if !reconcile.Controls(live, desired, false) {
				return &reconcile.NotOwnedError{Kind: "DatabaseRole", Namespace: p.Namespace, Name: desired.GetName()}
			}
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	case err != nil:
		return err
	case !reconcile.Controls(&existing, desired, false):
		return &reconcile.NotOwnedError{Kind: "DatabaseRole", Namespace: p.Namespace, Name: desired.GetName()}
	}
	raw, err := json.Marshal(desired.Object)
	if err != nil {
		return err
	}
	return r.Client.Patch(ctx, desired, client.RawPatch(types.ApplyPatchType, raw),
		client.FieldOwner(reconcile.FieldOwner), client.ForceOwnership, client.FieldValidation("Strict"))
}

// passwordFieldUnknown reports an API error refusing spec.password as a
// field the DatabaseRole schema does not have.
func passwordFieldUnknown(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "spec.password") &&
		(strings.Contains(message, "not declared in schema") || strings.Contains(message, "unknown field"))
}

func statusMessage(message string) string {
	if message == "" {
		return ""
	}
	return ": " + reconcile.BoundedMessage(message)
}
