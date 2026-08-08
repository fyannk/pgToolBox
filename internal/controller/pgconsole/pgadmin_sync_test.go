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
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
)

func TestClusterServiceHost(t *testing.T) {
	cluster := testCluster()

	if got := clusterServiceHost(cluster, cluster.Name); got != "cluster-1-rw.test.svc" {
		t.Fatalf("fallback host = %q", got)
	}

	cluster.Status.WriteService = "cluster-1-rw"
	if got := clusterServiceHost(cluster, cluster.Name); got != "cluster-1-rw.test.svc" {
		t.Fatalf("status host = %q", got)
	}
}

func TestPgAdminRoleForLevel(t *testing.T) {
	cases := []struct {
		level pgtoolboxv1alpha1.RoleLevel
		want  string
	}{
		{pgtoolboxv1alpha1.RoleLevelView, "User"},
		{pgtoolboxv1alpha1.RoleLevelPowerUser, "User"},
		{pgtoolboxv1alpha1.RoleLevelDBA, "Administrator"},
	}
	for _, tc := range cases {
		if got := pgAdminRoleForLevel(tc.level); got != tc.want {
			t.Fatalf("pgAdminRoleForLevel(%q) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestSyncRevision(t *testing.T) {
	server := adminsync.Server{
		Name:          "application (app)",
		Host:          "cluster-1-rw.test.svc",
		Port:          5432,
		MaintenanceDB: "app",
		Username:      "app",
		Password:      "secret",
	}
	a := adminsync.SyncRequest{Servers: []adminsync.Server{server}}
	b := adminsync.SyncRequest{Servers: []adminsync.Server{server}}

	if syncRevision(a) != syncRevision(b) {
		t.Fatalf("identical payloads produced different revisions")
	}

	// A rotated password must roll the revision, or the sidecar keeps
	// serving a credential the cluster has already replaced.
	b.Servers[0].Password = "different"
	if syncRevision(a) == syncRevision(b) {
		t.Fatalf("changed password must change revision")
	}
}
