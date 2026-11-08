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
	"fmt"
	"slices"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/adminsync"
	"github.com/fyannk/pgToolBox/internal/operator/cnpgcred"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

// The connections pgAdmin offers come from the CloudNativePG Clusters, not
// from whoever signs in: the credentials the Clusters publish as Secrets,
// filtered by an allowlist:
//
//   - the application user, unless spec.credentials.application is false;
//   - the superuser, only with spec.credentials.superuser and when the
//     Cluster enables superuser access;
//   - DatabaseRoles CloudNativePG reports applied at their current
//     generation, with the password Secret at the version it applied;
//     privileged ones (roles.go) only with spec.credentials.superuser.
//
// The reconciler resolves names only. It never reads a password: the
// passfile-sync sidecar in the pgAdmin Pod reads the Secrets through its own
// name-scoped Role and refuses one whose username differs from the role the
// offer names, or whose version differs from the one CloudNativePG applied.

const (
	postgresPort      int32 = 5432
	poolerPort        int32 = 5432
	defaultDatabase         = cnpgcred.DefaultDatabase
	superuserRoleName       = cnpgcred.SuperuserRole
)

// offerInputs is everything the offered set depends on, read by the
// reconciler from its caches.
type offerInputs struct {
	PgAdmin *pgtoolboxv1alpha1.PgAdmin
	// Clusters are the registered Clusters: the PgAdmin's namespace's, and
	// those of other namespaces a PgAdminAccess grants.
	Clusters []cnpgv1.Cluster
	// Roles are the DatabaseRoles of the Clusters' namespaces (nil when the
	// API is unavailable).
	Roles []cnpgv1.DatabaseRole
	// RolesAvailable reports whether the DatabaseRole API was readable.
	RolesAvailable bool
	// Poolers are the namespace's Poolers.
	Poolers []cnpgv1.Pooler
	// PoolersAvailable reports whether the Pooler API was readable.
	PoolersAvailable bool
	// PrivilegedGranted names the Clusters of other namespaces
	// ("namespace/name") whose grant allows privileged credentials; a
	// granted Cluster absent from it offers none, whatever the PgAdmin asks.
	PrivilegedGranted map[string]bool
}

// offerResult is the resolved offer, the status rows, and the problems that
// make the configuration invalid.
type offerResult struct {
	Document offer.Document
	Rows     []pgtoolboxv1alpha1.PgAdminServerStatus
	// Problems are user-fixable configuration errors (a pooler endpoint
	// naming no Pooler of a registered Cluster).
	Problems []string
	// TooMany is set when the offered set exceeds offer.MaxServers.
	TooMany bool
}

// credential is one role a Cluster offers, before endpoints multiply it.
// owner is the pgAdmin user of a personal role, empty for a
// shared one.
type credential struct {
	label, user, database, secret, resourceVersion, owner string
}

// resolveOffer computes the offered set. It is pure: the same inputs give
// the same document, byte for byte once encoded, so a no-op reconcile
// changes nothing.
func resolveOffer(in offerInputs) offerResult {
	var result offerResult
	spec := &in.PgAdmin.Spec
	clusters := slices.Clone(in.Clusters)
	// The PgAdmin's own Clusters first, then the granted ones by
	// namespace: a cross-namespace reference added later never reorders
	// the servers of the namespace.
	slices.SortFunc(clusters, func(a, b cnpgv1.Cluster) int {
		if c := strings.Compare(clusterNamespaceKey(in.PgAdmin, &a), clusterNamespaceKey(in.PgAdmin, &b)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})

	var servers []adminsync.Server // only for section names
	for i := range clusters {
		cluster := &clusters[i]
		label, remote := clusterLabel(in.PgAdmin, cluster), clusterNamespaceKey(in.PgAdmin, cluster)
		clusterSpec := spec
		if remote != "" && spec.Credentials.OffersSuperuser() && !in.PrivilegedGranted[label] {
			// The grant, not the PgAdmin, decides what crosses the namespace.
			clusterSpec = spec.DeepCopy()
			clusterSpec.Credentials.Superuser = new(false)
		}
		credentials, notOffered := clusterCredentials(clusterSpec, cluster, label, in.Roles, in.RolesAvailable)
		result.Rows = append(result.Rows, notOffered...)
		for _, endpoint := range spec.EndpointsOrDefault() {
			if why := cnpgcred.EndpointNotServed(cluster, string(endpoint)); why != "" {
				// The Service would refuse or not resolve: say why the
				// entry is missing instead of offering it.
				for _, cred := range credentials {
					result.Rows = append(result.Rows, pgtoolboxv1alpha1.PgAdminServerStatus{
						Cluster: label, Endpoint: string(endpoint), User: truncate(cred.user, 128),
						PgAdminUser: cred.owner, State: pgtoolboxv1alpha1.ServerStateNotOffered, Message: why,
					})
				}
				continue
			}
			host, port, problem := endpointHost(cluster, endpoint, in.Poolers, in.PoolersAvailable)
			if problem == errOtherCluster {
				// A pooler endpoint serves the one Cluster its Pooler
				// belongs to; the other Clusters simply have none.
				continue
			}
			if problem != "" {
				if !slices.Contains(result.Problems, problem) {
					result.Problems = append(result.Problems, problem)
				}
				continue
			}
			for _, cred := range credentials {
				name := fmt.Sprintf("%s %s (%s)", label, endpoint, cred.user)
				result.Document.Servers = append(result.Document.Servers, offer.Server{
					Name:                  name,
					Namespace:             remote,
					Owner:                 cred.owner,
					Cluster:               cluster.Name,
					Endpoint:              string(endpoint),
					Host:                  host,
					Port:                  port,
					DBName:                cred.database,
					User:                  cred.user,
					SecretName:            cred.secret,
					SecretResourceVersion: cred.resourceVersion,
					CASecretName:          cnpgcred.CASecretName(cluster),
				})
				servers = append(servers, adminsync.Server{Name: name})
				result.Rows = append(result.Rows, pgtoolboxv1alpha1.PgAdminServerStatus{
					Cluster: label, Endpoint: string(endpoint), User: truncate(cred.user, 128),
					PgAdminUser: cred.owner, State: pgtoolboxv1alpha1.ServerStatePending,
				})
			}
		}
	}
	for _, endpoint := range spec.EndpointsOrDefault() {
		name, ok := endpoint.PoolerName()
		if !ok || !in.PoolersAvailable {
			continue
		}
		for i := range in.Poolers {
			pooler := &in.Poolers[i]
			if pooler.Name == name && slices.ContainsFunc(clusters, func(c cnpgv1.Cluster) bool { return c.Namespace == pooler.Namespace }) &&
				!slices.ContainsFunc(clusters, func(c cnpgv1.Cluster) bool {
					return c.Name == pooler.Spec.Cluster.Name && c.Namespace == pooler.Namespace
				}) {
				result.Problems = append(result.Problems, fmt.Sprintf("endpoint %s: Pooler %s belongs to Cluster %s, which this PgAdmin does not register",
					endpoint, name, pooler.Spec.Cluster.Name))
			}
		}
	}
	for i, service := range adminsync.ServiceNames(servers) {
		result.Document.Servers[i].Service = service
	}
	if len(result.Document.Servers) > offer.MaxServers {
		result.TooMany = true
	}
	if len(result.Rows) > 256 {
		result.Rows = result.Rows[:256]
	}
	return result
}

// clusterCredentials returns the credentials one Cluster offers and a row
// for each it has but does not offer.
func clusterCredentials(
	spec *pgtoolboxv1alpha1.PgAdminSpec,
	cluster *cnpgv1.Cluster,
	label string,
	roles []cnpgv1.DatabaseRole,
	rolesAvailable bool,
) ([]credential, []pgtoolboxv1alpha1.PgAdminServerStatus) {
	var offered []credential
	var notOffered []pgtoolboxv1alpha1.PgAdminServerStatus
	refuse := func(user, why string) {
		notOffered = append(notOffered, pgtoolboxv1alpha1.PgAdminServerStatus{
			Cluster: label, Endpoint: "*", User: truncate(user, 128),
			State: pgtoolboxv1alpha1.ServerStateNotOffered, Message: truncate(why, 256),
		})
	}
	appDatabase := cnpgcred.ApplicationDatabase(cluster)

	if spec.Credentials.OffersApplication() {
		offered = append(offered, credential{
			label: "application", user: cnpgcred.ApplicationOwner(cluster), database: appDatabase,
			secret: cnpgcred.ApplicationSecretName(cluster),
		})
	}

	superuserEnabled := cnpgcred.SuperuserEnabled(cluster)
	switch {
	case spec.Credentials.OffersSuperuser() && superuserEnabled:
		offered = append(offered, credential{
			label: "superuser", user: superuserRoleName, database: defaultDatabase,
			secret: cnpgcred.SuperuserSecretName(cluster),
		})
	case spec.Credentials.OffersSuperuser():
		refuse(superuserRoleName, "the Cluster does not enable superuser access")
	case superuserEnabled:
		refuse(superuserRoleName, "privileged; offered only with spec.credentials.superuser")
	}

	if spec.Credentials.OffersDatabaseRoles() && rolesAvailable {
		privileges := newRolePrivileges(cluster, roles)
		ordered := slices.Clone(roles)
		slices.SortFunc(ordered, func(a, b cnpgv1.DatabaseRole) int { return strings.Compare(a.Name, b.Name) })
		for i := range ordered {
			role := &ordered[i]
			if role.Spec.ClusterRef.Name != cluster.Name || role.Namespace != cluster.Namespace || ownedByPgAdmin(role) {
				// Another Cluster's, or a PgAdmin's own settings database
				// role (configDatabase), which is never a server.
				continue
			}
			user := cnpgcred.DatabaseRoleName(role)
			if why := databaseRoleNotOffered(spec, role, privileges); why != "" {
				refuse(user, why)
				continue
			}
			offered = append(offered, credential{
				label: "role", user: user, database: defaultDatabase,
				secret: cnpgcred.PasswordSecretName(role), resourceVersion: role.Status.SecretResourceVersion,
				owner: personalOwner(role),
			})
		}
	}
	return offered, notOffered
}

// databaseRoleNotOffered returns why a DatabaseRole of the Cluster is not
// offered, or "" when it is.
func databaseRoleNotOffered(spec *pgtoolboxv1alpha1.PgAdminSpec, role *cnpgv1.DatabaseRole, privileges *rolePrivileges) string {
	// CloudNativePG's readiness rule (cnpgcred), with the personal-role
	// check between its two halves, where it always was.
	if why := cnpgcred.RoleNotApplied(role); why != "" {
		return why
	}
	switch {
	case role.Labels[pgtoolboxv1alpha1.LabelPersonalRole] == pgtoolboxv1alpha1.PersonalRoleLabelValue && personalOwner(role) == "":
		return "a personal role without a valid " + pgtoolboxv1alpha1.AnnotationPgAdminUser + " annotation"
	case cnpgcred.RoleSecretNotRecorded(role) != "":
		return cnpgcred.RoleSecretNotRecorded(role)
	case cnpgcred.DatabaseRoleName(role) == superuserRoleName:
		return "the superuser is offered through the Cluster's superuser Secret only"
	case privileges.privilegedRole(cnpgcred.DatabaseRoleName(role), role.Spec.RoleConfiguration) && !spec.Credentials.OffersSuperuser():
		return "privileged; offered only with spec.credentials.superuser"
	}
	return ""
}

// errOtherCluster is endpointHost's answer for a pooler endpoint whose
// Pooler belongs to another Cluster.
const errOtherCluster = "\x00other-cluster"

// endpointHost returns the Service host and port of one endpoint, or a
// configuration problem.
func endpointHost(cluster *cnpgv1.Cluster, endpoint pgtoolboxv1alpha1.Endpoint, poolers []cnpgv1.Pooler, poolersAvailable bool) (string, int32, string) {
	namespace := cluster.Namespace
	switch endpoint {
	case pgtoolboxv1alpha1.EndpointRW:
		service := cluster.Status.WriteService
		if service == "" {
			service = cluster.Name + "-rw"
		}
		return service + "." + namespace + ".svc", postgresPort, ""
	case pgtoolboxv1alpha1.EndpointR:
		service := cluster.Status.ReadService
		if service == "" {
			service = cluster.Name + "-r"
		}
		return service + "." + namespace + ".svc", postgresPort, ""
	case pgtoolboxv1alpha1.EndpointRO:
		return cluster.Name + "-ro." + namespace + ".svc", postgresPort, ""
	}
	name, ok := endpoint.PoolerName()
	if !ok {
		return "", 0, fmt.Sprintf("endpoint %q is not rw, ro, r or pooler:<name>", endpoint)
	}
	if !poolersAvailable {
		return "", 0, fmt.Sprintf("endpoint %s: the Pooler API is not available", endpoint)
	}
	for i := range poolers {
		if poolers[i].Name == name && poolers[i].Namespace == namespace {
			if poolers[i].Spec.Cluster.Name != cluster.Name {
				return "", 0, errOtherCluster
			}
			return name + "." + namespace + ".svc", poolerPort, ""
		}
	}
	return "", 0, fmt.Sprintf("endpoint %s: no Pooler %s in namespace %s", endpoint, name, namespace)
}

// clusterNamespaceKey is "" for a Cluster of the PgAdmin's own namespace,
// its namespace otherwise (offer.Server.Namespace).
func clusterNamespaceKey(p *pgtoolboxv1alpha1.PgAdmin, cluster *cnpgv1.Cluster) string {
	if cluster.Namespace == p.Namespace {
		return ""
	}
	return cluster.Namespace
}

// clusterLabel names a Cluster in server names and status rows: its name,
// qualified by its namespace when it is not the PgAdmin's.
func clusterLabel(p *pgtoolboxv1alpha1.PgAdmin, cluster *cnpgv1.Cluster) string {
	if ns := clusterNamespaceKey(p, cluster); ns != "" {
		return ns + "/" + cluster.Name
	}
	return cluster.Name
}

// ownedByPgAdmin reports a DatabaseRole a PgAdmin controls: the role of a
// configDatabase, never offered as a server.
func ownedByPgAdmin(role *cnpgv1.DatabaseRole) bool {
	owner := metav1.GetControllerOf(role)
	return owner != nil && owner.Kind == "PgAdmin" && strings.HasPrefix(owner.APIVersion, pgtoolboxv1alpha1.GroupVersion.Group+"/")
}

// personalOwner is the pgAdmin user of a personal DatabaseRole (the
// console's per-person roles), "" for a shared one or an invalid user.
func personalOwner(role *cnpgv1.DatabaseRole) string {
	if role.Labels[pgtoolboxv1alpha1.LabelPersonalRole] != pgtoolboxv1alpha1.PersonalRoleLabelValue {
		return ""
	}
	user := role.Annotations[pgtoolboxv1alpha1.AnnotationPgAdminUser]
	if !ValidPgAdminUser(user) {
		return ""
	}
	return user
}

// ValidPgAdminUser reports whether user can be a personal server's owner
// (api/v1alpha1.ValidPgAdminUser, shared with the console).
func ValidPgAdminUser(user string) bool {
	return pgtoolboxv1alpha1.ValidPgAdminUser(user)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
