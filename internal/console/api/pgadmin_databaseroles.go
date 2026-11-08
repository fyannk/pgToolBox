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

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// OpCreateDatabaseRole creates a per-person DatabaseRole.
const OpCreateDatabaseRole = "createDatabaseRole"

// DatabaseRoleCreator is the writer of per-person DatabaseRoles: the
// ops.UserWriter, which writes with the caller's own token.
type DatabaseRoleCreator interface {
	CreatePersonalDatabaseRole(ctx context.Context, req ops.PersonalRoleRequest) (ops.PersonalRole, error)
}

// createDatabaseRoleBody is the request.
type createDatabaseRoleBody struct {
	Role        string   `json:"role" minLength:"1" maxLength:"63" pattern:"^[a-z_][a-z0-9_]*$" doc:"The PostgreSQL role to create (not pg_-prefixed, not postgres)."`
	PgAdminUser string   `json:"pgAdminUser,omitempty" maxLength:"254" doc:"The pgAdmin username whose personal server the role becomes (as the identity provider presents it, e.g. an e-mail address). Defaults to your Kubernetes username."`
	InRoles     []string `json:"inRoles,omitempty" maxItems:"16" doc:"Roles to make it a member of. A role that is privileged (pg_*, the superuser, the application owner, or one with superuser, createrole, bypassrls or replication) is created, but a PgAdmin offers it only with spec.credentials.superuser."`
}

type createDatabaseRoleInput struct {
	ClusterPath
	Body createDatabaseRoleBody
}

type createDatabaseRoleOutput struct {
	Status int
	Body   viewmodel.PersonalDatabaseRole
}

// RegisterPgAdminDatabaseRoles registers POST .../clusters/{name}/database-roles
// creator nil serves 501. The read rules apply: get on
// the Cluster first (403 otherwise, whether it exists or not), then the
// API server's answer for create databaseroles in the namespace, then the
// write with the caller's own token, which the API server authorizes and
// audits again under their name.
func RegisterPgAdminDatabaseRoles(a huma.API, d Deps, creator DatabaseRoleCreator) {
	huma.Register(a, huma.Operation{
		OperationID: OpCreateDatabaseRole, Method: http.MethodPost, Path: clusterPathPrefix + "/database-roles",
		Summary: "Create a personal database role", Tags: []string{"pgadmin"},
		Description: "Creates, as you, a CloudNativePG DatabaseRole for one person on this Cluster, with a password CloudNativePG " +
			"generates (CloudNativePG 1.31 or later; 501 before) into a Secret the console never reads. Every PgAdmin that registers " +
			"the Cluster adds it as a personal, non-shared server of pgAdminUser at its next registration. Needs create on " +
			"databaseroles.postgresql.cnpg.io in the namespace (the pgtoolbox-console-operator role). 409: a DatabaseRole of that name exists. " +
			"Requires the X-PgToolBox-Request: 1 header and a same-origin request.",
		DefaultStatus: http.StatusCreated,
		Errors:        append([]int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusNotImplemented}, problemCodes...),
	}, func(ctx context.Context, in *createDatabaseRoleInput) (*createDatabaseRoleOutput, error) {
		p, err := authorizeCluster(ctx, d.Authz, &in.ClusterPath)
		if err != nil {
			return nil, err
		}
		if creator == nil {
			return nil, huma.Error501NotImplemented("personal database roles are not served by this console")
		}
		decision := d.Authz.Allowed(ctx, p.User, authz.Attributes{
			Namespace: in.Namespace, Verb: "create", Group: "postgresql.cnpg.io", Resource: "databaseroles",
		})
		switch {
		case decision.Err != nil:
			return nil, huma.Error503ServiceUnavailable("the Kubernetes API server could not be asked whether you may create database roles; retry")
		case !decision.Allowed:
			return nil, huma.Error403Forbidden("you may not create databaseroles.postgresql.cnpg.io in namespace " + in.Namespace +
				": ask for the pgtoolbox-console-operator role there")
		}
		req, problem := personalRoleRequest(in, p.User.Name)
		if problem != "" {
			return nil, huma.Error422UnprocessableEntity(problem)
		}
		created, err := creator.CreatePersonalDatabaseRole(p.Context(ctx), req)
		if err != nil {
			return nil, databaseRoleError(err)
		}
		return &createDatabaseRoleOutput{Status: http.StatusCreated, Body: viewmodel.PersonalDatabaseRole{
			Namespace: in.Namespace, Cluster: in.Name, Name: created.Name, Role: req.Role, PgAdminUser: req.PgAdminUser,
			InRoles: req.InRoles, PasswordSecret: created.PasswordSecret,
			Note: "CloudNativePG creates the role and generates its password; each PgAdmin registering this Cluster adds it " +
				"as a personal server of " + req.PgAdminUser + " at its next registration (kubectl get pgadmin shows ServersSynced).",
		}}, nil
	})
}

// personalRoleRequest validates the body against what the operator will
// accept, so a role the PgAdmins would refuse is not created at all.
func personalRoleRequest(in *createDatabaseRoleInput, caller string) (ops.PersonalRoleRequest, string) {
	user := in.Body.PgAdminUser
	if user == "" {
		user = caller
	}
	switch {
	case !pgtoolboxv1alpha1.ValidPgAdminUser(user):
		return ops.PersonalRoleRequest{}, "pgAdminUser is not a valid pgAdmin username (no leading dash, whitespace, quote, backslash or control character)"
	case !pgtoolboxv1alpha1.ValidPersonalRoleName(in.Body.Role):
		return ops.PersonalRoleRequest{}, "role is not a valid personal role name (a lower-case identifier, not pg_-prefixed, not postgres)"
	case ops.PersonalRoleName(in.Name, in.Body.Role) == "":
		return ops.PersonalRoleRequest{}, "the Cluster and role names give no valid DatabaseRole name"
	}
	roles := slices.Clone(in.Body.InRoles)
	slices.Sort(roles)
	roles = slices.Compact(roles)
	for _, role := range roles {
		if role == "" || len(role) > 63 || !validRoleMember(role) {
			return ops.PersonalRoleRequest{}, "inRoles holds an invalid role name"
		}
	}
	return ops.PersonalRoleRequest{
		Namespace: in.Namespace, Cluster: in.Name, Role: in.Body.Role, PgAdminUser: user, InRoles: roles, CreatedBy: caller,
	}, ""
}

// validRoleMember accepts the role names CloudNativePG's inRoles can name:
// printable, no quote, backslash or control character.
func validRoleMember(role string) bool {
	for _, r := range role {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\'' || r == '\\' {
			return false
		}
	}
	return true
}

func databaseRoleError(err error) error {
	var rejected interface {
		HTTPStatus() int
		SafeText() string
	}
	if errors.As(err, &rejected) {
		return huma.NewError(rejected.HTTPStatus(), rejected.SafeText())
	}
	return huma.Error503ServiceUnavailable("the database role could not be created: " + safeReason(err))
}

// FixtureExistingRole is the role FixtureDatabaseRoles reports as existing.
const FixtureExistingRole = "jane"

// FixtureDatabaseRoles plays the writer for the fixtures: Cluster billing
// runs a CloudNativePG without password generation, FixtureExistingRole
// exists, everything else is created.
type FixtureDatabaseRoles struct{}

// CreatePersonalDatabaseRole implements DatabaseRoleCreator.
func (FixtureDatabaseRoles) CreatePersonalDatabaseRole(_ context.Context, req ops.PersonalRoleRequest) (ops.PersonalRole, error) {
	name := ops.PersonalRoleName(req.Cluster, req.Role)
	switch {
	case req.Cluster == "billing":
		return ops.PersonalRole{}, &ops.WriteError{
			Status: http.StatusNotImplemented,
			Text:   "creating the database role: not available on this CloudNativePG minor (generated DatabaseRole passwords need CloudNativePG 1.31 or later)",
		}
	case req.Role == FixtureExistingRole:
		return ops.PersonalRole{}, &ops.WriteError{Status: http.StatusConflict, Text: "creating the database role: DatabaseRole " + name + " already exists"}
	}
	return ops.PersonalRole{Name: name, PasswordSecret: name + "-password"}, nil
}
