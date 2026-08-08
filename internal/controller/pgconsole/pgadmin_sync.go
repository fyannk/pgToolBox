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

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	appsv1 "k8s.io/api/apps/v1"
)

// reconcilePgAdminSync provisions pgAdmin's server list.
//
// It provisions nothing today, and says so. The per-user provisioning it
// used to do was built on a premise that turned out to be wrong:
// PgToolBoxRole and PgToolBoxUser configure the pgtoolbox-proxy and have no
// postgres backing, so there was never a per-user database identity to give
// pgAdmin. That machinery is gone rather than left running on a fiction.
//
// What replaces it is a shared server list built from the cluster's own
// credentials — the application user, the superuser where one is enabled,
// and the owners of any declared databases — visible to every session the
// proxy admits to pgAdmin. Until that lands, the console reports the state
// plainly instead of claiming a sync it is not doing.
func (r *Reconciler) reconcilePgAdminSync(
	_ context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	_ *appsv1.Deployment,
	_ string,
	_ []resolvedConsoleUser,
) error {
	if !pgAdminEnabled(console) {
		conditions.MarkUnknown(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
			pgtoolboxv1alpha1.ReasonNoneConfigured,
			"pgAdmin is not composed into this console",
		)
		return nil
	}
	console.Status.UserSync = pgtoolboxv1alpha1.UserSyncStatus{}
	conditions.MarkUnknown(
		console,
		pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
		pgtoolboxv1alpha1.ReasonNoneConfigured,
		"pgAdmin server provisioning is being rebuilt on the cluster's own credentials",
	)
	return nil
}

// clusterServiceHost resolves the read-write Service host for the cluster.
// It prefers the Cluster status and falls back to the CNPG naming convention.
func clusterServiceHost(cluster *cnpgv1.Cluster, clusterName string) string {
	if cluster != nil && cluster.Status.WriteService != "" {
		return cluster.Status.WriteService + "." + cluster.Namespace + ".svc"
	}
	return clusterName + "-rw." + cluster.Namespace + ".svc"
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
