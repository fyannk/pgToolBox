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

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

	// The recorded revision names the Pod it was applied to, not just the
	// desired state: the credential files the sidecar writes live with the
	// Pod, so a replaced Pod has none of them while a Deployment annotation
	// would happily still match.
	podIdentity, err := r.syncedPodIdentity(ctx, console)
	if err != nil {
		return err
	}
	revision := syncRevision(adminsync.SyncRequest{Servers: servers}) + "@" + podIdentity
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
		"pgAdmin offers %d connection(s) from the cluster's credentials",
		len(servers),
	)
	return nil
}

// syncedPodIdentity returns a value that changes whenever the console Pod
// holding the synced state is replaced, so a rollout, an eviction or a
// crash-restart all invalidate a recorded revision.
func (r *Reconciler) syncedPodIdentity(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(console.Namespace),
		client.MatchingLabels(application.CommonLabels(console.Name)),
	); err != nil {
		return "", fmt.Errorf("list console pods: %w", err)
	}
	identity := ""
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		if uid := string(pod.UID); uid > identity {
			identity = uid
		}
	}
	return identity, nil
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
