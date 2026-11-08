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

// Package cnpgcred holds CloudNativePG's rules for which Secret carries
// which credential of a Cluster, and when a DatabaseRole's password may be
// used. Two programs apply them: the PgAdmin reconciler, which works out
// the offered set from the Clusters (internal/operator/pgadmin), and the
// passfile-sync sidecar of the team chart, which checks each Secret a
// values file names against the live Cluster or DatabaseRole before using
// it. Keeping them in one place means both modes refuse the
// same things.
//
// Everything here is pure and reads names only, never a Secret's content.
package cnpgcred

import (
	"slices"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
)

const (
	// DefaultDatabase is the database every CloudNativePG Cluster has.
	DefaultDatabase = "postgres"
	// SuperuserRole is the PostgreSQL superuser every CloudNativePG
	// Cluster has, whether or not superuser access is enabled.
	SuperuserRole = "postgres"
	// GeneratedPasswordSuffix is CloudNativePG's default name suffix of the
	// Secret it generates a DatabaseRole's password into (spec.password.mode
	// generate, CloudNativePG 1.31).
	GeneratedPasswordSuffix = "-password"
)

// ApplicationSecretName is the Secret of the application user, in
// CloudNativePG's own order (Cluster.GetApplicationSecretName): the secret
// bootstrap.recovery, bootstrap.pg_basebackup or bootstrap.initdb names,
// else the <cluster>-app Secret CloudNativePG creates.
func ApplicationSecretName(cluster *cnpgv1.Cluster) string {
	if bootstrap := cluster.Spec.Bootstrap; bootstrap != nil {
		switch {
		case bootstrap.Recovery != nil && bootstrap.Recovery.Secret != nil && bootstrap.Recovery.Secret.Name != "":
			return bootstrap.Recovery.Secret.Name
		case bootstrap.PgBaseBackup != nil && bootstrap.PgBaseBackup.Secret != nil && bootstrap.PgBaseBackup.Secret.Name != "":
			return bootstrap.PgBaseBackup.Secret.Name
		case bootstrap.InitDB != nil && bootstrap.InitDB.Secret != nil && bootstrap.InitDB.Secret.Name != "":
			return bootstrap.InitDB.Secret.Name
		}
	}
	return cluster.Name + "-app"
}

// ApplicationOwner is the role that owns the cluster's application
// database, from whichever bootstrap method the cluster names; CNPG
// defaults the owner to the database name and the database to "app".
func ApplicationOwner(cluster *cnpgv1.Cluster) string {
	owner, database := "", ""
	if bootstrap := cluster.Spec.Bootstrap; bootstrap != nil {
		switch {
		case bootstrap.InitDB != nil:
			owner, database = bootstrap.InitDB.Owner, bootstrap.InitDB.Database
		case bootstrap.Recovery != nil:
			owner, database = bootstrap.Recovery.Owner, bootstrap.Recovery.Database
		case bootstrap.PgBaseBackup != nil:
			owner, database = bootstrap.PgBaseBackup.Owner, bootstrap.PgBaseBackup.Database
		}
	}
	switch {
	case owner != "":
		return owner
	case database != "":
		return database
	default:
		return "app"
	}
}

// ApplicationDatabase is the database the application user opens first.
func ApplicationDatabase(cluster *cnpgv1.Cluster) string {
	if bootstrap := cluster.Spec.Bootstrap; bootstrap != nil {
		switch {
		case bootstrap.InitDB != nil && bootstrap.InitDB.Database != "":
			return bootstrap.InitDB.Database
		case bootstrap.Recovery != nil && bootstrap.Recovery.Database != "":
			return bootstrap.Recovery.Database
		case bootstrap.PgBaseBackup != nil && bootstrap.PgBaseBackup.Database != "":
			return bootstrap.PgBaseBackup.Database
		}
	}
	return "app"
}

// SuperuserEnabled reports whether the Cluster enables superuser access,
// without which CloudNativePG keeps no usable superuser Secret.
func SuperuserEnabled(cluster *cnpgv1.Cluster) bool {
	return cluster.Spec.EnableSuperuserAccess != nil && *cluster.Spec.EnableSuperuserAccess
}

// SuperuserSecretName is the Secret of the superuser.
func SuperuserSecretName(cluster *cnpgv1.Cluster) string {
	if cluster.Spec.SuperuserSecret != nil && cluster.Spec.SuperuserSecret.Name != "" {
		return cluster.Spec.SuperuserSecret.Name
	}
	return cluster.Name + "-superuser"
}

// CASecretName is the Secret holding the Cluster's server CA: the spec's,
// else CNPG's default. Never the status: whoever may patch clusters/status
// could point it at any Secret, which the pgAdmin Pod's Role would then
// grant.
func CASecretName(cluster *cnpgv1.Cluster) string {
	if certificates := cluster.Spec.Certificates; certificates != nil && certificates.ServerCASecret != "" {
		return certificates.ServerCASecret
	}
	return cluster.Name + "-ca"
}

// DatabaseRoleName is the PostgreSQL role a DatabaseRole declares.
func DatabaseRoleName(role *cnpgv1.DatabaseRole) string {
	if role.Spec.Name != "" {
		return role.Spec.Name
	}
	return role.Name
}

// PasswordSecretName is the Secret holding a DatabaseRole's password: the
// one spec.passwordSecret names, or, for a role whose password
// CloudNativePG generates, the Secret it generates into. The vendored
// CloudNativePG API (1.30) does not carry spec.password, so a generated
// password is recognised by its effect: no passwordSecret, password not
// disabled, and a Secret version CloudNativePG recorded applying. The
// default generated name is assumed; a role generating into another name
// is then refused by the version check (the recorded version is that of
// the real Secret), never offered with a wrong password.
func PasswordSecretName(role *cnpgv1.DatabaseRole) string {
	switch {
	case role.Spec.PasswordSecret != nil && role.Spec.PasswordSecret.Name != "":
		return role.Spec.PasswordSecret.Name
	case role.Spec.DisablePassword || role.Status.SecretResourceVersion == "":
		return ""
	case role.Spec.ClientCertificate != nil && (role.Spec.ClientCertificate.Enabled == nil || *role.Spec.ClientCertificate.Enabled):
		return ""
	}
	return role.Name + GeneratedPasswordSuffix
}

// RoleNotApplied returns why CloudNativePG has not made a DatabaseRole's
// password usable yet, from what the role and its status say before the
// Secret version is compared: not applied, not at the current generation,
// or no password Secret at all. "" when none of these holds.
func RoleNotApplied(role *cnpgv1.DatabaseRole) string {
	switch {
	case role.Status.Applied == nil || !*role.Status.Applied:
		return "not applied by CloudNativePG yet"
	case role.Status.ObservedGeneration < role.Generation:
		return "CloudNativePG has not applied the current generation yet"
	case PasswordSecretName(role) == "":
		if role.Spec.ClientCertificate != nil && role.Spec.ClientCertificate.Enabled != nil && *role.Spec.ClientCertificate.Enabled {
			return "certificate authentication is not offered"
		}
		return "no password Secret (none named, and none CloudNativePG generated and applied yet)"
	}
	return ""
}

// RoleSecretNotRecorded returns why the version of a DatabaseRole's
// password Secret cannot be checked: CloudNativePG has not recorded the
// version it applied. "" once it has.
func RoleSecretNotRecorded(role *cnpgv1.DatabaseRole) string {
	if role.Status.SecretResourceVersion == "" {
		return "CloudNativePG has not recorded applying the password Secret"
	}
	return ""
}

// RoleNotReady is RoleNotApplied, then RoleSecretNotRecorded: "" when the
// password Secret at status.secretResourceVersion is the one CloudNativePG
// set in PostgreSQL for the role's current generation. The caller still
// compares that version with the Secret it reads (SecretAtAppliedVersion).
func RoleNotReady(role *cnpgv1.DatabaseRole) string {
	if why := RoleNotApplied(role); why != "" {
		return why
	}
	return RoleSecretNotRecorded(role)
}

// SecretAtAppliedVersion reports whether a password Secret of the given
// resourceVersion is the one CloudNativePG applied to role.
func SecretAtAppliedVersion(role *cnpgv1.DatabaseRole, secretResourceVersion string) bool {
	return role.Status.SecretResourceVersion != "" && role.Status.SecretResourceVersion == secretResourceVersion
}

// EndpointNotServed returns why a Cluster cannot serve one of its own
// Service endpoints ("rw", "ro" or "r"), or "" when it can or when the
// endpoint is not one of them (a Pooler's). Both modes skip such a server
// rather than register one that cannot answer: the operator's
// offer and the team chart's declared list.
//
// It reads the spec, never readiness: a replica that restarts must not make
// the server disappear from pgAdmin and come back. CloudNativePG's ro
// Service selects the replicas only (cnpg.io/instanceRole: replica), so a
// Cluster declaring one instance has none behind it and every connection is
// refused; a Service listed in spec.managed.services.disabledDefaultServices
// does not exist at all (cluster_funcs.go v1.30.0,
// Is{Read,ReadOnly}ServiceEnabled). rw cannot be disabled.
func EndpointNotServed(cluster *cnpgv1.Cluster, endpoint string) string {
	switch cnpgv1.ServiceSelectorType(endpoint) {
	case cnpgv1.ServiceSelectorTypeRO, cnpgv1.ServiceSelectorTypeR:
	default:
		return ""
	}
	if m := cluster.Spec.Managed; m != nil && m.Services != nil &&
		slices.Contains(m.Services.DisabledDefaultServices, cnpgv1.ServiceSelectorType(endpoint)) {
		return "the Cluster disabled its " + endpoint + " Service"
	}
	if cnpgv1.ServiceSelectorType(endpoint) == cnpgv1.ServiceSelectorTypeRO && cluster.Spec.Instances < 2 {
		return "no replica: the Cluster has one instance"
	}
	return ""
}
