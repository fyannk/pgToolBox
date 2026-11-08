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

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// pgBackRestAgentEnvironment is the environment the operator and the chart
// render for a pgBackRest repository: the agent profile with
// REPOSITORY_FORMAT=pgbackrest, the stanza variables and the repository
// root as DESTINATION_PATH.
func pgBackRestAgentEnvironment(t *testing.T) map[string]string {
	t.Helper()
	accessPath, secretPath := agentCredentialFiles(t)
	environment := validAgentEnvironment(accessPath, secretPath)
	delete(environment, "BARMAN_SERVER_NAMES")
	environment["REPOSITORY_FORMAT"] = "pgbackrest"
	environment["DESTINATION_PATH"] = "s3://backups/pgbr/orders"
	environment["PGBACKREST_STANZA"] = "orders"
	return environment
}

func writeCipher(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cipher-pass")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPGBackRestAgent(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := loadExact(pgBackRestAgentEnvironment(t))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RepositoryFormat != FormatPGBackRest || cfg.PGBackRestStanza != "orders" || cfg.PGBackRestArchiveStanza != "orders" || cfg.PGBackRestRepository != 1 || !cfg.PGBackRestCipher.Empty() || len(cfg.PGBackRestStanzas) != 0 || len(cfg.BarmanServerNames) != 0 {
			t.Fatalf("pgBackRest agent config = %#v", cfg)
		}
		// DESTINATION_PATH is the repository root: bucket + repo-path.
		if cfg.DestinationBucket != "backups" || cfg.DestinationPrefix != "pgbr/orders" {
			t.Fatalf("destination = %q / %q", cfg.DestinationBucket, cfg.DestinationPrefix)
		}
		if cfg.ScopeKind() != "stanza" || cfg.ScopeName() != "orders" || !reflect.DeepEqual(cfg.ScopePrefixes(), []string{"archive/orders/", "backup/orders/"}) {
			t.Fatalf("scope = %s %s %v", cfg.ScopeKind(), cfg.ScopeName(), cfg.ScopePrefixes())
		}
		// No PGBACKREST_RETENTION_*: no expectation, not "unset".
		if cfg.PGBackRestRetention != nil {
			t.Fatalf("retention = %+v", cfg.PGBackRestRetention)
		}
	})
	t.Run("retention", func(t *testing.T) {
		for name, test := range map[string]struct {
			environment map[string]string
			want        PGBackRestRetention
		}{
			"the operator's rendering of a count retention": {
				environment: map[string]string{"PGBACKREST_RETENTION_FULL": "7", "PGBACKREST_RETENTION_FULL_TYPE": "count", "PGBACKREST_RETENTION_DIFF": "14", "PGBACKREST_RETENTION_ARCHIVE": "2", "PGBACKREST_RETENTION_ARCHIVE_TYPE": "full"},
				want:        PGBackRestRetention{Full: 7, FullType: "count", Diff: 14, Archive: 2, ArchiveType: "full"},
			},
			"time-based": {
				environment: map[string]string{"PGBACKREST_RETENTION_FULL": "30", "PGBACKREST_RETENTION_FULL_TYPE": "time", "PGBACKREST_RETENTION_ARCHIVE_TYPE": "full"},
				want:        PGBackRestRetention{Full: 30, FullType: "time", ArchiveType: "full"},
			},
			"repo-retention-full unset: the types alone declare the expectation": {
				environment: map[string]string{"PGBACKREST_RETENTION_FULL_TYPE": "count", "PGBACKREST_RETENTION_ARCHIVE_TYPE": "full"},
				want:        PGBackRestRetention{FullType: "count", ArchiveType: "full"},
			},
			"a count alone takes pgBackRest's default types": {
				environment: map[string]string{"PGBACKREST_RETENTION_FULL": "2"},
				want:        PGBackRestRetention{Full: 2},
			},
		} {
			t.Run(name, func(t *testing.T) {
				environment := pgBackRestAgentEnvironment(t)
				for key, value := range test.environment {
					environment[key] = value
				}
				cfg, err := loadExact(environment)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.PGBackRestRetention == nil || *cfg.PGBackRestRetention != test.want {
					t.Fatalf("retention = %+v, want %+v", cfg.PGBackRestRetention, test.want)
				}
			})
		}
	})
	t.Run("split stanza, repository 2, cipher", func(t *testing.T) {
		environment := pgBackRestAgentEnvironment(t)
		environment["PGBACKREST_ARCHIVE_STANZA"] = "orders-wal"
		environment["PGBACKREST_REPOSITORY"] = "2"
		environment["PGBACKREST_CIPHER_PASS_FILE"] = writeCipher(t, "cipher-canary-value")
		cfg, err := loadExact(environment)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.PGBackRestArchiveStanza != "orders-wal" || cfg.PGBackRestRepository != 2 || string(cfg.PGBackRestCipher.Bytes()) != "cipher-canary-value" {
			t.Fatalf("pgBackRest agent config = %#v", cfg)
		}
		if !reflect.DeepEqual(cfg.ScopePrefixes(), []string{"archive/orders-wal/", "backup/orders/"}) {
			t.Fatalf("prefixes = %v", cfg.ScopePrefixes())
		}
		// The passphrase reaches the redactor and never a formatted value.
		if rendered := fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg); strings.Contains(rendered, "cipher-canary") {
			t.Fatal("the cipher passphrase is rendered by fmt")
		}
		found := false
		for _, value := range cfg.SecretValues() {
			found = found || string(value) == "cipher-canary-value"
		}
		if !found {
			t.Fatal("the cipher passphrase is not handed to the redactor")
		}
	})
	t.Run("azure", func(t *testing.T) {
		directory := t.TempDir()
		keyPath := filepath.Join(directory, "key")
		if err := os.WriteFile(keyPath, []byte("c3ludGhldGljLWNhbmFyeQ=="), 0o600); err != nil {
			t.Fatal(err)
		}
		environment := pgBackRestAgentEnvironment(t)
		for _, key := range []string{"AWS_ACCESS_KEY_ID_FILE", "AWS_SECRET_ACCESS_KEY_FILE", "AWS_REGION"} {
			delete(environment, key)
		}
		environment["PROVIDER"] = "azure"
		environment["DESTINATION_PATH"] = "azure://backups/pgbr/orders"
		environment["AZURE_STORAGE_ACCOUNT"] = "account"
		environment["STORE_CREDENTIAL_MODE"] = "account-key-file"
		environment["AZURE_STORAGE_ACCOUNT_KEY_FILE"] = keyPath
		cfg, err := loadExact(environment)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RepositoryError != nil || cfg.Provider != ProviderAzure || cfg.DestinationPrefix != "pgbr/orders" || cfg.PGBackRestStanza != "orders" {
			t.Fatalf("Azure pgBackRest config = %#v", cfg)
		}
	})
}

func TestLoadPGBackRestRefusals(t *testing.T) {
	unreadable := filepath.Join(t.TempDir(), "absent")
	tests := []struct {
		name     string
		base     func(*testing.T) map[string]string
		changes  map[string]string
		remove   []string
		variable string
	}{
		{name: "missing stanza", base: pgBackRestAgentEnvironment, remove: []string{"PGBACKREST_STANZA"}, variable: "PGBACKREST_STANZA"},
		{name: "stanza with a slash", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_STANZA": "a/b"}, variable: "PGBACKREST_STANZA"},
		{name: "stanza dot-dot", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_STANZA": ".."}, variable: "PGBACKREST_STANZA"},
		{name: "stanza with a control", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_STANZA": "a\tb"}, variable: "PGBACKREST_STANZA"},
		{name: "stanza with a space", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_STANZA": " orders"}, variable: "PGBACKREST_STANZA"},
		{name: "stanza too long", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_STANZA": strings.Repeat("s", 129)}, variable: "PGBACKREST_STANZA"},
		{name: "archive stanza with a backslash", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_ARCHIVE_STANZA": `a\b`}, variable: "PGBACKREST_ARCHIVE_STANZA"},
		{name: "repository zero", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_REPOSITORY": "0"}, variable: "PGBACKREST_REPOSITORY"},
		{name: "repository 257", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_REPOSITORY": "257"}, variable: "PGBACKREST_REPOSITORY"},
		{name: "repository not a number", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_REPOSITORY": "repo1"}, variable: "PGBACKREST_REPOSITORY"},
		{name: "viewer list in an agent", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_STANZAS": "orders"}, variable: "PGBACKREST_STANZAS"},
		{name: "retention full zero", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_RETENTION_FULL": "0"}, variable: "PGBACKREST_RETENTION_FULL"},
		{name: "retention full above pgBackRest's bound", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_RETENTION_FULL": "10000000"}, variable: "PGBACKREST_RETENTION_FULL"},
		{name: "retention diff not a number", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_RETENTION_DIFF": "two"}, variable: "PGBACKREST_RETENTION_DIFF"},
		{name: "retention archive negative", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_RETENTION_ARCHIVE": "-1"}, variable: "PGBACKREST_RETENTION_ARCHIVE"},
		{name: "retention full type days", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_RETENTION_FULL_TYPE": "days"}, variable: "PGBACKREST_RETENTION_FULL_TYPE"},
		{name: "retention archive type log", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_RETENTION_ARCHIVE_TYPE": "log"}, variable: "PGBACKREST_RETENTION_ARCHIVE_TYPE"},
		{name: "retention with Barman", base: barmanAgentEnvironment, changes: map[string]string{"PGBACKREST_RETENTION_FULL": "2"}, variable: "PGBACKREST_RETENTION_FULL"},
		{name: "viewer refuses a retention", base: pgBackRestViewerEnvironment, changes: map[string]string{"PGBACKREST_RETENTION_FULL_TYPE": "count"}, variable: "PGBACKREST_RETENTION_FULL_TYPE"},
		{name: "self-resolving agent refuses a retention", base: func(*testing.T) map[string]string { return teamAgentEnvironment() }, changes: map[string]string{"PGBACKREST_RETENTION_FULL": "2"}, variable: "PGBACKREST_RETENTION_FULL"},
		{name: "Barman server in pgBackRest", base: pgBackRestAgentEnvironment, changes: map[string]string{"BARMAN_SERVER_NAMES": "orders"}, variable: "BARMAN_SERVER_NAMES"},
		{name: "cipher file unreadable", base: pgBackRestAgentEnvironment, changes: map[string]string{"PGBACKREST_CIPHER_PASS_FILE": unreadable}, variable: "PGBACKREST_CIPHER_PASS_FILE"},
		{name: "stanza with Barman", base: barmanAgentEnvironment, changes: map[string]string{"PGBACKREST_STANZA": "orders"}, variable: "PGBACKREST_STANZA"},
		{name: "archive stanza with Barman", base: barmanAgentEnvironment, changes: map[string]string{"PGBACKREST_ARCHIVE_STANZA": "orders"}, variable: "PGBACKREST_ARCHIVE_STANZA"},
		{name: "repository with Barman", base: barmanAgentEnvironment, changes: map[string]string{"PGBACKREST_REPOSITORY": "1"}, variable: "PGBACKREST_REPOSITORY"},
		{name: "viewer refuses the agent stanza", base: pgBackRestViewerEnvironment, changes: map[string]string{"PGBACKREST_STANZA": "orders"}, variable: "PGBACKREST_STANZA"},
		{name: "viewer refuses the repository number", base: pgBackRestViewerEnvironment, changes: map[string]string{"PGBACKREST_REPOSITORY": "1"}, variable: "PGBACKREST_REPOSITORY"},
		{name: "self-resolving agent refuses a stanza", base: func(*testing.T) map[string]string { return teamAgentEnvironment() }, changes: map[string]string{"PGBACKREST_STANZA": "orders"}, variable: "PGBACKREST_STANZA"},
		{name: "self-resolving agent refuses an archive stanza", base: func(*testing.T) map[string]string { return teamAgentEnvironment() }, changes: map[string]string{"PGBACKREST_ARCHIVE_STANZA": "orders"}, variable: "PGBACKREST_ARCHIVE_STANZA"},
		{name: "self-resolving agent refuses a repository number", base: func(*testing.T) map[string]string { return teamAgentEnvironment() }, changes: map[string]string{"PGBACKREST_REPOSITORY": "2"}, variable: "PGBACKREST_REPOSITORY"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := test.base(t)
			for _, key := range test.remove {
				delete(environment, key)
			}
			for key, value := range test.changes {
				environment[key] = value
			}
			_, err := loadExact(environment)
			var configErr *Error
			if !errors.As(err, &configErr) || configErr.Variable != test.variable {
				t.Fatalf("Load() = %v, want an error naming %s", err, test.variable)
			}
			if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), unreadable) {
				t.Fatal("the error discloses a value or a path")
			}
		})
	}
}

func TestLoadPGBackRestViewerKeepsTheList(t *testing.T) {
	environment := pgBackRestViewerEnvironment(t)
	environment["PGBACKREST_STANZAS"] = "orders, billing"
	environment["PGBACKREST_CIPHER_PASS_FILE"] = writeCipher(t, "viewer-canary")
	cfg, err := loadExact(environment)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.PGBackRestStanzas, []string{"billing", "orders"}) || cfg.PGBackRestStanza != "" || cfg.PGBackRestCipher.Empty() || cfg.ScopePrefixes() != nil {
		t.Fatalf("viewer config = %#v", cfg)
	}
}

func barmanAgentEnvironment(t *testing.T) map[string]string {
	t.Helper()
	accessPath, secretPath := agentCredentialFiles(t)
	return validAgentEnvironment(accessPath, secretPath)
}

func pgBackRestViewerEnvironment(*testing.T) map[string]string {
	return map[string]string{
		"REPOSITORY_FORMAT": "pgbackrest", "PROVIDER": "gcs", "DESTINATION_PATH": "gs://backups/pgbr",
	}
}

// TestPGBackRestScope covers both ways a producing mode names its stanza:
// PGBACKREST_STANZA, and the single-entry list a self-resolved repository
// sets in ApplyResolved.
func TestPGBackRestScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		cfg             Config
		stanza, archive string
		prefixes        []string
	}{
		{"agent, one stanza", Config{RuntimeMode: RuntimeAgent, RepositoryFormat: FormatPGBackRest, PGBackRestStanza: "orders"}, "orders", "orders", []string{"archive/orders/", "backup/orders/"}},
		{"agent, split", Config{RuntimeMode: RuntimeAgent, RepositoryFormat: FormatPGBackRest, PGBackRestStanza: "orders", PGBackRestArchiveStanza: "orders-wal"}, "orders", "orders-wal", []string{"archive/orders-wal/", "backup/orders/"}},
		{"self-resolved list of one", Config{RuntimeMode: RuntimeAgent, RepositoryFormat: FormatPGBackRest, PGBackRestStanzas: []string{"orders"}}, "orders", "orders", []string{"archive/orders/", "backup/orders/"}},
		{"viewer list", Config{RuntimeMode: RuntimeStandalone, RepositoryFormat: FormatPGBackRest, PGBackRestStanzas: []string{"orders"}}, "", "", nil},
		{"agent without a stanza", Config{RuntimeMode: RuntimeAgent, RepositoryFormat: FormatPGBackRest}, "", "", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			stanza, archive := test.cfg.PGBackRestScope()
			if stanza != test.stanza || archive != test.archive || test.cfg.ScopeName() != test.stanza || !slices.Equal(test.cfg.ScopePrefixes(), test.prefixes) {
				t.Fatalf("scope %q/%q, name %q, prefixes %v", stanza, archive, test.cfg.ScopeName(), test.cfg.ScopePrefixes())
			}
			if test.stanza != "" && test.cfg.ScopeKind() != "stanza" {
				t.Fatalf("scope kind %q", test.cfg.ScopeKind())
			}
		})
	}
}
