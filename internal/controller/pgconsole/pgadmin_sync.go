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

package pgconsole

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcilePgAdminSync drives the in-pod admin-sync sidecar to provision
// pgAdmin accounts and shared server definitions from the already-resolved
// console users. A missing role or credential marks that user degraded but
// does not fail the whole reconcile, so a partially provisioned console still
// reports status.
func (r *Reconciler) reconcilePgAdminSync(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	deployment *appsv1.Deployment,
	checksum string,
	resolved []resolvedConsoleUser,
) error {
	if !pgAdminEnabled(console) || r.OperatorImage == "" || r.AdminSync == nil {
		conditions.MarkUnknown(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
			pgtoolboxv1alpha1.ReasonNoneConfigured,
			"pgAdmin sync is not configured for this console",
		)
		return nil
	}

	if !rolloutComplete(deployment) {
		conditions.MarkFalse(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
			pgtoolboxv1alpha1.ReasonPendingRollout,
			"waiting for console rollout to complete before syncing pgAdmin",
		)
		return nil
	}

	if deployment.Annotations[pgtoolboxv1alpha1.ConfigChecksumAnnotation] != checksum {
		conditions.MarkFalse(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
			pgtoolboxv1alpha1.ReasonPendingRollout,
			"deployment is not at the current configuration revision",
		)
		return nil
	}

	var cluster cnpgv1.Cluster
	clusterKey := client.ObjectKey{Namespace: console.Namespace, Name: console.Spec.CNPGClusterRef.Name}
	if err := r.APIReader.Get(ctx, clusterKey, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			conditions.MarkFalse(
				console,
				pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
				pgtoolboxv1alpha1.ReasonClusterNotFound,
				"CNPG Cluster %s was not found",
				clusterKey.Name,
			)
			return nil
		}
		return err
	}

	syncRequest, degraded := buildSyncRequest(resolved, &cluster)

	console.Status.UserSync = pgtoolboxv1alpha1.UserSyncStatus{
		Desired:  int32Count(len(resolved)),
		Synced:   int32Count(len(resolved) - len(degraded)),
		Degraded: int32Count(len(degraded)),
	}

	if len(degraded) > 0 {
		conditions.MarkFalse(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
			pgtoolboxv1alpha1.ReasonSomeDegraded,
			"%d user(s) could not be synced: %v",
			len(degraded), degraded,
		)
		// Continue only with the users we can provision.
	}

	if len(syncRequest.Users) == 0 {
		if len(degraded) == 0 {
			conditions.MarkTrue(
				console,
				pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
				pgtoolboxv1alpha1.ReasonAsExpected,
				"no pgAdmin users configured",
			)
		}
		return nil
	}

	revision := syncRevision(syncRequest)
	if deployment.Annotations[pgtoolboxv1alpha1.PgAdminSyncRevisionAnnotation] == revision {
		conditions.MarkTrue(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
			pgtoolboxv1alpha1.ReasonAsExpected,
			"pgAdmin sync is up to date",
		)
		return nil
	}

	if err := r.AdminSync.Sync(ctx, adminsync.Request{
		Namespace:   console.Namespace,
		ConsoleName: console.Name,
		Selector:    application.SelectorLabels(console.Name),
		Checksum:    checksum,
		Users:       syncRequest.Users,
	}); err != nil {
		conditions.MarkFalse(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
			pgtoolboxv1alpha1.ReasonSyncFailed,
			"pgAdmin sync failed: %v",
			err,
		)
		return nil
	}

	before := deployment.DeepCopy()
	if deployment.Annotations == nil {
		deployment.Annotations = map[string]string{}
	}
	deployment.Annotations[pgtoolboxv1alpha1.PgAdminSyncRevisionAnnotation] = revision
	if err := r.Patch(ctx, deployment, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("record pgAdmin sync revision: %w", err)
	}

	conditions.MarkTrue(
		console,
		pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"pgAdmin synced for %d user(s)",
		len(syncRequest.Users),
	)
	return nil
}

// buildSyncRequest builds the admin-sync payload from the resolved console
// users that are eligible for pgAdmin provisioning. It returns the payload and
// the list of degraded user subjects.
func buildSyncRequest(
	resolved []resolvedConsoleUser,
	cluster *cnpgv1.Cluster,
) (adminsync.SyncRequest, []string) {
	var request adminsync.SyncRequest
	var degraded []string

	host := clusterServiceHost(cluster, cluster.Name)

	for _, u := range resolved {
		if !u.pgAdminIncluded() {
			degraded = append(degraded, u.user.Spec.Subject)
			continue
		}
		request.Users = append(request.Users, adminsync.User{
			Subject:     u.user.Spec.Subject,
			PgAdminRole: pgAdminRoleForLevel(u.role.Spec.Level),
			Server: adminsync.Server{
				Name:          cluster.Name,
				Group:         "PgToolBox",
				Host:          host,
				Port:          5432,
				MaintenanceDB: "postgres",
				Username:      u.credential.username,
				PassFile:      adminsync.DefaultPassFilePath,
				SSLMode:       "prefer",
			},
			Password: u.credential.password,
		})
	}
	return request, degraded
}

// clusterServiceHost resolves the read-write Service host for the cluster.
// It prefers the Cluster status and falls back to the CNPG naming convention.
func clusterServiceHost(cluster *cnpgv1.Cluster, clusterName string) string {
	if cluster != nil && cluster.Status.WriteService != "" {
		return cluster.Status.WriteService + "." + cluster.Namespace + ".svc"
	}
	return clusterName + "-rw." + cluster.Namespace + ".svc"
}

type roleCredential struct {
	username string
	password string
}

// databaseRoleNameForRole returns the name of the CNPG DatabaseRole backing
// the PgToolBoxRole. It prefers the status set by the role controller, then
// an explicit databaseRoleRef. An empty return means the role has not been
// resolved yet.
func databaseRoleNameForRole(role *pgtoolboxv1alpha1.PgToolBoxRole) string {
	if role.Status.DatabaseRoleName != "" {
		return role.Status.DatabaseRoleName
	}
	if role.Spec.PostgresRole.DatabaseRoleRef != nil {
		return role.Spec.PostgresRole.DatabaseRoleRef.Name
	}
	return ""
}

// pgAdminRoleForLevel maps a PgToolBox level to a pgAdmin role.
func pgAdminRoleForLevel(level pgtoolboxv1alpha1.RoleLevel) string {
	if level == pgtoolboxv1alpha1.RoleLevelDBA {
		return "Administrator"
	}
	return "User"
}

// syncRevision returns a stable sha256 digest of the sync payload so the
// operator can skip re-applying an unchanged desired state.
func syncRevision(request adminsync.SyncRequest) string {
	payload, _ := json.Marshal(request)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// int32Count clamps a count into the int32 status fields.
func int32Count(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n) // #nosec G115 -- clamped above
}
