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

package adminsync

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// pgAdmin creates a webserver account from the REMOTE_USER header with
// username set and email left NULL, so the account set has to be read from
// the column that is actually populated. Reading email produced the string
// "None" and every subsequent sync failed on a user that cannot exist.
func TestAccountsReadsUsernameNotEmail(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to read pgAdmin's settings database")
	}

	db := filepath.Join(t.TempDir(), "pgadmin4.db")
	seed := "import sqlite3,sys\n" +
		"c=sqlite3.connect(sys.argv[1])\n" +
		"c.execute('CREATE TABLE user (id INTEGER, username TEXT, email TEXT, auth_source TEXT)')\n" +
		// The bootstrap account, which nobody signs in with.
		"c.execute(\"INSERT INTO user VALUES (1,'boot@x','boot@x','internal')\")\n" +
		// A real account exactly as pgAdmin writes it: no email.
		"c.execute(\"INSERT INTO user VALUES (2,'dba@corp.example',NULL,'webserver')\")\n" +
		"c.commit()\n"
	if out, err := exec.Command(python, "-c", seed, db).CombinedOutput(); err != nil {
		t.Fatalf("seed settings database: %v: %s", err, out)
	}

	o := SidecarOptions{PythonPath: python, SettingsDB: db}
	accounts, err := o.accounts(context.Background())
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if !slices.Equal(accounts, []string{"dba@corp.example"}) {
		t.Fatalf("accounts = %q, want [dba@corp.example]", accounts)
	}
}

// An uninitialized settings database is not a failure: pgAdmin creates it
// at first start, and a sync can arrive before that.
func TestAccountsToleratesMissingTable(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to read pgAdmin's settings database")
	}

	o := SidecarOptions{PythonPath: python, SettingsDB: filepath.Join(t.TempDir(), "pgadmin4.db")}
	accounts, err := o.accounts(context.Background())
	if err != nil {
		t.Fatalf("accounts on an empty database: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts = %q, want none", accounts)
	}
}
