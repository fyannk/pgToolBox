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
	"reflect"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildSyncRequest(t *testing.T) {
	cluster := testCluster()
	cluster.Status.WriteService = "cluster-1-rw"

	resolved := []resolvedConsoleUser{
		{
			user: pgtoolboxv1alpha1.PgToolBoxUser{
				ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "test"},
				Spec:       pgtoolboxv1alpha1.PgToolBoxUserSpec{Subject: "alice@example.com"},
			},
			role: &pgtoolboxv1alpha1.PgToolBoxRole{
				Spec: pgtoolboxv1alpha1.PgToolBoxRoleSpec{Level: pgtoolboxv1alpha1.RoleLevelDBA},
			},
			credential: roleCredential{username: "app_owner", password: "hunter2"},
		},
		{
			// At the level pgAdmin admits, but not provisionable: reported.
			user: pgtoolboxv1alpha1.PgToolBoxUser{
				ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "test"},
				Spec:       pgtoolboxv1alpha1.PgToolBoxUserSpec{Subject: "bob@example.com"},
			},
			role: &pgtoolboxv1alpha1.PgToolBoxRole{
				Spec: pgtoolboxv1alpha1.PgToolBoxRoleSpec{Level: pgtoolboxv1alpha1.RoleLevelDBA},
			},
			credential:           roleCredential{username: "owner2", password: "secret"},
			pgAdminExcluded:      true,
			pgAdminExcludeReason: "DatabaseRole not applied",
		},
		{
			// Below accessMinLevel: the proxy refuses them at the route, so
			// provisioning an account would write their postgres credential
			// into the Pod for a screen they cannot open. Skipped silently —
			// nothing about this user is wrong.
			user: pgtoolboxv1alpha1.PgToolBoxUser{
				ObjectMeta: metav1.ObjectMeta{Name: "carol", Namespace: "test"},
				Spec:       pgtoolboxv1alpha1.PgToolBoxUserSpec{Subject: "carol@example.com"},
			},
			role: &pgtoolboxv1alpha1.PgToolBoxRole{
				Spec: pgtoolboxv1alpha1.PgToolBoxRoleSpec{Level: pgtoolboxv1alpha1.RoleLevelView},
			},
			credential: roleCredential{username: "viewer", password: "secret"},
		},
	}

	request, degraded := buildSyncRequest(testConsole(), resolved, cluster)
	if len(request.Users) != 1 {
		t.Fatalf("users = %v, want 1", request.Users)
	}
	if !reflect.DeepEqual(degraded, []string{"bob@example.com"}) {
		t.Fatalf("degraded = %v", degraded)
	}

	got := request.Users[0]
	if got.Subject != "alice@example.com" || got.PgAdminRole != "Administrator" {
		t.Fatalf("user = %+v", got)
	}
	if got.Password != "hunter2" || got.Server.Username != "app_owner" {
		t.Fatalf("credential = %+v", got)
	}
	if got.Server.Host != "cluster-1-rw.test.svc" {
		t.Fatalf("host = %q", got.Server.Host)
	}
}

func TestBuildSyncRequestNoCluster(t *testing.T) {
	cluster := testCluster()
	resolved := []resolvedConsoleUser{{
		user: pgtoolboxv1alpha1.PgToolBoxUser{
			Spec: pgtoolboxv1alpha1.PgToolBoxUserSpec{Subject: "alice@example.com"},
		},
		role:       &pgtoolboxv1alpha1.PgToolBoxRole{Spec: pgtoolboxv1alpha1.PgToolBoxRoleSpec{Level: pgtoolboxv1alpha1.RoleLevelDBA}},
		credential: roleCredential{username: "app_owner", password: "hunter2"},
	}}

	request, degraded := buildSyncRequest(testConsole(), resolved, cluster)
	if len(request.Users) != 1 {
		t.Fatalf("users = %v", request.Users)
	}
	if len(degraded) != 0 {
		t.Fatalf("degraded = %v", degraded)
	}
	if request.Users[0].Server.Host != "cluster-1-rw.test.svc" {
		t.Fatalf("fallback host = %q", request.Users[0].Server.Host)
	}
}

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
	a := adminsync.SyncRequest{Users: []adminsync.User{{
		Subject:     "alice@example.com",
		PgAdminRole: "User",
		Password:    "secret",
		Server: adminsync.Server{
			Name:     "cluster-1",
			Host:     "cluster-1-rw.test.svc",
			Port:     5432,
			Username: "app_owner",
		},
	}}}
	b := adminsync.SyncRequest{Users: []adminsync.User{a.Users[0]}}

	if syncRevision(a) != syncRevision(b) {
		t.Fatalf("identical payloads produced different revisions")
	}

	b.Users[0].Password = "different"
	if syncRevision(a) == syncRevision(b) {
		t.Fatalf("changed password must change revision")
	}
}
