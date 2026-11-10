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

package v1alpha1

import (
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// CataloguePgAdmin is the owner name of the PgAdmin reasons in Catalogue.
const CataloguePgAdmin = "pgadmin"

// PgAdminConnectVerb is the RBAC verb on pgadmins/<name> that opens that
// pgAdmin: the OpenShift oauth-proxy asks a SubjectAccessReview for it, and
// the console offers the link to whoever holds it. Whoever may open pgAdmin
// holds the credentials of every role it offers, so it is not get: OLM
// aggregates get, list and watch on an owned CRD into the built-in view
// role. No built-in or OLM-generated role names it, except through `*`
// (admin, which may read the namespace's Secrets anyway); it is granted by
// pgtoolbox-console-operator and pgtoolbox-aggregate-to-admin.
const PgAdminConnectVerb = "connect"

// pgAdminReasons are the PgAdmin-specific reasons of the frozen catalogue.
var pgAdminReasons = []Reason{
	ReasonNoClusterSelected,
	ReasonPoolerNotFound,
	ReasonTooManyServers,
	ReasonPgAdminNotRunning,
	ReasonRegistrationRunning,
	ReasonRegistrationFailed,
	ReasonServersRegistered,
	ReasonRegistrationStarted,
	ReasonUpgradeBackupRunning,
	ReasonUpgradeBackupFailed,
	ReasonUpgradeBackupCompleted,
	ReasonUpgradeBackupSkipped,
	ReasonRolloutStarted,
	ReasonPgAdminEgressOpen,
	ReasonDatabaseRoleAPIUnavailable,
	ReasonPoolerAPIUnavailable,
	ReasonGrantMissing,
	ReasonConfigDatabaseNotReady,
	ReasonPasswordGenerationUnavailable,
	ReasonDatabaseAPIUnavailable,
}

func init() {
	SchemeBuilder.Register(&PgAdmin{}, &PgAdminList{})
	registerReasons(CataloguePgAdmin, pgAdminReasons...)
}

// DefaultPgAdminStorageSize is the settings volume size when
// spec.storage.size is omitted.
var DefaultPgAdminStorageSize = resource.MustParse("1Gi")

// IsPaused reports whether reconciliation is paused by annotation.
func (p *PgAdmin) IsPaused() bool {
	return p.Annotations[AnnotationReconcile] == ReconcileSkip
}

// OffersApplication reports whether the application credential is offered.
func (s *PgAdminCredentialsSpec) OffersApplication() bool {
	return s.Application == nil || *s.Application
}

// OffersSuperuser reports whether privileged credentials are offered.
func (s *PgAdminCredentialsSpec) OffersSuperuser() bool {
	return s.Superuser != nil && *s.Superuser
}

// OffersDatabaseRoles reports whether applied DatabaseRoles are offered.
func (s *PgAdminCredentialsSpec) OffersDatabaseRoles() bool {
	return s.DatabaseRoles == nil || *s.DatabaseRoles
}

// EndpointsOrDefault returns the endpoints, rw when none are given.
func (s *PgAdminSpec) EndpointsOrDefault() []Endpoint {
	if len(s.Endpoints) == 0 {
		return []Endpoint{EndpointRW}
	}
	return s.Endpoints
}

// PoolerName returns the Pooler an endpoint names, and whether it names one.
func (e Endpoint) PoolerName() (string, bool) {
	name, ok := strings.CutPrefix(string(e), EndpointPoolerPrefix)
	return name, ok && name != ""
}

// SizeOrDefault returns the requested settings volume size.
func (s *PgAdminStorageSpec) SizeOrDefault() resource.Quantity {
	if s.Size == nil || s.Size.IsZero() {
		return DefaultPgAdminStorageSize.DeepCopy()
	}
	return s.Size.DeepCopy()
}

// RetainPolicyOrDefault returns the retain policy, Retain when unset.
func (s *PgAdminStorageSpec) RetainPolicyOrDefault() PgAdminRetainPolicy {
	if s.RetainPolicy == "" {
		return PgAdminRetainPolicyRetain
	}
	return s.RetainPolicy
}

// ReplicasOrDefault returns the pgAdmin Pod count, 1 when unset.
func (s *PgAdminSpec) ReplicasOrDefault() int32 {
	if s.Replicas == nil || *s.Replicas < 1 {
		return 1
	}
	return *s.Replicas
}

// DatabaseOrDefault returns the settings database (and its owner role).
func (s *PgAdminConfigDatabaseSpec) DatabaseOrDefault(pgAdminName string) string {
	if s.Database != "" {
		return s.Database
	}
	return "pgadmin_" + strings.ReplaceAll(pgAdminName, "-", "_")
}

// IsEnabled reports whether the NetworkPolicy is rendered.
func (s *PgAdminNetworkPolicySpec) IsEnabled() bool {
	return s.Enabled != nil && *s.Enabled
}

// UsernameClaimOrDefault returns the username claim, email when unset.
func (s *PgAdminOAuth2Spec) UsernameClaimOrDefault() string {
	if s.UsernameClaim == "" {
		return "email"
	}
	return s.UsernameClaim
}

// ScopesOrDefault returns the scopes, openid email profile when unset.
func (s *PgAdminOAuth2Spec) ScopesOrDefault() []string {
	if len(s.Scopes) == 0 {
		return []string{"openid", "email", "profile"}
	}
	return s.Scopes
}

// KeyOrDefault returns the key, client-secret when unset.
func (s SecretKeySelector) KeyOrDefault() string {
	if s.Key == "" {
		return "client-secret"
	}
	return s.Key
}

// pgAdminUsername is what spec.admins accepts as a pgAdmin username (its
// items' pattern): no leading dash, no whitespace, quote or backslash, no
// control character, at most 254 characters.
var pgAdminUsername = regexp.MustCompile(`^[^-\s'"\\\x00-\x1f\x7f][^\s'"\\\x00-\x1f\x7f]{0,253}$`)

// ValidPgAdminUser reports whether user can own personal servers (the
// AnnotationPgAdminUser of a personal DatabaseRole): the same rule as
// spec.admins. The console checks it before creating such a role, the
// operator before registering one.
func ValidPgAdminUser(user string) bool {
	return pgAdminUsername.MatchString(user)
}

// PostgreSQLRoleName is the shape of the role names the console creates
// for a person (lower-case identifiers, never pg_-prefixed: PostgreSQL
// reserves those).
var postgresRole = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ValidPersonalRoleName reports whether name may be a per-person role.
func ValidPersonalRoleName(name string) bool {
	return postgresRole.MatchString(name) && !strings.HasPrefix(name, "pg_") && name != "postgres"
}
