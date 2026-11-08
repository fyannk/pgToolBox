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

package register

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

// treeSetup plays setup.py with one server tree per user.
type treeSetup struct {
	mu      sync.Mutex
	users   string
	trees   map[string]string // user -> dumped document
	loaded  map[string][]byte // user -> loaded document
	authsOf map[string]string // user -> --auth-source of its load
	calls   []string
}

func flagValue(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func (f *treeSetup) run(_ context.Context, _ string, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if args[0] == "-c" {
		return []byte("9.18\n"), nil
	}
	f.calls = append(f.calls, strings.Join(args[1:], " "))
	user := flagValue(args, "--user")
	switch args[1] {
	case "dump-servers":
		if tree := f.trees[user]; tree != "" {
			return []byte("ok"), os.WriteFile(args[2], []byte(tree), 0o600)
		}
	case "load-servers":
		raw, err := os.ReadFile(args[2])
		if err != nil {
			return nil, err
		}
		f.loaded[user], f.authsOf[user] = raw, flagValue(args, "--auth-source")
	case "get-users":
		return []byte(f.users), nil
	}
	return []byte("ok"), nil
}

func personalOptions(t *testing.T, f *treeSetup, doc offer.Document) Options {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "pgadmin4.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := offer.Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "offer.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return Options{
		OfferFile: filepath.Join(dir, "offer.json"), Owner: "pgtoolbox@pgtoolbox.fyannk.dev", AuthSource: "oauth2",
		SettingsDB: db, LockFile: filepath.Join(dir, "setup.lock"), TempDir: dir, Setup: setupPath,
		WaitForDB: time.Second, Run: f.run,
	}
}

func server(name, user, owner string) offer.Server {
	return offer.Server{
		Name: name, Cluster: "one", Endpoint: "rw", Host: "one-rw.team.svc", Port: 5432, DBName: "postgres", User: user,
		Service: "pgtoolbox-" + strings.ReplaceAll(user, "_", "-"), SecretName: user + "-password", CASecretName: "one-ca", Owner: owner,
	}
}

func loadedEntries(t *testing.T, raw []byte) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Servers map[string]map[string]any `json:"Servers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, entry := range doc.Servers {
		byName[entry["Name"].(string)] = entry
	}
	return byName
}

func TestRegisterPersonalServers(t *testing.T) {
	f := &treeSetup{
		users: `[{"username":"bob@example.com","role":"User","auth_source":"oauth2"},{"username":"old@example.com","role":"User","auth_source":"oauth2"}]`,
		trees: map[string]string{
			"bob@example.com": `{"Servers":{"1":{"Name":"bob's own","Group":"Mine"},"2":{"Name":"stale personal","Group":"PgToolBox (personal)"}}}`,
			"old@example.com": `{"Servers":{"1":{"Name":"old personal","Group":"PgToolBox (personal)"},"2":{"Name":"old own","Group":"Mine"}}}`,
		},
		loaded: map[string][]byte{}, authsOf: map[string]string{},
	}
	doc := offer.Document{Servers: []offer.Server{
		server("one rw (app)", "app", ""),
		server("one rw (alice)", "alice", "alice@example.com"),
		server("one rw (bob)", "bob", "bob@example.com"),
	}}
	o := personalOptions(t, f, doc)
	o.ForgetOwners = []string{"old@example.com", "gone@example.com", "bob@example.com"}
	report, err := Register(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if report.Servers != 3 {
		t.Fatalf("report %+v", report)
	}

	shared := loadedEntries(t, f.loaded["pgtoolbox@pgtoolbox.fyannk.dev"])
	if len(shared) != 1 || shared["one rw (app)"]["Shared"] != true || f.authsOf["pgtoolbox@pgtoolbox.fyannk.dev"] != "internal" {
		t.Fatalf("the internal account gets the shared servers only: %v", shared)
	}
	if !slices.Contains(f.calls, "add-external-user alice@example.com --auth-source oauth2 --role User") {
		t.Fatalf("a personal server's user is created ahead of the first sign-in, as a User: %v", f.calls)
	}
	alice := loadedEntries(t, f.loaded["alice@example.com"])
	if entry := alice["one rw (alice)"]; len(alice) != 1 || entry["Shared"] != false || entry["Group"] != PersonalGroup ||
		entry["Service"] != "pgtoolbox-alice" || entry["SharedUsername"] != nil {
		t.Fatalf("alice's personal server: %v", alice)
	}
	bob := loadedEntries(t, f.loaded["bob@example.com"])
	if len(bob) != 2 || bob["bob's own"] == nil || bob["one rw (bob)"] == nil || bob["stale personal"] != nil {
		t.Fatalf("bob keeps his own servers, his managed personal group is replaced: %v", bob)
	}
	old := loadedEntries(t, f.loaded["old@example.com"])
	if len(old) != 1 || old["old own"] == nil {
		t.Fatalf("a forgotten owner loses the managed personal group only: %v", old)
	}
	if _, loaded := f.loaded["gone@example.com"]; loaded || slices.Contains(f.calls, "add-external-user gone@example.com --auth-source oauth2 --role User") {
		t.Fatal("a forgotten owner who is no pgAdmin user is not created")
	}
	for _, user := range []string{"alice@example.com", "bob@example.com", "old@example.com"} {
		if f.authsOf[user] != "oauth2" {
			t.Fatalf("%s loaded with auth source %q", user, f.authsOf[user])
		}
	}
}

const testCA = "-----BEGIN CERTIFICATE-----\nMIIBtest\n-----END CERTIFICATE-----\n"

func TestRegisterOpensTheConfigDatabase(t *testing.T) {
	f := &treeSetup{users: "[]", trees: map[string]string{}, loaded: map[string][]byte{}, authsOf: map[string]string{}}
	settings := server(configServerName, "pgadmin_team", "")
	settings.Service, settings.SecretName, settings.SecretResourceVersion = "pgtoolbox-config-database", "team-pgadmin-config-password", "81"
	o := personalOptions(t, f, offer.Document{Servers: []offer.Server{server("one rw (app)", "app", "")}, ConfigDatabase: &settings})
	o.SettingsDB = filepath.Join(o.TempDir, "absent.db") // never waited for
	o.ConfigDatabaseURI = "postgresql://pgadmin_team@one-rw.team.svc:5432/pgadmin_team?sslmode=verify-full"
	o.CredentialsDir = filepath.Join(o.TempDir, "credentials")
	var asked []string
	o.Secrets = func(_ context.Context, namespace, name string) (*corev1.Secret, error) {
		asked = append(asked, namespace+"/"+name)
		switch name {
		case "team-pgadmin-config-password":
			secret := &corev1.Secret{Data: map[string][]byte{"username": []byte("pgadmin_team"), "password": []byte("generated")}}
			secret.ResourceVersion = "81"
			return secret, nil
		case "one-ca":
			return &corev1.Secret{Data: map[string][]byte{"ca.crt": []byte(testCA), "ca.key": []byte("never copied")}}, nil
		}
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	if _, err := Register(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{"/team-pgadmin-config-password", "/one-ca"}) {
		t.Fatalf("only the settings database's Secrets are read: %v", asked)
	}
	local, err := os.ReadFile(filepath.Join(o.TempDir, configLocalDir, "config_local.py"))
	if err != nil {
		t.Fatal(err)
	}
	if string(local) != "CONFIG_DATABASE_URI = \""+o.ConfigDatabaseURI+"\"\n" {
		t.Fatalf("config_local.py = %q", local)
	}
	pgpass, err := os.ReadFile(filepath.Join(o.CredentialsDir, "pgpass"))
	if err != nil || !strings.Contains(string(pgpass), "generated") {
		t.Fatalf("pgpass %q (%v)", pgpass, err)
	}
	// setup.py's environment: the config_local.py directory first, the
	// credential files.
	doc := offer.Document{ConfigDatabase: &settings}
	o.defaults()
	o.Env = nil
	if err := o.prepareConfigDatabase(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PYTHONPATH=" + filepath.Join(o.TempDir, configLocalDir) + ":/pgadmin4", "PGPASSFILE=" + filepath.Join(o.CredentialsDir, "pgpass")} {
		if !slices.Contains(o.Env, want) {
			t.Fatalf("setup.py's environment %v lacks %s", o.Env, want)
		}
	}
}

// TestRegisterOpensASelfContainedConfigDatabase: the pgtools chart's URI
// comes from a Secret the team names and carries everything libpq needs:
// no offer connection, no credential files, no API call; setup.py still
// gets the config_local.py.
func TestRegisterOpensASelfContainedConfigDatabase(t *testing.T) {
	f := &treeSetup{users: "[]", trees: map[string]string{}, loaded: map[string][]byte{}, authsOf: map[string]string{}}
	o := personalOptions(t, f, offer.Document{Servers: []offer.Server{server("one rw (app)", "app", "")}})
	o.SettingsDB = filepath.Join(o.TempDir, "absent.db") // never waited for
	o.ConfigDatabaseURI = "postgresql://pgadmin:s3cret@db.example.com:5432/pgadmin?sslmode=require"
	o.ConfigDatabaseSelfContained = true
	if _, err := Register(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	local, err := os.ReadFile(filepath.Join(o.TempDir, configLocalDir, "config_local.py"))
	if err != nil {
		t.Fatal(err)
	}
	if string(local) != "CONFIG_DATABASE_URI = \""+o.ConfigDatabaseURI+"\"\n" {
		t.Fatalf("config_local.py = %q", local)
	}
	if _, err := os.Stat(filepath.Join(o.TempDir, "credentials")); err == nil {
		t.Fatal("no credential files are written for a self-contained URI")
	}
	o.ConfigDatabaseURI = "postgresql://pgadmin:it's@db/pgadmin"
	if err := o.prepareConfigDatabase(context.Background(), offer.Document{}); err == nil {
		t.Fatal("a quote in the URI must be refused")
	}
}

func TestRegisterRefusesAConfigDatabaseWithoutCredentials(t *testing.T) {
	f := &treeSetup{users: "[]", trees: map[string]string{}, loaded: map[string][]byte{}, authsOf: map[string]string{}}
	settings := server(configServerName, "pgadmin_team", "")
	settings.Service, settings.SecretName = "pgtoolbox-config-database", "team-pgadmin-config-password"
	o := personalOptions(t, f, offer.Document{ConfigDatabase: &settings})
	o.ConfigDatabaseURI = "postgresql://pgadmin_team@one-rw.team.svc:5432/pgadmin_team"
	o.CredentialsDir = filepath.Join(o.TempDir, "credentials")
	o.Secrets = func(_ context.Context, _, name string) (*corev1.Secret, error) {
		return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, name, nil)
	}
	_, err := Register(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "not usable") {
		t.Fatalf("an unreadable password must stop the Job: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("setup.py must not run: %v", f.calls)
	}
}

const configServerName = "pgAdmin settings database"
