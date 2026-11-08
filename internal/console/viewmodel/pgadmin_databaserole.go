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

package viewmodel

// PersonalDatabaseRole is a per-person DatabaseRole the console created:
// names only, never a password.
type PersonalDatabaseRole struct {
	Namespace string `json:"namespace" doc:"Namespace of the Cluster and of the DatabaseRole."`
	Cluster   string `json:"cluster" doc:"The Cluster the role is created on."`
	// Name is the DatabaseRole object.
	Name string `json:"name" doc:"The DatabaseRole object (kubectl get databaserole <name>)."`
	Role string `json:"role" doc:"The PostgreSQL role."`
	// PgAdminUser is whose personal server it becomes.
	PgAdminUser    string   `json:"pgAdminUser" doc:"The pgAdmin user whose personal (non-shared) server the role becomes in every PgAdmin that registers the Cluster."`
	InRoles        []string `json:"inRoles,omitempty" doc:"Roles it is a member of."`
	PasswordSecret string   `json:"passwordSecret" doc:"The Secret CloudNativePG generates the password into (the console never reads it)."`
	Note           string   `json:"note" doc:"What happens next."`
}
