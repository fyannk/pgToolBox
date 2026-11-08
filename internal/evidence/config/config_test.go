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
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func TestLoadValidMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		environment map[string]string
		assert      func(*testing.T, Config)
	}{
		{
			name: "barman on s3 with deterministic scopes",
			environment: map[string]string{
				"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3",
				"DESTINATION_PATH": "s3://backups/cluster-a", "BARMAN_SERVER_NAMES": "zeta, alpha,zeta", "AWS_REGION": "eu-west-1",
			},
			assert: func(t *testing.T, cfg Config) {
				t.Helper()
				if !reflect.DeepEqual(cfg.BarmanServerNames, []string{"alpha", "zeta"}) {
					t.Fatalf("server names = %v", cfg.BarmanServerNames)
				}
				if cfg.Credentials.Mode != CredentialAmbient || cfg.ListenAddr != ":3000" || cfg.TrustedUserHeader != "X-Forwarded-User" {
					t.Fatalf("defaults not applied: %#v", cfg)
				}
				if cfg.MaxObjectsPerScan != 1_000_000 || cfg.WALPageSize != 200 {
					t.Fatalf("safety defaults not frozen: %#v", cfg)
				}
				if cfg.DestinationBucket != "backups" || cfg.DestinationPrefix != "cluster-a" {
					t.Fatalf("destination = %q, %q", cfg.DestinationBucket, cfg.DestinationPrefix)
				}
			},
		},
		{
			name: "S3 destination prefix is decoded once by the shared parser",
			environment: map[string]string{
				"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3",
				"DESTINATION_PATH": "s3://backups/team%20a/cluster%2Fb/", "ENDPOINT_URL": "https://minio.example.test",
			},
			assert: func(t *testing.T, cfg Config) {
				t.Helper()
				bucket, prefix, err := evidencev1alpha1.ParseS3Destination("s3://backups/team%20a/cluster%2Fb/")
				if err != nil || cfg.DestinationBucket != bucket || cfg.DestinationPrefix != prefix || prefix != "team a/cluster/b" {
					t.Fatalf("destination = %q, %q; shared parser = %q, %q, %v", cfg.DestinationBucket, cfg.DestinationPrefix, bucket, prefix, err)
				}
			},
		},
		{
			name: "pgBackRest on GCS",
			environment: map[string]string{
				"REPOSITORY_FORMAT": "pgbackrest", "PROVIDER": "gcs",
				"DESTINATION_PATH": "gs://backups/repository", "PGBACKREST_STANZAS": "demo",
				"ALLOW_DOWNLOAD": "true", "CATALOG_REFRESH_INTERVAL": "10m",
				"STORE_REQUEST_TIMEOUT": "20s", "SCAN_CONCURRENCY": "8", "WAL_PAGE_SIZE": "50",
			},
			assert: func(t *testing.T, cfg Config) {
				t.Helper()
				if !cfg.AllowDownload || cfg.CatalogRefreshInterval != 10*time.Minute || cfg.StoreRequestTimeout != 20*time.Second || cfg.ScanConcurrency != 8 {
					t.Fatalf("configured values not applied: %#v", cfg)
				}
			},
		},
		{
			name: "Azure workload identity",
			environment: map[string]string{
				"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "azure", "DESTINATION_PATH": "azure://container/prefix",
			},
			assert: func(t *testing.T, cfg Config) {
				t.Helper()
				if cfg.Credentials.Mode != CredentialAmbient {
					t.Fatalf("credential mode = %s", cfg.Credentials.Mode)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(mapEnvironment(test.environment))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			test.assert(t, cfg)
		})
	}
}

func TestLoadInvalidMatrix(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"REPOSITORY_FORMAT": "barman-cloud",
		"PROVIDER":          "s3",
		"DESTINATION_PATH":  "s3://backups/prefix",
	}
	tests := []struct {
		name     string
		changes  map[string]string
		contains string
	}{
		{name: "missing format", changes: map[string]string{"REPOSITORY_FORMAT": ""}, contains: "REPOSITORY_FORMAT"},
		{name: "unknown format", changes: map[string]string{"REPOSITORY_FORMAT": "barman"}, contains: "REPOSITORY_FORMAT"},
		{name: "missing provider", changes: map[string]string{"PROVIDER": ""}, contains: "PROVIDER"},
		{name: "scheme mismatch", changes: map[string]string{"DESTINATION_PATH": "gs://backups/prefix"}, contains: "DESTINATION_PATH"},
		{name: "destination credentials", changes: map[string]string{"DESTINATION_PATH": "s3://user:pass@backups/prefix"}, contains: "DESTINATION_PATH"},
		{name: "destination query", changes: map[string]string{"DESTINATION_PATH": "s3://backups/prefix?token=canary"}, contains: "DESTINATION_PATH"},
		{name: "destination empty query", changes: map[string]string{"DESTINATION_PATH": "s3://backups/prefix?"}, contains: "DESTINATION_PATH"},
		{name: "destination port", changes: map[string]string{"DESTINATION_PATH": "s3://backups:9000/prefix"}, contains: "DESTINATION_PATH"},
		{name: "destination encoded bucket", changes: map[string]string{"DESTINATION_PATH": "s3://b%61ckups/prefix"}, contains: "DESTINATION_PATH"},
		{name: "destination encoded empty segment", changes: map[string]string{"DESTINATION_PATH": "s3://backups/a%2F%2Fb"}, contains: "DESTINATION_PATH"},
		{name: "pg scope with barman", changes: map[string]string{"PGBACKREST_STANZAS": "demo"}, contains: "PGBACKREST_STANZAS"},
		{name: "cipher with barman", changes: map[string]string{"PGBACKREST_CIPHER_PASS_FILE": "/not/read"}, contains: "PGBACKREST_CIPHER_PASS_FILE"},
		{name: "bad scope", changes: map[string]string{"BARMAN_SERVER_NAMES": "../escape"}, contains: "BARMAN_SERVER_NAMES"},
		{name: "endpoint on gcs", changes: map[string]string{"PROVIDER": "gcs", "DESTINATION_PATH": "gs://backups/prefix", "ENDPOINT_URL": "https://example.test"}, contains: "ENDPOINT_URL"},
		{name: "custom CA on GCS", changes: map[string]string{"PROVIDER": "gcs", "DESTINATION_PATH": "gs://backups/prefix", "ENDPOINT_CA_FILE": "/not/read"}, contains: "ENDPOINT_CA_FILE"},
		{name: "endpoint credentials", changes: map[string]string{"ENDPOINT_URL": "https://user:secret@example.test"}, contains: "ENDPOINT_URL"},
		{name: "endpoint path", changes: map[string]string{"ENDPOINT_URL": "https://example.test/api"}, contains: "ENDPOINT_URL"},
		{name: "bad listen", changes: map[string]string{"LISTEN_ADDR": "3000"}, contains: "LISTEN_ADDR"},
		{name: "bad trusted header", changes: map[string]string{"TRUSTED_USER_HEADER": "bad header"}, contains: "TRUSTED_USER_HEADER"},
		{name: "bad bool", changes: map[string]string{"ALLOW_DOWNLOAD": "yes"}, contains: "ALLOW_DOWNLOAD"},
		{name: "short refresh", changes: map[string]string{"CATALOG_REFRESH_INTERVAL": "1s"}, contains: "CATALOG_REFRESH_INTERVAL"},
		{name: "large concurrency", changes: map[string]string{"SCAN_CONCURRENCY": "65"}, contains: "SCAN_CONCURRENCY"},
		{name: "small object ceiling", changes: map[string]string{"MAX_OBJECTS_PER_SCAN": "10"}, contains: "MAX_OBJECTS_PER_SCAN"},
		{name: "direct static AWS environment", changes: map[string]string{"AWS_ACCESS_KEY_ID": "canary-access"}, contains: "AWS_ACCESS_KEY_ID"},
		{name: "unpaired AWS file", changes: map[string]string{"AWS_ACCESS_KEY_ID_FILE": "/not/read"}, contains: "AWS_ACCESS_KEY_ID_FILE"},
		{name: "session token without AWS pair", changes: map[string]string{"AWS_SESSION_TOKEN_FILE": "/not/read"}, contains: "AWS_SESSION_TOKEN_FILE"},
		{name: "foreign GCS file", changes: map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/not/read"}, contains: "GOOGLE_APPLICATION_CREDENTIALS"},
		{name: "Azure connection string conflict", changes: map[string]string{
			"PROVIDER": "azure", "DESTINATION_PATH": "azure://container/prefix",
			"AZURE_STORAGE_CONNECTION_STRING_FILE": "/not/read", "AZURE_STORAGE_ACCOUNT": "demo",
		}, contains: "AZURE_STORAGE_CONNECTION_STRING_FILE"},
		{name: "Azure key and SAS conflict", changes: map[string]string{
			"PROVIDER": "azure", "DESTINATION_PATH": "azure://container/prefix", "AZURE_STORAGE_ACCOUNT": "demo",
			"AZURE_STORAGE_ACCOUNT_KEY_FILE": "/not/read", "AZURE_STORAGE_SAS_TOKEN_FILE": "/also-not-read",
		}, contains: "AZURE_STORAGE_ACCOUNT_KEY_FILE"},
		{name: "barman scope with pgBackRest", changes: map[string]string{
			"REPOSITORY_FORMAT": "pgbackrest", "BARMAN_SERVER_NAMES": "demo",
		}, contains: "BARMAN_SERVER_NAMES"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			environment := cloneMap(base)
			for key, value := range test.changes {
				environment[key] = value
			}
			_, err := Load(mapEnvironment(environment))
			if err == nil || !IsError(err) {
				t.Fatalf("Load() error = %v, want redacted config error", err)
			}
			if !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error %q does not identify %s", err, test.contains)
			}
			for _, value := range test.changes {
				if strings.Contains(value, "canary") && strings.Contains(err.Error(), value) {
					t.Fatalf("error disclosed value %q", value)
				}
			}
		})
	}
}

func TestLoadRejectsMoreThanScannerScopeLimit(t *testing.T) {
	t.Parallel()
	names := make([]string, maxConfiguredScopes+1)
	for index := range names {
		names[index] = fmt.Sprintf("scope-%04d", index)
	}
	_, err := Load(mapEnvironment(map[string]string{
		"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3", "DESTINATION_PATH": "s3://backups/prefix",
		"BARMAN_SERVER_NAMES": strings.Join(names, ","),
	}))
	if err == nil || !strings.Contains(err.Error(), "BARMAN_SERVER_NAMES") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadFileCredentialsAndRedaction(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	write := func(name, value string) string {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	accessCanary := "AKIA-CANARY-NEVER-LOG"
	secretCanary := "secret-canary-never-log"
	environment := map[string]string{
		"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3", "DESTINATION_PATH": "s3://backups/prefix", "AWS_REGION": "eu-west-1",
		"AWS_ACCESS_KEY_ID_FILE":     write("access", accessCanary+"\n"),
		"AWS_SECRET_ACCESS_KEY_FILE": write("secret", secretCanary+"\n"),
	}
	cfg, err := Load(mapEnvironment(environment))
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.Credentials.AWSAccessKeyID.Bytes()) != accessCanary || string(cfg.Credentials.AWSSecretKey.Bytes()) != secretCanary {
		t.Fatal("credential files were not loaded exactly")
	}
	formatted := fmt.Sprintf("%v %#v", cfg.Credentials.AWSAccessKeyID, cfg.Credentials.AWSSecretKey)
	if strings.Contains(formatted, accessCanary) || strings.Contains(formatted, secretCanary) {
		t.Fatalf("Secret formatting disclosed a canary: %s", formatted)
	}
	if len(cfg.SecretValues()) != 2 {
		t.Fatalf("SecretValues() count = %d", len(cfg.SecretValues()))
	}
	// The adapter re-reads rotated keys from these paths.
	if cfg.Credentials.AWSAccessKeyIDFile != environment["AWS_ACCESS_KEY_ID_FILE"] || cfg.Credentials.AWSSecretKeyFile != environment["AWS_SECRET_ACCESS_KEY_FILE"] || cfg.Credentials.AWSSessionTokenFile != "" {
		t.Fatalf("credential file paths = %q, %q, %q", cfg.Credentials.AWSAccessKeyIDFile, cfg.Credentials.AWSSecretKeyFile, cfg.Credentials.AWSSessionTokenFile)
	}
}

func TestLoadProviderCredentialModes(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	write := func(name, value string) string {
		t.Helper()
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	tests := []struct {
		name        string
		environment map[string]string
		wantMode    CredentialMode
	}{
		{
			name: "Azure workload identity with account endpoint",
			environment: map[string]string{
				"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "azure", "DESTINATION_PATH": "azure://container/prefix", "AZURE_STORAGE_ACCOUNT": "demo",
			},
			wantMode: CredentialAmbient,
		},
		{
			name: "Azure connection string file takes its exclusive mode",
			environment: map[string]string{
				"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "azure", "DESTINATION_PATH": "azure://container/prefix",
				"AZURE_STORAGE_CONNECTION_STRING_FILE": write("connection", "AccountName=demo;AccountKey=azure-canary"),
			},
			wantMode: CredentialConnectionFile,
		},
		{
			name: "Azure account key file",
			environment: map[string]string{
				"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "azure", "DESTINATION_PATH": "azure://container/prefix",
				"AZURE_STORAGE_ACCOUNT": "demo", "AZURE_STORAGE_ACCOUNT_KEY_FILE": write("azure-key", "azure-key-canary"),
			},
			wantMode: CredentialAccountKeyFile,
		},
		{
			name: "Azure SAS token file",
			environment: map[string]string{
				"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "azure", "DESTINATION_PATH": "azure://container/prefix",
				"AZURE_STORAGE_ACCOUNT": "demo", "AZURE_STORAGE_SAS_TOKEN_FILE": write("azure-sas", "sig=azure-sas-canary"),
			},
			wantMode: CredentialSASTokenFile,
		},
		{
			name: "GCS JSON file",
			environment: map[string]string{
				"REPOSITORY_FORMAT": "pgbackrest", "PROVIDER": "gcs", "DESTINATION_PATH": "gs://bucket/prefix",
				"GOOGLE_APPLICATION_CREDENTIALS": write("gcs.json", `{"type":"service_account","private_key":"gcs-canary"}`),
				"PGBACKREST_CIPHER_PASS_FILE":    write("pgbackrest-cipher", "cipher-canary"),
			},
			wantMode: CredentialJSONFile,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(mapEnvironment(test.environment))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Credentials.Mode != test.wantMode {
				t.Fatalf("credential mode = %s, want %s", cfg.Credentials.Mode, test.wantMode)
			}
			if cfg.RepositoryFormat == FormatPGBackRest && cfg.PGBackRestCipher.Empty() {
				t.Fatal("pgBackRest cipher file was not loaded")
			}
			formatted := fmt.Sprintf("%#v", cfg.Credentials)
			if strings.Contains(formatted, "canary") {
				t.Fatalf("credential formatting disclosed canary: %s", formatted)
			}
		})
	}
}

func TestLoadMalformedSecretNeverReturnsContentOrPath(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "path-canary")
	content := "content-canary-not-json"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(mapEnvironment(map[string]string{
		"REPOSITORY_FORMAT": "pgbackrest", "PROVIDER": "gcs", "DESTINATION_PATH": "gs://backups/prefix",
		"GOOGLE_APPLICATION_CREDENTIALS": path,
	}))
	if err == nil {
		t.Fatal("Load() error = nil")
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), content) {
		t.Fatalf("error disclosed path or content: %q", err)
	}
}

func TestLoadExplicitlyEmptyTrustedHeader(t *testing.T) {
	t.Parallel()
	environment := map[string]string{
		"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3", "DESTINATION_PATH": "s3://backups/prefix",
		"TRUSTED_USER_HEADER": "", "AWS_REGION": "eu-west-1",
	}
	cfg, err := load(mapEnvironment(environment), func(key string) (string, bool) {
		value, ok := environment[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedUserHeader != "" {
		t.Fatalf("TrustedUserHeader = %q", cfg.TrustedUserHeader)
	}
}

func TestLoadAgentCredentialProfiles(t *testing.T) {
	directory := t.TempDir()
	accessPath := filepath.Join(directory, "access")
	secretPath := filepath.Join(directory, "secret")
	if err := os.WriteFile(accessPath, []byte("synthetic-access"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("synthetic-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	static := validAgentEnvironment(accessPath, secretPath)
	cfg, err := Load(mapEnvironment(static))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeMode != RuntimeAgent || cfg.TrustedUserHeader != "" || cfg.AllowDownload || cfg.Credentials.Mode != CredentialStaticFiles {
		t.Fatalf("static agent config = %#v", cfg)
	}
	if cfg.CNPGClusterNamespace != "database-team" || cfg.CNPGClusterUID != "2f12b7d1-7e8d-4c37-a68f-233efc5f3191" || cfg.CNPGClusterName != "orders" || !reflect.DeepEqual(cfg.BarmanServerNames, []string{"orders"}) {
		t.Fatalf("agent identity = %#v", cfg)
	}

	webIdentity := cloneMap(static)
	delete(webIdentity, "AWS_ACCESS_KEY_ID_FILE")
	delete(webIdentity, "AWS_SECRET_ACCESS_KEY_FILE")
	webIdentity["STORE_CREDENTIAL_MODE"] = "aws-web-identity"
	webIdentity["AWS_WEB_IDENTITY_TOKEN_FILE"] = "/var/run/secrets/aws/token"
	webIdentity["AWS_ROLE_ARN"] = "arn:aws:iam::123456789012:role/evidence-agent"
	webIdentity["AWS_REGION"] = "eu-west-1"
	cfg, err = Load(mapEnvironment(webIdentity))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Credentials.Mode != CredentialAWSWebIdentity || cfg.Credentials.AWSWebIdentityTokenFile != webIdentity["AWS_WEB_IDENTITY_TOKEN_FILE"] || cfg.Credentials.AWSRoleARN != webIdentity["AWS_ROLE_ARN"] || !cfg.Credentials.AWSAccessKeyID.Empty() {
		t.Fatalf("web-identity agent config = %#v", cfg.Credentials)
	}
}

func TestLoadRejectsInvalidAgentRepositoryProfiles(t *testing.T) {
	directory := t.TempDir()
	accessPath := filepath.Join(directory, "access")
	secretPath := filepath.Join(directory, "secret")
	if err := os.WriteFile(accessPath, []byte("synthetic-access"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte("synthetic-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := validAgentEnvironment(accessPath, secretPath)
	tests := []struct {
		name     string
		changes  map[string]string
		remove   []string
		variable string
	}{
		{name: "unknown runtime", changes: map[string]string{"RUNTIME_MODE": "console-sidecar"}, variable: "RUNTIME_MODE"},
		{name: "pgBackRest viewer list", changes: map[string]string{"REPOSITORY_FORMAT": "pgbackrest", "BARMAN_SERVER_NAMES": "", "PGBACKREST_STANZAS": "orders"}, variable: "PGBACKREST_STANZAS"},
		{name: "missing server", remove: []string{"BARMAN_SERVER_NAMES"}, variable: "BARMAN_SERVER_NAMES"},
		{name: "multiple servers", changes: map[string]string{"BARMAN_SERVER_NAMES": "orders,archive"}, variable: "BARMAN_SERVER_NAMES"},
		{name: "missing namespace", remove: []string{"CNPG_CLUSTER_NAMESPACE"}, variable: "CNPG_CLUSTER_NAMESPACE"},
		{name: "missing UID", remove: []string{"CNPG_CLUSTER_UID"}, variable: "CNPG_CLUSTER_UID"},
		{name: "trusted header", changes: map[string]string{"TRUSTED_USER_HEADER": "X-Forwarded-User"}, variable: "TRUSTED_USER_HEADER"},
		{name: "download", changes: map[string]string{"ALLOW_DOWNLOAD": "true"}, variable: "ALLOW_DOWNLOAD"},
		{name: "missing credential mode", remove: []string{"STORE_CREDENTIAL_MODE"}, variable: "STORE_CREDENTIAL_MODE"},
		{name: "ambient credential mode", changes: map[string]string{"STORE_CREDENTIAL_MODE": "ambient"}, variable: "STORE_CREDENTIAL_MODE"},
		{name: "static mode without files", remove: []string{"AWS_ACCESS_KEY_ID_FILE", "AWS_SECRET_ACCESS_KEY_FILE"}, variable: "STORE_CREDENTIAL_MODE"},
		{name: "web identity with static", changes: map[string]string{"STORE_CREDENTIAL_MODE": "aws-web-identity", "AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/aws/token", "AWS_ROLE_ARN": "arn:aws:iam::123456789012:role/demo", "AWS_REGION": "eu-west-1"}, variable: "STORE_CREDENTIAL_MODE"},
		{name: "web identity without token", remove: []string{"AWS_ACCESS_KEY_ID_FILE", "AWS_SECRET_ACCESS_KEY_FILE"}, changes: map[string]string{"STORE_CREDENTIAL_MODE": "aws-web-identity", "AWS_ROLE_ARN": "arn:aws:iam::123456789012:role/demo", "AWS_REGION": "eu-west-1"}, variable: "AWS_WEB_IDENTITY_TOKEN_FILE"},
		{name: "web identity without role", remove: []string{"AWS_ACCESS_KEY_ID_FILE", "AWS_SECRET_ACCESS_KEY_FILE"}, changes: map[string]string{"STORE_CREDENTIAL_MODE": "aws-web-identity", "AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/aws/token", "AWS_REGION": "eu-west-1"}, variable: "AWS_ROLE_ARN"},
		{name: "web identity with non-role ARN", remove: []string{"AWS_ACCESS_KEY_ID_FILE", "AWS_SECRET_ACCESS_KEY_FILE"}, changes: map[string]string{"STORE_CREDENTIAL_MODE": "aws-web-identity", "AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/aws/token", "AWS_ROLE_ARN": "arn:aws:s3:::synthetic-bucket", "AWS_REGION": "eu-west-1"}, variable: "AWS_ROLE_ARN"},
		{name: "web identity without region", remove: []string{"AWS_ACCESS_KEY_ID_FILE", "AWS_SECRET_ACCESS_KEY_FILE", "AWS_REGION"}, changes: map[string]string{"STORE_CREDENTIAL_MODE": "aws-web-identity", "AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/aws/token", "AWS_ROLE_ARN": "arn:aws:iam::123456789012:role/demo"}, variable: "AWS_REGION"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := cloneMap(base)
			for _, key := range test.remove {
				delete(environment, key)
			}
			for key, value := range test.changes {
				environment[key] = value
			}
			cfg, err := Load(mapEnvironment(environment))
			// Repository-coordinate errors (credentials, region) are deferred:
			// the agent starts and publishes them as unknown evidence.
			if test.variable == "STORE_CREDENTIAL_MODE" || strings.HasPrefix(test.variable, "AWS_") {
				if err != nil || cfg.RepositoryError == nil || !strings.Contains(cfg.RepositoryError.Error(), test.variable) {
					t.Fatalf("Load() = %v, deferred %v; want deferred %s", err, cfg.RepositoryError, test.variable)
				}
				if !reflect.DeepEqual(cfg.Credentials, Credentials{}) || cfg.Endpoint != nil {
					t.Fatal("deferred repository error left credentials or endpoint for an adapter")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.variable) {
				t.Fatalf("Load() error = %v, want %s", err, test.variable)
			}
		})
	}
}

// In agent mode a repository-coordinate error must not stop the process;
// standalone mode keeps failing at startup.
func TestLoadDefersRepositoryErrorsOnlyInAgentMode(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	accessPath := filepath.Join(directory, "access")
	secretPath := filepath.Join(directory, "secret")
	for _, path := range []string{accessPath, secretPath} {
		if err := os.WriteFile(path, []byte("synthetic-canary"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	agent := validAgentEnvironment(accessPath, secretPath)
	tests := []struct {
		name     string
		changes  map[string]string
		remove   []string
		variable string
	}{
		{name: "missing region", remove: []string{"AWS_REGION"}, variable: "AWS_REGION"},
		{name: "destination with query", changes: map[string]string{"DESTINATION_PATH": "s3://backups/prefix?token=canary"}, variable: "DESTINATION_PATH"},
		{name: "missing destination", remove: []string{"DESTINATION_PATH"}, variable: "DESTINATION_PATH"},
		{name: "endpoint with path", changes: map[string]string{"ENDPOINT_URL": "https://minio.example.test/path"}, variable: "ENDPOINT_URL"},
		{name: "unreadable CA", changes: map[string]string{"ENDPOINT_CA_FILE": filepath.Join(directory, "missing-ca")}, variable: "ENDPOINT_CA_FILE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			environment := cloneMap(agent)
			for _, key := range test.remove {
				delete(environment, key)
			}
			for key, value := range test.changes {
				environment[key] = value
			}
			cfg, err := Load(mapEnvironment(environment))
			var configErr *Error
			if err != nil || !errors.As(cfg.RepositoryError, &configErr) || configErr.Variable != test.variable {
				t.Fatalf("agent Load() = %v, deferred %v; want deferred %s", err, cfg.RepositoryError, test.variable)
			}
			if strings.Contains(cfg.RepositoryError.Error(), "canary") || !reflect.DeepEqual(cfg.Credentials, Credentials{}) || cfg.Endpoint != nil {
				t.Fatalf("deferred error disclosed a value or kept adapter inputs: %v", cfg.RepositoryError)
			}
			if cfg.AgentName == "" || cfg.CNPGClusterUID == "" || len(cfg.BarmanServerNames) != 1 {
				t.Fatal("process-level agent identity must still load")
			}

			standalone := map[string]string{"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3", "DESTINATION_PATH": "s3://backups/prefix", "AWS_REGION": "eu-west-1"}
			for _, key := range test.remove {
				delete(standalone, key)
			}
			for key, value := range test.changes {
				standalone[key] = value
			}
			if _, err := Load(mapEnvironment(standalone)); !errors.As(err, &configErr) || configErr.Variable != test.variable {
				t.Fatalf("standalone Load() = %v; want %s", err, test.variable)
			}
		})
	}
}

func TestLoadStandaloneRejectsAgentOnlyConfiguration(t *testing.T) {
	base := map[string]string{
		"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3", "DESTINATION_PATH": "s3://backups/prefix",
	}
	for _, variable := range []string{"CNPG_CLUSTER_NAMESPACE", "CNPG_CLUSTER_UID", "CNPG_CLUSTER_NAME", "STORE_CREDENTIAL_MODE"} {
		t.Run(variable, func(t *testing.T) {
			environment := cloneMap(base)
			environment[variable] = "agent-only-canary"
			_, err := Load(mapEnvironment(environment))
			if err == nil || !strings.Contains(err.Error(), variable) || strings.Contains(err.Error(), "agent-only-canary") {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func mapEnvironment(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func cloneMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// The S3 client must always get a signing region. Without ENDPOINT_URL the
// target is AWS itself, where no default is safe, so configuration fails with
// a named variable instead of every scan failing as an opaque "unavailable".
func TestLoadResolvesS3SigningRegion(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	accessPath := filepath.Join(directory, "access")
	secretPath := filepath.Join(directory, "secret")
	for _, path := range []string{accessPath, secretPath} {
		if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	standalone := map[string]string{"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3", "DESTINATION_PATH": "s3://backups/prefix"}
	agent := validAgentEnvironment(accessPath, secretPath)
	delete(agent, "AWS_REGION")
	tests := []struct {
		name    string
		base    map[string]string
		changes map[string]string
		region  string
	}{
		{name: "standalone explicit region", base: standalone, changes: map[string]string{"AWS_REGION": "eu-west-3"}, region: "eu-west-3"},
		{name: "standalone compatible endpoint defaults", base: standalone, changes: map[string]string{"ENDPOINT_URL": "https://minio.example.test"}, region: "us-east-1"},
		{name: "explicit region wins over endpoint default", base: standalone, changes: map[string]string{"ENDPOINT_URL": "https://s3.eu-west-1.amazonaws.com", "AWS_REGION": "eu-west-1"}, region: "eu-west-1"},
		{name: "standalone AWS without region", base: standalone},
		{name: "agent static files on AWS without region", base: agent},
		{name: "agent static files with compatible endpoint", base: agent, changes: map[string]string{"ENDPOINT_URL": "http://minio.minio.svc:9000"}, region: "us-east-1"},
		{name: "agent static files with explicit region", base: agent, changes: map[string]string{"AWS_REGION": "ap-southeast-2"}, region: "ap-southeast-2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			environment := cloneMap(test.base)
			for key, value := range test.changes {
				environment[key] = value
			}
			cfg, err := Load(mapEnvironment(environment))
			if test.region == "" {
				if err == nil {
					err = cfg.RepositoryError // deferred in agent mode
				}
				var configErr *Error
				if !errors.As(err, &configErr) || configErr.Variable != "AWS_REGION" || !strings.Contains(configErr.Code, "ENDPOINT_URL") {
					t.Fatalf("Load() error = %v; want AWS_REGION configuration error", err)
				}
				return
			}
			if err != nil || cfg.Credentials.AWSRegion != test.region {
				t.Fatalf("Load() region = %q, %v; want %q", cfg.Credentials.AWSRegion, err, test.region)
			}
		})
	}

	// aws-web-identity still requires an explicit region even when an
	// endpoint would supply the compatible default: STS must not silently
	// be addressed in us-east-1.
	webIdentity := cloneMap(agent)
	delete(webIdentity, "AWS_ACCESS_KEY_ID_FILE")
	delete(webIdentity, "AWS_SECRET_ACCESS_KEY_FILE")
	webIdentity["STORE_CREDENTIAL_MODE"] = "aws-web-identity"
	webIdentity["AWS_WEB_IDENTITY_TOKEN_FILE"] = "/var/run/secrets/aws/token"
	webIdentity["AWS_ROLE_ARN"] = "arn:aws:iam::123456789012:role/evidence-agent"
	webIdentity["ENDPOINT_URL"] = "https://minio.example.test"
	var configErr *Error
	if cfg, err := Load(mapEnvironment(webIdentity)); err != nil || !errors.As(cfg.RepositoryError, &configErr) || configErr.Variable != "AWS_REGION" {
		t.Fatalf("web identity without region = %v", err)
	}
}

func TestParseLogLevel(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]slog.Level{"": slog.LevelInfo, "info": slog.LevelInfo, "DEBUG": slog.LevelDebug, "warn": slog.LevelWarn, "Error": slog.LevelError} {
		if got, err := ParseLogLevel(raw); err != nil || got != want {
			t.Fatalf("ParseLogLevel(%q) = %v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"trace", "warning", "0", " info"} {
		var configErr *Error
		if _, err := ParseLogLevel(raw); !errors.As(err, &configErr) || configErr.Variable != "LOG_LEVEL" {
			t.Fatalf("ParseLogLevel(%q) = %v", raw, err)
		}
	}
}
