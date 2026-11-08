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

func init() {
	SchemeBuilder.Register(&PgAdminAccess{}, &PgAdminAccessList{})
}

// AllowsPrivileged reports whether the grant allows privileged credentials.
func (g *PgAdminAccess) AllowsPrivileged() bool {
	return g.Spec.AllowPrivileged != nil && *g.Spec.AllowPrivileged
}

// Grants reports whether the grant names the PgAdmin namespace/name and
// the Cluster.
func (g *PgAdminAccess) Grants(pgAdminNamespace, pgAdminName, cluster string) bool {
	if g.Spec.PgAdmin.Namespace != pgAdminNamespace || g.Spec.PgAdmin.Name != pgAdminName {
		return false
	}
	for _, ref := range g.Spec.Clusters {
		if ref.Name == cluster {
			return true
		}
	}
	return false
}
