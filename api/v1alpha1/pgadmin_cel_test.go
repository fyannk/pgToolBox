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
)

// pgAdminYAML is a valid PgAdmin; each case patches it.
const pgAdminYAML = `
apiVersion: pgtoolbox.fyannk.dev/v1alpha1
kind: PgAdmin
metadata:
  name: team-a
  namespace: default
spec:
  clusterRefs: [{name: orders}]
  auth:
    oauth2:
      issuerURL: https://idp.example.com/realms/corp
      clientID: pgadmin
      clientSecretRef: {name: pgadmin-oidc}
      requiredClaims: {groups: [dba]}
`

func pgAdminObject(t *testing.T, mutate func(spec map[string]any, meta map[string]any)) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(pgAdminYAML), &obj.Object); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(obj.Object["spec"].(map[string]any), obj.Object["metadata"].(map[string]any))
	}
	return obj
}

// TestPgAdminCEL applies PgAdmins to a real API server (the oldest the
// bundle supports, via make test-envtest) and checks every admission rule
// of the CRD: the metadata.name rule, the exactly-one rules, bounds,
// patterns and the immutable storage class.
func TestPgAdminCEL(t *testing.T) {
	c := apiServer(t)
	cases := []struct {
		name    string
		mutate  func(spec, meta map[string]any)
		wantErr string
	}{
		{name: "valid"},
		{name: "name of 40 characters", mutate: func(_, m map[string]any) { m["name"] = "a" + strings.Repeat("b", 39) }},
		{name: "name too long", mutate: func(_, m map[string]any) { m["name"] = "a" + strings.Repeat("b", 40) }, wantErr: "DNS-1035 label of at most 40"},
		{name: "name starting with a digit", mutate: func(_, m map[string]any) { m["name"] = "1team" }, wantErr: "DNS-1035 label of at most 40"},
		{name: "name with a dot", mutate: func(_, m map[string]any) { m["name"] = "team.a" }, wantErr: "DNS-1035 label of at most 40"},
		{name: "selector instead of refs", mutate: func(s, _ map[string]any) {
			delete(s, "clusterRefs")
			s["clusterSelector"] = map[string]any{"matchLabels": map[string]any{"team": "a"}}
		}},
		{name: "neither refs nor selector", mutate: func(s, _ map[string]any) { delete(s, "clusterRefs") }, wantErr: "one of clusterRefs, clusterSelector and crossNamespaceClusterRefs is required"},
		{name: "both refs and selector", mutate: func(s, _ map[string]any) { s["clusterSelector"] = map[string]any{} }, wantErr: "clusterRefs and clusterSelector are mutually exclusive"},
		{name: "cross-namespace refs only", mutate: func(s, _ map[string]any) {
			delete(s, "clusterRefs")
			s["crossNamespaceClusterRefs"] = []any{map[string]any{"namespace": "team-b", "name": "payments"}}
		}},
		{name: "cross-namespace ref with a bad namespace", mutate: func(s, _ map[string]any) {
			s["crossNamespaceClusterRefs"] = []any{map[string]any{"namespace": "Team_B", "name": "payments"}}
		}, wantErr: "crossNamespaceClusterRefs[0].namespace"},
		{name: "duplicate cross-namespace refs", mutate: func(s, _ map[string]any) {
			ref := map[string]any{"namespace": "team-b", "name": "payments"}
			s["crossNamespaceClusterRefs"] = []any{ref, ref}
		}, wantErr: "Duplicate value"},
		{name: "replicas without configDatabase", mutate: func(s, _ map[string]any) { s["replicas"] = int64(2) }, wantErr: "replicas above 1 need configDatabase"},
		{name: "replicas with configDatabase", mutate: func(s, _ map[string]any) {
			s["replicas"] = int64(3)
			s["configDatabase"] = map[string]any{"clusterRef": map[string]any{"name": "settings"}}
		}},
		{name: "too many replicas", mutate: func(s, _ map[string]any) {
			s["replicas"] = int64(11)
			s["configDatabase"] = map[string]any{"clusterRef": map[string]any{"name": "settings"}}
		}, wantErr: "replicas"},
		{name: "configDatabase with a bad database name", mutate: func(s, _ map[string]any) {
			s["configDatabase"] = map[string]any{"clusterRef": map[string]any{"name": "settings"}, "database": "pg-admin"}
		}, wantErr: "configDatabase.database"},
		{name: "empty clusterRefs", mutate: func(s, _ map[string]any) { s["clusterRefs"] = []any{} }, wantErr: "clusterRefs"},
		{name: "cluster ref with an uppercase name", mutate: func(s, _ map[string]any) { s["clusterRefs"] = []any{map[string]any{"name": "Orders"}} }, wantErr: "clusterRefs[0].name"},
		{name: "pooler endpoint", mutate: func(s, _ map[string]any) { s["endpoints"] = []any{"rw", "pooler:orders-pooler"} }},
		{name: "unknown endpoint", mutate: func(s, _ map[string]any) { s["endpoints"] = []any{"primary"} }, wantErr: "endpoints[0]"},
		{name: "no auth mode", mutate: func(s, _ map[string]any) { s["auth"] = map[string]any{} }, wantErr: "exactly one of oauth2 and openshiftOAuthProxy"},
		{name: "oauth-proxy session of 8h", mutate: func(s, _ map[string]any) {
			s["auth"] = map[string]any{"openshiftOAuthProxy": map[string]any{"sessionMaxAge": "8h"}}
		}},
		{name: "oauth-proxy session too short", mutate: func(s, _ map[string]any) {
			s["auth"] = map[string]any{"openshiftOAuthProxy": map[string]any{"sessionMaxAge": "1m"}}
		}, wantErr: "sessionMaxAge must be between 5m and 12h"},
		{name: "oauth-proxy session too long", mutate: func(s, _ map[string]any) {
			s["auth"] = map[string]any{"openshiftOAuthProxy": map[string]any{"sessionMaxAge": "13h"}}
		}, wantErr: "sessionMaxAge must be between 5m and 12h"},
		{name: "both auth modes", mutate: func(s, _ map[string]any) {
			s["auth"].(map[string]any)["openshiftOAuthProxy"] = map[string]any{}
		}, wantErr: "exactly one of oauth2 and openshiftOAuthProxy"},
		{name: "oauth2 without requiredClaims", mutate: func(s, _ map[string]any) {
			delete(s["auth"].(map[string]any)["oauth2"].(map[string]any), "requiredClaims")
		}, wantErr: "requiredClaims"},
		{name: "oauth2 over http", mutate: func(s, _ map[string]any) {
			s["auth"].(map[string]any)["oauth2"].(map[string]any)["issuerURL"] = "http://idp.example.com"
		}, wantErr: "issuerURL"},
		{name: "claim value with a quote", mutate: func(s, _ map[string]any) {
			s["auth"].(map[string]any)["oauth2"].(map[string]any)["requiredClaims"] = map[string]any{"groups": []any{"x'y"}}
		}, wantErr: "requiredClaims"},
		{name: "claim name with a quote", mutate: func(s, _ map[string]any) {
			s["auth"].(map[string]any)["oauth2"].(map[string]any)["requiredClaims"] = map[string]any{"gr'oups": []any{"dba"}}
		}, wantErr: "claim names are"},
		{name: "route without hostname", mutate: func(s, _ map[string]any) { s["exposure"] = map[string]any{"type": "route"} }, wantErr: "hostname is required"},
		{name: "gateway without gateway", mutate: func(s, _ map[string]any) {
			s["exposure"] = map[string]any{"type": "gateway", "hostname": "pga.example.com"}
		}, wantErr: "gateway is required exactly when type is gateway"},
		{name: "ingressClassName on a route", mutate: func(s, _ map[string]any) {
			s["exposure"] = map[string]any{"type": "route", "hostname": "pga.example.com", "ingressClassName": "nginx"}
		}, wantErr: "ingressClassName applies to ingress exposure only"},
		{name: "reserved exposure label", mutate: func(s, _ map[string]any) {
			s["exposure"] = map[string]any{"labels": map[string]any{"pgtoolbox.fyannk.dev/x": "y"}}
		}, wantErr: "reserved"},
		{name: "reserved propagated label", mutate: func(s, _ map[string]any) {
			s["metadataPropagation"] = []any{map[string]any{"target": "Pod", "labels": map[string]any{"app.kubernetes.io/name": "x"}}}
		}, wantErr: "reserved"},
		{name: "image without tag or digest", mutate: func(s, _ map[string]any) {
			s["image"] = map[string]any{"repository": "registry.example.com/pgadmin"}
		}, wantErr: "an image needs a tag, a digest or both"},
		{name: "image with a bad digest", mutate: func(s, _ map[string]any) {
			s["image"] = map[string]any{"repository": "registry.example.com/pgadmin", "digest": "sha256:short"}
		}, wantErr: "image.digest"},
		{name: "too many admins", mutate: func(s, _ map[string]any) {
			admins := make([]any, 0, 33)
			for i := range 33 {
				admins = append(admins, strings.Repeat("a", i+1))
			}
			s["admins"] = admins
		}, wantErr: "admins"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := pgAdminObject(t, tc.mutate)
			err := c.Create(context.Background(), obj)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				_ = c.Delete(context.Background(), obj)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				_ = c.Delete(context.Background(), obj)
				t.Fatalf("create error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestPgAdminCELStorageClassImmutable checks the transition rule.
func TestPgAdminCELStorageClassImmutable(t *testing.T) {
	c := apiServer(t)
	obj := pgAdminObject(t, func(s, m map[string]any) {
		m["name"] = "immutable"
		s["storage"] = map[string]any{"storageClassName": "fast"}
	})
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
	_ = unstructured.SetNestedField(obj.Object, "slow", "spec", "storage", "storageClassName")
	if err := c.Update(context.Background(), obj); err == nil || !strings.Contains(err.Error(), "storageClassName is immutable") {
		t.Fatalf("update error = %v", err)
	}
}

// TestPgAdminCELConfigDatabaseTransitions: configDatabase is chosen at
// creation; adding, removing or changing it is refused (pgAdmin's settings
// do not move between SQLite and PostgreSQL automatically).
func TestPgAdminCELConfigDatabaseTransitions(t *testing.T) {
	c := apiServer(t)
	sqlite := pgAdminObject(t, func(_, m map[string]any) { m["name"] = "sqlite" })
	if err := c.Create(context.Background(), sqlite); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), sqlite) })
	_ = unstructured.SetNestedMap(sqlite.Object, map[string]any{"clusterRef": map[string]any{"name": "settings"}}, "spec", "configDatabase")
	if err := c.Update(context.Background(), sqlite); err == nil || !strings.Contains(err.Error(), "configDatabase cannot be added to or removed") {
		t.Fatalf("adding configDatabase: %v", err)
	}

	postgres := pgAdminObject(t, func(s, m map[string]any) {
		m["name"] = "postgres"
		s["configDatabase"] = map[string]any{"clusterRef": map[string]any{"name": "settings"}}
	})
	if err := c.Create(context.Background(), postgres); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), postgres) })
	changed := postgres.DeepCopy()
	_ = unstructured.SetNestedField(changed.Object, "other", "spec", "configDatabase", "clusterRef", "name")
	if err := c.Update(context.Background(), changed); err == nil || !strings.Contains(err.Error(), "configDatabase is immutable") {
		t.Fatalf("changing configDatabase: %v", err)
	}
	unstructured.RemoveNestedField(postgres.Object, "spec", "configDatabase")
	if err := c.Update(context.Background(), postgres); err == nil || !strings.Contains(err.Error(), "configDatabase cannot be added to or removed") {
		t.Fatalf("removing configDatabase: %v", err)
	}
}

// TestPgAdminSchemaDefaults checks the static defaults the CRD applies.
func TestPgAdminSchemaDefaults(t *testing.T) {
	c := apiServer(t)
	obj := pgAdminObject(t, func(_, m map[string]any) { m["name"] = "defaults" })
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
	for path, want := range map[string]any{
		"spec.endpoints":                       []any{"rw"},
		"spec.replicas":                        int64(1),
		"spec.credentials.application":         true,
		"spec.credentials.superuser":           false,
		"spec.credentials.databaseRoles":       true,
		"spec.storage.retainPolicy":            "Retain",
		"spec.exposure.type":                   "clusterIP",
		"spec.networkPolicy.enabled":           false,
		"spec.auth.oauth2.usernameClaim":       "email",
		"spec.auth.oauth2.clientSecretRef.key": "client-secret",
	} {
		got, _, _ := unstructured.NestedFieldNoCopy(obj.Object, strings.Split(path, ".")...)
		if gotYAML, _ := yaml.Marshal(got); string(gotYAML) != mustYAML(want) {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
}

func mustYAML(v any) string {
	raw, _ := yaml.Marshal(v)
	return string(raw)
}
