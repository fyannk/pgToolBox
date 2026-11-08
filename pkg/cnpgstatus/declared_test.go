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

package cnpgstatus

import (
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeclared(t *testing.T) {
	tests := []struct {
		name     string
		applied  *bool
		observed int64
		gen      int64
		message  string
		state    string
		class    Class
	}{
		{"applied current", new(true), 3, 3, "", DeclaredApplied, ClassOK},
		{"applied older generation", new(true), 2, 3, "", DeclaredPendingGeneration, ClassProgressing},
		{"failed keeps the PostgreSQL error", new(false), 2, 3, `role "x" does not exist`, DeclaredFailed, ClassWarning},
		{"failed even at current generation", new(false), 3, 3, "conflict", DeclaredFailed, ClassWarning},
		{"replica cluster", nil, 3, 3, waitingForPrimaryMessage, DeclaredWaitingForPrimary, ClassInactive},
		{"unknown with message", nil, 0, 1, "cannot connect", DeclaredUnknown, ClassUnknown},
		{"never reconciled", nil, 0, 1, "", DeclaredNotReconciled, ClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Declared(tt.applied, tt.observed, tt.gen, tt.message)
			if got.State != tt.state || got.Class != tt.class || got.Message != tt.message {
				t.Errorf("Declared = %+v, want %s/%s", got, tt.state, tt.class)
			}
		})
	}
}

func TestDeclaredObjects(t *testing.T) {
	db := &cnpgv1.Database{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: cnpgv1.DatabaseStatus{
			Applied: new(true), ObservedGeneration: 2,
			Extensions: []cnpgv1.DatabaseObjectStatus{{Name: "postgis", Applied: false, Message: "not available"}, {Name: "pg_trgm", Applied: true}},
		},
	}
	got := DeclaredDatabase(db)
	if got.Class != ClassWarning || len(got.FailedObjects) != 1 || got.FailedObjects[0] != "extension/postgis: not available" {
		t.Errorf("DeclaredDatabase = %+v", got)
	}
	if DeclaredDatabase(nil).Class != ClassUnknown || DeclaredPublication(nil).Class != ClassUnknown || DeclaredSubscription(nil).Class != ClassUnknown {
		t.Error("nil declarative objects must be unknown")
	}
	pub := &cnpgv1.Publication{ObjectMeta: metav1.ObjectMeta{Generation: 1}, Status: cnpgv1.PublicationStatus{Applied: new(true), ObservedGeneration: 1}}
	if DeclaredPublication(pub).Class != ClassOK {
		t.Error("applied publication not ok")
	}
	sub := &cnpgv1.Subscription{ObjectMeta: metav1.ObjectMeta{Generation: 4}, Status: cnpgv1.SubscriptionStatus{Applied: new(true), ObservedGeneration: 3}}
	if DeclaredSubscription(sub).Class != ClassProgressing {
		t.Error("subscription with a new generation not progressing")
	}

	role := &cnpgv1.DatabaseRole{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Status:     cnpgv1.DatabaseRoleStatus{Applied: new(true), ObservedGeneration: 1, SecretResourceVersion: "42"},
	}
	if got := DeclaredRole(Minor130, role); !got.Applicable || got.Class != ClassOK || got.SecretResourceVersion != "42" {
		t.Errorf("DatabaseRole on 1.30: %+v", got)
	}
	if got := DeclaredRole(Minor131, nil); !got.Applicable || got.Class != ClassUnknown {
		t.Errorf("nil DatabaseRole: %+v", got)
	}
}

func TestManagedRoles(t *testing.T) {
	if got := ManagedRoles(nil); got.Class != ClassUnknown {
		t.Errorf("nil cluster: %+v", got)
	}
	if got := ManagedRoles(&cnpgv1.Cluster{}); got.Class != ClassInfo || got.Roles != nil {
		t.Errorf("no roles: %+v", got)
	}
	c := &cnpgv1.Cluster{
		Spec: cnpgv1.ClusterSpec{Managed: &cnpgv1.ManagedConfiguration{Roles: []cnpgv1.RoleConfiguration{
			{Name: "app"}, {Name: "reporting"}, {Name: "ghost"},
		}}},
		Status: cnpgv1.ClusterStatus{ManagedRolesStatus: cnpgv1.ManagedRoles{
			ByStatus: map[cnpgv1.RoleStatus][]string{
				cnpgv1.RoleStatusReconciled:            {"app"},
				cnpgv1.RoleStatusPendingReconciliation: {"reporting"},
				cnpgv1.RoleStatusReserved:              {"postgres", "streaming_replica"},
				"brand-new":                            {"odd"},
			},
			CannotReconcile: map[string][]string{"reporting": {"role owns objects"}},
			PasswordStatus:  map[string]cnpgv1.PasswordState{"app": {TransactionID: 77}},
		}},
	}
	got := ManagedRoles(c)
	byName := map[string]ManagedRoleRow{}
	for _, r := range got.Roles {
		byName[r.Name] = r
	}
	checks := []struct {
		name     string
		status   string
		class    Class
		declared bool
	}{
		{"app", RoleReconciled, ClassOK, true},
		{"reporting", RolePendingReconciliation, ClassWarning, true},
		{"ghost", RoleUnreported, ClassUnknown, true},
		{"postgres", RoleReserved, ClassInfo, false},
		{"odd", "brand-new", ClassUnknown, false},
	}
	for _, check := range checks {
		r, ok := byName[check.name]
		if !ok || r.Status != check.status || r.Class != check.class || r.Declared != check.declared {
			t.Errorf("role %s = %+v, want %s/%s declared=%v", check.name, r, check.status, check.class, check.declared)
		}
	}
	if byName["app"].PasswordTransactionID != 77 || len(byName["reporting"].Errors) != 1 {
		t.Errorf("password / errors: %+v %+v", byName["app"], byName["reporting"])
	}
	if got.Class != ClassWarning {
		t.Errorf("roll-up = %s, want warning", got.Class)
	}
	for i := 1; i < len(got.Roles); i++ {
		if got.Roles[i-1].Name > got.Roles[i].Name {
			t.Fatal("roles are not sorted")
		}
	}
}

func TestTablespaces(t *testing.T) {
	if got := Tablespaces(nil); got.Class != ClassUnknown {
		t.Errorf("nil cluster: %+v", got)
	}
	if got := Tablespaces(&cnpgv1.Cluster{}); got.Class != ClassInfo {
		t.Errorf("none: %+v", got)
	}
	c := &cnpgv1.Cluster{
		Spec: cnpgv1.ClusterSpec{Tablespaces: []cnpgv1.TablespaceConfiguration{{Name: "idx"}, {Name: "tmp", Temporary: true}, {Name: "missing"}}},
		Status: cnpgv1.ClusterStatus{TablespacesStatus: []cnpgv1.TablespaceState{
			{Name: "idx", Owner: "app", State: cnpgv1.TablespaceStatusReconciled},
			{Name: "tmp", State: cnpgv1.TablespaceStatusPendingReconciliation, Error: "directory not empty"},
			{Name: "orphan", State: "weird"},
		}},
	}
	got := Tablespaces(c)
	want := map[string]Class{"idx": ClassOK, "tmp": ClassWarning, "missing": ClassUnknown, "orphan": ClassUnknown}
	if len(got.Tablespaces) != len(want) {
		t.Fatalf("rows = %+v", got.Tablespaces)
	}
	for _, r := range got.Tablespaces {
		if want[r.Name] != r.Class {
			t.Errorf("%s = %s, want %s", r.Name, r.Class, want[r.Name])
		}
		if r.Name == "orphan" && r.Declared {
			t.Error("orphan is not declared")
		}
		if r.Name == "tmp" && !r.Temporary {
			t.Error("tmp is temporary")
		}
	}
	if got.Class != ClassWarning {
		t.Errorf("roll-up = %s", got.Class)
	}
}

func TestPoolerState(t *testing.T) {
	pooler := func(phase cnpgv1.PoolerPhase, desired, instances int32, paused bool) *cnpgv1.Pooler {
		return &cnpgv1.Pooler{
			Spec: cnpgv1.PoolerSpec{
				Instances: new(desired),
				PgBouncer: &cnpgv1.PgBouncerSpec{Paused: new(paused)},
			},
			Status: cnpgv1.PoolerStatus{Phase: phase, Instances: instances},
		}
	}
	tests := []struct {
		name  string
		minor Minor
		in    *cnpgv1.Pooler
		class Class
		known bool
	}{
		{"active ready count", Minor130, pooler(cnpgv1.PoolerPhaseActive, 2, 2, false), ClassOK, true},
		{"active short of pods", Minor130, pooler(cnpgv1.PoolerPhaseActive, 2, 1, false), ClassProgressing, true},
		{"paused", Minor130, pooler(cnpgv1.PoolerPhasePaused, 1, 1, true), ClassInactive, true},
		{"inactive", Minor131, pooler(cnpgv1.PoolerPhaseInactive, 1, 0, false), ClassWarning, true},
		{"failed", Minor130, pooler(cnpgv1.PoolerPhaseFailed, 1, 1, false), ClassCritical, true},
		{"foreign phase", Minor130, pooler("degraded", 1, 1, false), ClassWarning, false},
		{"1.30 without phase", Minor130, pooler("", 1, 1, false), ClassUnknown, false},
		{"paused without phase", Minor130, pooler("", 1, 1, true), ClassInactive, false},
		{"scaled to zero", Minor130, pooler(cnpgv1.PoolerPhaseActive, 0, 0, false), ClassInactive, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PoolerState(tt.minor, tt.in)
			if got.Class != tt.class || got.PhaseKnown != tt.known {
				t.Errorf("PoolerState = %+v, want %s known=%v", got, tt.class, tt.known)
			}
			if got.Type != "rw" || got.PoolMode != "session" {
				t.Errorf("defaults: type %q poolMode %q", got.Type, got.PoolMode)
			}
		})
	}
	withError := pooler(cnpgv1.PoolerPhaseActive, 1, 1, false)
	withError.Status.Error = "admission refused"
	if got := PoolerState(Minor130, withError); got.Class != ClassWarning {
		t.Errorf("admission error: %+v", got)
	}
	if got := PoolerState(Minor130, nil); got.Class != ClassUnknown {
		t.Errorf("nil pooler: %+v", got)
	}
}
