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

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

const pgAdminAccessYAML = `
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdminAccess
metadata:
  name: pgadmin-team-a
  namespace: default
spec:
  pgAdmin: {namespace: team-a, name: team}
  clusters: [{name: payments}]
`

// TestPgAdminAccessCEL checks the grant's schema on a real API server.
func TestPgAdminAccessCEL(t *testing.T) {
	c := apiServer(t)
	cases := []struct {
		name    string
		mutate  func(spec map[string]any)
		wantErr string
	}{
		{name: "valid"},
		{name: "no clusters", mutate: func(s map[string]any) { s["clusters"] = []any{} }, wantErr: "clusters"},
		{name: "no pgAdmin", mutate: func(s map[string]any) { delete(s, "pgAdmin") }, wantErr: "pgAdmin"},
		{name: "pgAdmin name too long", mutate: func(s map[string]any) {
			s["pgAdmin"] = map[string]any{"namespace": "team-a", "name": "a" + strings.Repeat("b", 40)}
		}, wantErr: "pgAdmin.name"},
		{name: "pgAdmin namespace with a slash", mutate: func(s map[string]any) {
			s["pgAdmin"] = map[string]any{"namespace": "team/a", "name": "team"}
		}, wantErr: "pgAdmin.namespace"},
		{name: "duplicate cluster", mutate: func(s map[string]any) {
			s["clusters"] = []any{map[string]any{"name": "payments"}, map[string]any{"name": "payments"}}
		}, wantErr: "Duplicate value"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(pgAdminAccessYAML), &obj.Object); err != nil {
				t.Fatal(err)
			}
			obj.SetName("grant-" + strings.Repeat("x", i+1))
			if tc.mutate != nil {
				tc.mutate(obj.Object["spec"].(map[string]any))
			}
			err := c.Create(context.Background(), obj)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("valid grant refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if err == nil {
				t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
			}
		})
	}
}

func TestPgAdminAccessGrants(t *testing.T) {
	g := &pgtoolboxv1alpha1.PgAdminAccess{Spec: pgtoolboxv1alpha1.PgAdminAccessSpec{
		PgAdmin:  pgtoolboxv1alpha1.PgAdminReference{Namespace: "team-a", Name: "team"},
		Clusters: []pgtoolboxv1alpha1.LocalObjectReference{{Name: "payments"}},
	}}
	for _, tc := range []struct {
		namespace, name, cluster string
		want                     bool
	}{
		{"team-a", "team", "payments", true},
		{"team-a", "team", "ledger", false},
		{"team-a", "other", "payments", false},
		{"team-b", "team", "payments", false},
	} {
		if got := g.Grants(tc.namespace, tc.name, tc.cluster); got != tc.want {
			t.Errorf("Grants(%s, %s, %s) = %v", tc.namespace, tc.name, tc.cluster, got)
		}
	}
}
