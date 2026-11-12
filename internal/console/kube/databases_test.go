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

package kube

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

func rawDeclared(kind, name, cluster string, spec, status map[string]any) *unstructured.Unstructured {
	if spec == nil {
		spec = map[string]any{}
	}
	spec["cluster"] = map[string]any{"name": cluster}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": "payments", "uid": "u-" + name},
		"spec":     spec,
		"status":   status,
	}}
}

// TestDeclaredObjectsRequireTheExactClusterReference proves each of the
// four kinds selects on spec.cluster.name. These watches are
// namespace-scoped because RBAC cannot pin one by name, so another
// cluster's declarations are expected traffic that must not be adopted.
func TestDeclaredObjectsRequireTheExactClusterReference(t *testing.T) {
	t.Parallel()
	f := NewFacts("payments", "orders", testNow)
	objs := []*unstructured.Unstructured{
		rawDeclared("Database", "app-db", "orders", map[string]any{"name": "app", "owner": "app"}, map[string]any{"applied": true}),
		rawDeclared("Database", "other-db", "other", map[string]any{"name": "other"}, nil),
		rawDeclared("DatabaseRole", "app-role", "orders", map[string]any{"name": "app"}, map[string]any{"applied": true}),
		rawDeclared("DatabaseRole", "other-role", "other", map[string]any{"name": "app"}, nil),
		rawDeclared("Publication", "app-pub", "orders", map[string]any{"name": "pub", "dbname": "app"}, nil),
		rawDeclared("Publication", "other-pub", "other", map[string]any{"name": "pub", "dbname": "app"}, nil),
		rawDeclared("Subscription", "app-sub", "orders", map[string]any{"name": "sub", "dbname": "app", "publicationName": "pub"}, nil),
		rawDeclared("Subscription", "other-sub", "other", map[string]any{"name": "sub", "dbname": "app", "publicationName": "pub"}, nil),
	}
	var state struct {
		Databases     []observe.DatabaseFacts
		Roles         []observe.DatabaseRoleFacts
		Publications  []observe.PublicationFacts
		Subscriptions []observe.SubscriptionFacts
	}
	for _, o := range objs {
		var ok bool
		var err error
		switch o.GetKind() {
		case "Database":
			var d observe.DatabaseFacts
			if d, ok, err = f.c.convertDatabase(o.Object); ok {
				state.Databases = append(state.Databases, d)
			}
		case "DatabaseRole":
			var d observe.DatabaseRoleFacts
			if d, ok, err = f.c.convertDatabaseRole(o.Object); ok {
				state.Roles = append(state.Roles, d)
			}
		case "Publication":
			var d observe.PublicationFacts
			if d, ok, err = f.c.convertPublication(o.Object); ok {
				state.Publications = append(state.Publications, d)
			}
		case "Subscription":
			var d observe.SubscriptionFacts
			if d, ok, err = f.c.convertSubscription(o.Object); ok {
				state.Subscriptions = append(state.Subscriptions, d)
			}
		}
		if err != nil {
			t.Fatalf("convert %s/%s: %v", o.GetKind(), o.GetName(), err)
		}
	}
	if len(state.Databases) != 1 || state.Databases[0].Name != "app-db" {
		t.Errorf("databases = %+v, want only the one referencing orders", state.Databases)
	}
	if len(state.Roles) != 1 || len(state.Publications) != 1 || len(state.Subscriptions) != 1 {
		t.Errorf("selected roles=%d publications=%d subscriptions=%d, want one each",
			len(state.Roles), len(state.Publications), len(state.Subscriptions))
	}
	if applied := state.Databases[0].Applied; applied == nil || !*applied {
		t.Errorf("applied = %v, want the operator's reported verdict", applied)
	}
}

// TestDeclaredRoleReportsTheSecretReferenceWithoutReadingIt proves the
// console records only that a password Secret is referenced. It holds no
// Secret permission, so neither the Secret's name nor its content may
// cross the adapter boundary.
func TestDeclaredRoleReportsTheSecretReferenceWithoutReadingIt(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	raw := rawDeclared("DatabaseRole", "app-role", "orders", map[string]any{
		"name":           "app",
		"passwordSecret": map[string]any{"name": "app-role-password-canary"},
		"superuser":      true,
		"inRoles":        []any{"reader", "writer"},
	}, map[string]any{"applied": true})

	facts, member, err := c.convertDatabaseRole(raw.Object)
	if err != nil || !member {
		t.Fatalf("conversion: member=%v err=%v", member, err)
	}
	if !facts.HasPasswordSecret {
		t.Error("a declared password Secret was not reported as referenced")
	}
	if !facts.Superuser || len(facts.InRoles) != 2 {
		t.Errorf("facts = %+v, want the declared attributes", facts)
	}
	// The Secret's name must appear in no field that reaches the page.
	for _, field := range append([]string{facts.Name, facts.Role, facts.Message}, facts.InRoles...) {
		if strings.Contains(field, "canary") {
			t.Fatalf("the Secret's name reached the facts: %q", field)
		}
	}
}

// TestDeclaredPublicationDoesNotRenderTheObjectList proves the target is
// reduced to whether it is the whole database. The enumerated object
// list is a table list — database content, which is pgAdmin's to show
// and not this console's.
func TestDeclaredPublicationDoesNotRenderTheObjectList(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	raw := rawDeclared("Publication", "app-pub", "orders", map[string]any{
		"name": "pub", "dbname": "app",
		"target": map[string]any{"objects": []any{
			map[string]any{"tablesInSchema": "secret_schema_canary"},
		}},
	}, nil)

	facts, member, err := c.convertPublication(raw.Object)
	if err != nil || !member {
		t.Fatalf("conversion: member=%v err=%v", member, err)
	}
	if facts.AllTables {
		t.Error("an enumerated target was reported as all tables")
	}
	if strings.Contains(facts.Publication+facts.Database+facts.Message, "canary") {
		t.Error("a declared table list reached the facts")
	}
}
