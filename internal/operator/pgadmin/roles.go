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
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/operator/cnpgcred"
)

// Which roles count as privileged, transitively over inRoles. A privileged role is
// offered only with spec.credentials.superuser.

// cnpgSuperuserName is the PostgreSQL superuser every CloudNativePG
// cluster has, whether or not superuser access is enabled.
const cnpgSuperuserName = "postgres"

// rolePrivileges decides whether a role reaches beyond its own objects,
// for one cluster. A role is privileged when it carries superuser,
// createrole, bypassrls or replication, or is a member (inRoles) of a role
// that does: a pg_* predefined role (pg_read_all_data,
// pg_write_server_files, pg_execute_server_program, ...), the superuser,
// the application owner (which owns the application database), or a role
// the cluster declares — a DatabaseRole of the cluster or an entry of its
// spec.managed.roles — that is itself privileged, resolved transitively.
// A membership in a role the cluster does not declare (one created in SQL)
// cannot be resolved and is not counted.
type rolePrivileges struct {
	// reserved are the role names privileged by what they are.
	reserved map[string]struct{}
	// declared are the cluster's declared roles by PostgreSQL name; a name
	// declared more than once is privileged if any declaration is.
	declared map[string][]cnpgv1.RoleConfiguration
	// memo caches resolved names.
	memo map[string]bool
}

// newRolePrivileges indexes the declared roles of cluster from its own
// spec and from roles, of which only those referencing cluster count.
func newRolePrivileges(cluster *cnpgv1.Cluster, roles []cnpgv1.DatabaseRole) *rolePrivileges {
	p := &rolePrivileges{
		reserved: map[string]struct{}{cnpgSuperuserName: {}, cnpgcred.ApplicationOwner(cluster): {}},
		declared: map[string][]cnpgv1.RoleConfiguration{},
		memo:     map[string]bool{},
	}
	if cluster.Spec.Managed != nil {
		for _, role := range cluster.Spec.Managed.Roles {
			p.declared[role.Name] = append(p.declared[role.Name], role)
		}
	}
	for i := range roles {
		role := &roles[i]
		if role.Spec.ClusterRef.Name != cluster.Name || role.Namespace != cluster.Namespace {
			continue
		}
		name := role.Spec.Name
		if name == "" {
			name = role.Name
		}
		p.declared[name] = append(p.declared[name], role.Spec.RoleConfiguration)
	}
	return p
}

// privilegedRole reports whether the role named name, declared by role, is
// privileged: by what the name is (the superuser, the application owner, a
// pg_* role), by any other declaration of the same name, or by role's own
// attributes and memberships. A DatabaseRole cannot make the application
// owner unprivileged by declaring it again without attributes.
func (p *rolePrivileges) privilegedRole(name string, role cnpgv1.RoleConfiguration) bool {
	return p.namePrivileged(name, map[string]bool{}) || p.privileged(role)
}

// privileged reports whether a role configuration is privileged.
func (p *rolePrivileges) privileged(role cnpgv1.RoleConfiguration) bool {
	return p.configPrivileged(role, map[string]bool{role.Name: true})
}

// configPrivileged checks the attributes, then each membership. visiting
// holds the names on the current path: a cycle adds nothing, it neither
// makes a role privileged nor hides another path that does.
func (p *rolePrivileges) configPrivileged(role cnpgv1.RoleConfiguration, visiting map[string]bool) bool {
	if role.Superuser || role.CreateRole || role.BypassRLS || role.Replication {
		return true
	}
	for _, member := range role.InRoles {
		if p.namePrivileged(member, visiting) {
			return true
		}
	}
	return false
}

// namePrivileged resolves one role name.
func (p *rolePrivileges) namePrivileged(name string, visiting map[string]bool) bool {
	if strings.HasPrefix(name, "pg_") {
		return true
	}
	if _, ok := p.reserved[name]; ok {
		return true
	}
	if privileged, ok := p.memo[name]; ok {
		return privileged
	}
	if visiting[name] {
		return false
	}
	visiting[name] = true
	defer delete(visiting, name)
	privileged := false
	for _, declared := range p.declared[name] {
		if p.configPrivileged(declared, visiting) {
			privileged = true
			break
		}
	}
	// A false result reached under a cycle may be incomplete, so only a
	// privileged answer, which no path can undo, is cached.
	if privileged {
		p.memo[name] = true
	}
	return privileged
}
