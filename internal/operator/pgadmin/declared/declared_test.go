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

package declared

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// chartRendering is what the chart's ConfigMap carries for a pgadmin.servers
// list: the servers as written, wrapped with the version.
const chartRendering = `{"version":1,"servers":[
 {"cluster":"orders","endpoint":"rw","credentials":{"application":{"secret":"orders-app"}}},
 {"cluster":"orders","endpoint":"ro","credentials":{"databaseRole":{"name":"orders-reporting","secret":"orders-reporting-password"}}},
 {"cluster":"orders","endpoint":"pooler:orders-pool","credentials":{"superuser":{"secret":"orders-superuser"}}},
 {"cluster":"billing","endpoint":"rw","database":"billing","user":"billing_owner","credentials":{"secret":"billing-owner"}},
 {"cluster":"legacy","endpoint":"rw","database":"app","user":"alice","caSecret":"legacy-server-ca","credentials":"none"}
]}`

func TestParseTheChartRendering(t *testing.T) {
	doc, err := Parse([]byte(chartRendering))
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]Kind, 0, len(doc.Servers))
	for _, s := range doc.Servers {
		kinds = append(kinds, s.Credentials.Kind)
	}
	if want := []Kind{KindApplication, KindDatabaseRole, KindSuperuser, KindSecret, KindNone}; !slices.Equal(kinds, want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
	if got := doc.Servers[1].Credentials; got.Role != "orders-reporting" || got.Secret != "orders-reporting-password" {
		t.Fatalf("databaseRole %+v", got)
	}
	names := doc.Names()
	want := Names{
		Clusters:      []string{"billing", "legacy", "orders"},
		DatabaseRoles: []string{"orders-reporting"},
		Poolers:       []string{"orders-pool"},
		Secrets: []string{
			"billing-ca", "billing-owner", "legacy-server-ca", "orders-app", "orders-ca",
			"orders-reporting-password", "orders-superuser",
		},
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names\n got %+v\nwant %+v", names, want)
	}
	// Round trip: what the binary writes, it reads back the same.
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(raw)
	if err != nil || !reflect.DeepEqual(again, doc) {
		t.Fatalf("round trip: %v\n%+v\n%+v", err, again, doc)
	}
}

func TestServerDerivations(t *testing.T) {
	doc, err := Parse([]byte(chartRendering))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, host, ca, service string }{
		{"orders rw (application)", "orders-rw.team.svc", "orders-ca", "pgtoolbox-orders-rw-application"},
		{"orders ro (orders-reporting)", "orders-ro.team.svc", "orders-ca", "pgtoolbox-orders-ro-orders-reporting"},
		{"orders pooler:orders-pool (superuser)", "orders-pool.team.svc", "orders-ca", "pgtoolbox-orders-pooler-orders-pool-superuser"},
		{"billing rw (billing_owner)", "billing-rw.team.svc", "billing-ca", "pgtoolbox-billing-rw-billing-owner"},
		{"legacy rw (alice)", "legacy-rw.team.svc", "legacy-server-ca", "pgtoolbox-legacy-rw-alice"},
	}
	services := doc.Services()
	for i, tc := range cases {
		s := doc.Servers[i]
		if s.DisplayName() != tc.name || s.Host("team") != tc.host || s.CASecretName() != tc.ca || services[i] != tc.service {
			t.Errorf("server %d: %q %q %q %q, want %+v", i, s.DisplayName(), s.Host("team"), s.CASecretName(), services[i], tc)
		}
	}
	named := Server{Name: "Orders (prod)", Cluster: "orders", Endpoint: "rw"}
	if named.DisplayName() != "Orders (prod)" {
		t.Fatalf("a declared name is kept: %s", named.DisplayName())
	}
}

func TestParseRefusals(t *testing.T) {
	server := func(fields string) string {
		return `{"version":1,"servers":[{"cluster":"orders","endpoint":"rw"` + fields + `}]}`
	}
	cases := []struct{ name, raw, want string }{
		{"credentials missing: no default", server(``), "credentials missing"},
		{"credentials null", server(`,"credentials":null`), "credentials missing"},
		{"two sources", server(`,"credentials":{"application":{"secret":"a"},"secret":"b"}`), "exactly one of"},
		{"empty object", server(`,"credentials":{}`), "exactly one of"},
		{"another word", server(`,"credentials":"application"`), `only string form is "none"`},
		{"unknown source", server(`,"credentials":{"password":"x"}`), "unknown field"},
		{"secret needs user and database", server(`,"credentials":{"secret":"s"}`), "need user and database"},
		{"none needs user and database", server(`,"user":"u","credentials":"none"`), "need user and database"},
		{"bad endpoint", `{"version":1,"servers":[{"cluster":"orders","endpoint":"primary","credentials":"none","user":"u","database":"d"}]}`, "invalid endpoint"},
		{"bad cluster", `{"version":1,"servers":[{"cluster":"Orders","endpoint":"rw","credentials":"none","user":"u","database":"d"}]}`, "invalid cluster"},
		{"bad secret", server(`,"credentials":{"application":{"secret":"../x"}}`), "invalid credentials secret"},
		{"role without a name", server(`,"credentials":{"databaseRole":{"secret":"s"}}`), "invalid credentials databaseRole name"},
		{"user with a line break", server(`,"user":"a\nb","database":"d","credentials":"none"`), "invalid user"},
		{"unknown field", server(`,"credentials":"none","user":"u","database":"d","password":"x"`), "unknown field"},
		{"another version", `{"version":2,"servers":[]}`, "version 2"},
		{"trailing data", `{"version":1,"servers":[]} {}`, "trailing data"},
		{"duplicate names", `{"version":1,"servers":[
			{"cluster":"orders","endpoint":"rw","credentials":{"application":{"secret":"a"}}},
			{"cluster":"orders","endpoint":"rw","credentials":{"application":{"secret":"b"}}}]}`, "duplicate name"},
		{"caSecret and caConfigMap", server(`,"caSecret":"s","caConfigMap":"c","credentials":{"application":{"secret":"a"}}`), "caSecret and caConfigMap are exclusive"},
		{"bad caConfigMap", server(`,"caConfigMap":"../c","credentials":{"application":{"secret":"a"}}`), "invalid caConfigMap"},
		{"one Cluster, a ConfigMap and a Secret", `{"version":1,"servers":[
			{"cluster":"orders","endpoint":"rw","caConfigMap":"orders-ca-crt","credentials":{"application":{"secret":"a"}}},
			{"cluster":"orders","endpoint":"ro","credentials":{"application":{"secret":"a"}}}]}`, "must name the same caConfigMap"},
		{"one Cluster, two ConfigMaps", `{"version":1,"servers":[
			{"cluster":"orders","endpoint":"rw","caConfigMap":"one","credentials":{"application":{"secret":"a"}}},
			{"cluster":"orders","endpoint":"ro","caConfigMap":"two","credentials":{"application":{"secret":"a"}}}]}`, "must name the same caConfigMap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	var many strings.Builder
	many.WriteString(`{"version":1,"servers":[`)
	for i := range MaxServers + 1 {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`{"name":"s` + strings.Repeat("x", i) + `","cluster":"c","endpoint":"rw","credentials":{"application":{"secret":"c-app"}}}`)
	}
	many.WriteString(`]}`)
	if _, err := Parse([]byte(many.String())); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("over MaxServers: %v", err)
	}
}

// TestCASources: where each server's CA comes from, and what the Role
// must grant for it. A caConfigMap names no Secret at all (the point of
// it: CloudNativePG's <cluster>-ca also holds the signing key); only the
// derived default is reported as such.
func TestCASources(t *testing.T) {
	doc, err := Parse([]byte(`{"version":1,"servers":[
	 {"cluster":"orders","endpoint":"rw","caConfigMap":"orders-ca-crt","credentials":{"application":{"secret":"orders-app"}}},
	 {"cluster":"orders","endpoint":"ro","caConfigMap":"orders-ca-crt","credentials":{"application":{"secret":"orders-app"}}},
	 {"cluster":"billing","endpoint":"rw","caSecret":"billing-server-ca","credentials":{"application":{"secret":"billing-app"}}},
	 {"cluster":"legacy","endpoint":"rw","database":"app","user":"alice","credentials":"none"},
	 {"cluster":"shared","endpoint":"rw","caConfigMap":"a-bundle","credentials":{"application":{"secret":"shared-app"}}}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		caSecret string
		derived  bool
	}{
		{"", false}, {"", false}, {"billing-server-ca", false}, {"legacy-ca", true}, {"", false},
	}
	for i, tc := range cases {
		s := doc.Servers[i]
		if s.CASecretName() != tc.caSecret || s.DerivedCASecret() != tc.derived {
			t.Errorf("server %d: CA Secret %q derived %v, want %+v", i, s.CASecretName(), s.DerivedCASecret(), tc)
		}
	}
	if got, want := doc.CAConfigMaps(), []string{"a-bundle", "orders-ca-crt"}; !slices.Equal(got, want) {
		t.Errorf("CAConfigMaps %v, want %v", got, want)
	}
	want := []string{"billing-app", "billing-server-ca", "legacy-ca", "orders-app", "shared-app"}
	if got := doc.Names().Secrets; !slices.Equal(got, want) {
		t.Errorf("Names().Secrets %v, want %v: no CA Secret for a caConfigMap", got, want)
	}
	if got := CAConfigMapFile("/run/pgtoolbox/ca", "orders-ca-crt"); got != "/run/pgtoolbox/ca/orders-ca-crt/ca.crt" {
		t.Errorf("CAConfigMapFile %s", got)
	}
}

// FuzzParseDeclared: whatever Parse accepts is valid, names only objects
// of its own servers, and survives a round trip unchanged; nothing it
// accepts can name a Secret outside what Names returns.
func FuzzParseDeclared(f *testing.F) {
	f.Add([]byte(chartRendering))
	f.Add([]byte(`{"version":1,"servers":[]}`))
	f.Add([]byte(`{"version":1,"servers":[{"cluster":"a","endpoint":"pooler:b","credentials":{"databaseRole":{"name":"r","secret":"s"}}}]}`))
	f.Add([]byte(`{"version":1,"servers":[{"cluster":"a","endpoint":"rw","caConfigMap":"a-ca","credentials":{"application":{"secret":"a-app"}}}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		doc, err := Parse(raw)
		if err != nil {
			return
		}
		if err := doc.Validate(); err != nil {
			t.Fatalf("accepted an invalid list: %v", err)
		}
		names := doc.Names()
		for _, s := range doc.Servers {
			if s.Credentials.Kind != KindNone && !slices.Contains(names.Secrets, s.Credentials.Secret) {
				t.Fatalf("secret %q missing from Names", s.Credentials.Secret)
			}
			if !slices.Contains(names.Clusters, s.Cluster) {
				t.Fatalf("server %+v not covered by Names %+v", s, names)
			}
			switch {
			case s.CAConfigMap != "" && (s.CASecret != "" || s.CASecretName() != "" || !slices.Contains(doc.CAConfigMaps(), s.CAConfigMap)):
				t.Fatalf("server %+v: a caConfigMap names a CA Secret, or is not mounted", s)
			case s.CAConfigMap == "" && !slices.Contains(names.Secrets, s.CASecretName()):
				t.Fatalf("server %+v: CA Secret not covered by Names %+v", s, names)
			}
			if strings.ContainsAny(s.Host("ns"), "/:@ \n") {
				t.Fatalf("host %q", s.Host("ns"))
			}
		}
		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Parse(encoded)
		if err != nil || !reflect.DeepEqual(again, doc) {
			t.Fatalf("round trip: %v", err)
		}
	})
}
