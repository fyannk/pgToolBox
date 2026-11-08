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

package offer

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func validServer(i int) Server {
	n := strconv.Itoa(i)
	return Server{
		Name: "c" + n + " rw (app)", Cluster: "c" + n, Endpoint: "rw",
		Host: "c" + n + "-rw.team.svc", Port: 5432, DBName: "app", User: "app",
		Service: "pgtoolbox-c" + n + "-rw-app", SecretName: "c" + n + "-app", CASecretName: "c" + n + "-ca",
	}
}

func validDoc() Document {
	role := validServer(2)
	role.Name, role.User, role.Service, role.SecretName, role.SecretResourceVersion = "c2 rw (owner)", "owner", "pgtoolbox-c2-rw-owner", "owner-pw", "42"
	return Document{Version: Version, Servers: []Server{validServer(1), validServer(2), role}}
}

func TestEncodeParseRoundTrip(t *testing.T) {
	doc := validDoc()
	raw, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, doc) {
		t.Fatalf("round trip changed the document:\n%+v\n%+v", got, doc)
	}
	again, _ := Encode(got)
	if string(again) != string(raw) {
		t.Fatal("encoding is not stable")
	}
	empty, err := Encode(Document{})
	if err != nil || !strings.Contains(string(empty), `"servers":[]`) {
		t.Fatalf("empty document: %s, %v", empty, err)
	}
}

func TestParseRefuses(t *testing.T) {
	mutate := func(f func(*Document)) string {
		doc := validDoc()
		f(&doc)
		raw, err := encodeUnchecked(doc)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	many := Document{Version: Version}
	for i := 0; i <= MaxServers; i++ {
		many.Servers = append(many.Servers, validServer(i))
	}
	manyRaw, _ := encodeUnchecked(many)
	cases := []struct {
		name, raw, want string
	}{
		{"unknown field", `{"version":1,"servers":[],"extra":true}`, "unknown field"},
		{"wrong version", `{"version":2,"servers":[]}`, "version 2"},
		{"trailing data", `{"version":1,"servers":[]}{}`, "trailing"},
		{"oversize", `{"version":1,"servers":[]}` + strings.Repeat(" ", MaxDocumentBytes), "exceeds"},
		{"duplicate service", mutate(func(d *Document) { d.Servers[1].Service = d.Servers[0].Service }), "duplicate service"},
		{"duplicate name", mutate(func(d *Document) { d.Servers[1].Name = d.Servers[0].Name }), "duplicate name"},
		{"bad host", mutate(func(d *Document) { d.Servers[0].Host = "evil host" }), "invalid host"},
		{"host with newline", mutate(func(d *Document) { d.Servers[0].Host = "a\nb" }), "invalid host"},
		{"bad secret name", mutate(func(d *Document) { d.Servers[0].SecretName = "Secret_X" }), "invalid secretName"},
		{"bad CA secret name", mutate(func(d *Document) { d.Servers[0].CASecretName = "" }), "invalid caSecretName"},
		{"control character in user", mutate(func(d *Document) { d.Servers[0].User = "app\npassword=x" }), "invalid user"},
		{"control character in dbname", mutate(func(d *Document) { d.Servers[0].DBName = "app\x00" }), "invalid dbname"},
		{"bad service", mutate(func(d *Document) { d.Servers[0].Service = "other" }), "invalid service"},
		{"bad endpoint", mutate(func(d *Document) { d.Servers[0].Endpoint = "pooler:" }), "invalid endpoint"},
		{"bad port", mutate(func(d *Document) { d.Servers[0].Port = 0 }), "invalid port"},
		{"newline in resource version", mutate(func(d *Document) { d.Servers[0].SecretResourceVersion = "1\n2" }), "invalid secretResourceVersion"},
		{"too many servers", manyRaw, "over 64"},
		{"not JSON", "nope", "decode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestEncodeRefusesInvalid(t *testing.T) {
	doc := validDoc()
	doc.Servers[0].Host = "bad host"
	if _, err := Encode(doc); err == nil {
		t.Fatal("Encode accepted an invalid document")
	}
}

func TestSecretNamesSortedUnique(t *testing.T) {
	got := validDoc().SecretNames("")
	want := []string{"c1-app", "c1-ca", "c2-app", "c2-ca", "owner-pw"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SecretNames = %v, want %v", got, want)
	}
	if (Document{}).SecretNames("") != nil {
		t.Fatal("empty document names Secrets")
	}
}

func TestCAPaths(t *testing.T) {
	if got := CAFile("/run/c", "pg"); got != "/run/c/ca/pg.crt" {
		t.Fatal(got)
	}
	if got := CABundleFile("/run/c"); got != "/run/c/ca/bundle.crt" {
		t.Fatal(got)
	}
}

// FuzzParseOffer: whatever Parse accepts is valid, re-encodes, and parses
// back to the same document.
func FuzzParseOffer(f *testing.F) {
	seed, _ := Encode(validDoc())
	f.Add(seed)
	f.Add([]byte(`{"version":1,"servers":[]}`))
	remote, _ := Encode(remoteDoc())
	f.Add(remote)
	f.Fuzz(func(t *testing.T, raw []byte) {
		doc, err := Parse(raw)
		if err != nil {
			return
		}
		// What the sidecar watches and the Roles grant stays a name in a
		// namespace: no path separator, no empty name, CA files distinct
		// per namespace and Cluster.
		for _, ref := range doc.SecretRefs() {
			if ref.Name == "" || strings.Contains(ref.Name, "/") || strings.Contains(ref.Namespace, "/") {
				t.Fatalf("accepted Secret reference %+v", ref)
			}
		}
		for _, s := range doc.Connections() {
			if strings.ContainsAny(s.CAKey(), "/\\") {
				t.Fatalf("CA key %q escapes the CA directory", s.CAKey())
			}
		}
		if err := doc.Validate(); err != nil {
			t.Fatalf("Parse accepted an invalid document: %v", err)
		}
		encoded, err := Encode(doc)
		if err != nil {
			t.Fatalf("accepted document does not re-encode: %v", err)
		}
		again, err := Parse(encoded)
		if err != nil {
			t.Fatalf("re-encoded document does not parse: %v", err)
		}
		if !reflect.DeepEqual(normalize(again), normalize(doc)) {
			t.Fatalf("round trip changed the document")
		}
	})
}

func normalize(d Document) Document {
	if d.Servers == nil {
		d.Servers = []Server{}
	}
	return d
}

// remoteDoc has a granted namespace's server, a personal server and the
// settings database connection.
func remoteDoc() Document {
	doc := validDoc()
	remote := doc.Servers[0]
	remote.Name, remote.Namespace, remote.Service = "team-b/pg rw (app)", "team-b", "pgtoolbox-team-b-pg"
	personal := doc.Servers[0]
	personal.Name, personal.User, personal.Owner, personal.Service, personal.SecretName = "pg rw (alice)", "alice", "alice@example.com", "pgtoolbox-alice", "alice-password"
	settings := doc.Servers[0]
	settings.Name, settings.Service, settings.User, settings.SecretName = "pgAdmin settings database", "pgtoolbox-config-database", "pgadmin_team", "team-pgadmin-config-password"
	doc.Servers = append(doc.Servers, remote, personal)
	doc.ConfigDatabase = &settings
	return doc
}

func TestDocumentNamespacesOwnersAndSecrets(t *testing.T) {
	doc := remoteDoc()
	if _, err := Encode(doc); err != nil {
		t.Fatal(err)
	}
	if got := doc.Namespaces(); !slices.Equal(got, []string{"team-b"}) {
		t.Fatalf("namespaces %v", got)
	}
	if got := doc.Owners(); !slices.Equal(got, []string{"alice@example.com"}) {
		t.Fatalf("owners %v", got)
	}
	local := doc.SecretNames("")
	for _, want := range []string{"alice-password", "team-pgadmin-config-password", doc.Servers[0].SecretName, doc.Servers[0].CASecretName} {
		if !slices.Contains(local, want) {
			t.Fatalf("local Secrets %v lack %s", local, want)
		}
	}
	if got := doc.SecretNames("team-b"); !slices.Equal(got, []string{doc.Servers[0].SecretName, doc.Servers[0].CASecretName}) &&
		!slices.Equal(got, []string{doc.Servers[0].CASecretName, doc.Servers[0].SecretName}) {
		t.Fatalf("team-b Secrets %v", got)
	}
	keys := map[string]bool{}
	for _, ref := range doc.SecretRefs() {
		keys[ref.Key()] = true
	}
	if !keys["team-b/"+doc.Servers[0].SecretName] || !keys[doc.Servers[0].SecretName] {
		t.Fatalf("Secret refs %v must tell the namespaces apart", keys)
	}
	if doc.Servers[1].CAKey() == doc.Servers[0].CAKey() {
		t.Fatal("a granted namespace's Cluster of the same name must get its own CA file")
	}
	if got := len(doc.Connections()); got != len(doc.Servers)+1 {
		t.Fatalf("connections %d", got)
	}
}

func TestValidateNamespaceAndOwner(t *testing.T) {
	for _, mutate := range []func(*Server){
		func(s *Server) { s.Namespace = "Team_B" },
		func(s *Server) { s.Namespace = "a/b" },
		func(s *Server) { s.Owner = "-alice" },
		func(s *Server) { s.Owner = "al ice" },
		func(s *Server) { s.Owner = "al'ice" },
	} {
		doc := validDoc()
		mutate(&doc.Servers[0])
		if err := doc.Validate(); err == nil {
			t.Errorf("accepted %+v", doc.Servers[0])
		}
	}
	doc := validDoc()
	duplicate := doc.Servers[0]
	doc.ConfigDatabase = &duplicate
	if err := doc.Validate(); err == nil {
		t.Error("the settings database must not reuse a server's service")
	}
}
