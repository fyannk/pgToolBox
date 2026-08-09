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
	"sort"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The connections pgAdmin offers come from the CloudNativePG cluster, not
// from whoever signed into the console. A PgToolBoxUser is a proxy
// authorization level with no postgres backing, so there is no per-person
// database identity to hand out; what there is, is the set of credentials
// the cluster itself publishes as Secrets.
//
// Everyone who reaches pgAdmin sees all of them. That is not a widening:
// the proxy admits nobody below spec.pgAdmin.accessMinLevel, which defaults
// to dba, and a dba can already read these Secrets from the namespace.

const (
	// postgresServicePort is where a CNPG cluster listens.
	postgresServicePort int32 = 5432

	// serverGroup is the pgAdmin group every generated connection lands in,
	// so an operator's entries stay distinguishable from a reader's own.
	serverGroup = "PgToolBox"

	// CNPG publishes credentials as Secrets with these keys.
	credentialUsernameKey = "username"
	credentialPasswordKey = "password" // #nosec G101 -- Secret key name.
	credentialDatabaseKey = "dbname"
)

// clusterCredentials resolves every connection pgAdmin should offer for one
// cluster, ordered so a no-op reconcile renders an identical request.
//
// A credential that cannot be read is skipped rather than failed: a cluster
// with no superuser access enabled is the normal case, not an error, and a
// Secret that has not appeared yet resolves on a later reconcile.
func (r *Reconciler) clusterCredentials(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	cluster *cnpgv1.Cluster,
) ([]adminsync.Server, error) {
	host := clusterServiceHost(cluster, cluster.Name)
	var servers []adminsync.Server

	// The application user: the identity the cluster's own workload holds,
	// and the one a dba reaches for first.
	if server, ok, err := r.credentialServer(
		ctx, console.Namespace, applicationSecretName(cluster), host, "application",
	); err != nil {
		return nil, err
	} else if ok {
		servers = append(servers, server)
	}

	// The superuser, only where the cluster publishes one. CloudNativePG
	// leaves enableSuperuserAccess off by default, so its absence is the
	// common case and says nothing is wrong.
	if server, ok, err := r.credentialServer(
		ctx, console.Namespace, superuserSecretName(cluster), host, "superuser",
	); err != nil {
		return nil, err
	} else if ok {
		servers = append(servers, server)
	}

	// Roles the cluster declares with a password of their own, which is how
	// a per-database owner becomes reachable.
	roleServers, err := r.databaseRoleServers(ctx, console.Namespace, cluster, host)
	if err != nil {
		return nil, err
	}
	servers = append(servers, roleServers...)

	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	return servers, nil
}

// applicationSecretName is the Secret holding the application user, from the
// cluster's own configuration where it names one.
func applicationSecretName(cluster *cnpgv1.Cluster) string {
	if bootstrap := cluster.Spec.Bootstrap; bootstrap != nil && bootstrap.InitDB != nil &&
		bootstrap.InitDB.Secret != nil && bootstrap.InitDB.Secret.Name != "" {
		return bootstrap.InitDB.Secret.Name
	}
	return cluster.Name + "-app"
}

// superuserSecretName is the Secret holding the superuser, which exists only
// when the cluster enables superuser access.
func superuserSecretName(cluster *cnpgv1.Cluster) string {
	if cluster.Spec.SuperuserSecret != nil && cluster.Spec.SuperuserSecret.Name != "" {
		return cluster.Spec.SuperuserSecret.Name
	}
	return cluster.Name + "-superuser"
}

// credentialServer builds one connection from a CNPG credential Secret. It
// reports ok false when the Secret is absent or incomplete, which is a
// normal state rather than a failure.
func (r *Reconciler) credentialServer(
	ctx context.Context,
	namespace, secretName, host, label string,
) (adminsync.Server, bool, error) {
	// APIReader, not the cache: Secret contents are deliberately kept out
	// of the informer cache.
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: secretName}
	if err := r.APIReader.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return adminsync.Server{}, false, nil
		}
		return adminsync.Server{}, false, err
	}

	username := string(secret.Data[credentialUsernameKey])
	password := string(secret.Data[credentialPasswordKey])
	if username == "" || password == "" {
		return adminsync.Server{}, false, nil
	}
	database := string(secret.Data[credentialDatabaseKey])
	if database == "" {
		database = "postgres"
	}

	return adminsync.Server{
		Name:          label + " (" + username + ")",
		Group:         serverGroup,
		Host:          host,
		Port:          postgresServicePort,
		MaintenanceDB: database,
		Username:      username,
		PassFile:      adminsync.DefaultPassFilePath,
		SSLMode:       "prefer",
		Password:      password,
	}, true, nil
}

// databaseRoleServers builds one connection per CloudNativePG DatabaseRole
// that belongs to this cluster and carries a password. A role without one
// cannot be connected as, so it is left out rather than offered broken.
func (r *Reconciler) databaseRoleServers(
	ctx context.Context,
	namespace string,
	cluster *cnpgv1.Cluster,
	host string,
) ([]adminsync.Server, error) {
	var roles cnpgv1.DatabaseRoleList
	if err := r.List(ctx, &roles, client.InNamespace(namespace)); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}

	var servers []adminsync.Server
	for i := range roles.Items {
		role := &roles.Items[i]
		if role.Spec.ClusterRef.Name != cluster.Name || role.Spec.PasswordSecret == nil {
			continue
		}
		server, ok, err := r.credentialServer(
			ctx, namespace, role.Spec.PasswordSecret.Name, host, "role",
		)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		// The Secret names the role; the DatabaseRole is what makes it one
		// this cluster actually has.
		if server.Username != role.Spec.Name && role.Spec.Name != "" {
			server.Username = role.Spec.Name
			server.Name = "role (" + role.Spec.Name + ")"
		}
		servers = append(servers, server)
	}
	return servers, nil
}
