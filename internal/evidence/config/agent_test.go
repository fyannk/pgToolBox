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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func validAgentEnvironment(accessPath, secretPath string) map[string]string {
	return map[string]string{
		"RUNTIME_MODE": "agent", "REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3",
		"DESTINATION_PATH": "s3://synthetic-bucket/cluster", "BARMAN_SERVER_NAMES": "orders",
		"CNPG_CLUSTER_NAMESPACE": "database-team",
		"CNPG_CLUSTER_UID":       "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", "CNPG_CLUSTER_NAME": "orders",
		"STORE_CREDENTIAL_MODE": "static-files", "AWS_ACCESS_KEY_ID_FILE": accessPath,
		"AWS_SECRET_ACCESS_KEY_FILE": secretPath, "AWS_REGION": "eu-west-1",
		"TLS_CERT_FILE": "/var/run/secrets/pgtoolbox.fyannk.dev/serving/tls.crt",
		"TLS_KEY_FILE":  "/var/run/secrets/pgtoolbox.fyannk.dev/serving/tls.key",
		"AGENT_NAME":    "orders", "AGENT_NAMESPACE": "database-team", "PRODUCER_EPOCH": "4",
	}
}

func agentCredentialFiles(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	accessPath := filepath.Join(directory, "access")
	secretPath := filepath.Join(directory, "secret")
	for _, path := range []string{accessPath, secretPath} {
		if err := os.WriteFile(path, []byte("synthetic-canary"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return accessPath, secretPath
}

func TestLoadValidAgentProfile(t *testing.T) {
	accessPath, secretPath := agentCredentialFiles(t)
	environment := validAgentEnvironment(accessPath, secretPath)
	cfg, err := Load(mapEnvironment(environment))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeMode != RuntimeAgent || !cfg.RuntimeMode.Produces() || cfg.ListenAddr != ":8443" || cfg.TrustedUserHeader != "" ||
		cfg.AllowDownload || cfg.Credentials.Mode != CredentialStaticFiles {
		t.Fatalf("agent config = %#v", cfg)
	}
	if cfg.AgentName != "orders" || cfg.AgentNamespace != "database-team" || cfg.ProducerEpoch != 4 ||
		cfg.TLSCertFile != environment["TLS_CERT_FILE"] || cfg.TLSKeyFile != environment["TLS_KEY_FILE"] ||
		cfg.CNPGClusterUID == "" || !reflect.DeepEqual(cfg.BarmanServerNames, []string{"orders"}) {
		t.Fatalf("agent identity = %#v", cfg)
	}
	environment["LISTEN_ADDR"] = "0.0.0.0:9443"
	if cfg, err = Load(mapEnvironment(environment)); err != nil || cfg.ListenAddr != "0.0.0.0:9443" {
		t.Fatalf("LISTEN_ADDR override = %q, %v", cfg.ListenAddr, err)
	}
	if RuntimeStandalone.Produces() {
		t.Fatal("Produces() misclassifies a mode")
	}
}

func TestLoadRejectsInvalidAgentProfiles(t *testing.T) {
	accessPath, secretPath := agentCredentialFiles(t)
	tests := []struct {
		name     string
		changes  map[string]string
		remove   []string
		variable string
	}{
		{name: "trusted header", changes: map[string]string{"TRUSTED_USER_HEADER": "X-Forwarded-User"}, variable: "TRUSTED_USER_HEADER"},
		{name: "download", changes: map[string]string{"ALLOW_DOWNLOAD": "true"}, variable: "ALLOW_DOWNLOAD"},
		{name: "listen address", changes: map[string]string{"LISTEN_ADDR": "8443"}, variable: "LISTEN_ADDR"},
		{name: "missing cert", remove: []string{"TLS_CERT_FILE"}, variable: "TLS_CERT_FILE"},
		{name: "relative key", changes: map[string]string{"TLS_KEY_FILE": "tls.key"}, variable: "TLS_KEY_FILE"},
		{name: "missing agent name", remove: []string{"AGENT_NAME"}, variable: "AGENT_NAME"},
		{name: "agent name not a label", changes: map[string]string{"AGENT_NAME": "Orders"}, variable: "AGENT_NAME"},
		{name: "agent name too long", changes: map[string]string{"AGENT_NAME": strings.Repeat("a", 64)}, variable: "AGENT_NAME"},
		{name: "agent namespace not a label", changes: map[string]string{"AGENT_NAMESPACE": "-team"}, variable: "AGENT_NAMESPACE"},
		{name: "cross-namespace", changes: map[string]string{"AGENT_NAMESPACE": "other-team"}, variable: "AGENT_NAMESPACE"},
		{name: "missing epoch", remove: []string{"PRODUCER_EPOCH"}, variable: "PRODUCER_EPOCH"},
		{name: "zero epoch", changes: map[string]string{"PRODUCER_EPOCH": "0"}, variable: "PRODUCER_EPOCH"},
		{name: "pgbackrest viewer list", changes: map[string]string{"REPOSITORY_FORMAT": "pgbackrest", "PGBACKREST_STANZAS": "main"}, remove: []string{"BARMAN_SERVER_NAMES"}, variable: "PGBACKREST_STANZAS"},
		{name: "pgbackrest without a stanza", changes: map[string]string{"REPOSITORY_FORMAT": "pgbackrest"}, remove: []string{"BARMAN_SERVER_NAMES"}, variable: "PGBACKREST_STANZA"},
		{name: "two servers", changes: map[string]string{"BARMAN_SERVER_NAMES": "orders,billing"}, variable: "BARMAN_SERVER_NAMES"},
		{name: "missing cluster uid", remove: []string{"CNPG_CLUSTER_UID"}, variable: "CNPG_CLUSTER_UID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := validAgentEnvironment(accessPath, secretPath)
			for _, key := range test.remove {
				delete(environment, key)
			}
			for key, value := range test.changes {
				environment[key] = value
			}
			_, err := load(mapEnvironment(environment), func(key string) (string, bool) {
				value, exists := environment[key]
				return value, exists
			})
			var configErr *Error
			if !errors.As(err, &configErr) || configErr.Variable != test.variable {
				t.Fatalf("Load() = %v, want an error naming %s", err, test.variable)
			}
			if strings.Contains(err.Error(), "canary") {
				t.Fatal("error discloses a value")
			}
		})
	}
}

func TestLoadAgentDefersRepositoryErrors(t *testing.T) {
	accessPath, secretPath := agentCredentialFiles(t)
	environment := validAgentEnvironment(accessPath, secretPath)
	environment["DESTINATION_PATH"] = "s3://backups/prefix?token=canary"
	cfg, err := Load(mapEnvironment(environment))
	var configErr *Error
	if err != nil || !errors.As(cfg.RepositoryError, &configErr) || configErr.Variable != "DESTINATION_PATH" {
		t.Fatalf("agent Load() = %v, deferred %v", err, cfg.RepositoryError)
	}
	if strings.Contains(cfg.RepositoryError.Error(), "canary") || !reflect.DeepEqual(cfg.Credentials, Credentials{}) || cfg.AgentName == "" || cfg.ProducerEpoch == 0 {
		t.Fatal("deferred agent error disclosed a value or lost the process identity")
	}
}

func TestLoadStandaloneRejectsAgentVariables(t *testing.T) {
	base := map[string]string{"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3", "DESTINATION_PATH": "s3://backups/prefix", "AWS_REGION": "eu-west-1"}
	for _, variable := range agentVariables {
		environment := cloneMap(base)
		environment[variable] = "1"
		_, err := Load(mapEnvironment(environment))
		var configErr *Error
		if !errors.As(err, &configErr) || configErr.Variable != variable {
			t.Fatalf("standalone mode accepted %s: %v", variable, err)
		}
	}
}
