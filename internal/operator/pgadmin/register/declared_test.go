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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

const declaredList = `{"version":1,"servers":[
 {"cluster":"orders","endpoint":"rw","credentials":{"application":{"secret":"orders-app"}}},
 {"cluster":"billing","endpoint":"pooler:billing-pool","database":"billing","user":"billing_owner","credentials":{"secret":"billing-owner"}}
]}`

// TestRegisterDeclaredList: the chart's list becomes the managed group,
// next to the servers people added themselves; the user and database are
// written only when declared (libpq takes the rest from the service
// section), and no entry carries a password.
func TestRegisterDeclaredList(t *testing.T) {
	fake := &fakeSetup{
		dump:  `{"Servers":{"1":{"Name":"mine","Group":"Mine"},"2":{"Name":"old managed","Group":"PgToolBox"}}}`,
		users: "[]",
	}
	o := options(t, fake, "alice@corp")
	o.OfferFile, o.DeclaredFile, o.Namespace = "", filepath.Join(o.TempDir, "servers.json"), "team"
	if err := os.WriteFile(o.DeclaredFile, []byte(declaredList), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Register(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if report.Servers != 2 {
		t.Fatalf("report %+v", report)
	}
	var doc struct {
		Servers map[string]map[string]any `json:"Servers"`
	}
	if err := json.Unmarshal(fake.loaded, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Servers) != 3 || doc.Servers["1"]["Name"] != "mine" {
		t.Fatalf("own servers not kept: %v", doc.Servers)
	}
	derived, declared := doc.Servers["2"], doc.Servers["3"]
	if derived["Name"] != "orders rw (application)" || derived["Host"] != "orders-rw.team.svc" || derived["MaintenanceDB"] != "postgres" ||
		derived["Service"] != "pgtoolbox-orders-rw-application" || derived["Group"] != offer.ManagedGroup || derived["Shared"] != true {
		t.Fatalf("derived entry %v", derived)
	}
	if _, has := derived["Username"]; has {
		t.Fatalf("a derived user must be left to the service section, not written as %v", derived["Username"])
	}
	if _, has := derived["SharedUsername"]; has {
		t.Fatalf("derived entry carries SharedUsername %v", derived["SharedUsername"])
	}
	if declared["Host"] != "billing-pool.team.svc" || declared["Username"] != "billing_owner" ||
		declared["SharedUsername"] != "billing_owner" || declared["MaintenanceDB"] != "billing" {
		t.Fatalf("declared entry %v", declared)
	}
	for _, entry := range []map[string]any{derived, declared} {
		for key := range entry {
			if strings.Contains(strings.ToLower(key), "pass") {
				t.Fatalf("entry carries %s", key)
			}
		}
	}
	if !strings.Contains(strings.Join(fake.commands(), "\n"), "add-external-user alice@corp --auth-source oauth2 --role Administrator") {
		t.Fatalf("administrators not synced: %v", fake.commands())
	}
}

func TestRegisterRefusesAnInvalidList(t *testing.T) {
	fake := &fakeSetup{users: "[]"}
	o := options(t, fake)
	o.OfferFile, o.DeclaredFile = "", filepath.Join(o.TempDir, "servers.json")
	if err := os.WriteFile(o.DeclaredFile, []byte(`{"version":1,"servers":[{"cluster":"a","endpoint":"rw"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(context.Background(), o); err == nil || !strings.Contains(err.Error(), "credentials missing") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("setup.py ran on an invalid list: %v", fake.calls)
	}
}

// TestBackupOnImageChange: the first start records the image without a
// backup; the same image again does nothing; a new image is backed up
// before it is recorded; a failed backup records nothing, so the next start
// tries again.
func TestBackupOnImageChange(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "pgadmin4.db")
	marker := filepath.Join(dir, ".pgtoolbox", "image")
	copies := 0
	var fail bool
	run := func(_ context.Context, _ string, _ string, args ...string) ([]byte, error) {
		if fail {
			return []byte("disk full"), errors.New("exit status 1")
		}
		copies++
		return nil, os.WriteFile(args[len(args)-1], []byte("copy"), 0o600)
	}
	o := BackupOptions{SettingsDB: db, Dir: filepath.Join(dir, "backups"), LockFile: filepath.Join(dir, "lock"), Label: "x", Keep: 3, Run: run}
	recorded := func() string {
		raw, _ := os.ReadFile(marker) // #nosec G304 -- test path
		return strings.TrimSpace(string(raw))
	}

	// First start: no database yet.
	if path, err := BackupOnImageChange(context.Background(), o, "pgadmin:1", marker); err != nil || path != "" || recorded() != "pgadmin:1" {
		t.Fatalf("first start: %q %v, recorded %q", path, err, recorded())
	}
	if err := os.WriteFile(db, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Same image: nothing to do.
	if path, err := BackupOnImageChange(context.Background(), o, "pgadmin:1", marker); err != nil || path != "" || copies != 0 {
		t.Fatalf("same image: %q %v, %d copies", path, err, copies)
	}
	// A failed backup keeps the old image recorded.
	fail = true
	if _, err := BackupOnImageChange(context.Background(), o, "pgadmin:2", marker); err == nil || recorded() != "pgadmin:1" {
		t.Fatalf("failed backup: %v, recorded %q", err, recorded())
	}
	// A new image: backed up, then recorded.
	fail = false
	path, err := BackupOnImageChange(context.Background(), o, "pgadmin:2", marker)
	if err != nil || path == "" || copies != 1 || recorded() != "pgadmin:2" {
		t.Fatalf("new image: %q %v, %d copies, recorded %q", path, err, copies, recorded())
	}
	if _, err := BackupOnImageChange(context.Background(), o, "", marker); err == nil {
		t.Fatal("an empty image must be refused")
	}
}
