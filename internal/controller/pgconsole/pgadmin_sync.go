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
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcilePgAdminSync provisions the connections pgAdmin offers, from the
// credentials the CloudNativePG cluster publishes rather than from whoever
// signed into the console.
func (r *Reconciler) reconcilePgAdminSync(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	deployment *appsv1.Deployment,
	checksum string,
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

	// The checksum lives on the Pod template, not on the Deployment: it is
	// what makes a configuration change roll the pods.
	if deployment.Spec.Template.Annotations[pgtoolboxv1alpha1.ConfigChecksumAnnotation] != checksum {
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

	servers, err := r.clusterCredentials(ctx, console, &cluster)
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		conditions.MarkFalse(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
			pgtoolboxv1alpha1.ReasonSomeDegraded,
			"cluster %s publishes no credential pgAdmin could connect with",
			cluster.Name,
		)
		return nil
	}

	// No short-circuit here. Whether the desired state is already present is
	// something only the sidecar can answer: it lives in files and rows that
	// belong to each pgAdmin account, and accounts appear on their own as
	// people sign in. An operator-side revision could not see a new account
	// arrive, so it would leave that reader with an empty server list until
	// something unrelated changed. The sidecar skips the expensive work
	// per account instead.
	if err := r.AdminSync.Sync(ctx, adminsync.Request{
		Namespace:   console.Namespace,
		ConsoleName: console.Name,
		Selector:    application.SelectorLabels(console.Name),
		Checksum:    checksum,
		Servers:     servers,
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

	conditions.MarkTrue(
		console,
		pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"pgAdmin offers %d connection(s) from the cluster's credentials",
		len(servers),
	)
	return nil
}

// pgAdminResyncInterval is how often a console with pgAdmin re-syncs when
// nothing has changed.
//
// It is not a safety net for a lost write; it exists because pgAdmin
// accounts appear without anything in Kubernetes changing. An account is
// created the first time the proxy forwards an identity pgAdmin has not
// seen, which is a sign-in, not an API event — so no watch fires and no
// reconcile is queued. Without a periodic pass, that reader would sit in
// front of an empty server list until something unrelated moved.
const pgAdminResyncInterval = 2 * time.Minute

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
