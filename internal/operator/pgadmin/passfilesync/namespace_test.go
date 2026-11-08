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

package passfilesync

import (
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

// TestRenderGrantedNamespace covers a Cluster of another namespace (a
// PgAdminAccess grant) whose name equals a local one, and the settings
// database connection: Secrets are looked up in their own namespace only,
// and each Cluster gets its own CA file.
func TestRenderGrantedNamespace(t *testing.T) {
	files := Files{Dir: t.TempDir()}
	remote := server("one", "app", "one-app", "")
	remote.Name, remote.Namespace, remote.Service = "team-b/one rw (app)", "team-b", "pgtoolbox-team-b-one-app"
	settings := server("two", "pgadmin_team", "settings-pw", "9")
	settings.Name, settings.Service = "pgAdmin settings database", "pgtoolbox-config-database"
	doc := offer.Document{Version: offer.Version, Servers: []offer.Server{server("one", "app", "one-app", ""), remote}, ConfigDatabase: &settings}

	secrets := baseSecrets()
	secrets["settings-pw"] = secret("settings-pw", "9", map[string]string{"username": "pgadmin_team", "password": "pw-settings"})
	// The local Secret of the same name must not serve the other namespace.
	outcome, err := Render(files, doc, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Offered != 2 || len(outcome.Refused) != 1 || !strings.Contains(outcome.Refused[0], "team-b/one-ca") {
		t.Fatalf("outcome %+v: the granted server's Secrets are absent from its namespace", outcome)
	}

	secrets["team-b/one-app"] = secret("one-app", "3", map[string]string{"username": "app", "password": "pw-remote"})
	secrets["team-b/one-ca"] = caSecret("one-ca", caPEM2)
	outcome, err = Render(files, doc, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Offered != 3 || len(outcome.Refused) != 0 {
		t.Fatalf("outcome %+v", outcome)
	}
	if got := read(t, offer.CAFile(files.Dir, "team-b.one")); got != caPEM2 {
		t.Fatalf("the granted Cluster's CA file holds %q", got)
	}
	if got := read(t, offer.CAFile(files.Dir, "one")); got != caPEM1 {
		t.Fatalf("the local Cluster's CA file holds %q", got)
	}
	pass := read(t, files.PassFile())
	for _, want := range []string{"pw-one", "pw-remote", "pw-settings"} {
		if !strings.Contains(pass, want) {
			t.Fatalf("pgpass lacks %s", want)
		}
	}
	if service := read(t, files.ServiceFile()); !strings.Contains(service, "[pgtoolbox-config-database]") ||
		!strings.Contains(service, "sslrootcert="+offer.CAFile(files.Dir, "team-b.one")) {
		t.Fatalf("service file:\n%s", service)
	}
}
