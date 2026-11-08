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
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// teamAgentEnvironment is what the pgtools chart renders for one agent:
// shared token, plain HTTP, self-resolution.
func teamAgentEnvironment() map[string]string {
	return map[string]string{
		"RUNTIME_MODE": "agent", "AGENT_AUTH": "sharedtoken", "AGENT_TOKEN_FILE": "/var/run/pgtoolbox/evidence-token/token",
		"AGENT_PLAIN_HTTP": "true", "AGENT_RESOLVE": "cluster", "AGENT_NAME": "pgc-evidence-orders",
		"AGENT_NAMESPACE": "team-a", "CNPG_CLUSTER_NAME": "orders", "AGENT_ALLOWED_SECRETS": "minio-creds, minio-ca,minio-creds",
	}
}

func loadExact(environment map[string]string) (Config, error) {
	return load(mapEnvironment(environment), func(key string) (string, bool) {
		value, exists := environment[key]
		return value, exists
	})
}

func TestLoadSelfResolvingTeamAgent(t *testing.T) {
	cfg, err := loadExact(teamAgentEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentAuth != AgentAuthSharedToken || cfg.AgentTokenFile != "/var/run/pgtoolbox/evidence-token/token" || !cfg.AgentPlainHTTP ||
		cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" || cfg.AgentResolve != AgentResolveCluster {
		t.Fatalf("transport = %#v", cfg)
	}
	if cfg.CNPGClusterName != "orders" || cfg.CNPGClusterNamespace != "team-a" || cfg.CNPGClusterUID != "" || cfg.ProducerEpoch != 0 ||
		!reflect.DeepEqual(cfg.AgentAllowedSecrets, []string{"minio-ca", "minio-creds"}) || cfg.ListenAddr != ":8443" {
		t.Fatalf("identity = %#v", cfg)
	}
	if cfg.RepositoryFormat != FormatBarmanCloud || cfg.Provider != ProviderS3 || cfg.Destination != nil || cfg.RepositoryError != nil {
		t.Fatalf("a self-resolving agent has no repository before ApplyResolved: %#v", cfg)
	}
	if cfg.AgentAllowInsecureEndpoint || cfg.AgentWorkloadIdentity {
		t.Fatalf("an opt-in is on by default: %#v", cfg)
	}
	optedIn := teamAgentEnvironment()
	optedIn["AGENT_ALLOW_INSECURE_ENDPOINT"], optedIn["AGENT_WORKLOAD_IDENTITY"] = "true", "true"
	if cfg, err := loadExact(optedIn); err != nil || !cfg.AgentAllowInsecureEndpoint || !cfg.AgentWorkloadIdentity {
		t.Fatalf("opt-ins = %#v, %v", cfg, err)
	}

	// The operator's profile is unchanged: serviceaccount auth by default.
	accessPath, secretPath := agentCredentialFiles(t)
	operator, err := Load(mapEnvironment(validAgentEnvironment(accessPath, secretPath)))
	if err != nil || operator.AgentAuth != AgentAuthServiceAccount || operator.AgentPlainHTTP || operator.AgentResolve != AgentResolveEnvironment {
		t.Fatalf("operator profile = %#v, %v", operator, err)
	}
	// A shared token with the operator's TLS and environment-rendered
	// repository is a valid combination too.
	environment := validAgentEnvironment(accessPath, secretPath)
	environment["AGENT_AUTH"], environment["AGENT_TOKEN_FILE"] = "sharedtoken", "/var/run/token"
	if cfg, err := Load(mapEnvironment(environment)); err != nil || cfg.AgentAuth != AgentAuthSharedToken || cfg.TLSCertFile == "" {
		t.Fatalf("shared token over TLS = %#v, %v", cfg, err)
	}
}

func TestLoadRejectsInvalidTeamAgentProfiles(t *testing.T) {
	accessPath, secretPath := agentCredentialFiles(t)
	tests := []struct {
		name     string
		base     map[string]string
		changes  map[string]string
		remove   []string
		variable string
	}{
		{name: "unknown auth", changes: map[string]string{"AGENT_AUTH": "mtls"}, variable: "AGENT_AUTH"},
		{name: "shared token without a file", remove: []string{"AGENT_TOKEN_FILE"}, variable: "AGENT_TOKEN_FILE"},
		{name: "relative token file", changes: map[string]string{"AGENT_TOKEN_FILE": "token"}, variable: "AGENT_TOKEN_FILE"},
		{name: "unclean token file", changes: map[string]string{"AGENT_TOKEN_FILE": "/var/run/../token"}, variable: "AGENT_TOKEN_FILE"},
		{name: "token file with serviceaccount auth", base: validAgentEnvironment(accessPath, secretPath), changes: map[string]string{"AGENT_TOKEN_FILE": "/t"}, variable: "AGENT_TOKEN_FILE"},
		{name: "plain HTTP not a boolean", changes: map[string]string{"AGENT_PLAIN_HTTP": "yes"}, variable: "AGENT_PLAIN_HTTP"},
		{name: "plain HTTP and a certificate", changes: map[string]string{"TLS_CERT_FILE": "/tls/tls.crt", "TLS_KEY_FILE": "/tls/tls.key"}, variable: "TLS_CERT_FILE"},
		{name: "plain HTTP and a key", changes: map[string]string{"TLS_KEY_FILE": "/tls/tls.key"}, variable: "TLS_KEY_FILE"},
		{name: "neither TLS nor plain HTTP", remove: []string{"AGENT_PLAIN_HTTP"}, variable: "TLS_CERT_FILE"},
		{name: "plain HTTP with a bound token", base: validAgentEnvironment(accessPath, secretPath), changes: map[string]string{"AGENT_PLAIN_HTTP": "true"}, variable: "AGENT_PLAIN_HTTP"},
		{name: "unknown resolve mode", base: validAgentEnvironment(accessPath, secretPath), changes: map[string]string{"AGENT_RESOLVE": "operator"}, variable: "AGENT_RESOLVE"},
		{name: "allow-list without self-resolution", base: validAgentEnvironment(accessPath, secretPath), changes: map[string]string{"AGENT_ALLOWED_SECRETS": "minio"}, variable: "AGENT_ALLOWED_SECRETS"},
		{name: "allow-list too long", changes: map[string]string{"AGENT_ALLOWED_SECRETS": func() string {
			names := make([]string, 65)
			for i := range names {
				names[i] = "s" + strconv.Itoa(i)
			}
			return strings.Join(names, ",")
		}()}, variable: "AGENT_ALLOWED_SECRETS"},
		{name: "insecure opt-in without self-resolution", base: validAgentEnvironment(accessPath, secretPath), changes: map[string]string{"AGENT_ALLOW_INSECURE_ENDPOINT": "true"}, variable: "AGENT_ALLOW_INSECURE_ENDPOINT"},
		{name: "workload identity opt-in without self-resolution", base: validAgentEnvironment(accessPath, secretPath), changes: map[string]string{"AGENT_WORKLOAD_IDENTITY": "true"}, variable: "AGENT_WORKLOAD_IDENTITY"},
		{name: "instance metadata turned back on", changes: map[string]string{"AWS_EC2_METADATA_DISABLED": "false"}, variable: "AWS_EC2_METADATA_DISABLED"},
		{name: "insecure opt-in not a boolean", changes: map[string]string{"AGENT_ALLOW_INSECURE_ENDPOINT": "yes"}, variable: "AGENT_ALLOW_INSECURE_ENDPOINT"},
		{name: "workload identity opt-in not a boolean", changes: map[string]string{"AGENT_WORKLOAD_IDENTITY": "1"}, variable: "AGENT_WORKLOAD_IDENTITY"},
		{name: "allow-list with a bad name", changes: map[string]string{"AGENT_ALLOWED_SECRETS": "minio,Minio_Creds"}, variable: "AGENT_ALLOWED_SECRETS"},
		{name: "no cluster", remove: []string{"CNPG_CLUSTER_NAME"}, variable: "CNPG_CLUSTER_NAME"},
		{name: "cluster not a name", changes: map[string]string{"CNPG_CLUSTER_NAME": "Orders/x"}, variable: "CNPG_CLUSTER_NAME"},
		{name: "cluster in another namespace", changes: map[string]string{"CNPG_CLUSTER_NAMESPACE": "team-b"}, variable: "CNPG_CLUSTER_NAMESPACE"},
		{name: "rendered destination", changes: map[string]string{"DESTINATION_PATH": "s3://b/p"}, variable: "DESTINATION_PATH"},
		{name: "rendered epoch", changes: map[string]string{"PRODUCER_EPOCH": "3"}, variable: "PRODUCER_EPOCH"},
		{name: "rendered cluster uid", changes: map[string]string{"CNPG_CLUSTER_UID": "u"}, variable: "CNPG_CLUSTER_UID"},
		{name: "rendered key file", changes: map[string]string{"AWS_ACCESS_KEY_ID_FILE": "/k"}, variable: "AWS_ACCESS_KEY_ID_FILE"},
		{name: "rendered session token file", changes: map[string]string{"AWS_SESSION_TOKEN_FILE": "/t"}, variable: "AWS_SESSION_TOKEN_FILE"},
		{name: "rendered format", changes: map[string]string{"REPOSITORY_FORMAT": "barman-cloud"}, variable: "REPOSITORY_FORMAT"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := teamAgentEnvironment()
			if test.base != nil {
				environment = cloneMap(test.base)
			}
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
		})
	}
}

type staticKeys struct{}

func (staticKeys) Keys(context.Context) ([]byte, []byte, []byte, error) {
	return []byte("synthetic-canary"), []byte("synthetic-canary"), nil, nil
}

func TestApplyResolved(t *testing.T) {
	base, err := loadExact(teamAgentEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	resolved := ResolvedRepository{
		ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders-v2", DestinationPath: "s3://backups/team-a",
		EndpointURL: "https://minio.storage.svc:9000", Keys: staticKeys{},
	}
	cfg := ApplyResolved(base, resolved, 1730000000, nil)
	if cfg.RepositoryError != nil || cfg.ProducerEpoch != 1730000000 || cfg.CNPGClusterUID != resolved.ClusterUID ||
		!reflect.DeepEqual(cfg.BarmanServerNames, []string{"orders-v2"}) || cfg.DestinationBucket != "backups" || cfg.DestinationPrefix != "team-a" ||
		cfg.Endpoint == nil || cfg.Credentials.Mode != CredentialKubernetesSecrets || cfg.Credentials.AWSKeySource == nil ||
		cfg.Credentials.AWSRegion != defaultCompatibleS3Region {
		t.Fatalf("resolved config = %#v", cfg)
	}
	withRegion := resolved
	withRegion.Region = "eu-west-3"
	if cfg := ApplyResolved(base, withRegion, 2, nil); cfg.Credentials.AWSRegion != "eu-west-3" {
		t.Fatalf("explicit region = %q", cfg.Credentials.AWSRegion)
	}

	problem := errors.New("ObjectStoreNotFound: ObjectStore store was not found in namespace team-a")
	failed := ApplyResolved(base, ResolvedRepository{}, 3, problem)
	if !errors.Is(failed.RepositoryError, problem) || failed.ProducerEpoch != 3 || failed.CNPGClusterUID != unresolvedClusterUID ||
		!reflect.DeepEqual(failed.BarmanServerNames, []string{"orders"}) || failed.Credentials.AWSKeySource != nil || failed.Destination != nil {
		t.Fatalf("failed resolution = %#v", failed)
	}

	for name, change := range map[string]func(*ResolvedRepository){
		"credential in the destination": func(r *ResolvedRepository) { r.DestinationPath = "s3://AKIA:synthetic-canary@backups/x" },
		"endpoint with a path":          func(r *ResolvedRepository) { r.EndpointURL = "https://minio/path" },
		"CA without a certificate":      func(r *ResolvedRepository) { r.EndpointCABundle = []byte("synthetic-canary") },
		"AWS without a region":          func(r *ResolvedRepository) { r.EndpointURL = "" },
		"server with a slash":           func(r *ResolvedRepository) { r.ServerName = "a/b" },
		"no key source":                 func(r *ResolvedRepository) { r.Keys = nil },
		"no cluster uid":                func(r *ResolvedRepository) { r.ClusterUID = "" },
	} {
		candidate := resolved
		change(&candidate)
		cfg := ApplyResolved(base, candidate, 4, nil)
		if !IsError(cfg.RepositoryError) || strings.Contains(cfg.RepositoryError.Error(), "canary") || cfg.Credentials.AWSKeySource != nil || cfg.Endpoint != nil {
			t.Fatalf("%s: %#v", name, cfg)
		}
	}
}

func TestLoadSelfResolvingWithoutSecrets(t *testing.T) {
	environment := teamAgentEnvironment()
	delete(environment, "AGENT_ALLOWED_SECRETS")
	cfg, err := loadExact(environment)
	if err != nil || len(cfg.AgentAllowedSecrets) != 0 {
		t.Fatalf("a workload-identity agent without Secrets = %#v, %v", cfg, err)
	}
}

// TestLoadSelfResolvingPgBackRestSettings: the chart entry's pgbackrest
// settings (repository, endpointCA) are read, bounded and allow-listed.
func TestLoadSelfResolvingPgBackRestSettings(t *testing.T) {
	environment := teamAgentEnvironment()
	environment["AGENT_PGBACKREST_REPOSITORY"] = "2"
	environment["AGENT_PGBACKREST_ENDPOINT_CA_SECRET"], environment["AGENT_PGBACKREST_ENDPOINT_CA_KEY"] = "minio-ca", "ca.crt"
	environment["AGENT_PGBACKREST_ENDPOINT"] = "minio.storage.svc:9000"
	cfg, err := loadExact(environment)
	if err != nil || cfg.AgentPgBackRestRepository != 2 || cfg.AgentPgBackRestEndpointCA != (SecretKey{Name: "minio-ca", Key: "ca.crt"}) ||
		cfg.AgentPgBackRestEndpoint != "minio.storage.svc:9000" {
		t.Fatalf("pgbackrest settings = %d %+v %q, %v", cfg.AgentPgBackRestRepository, cfg.AgentPgBackRestEndpointCA, cfg.AgentPgBackRestEndpoint, err)
	}
	if cfg, err := loadExact(teamAgentEnvironment()); err != nil || cfg.AgentPgBackRestRepository != 0 || cfg.AgentPgBackRestEndpointCA != (SecretKey{}) {
		t.Fatalf("defaults = %#v, %v", cfg, err)
	}
	accessPath, secretPath := agentCredentialFiles(t)
	for name, test := range map[string]struct {
		base     map[string]string
		changes  map[string]string
		variable string
	}{
		"repository zero":         {changes: map[string]string{"AGENT_PGBACKREST_REPOSITORY": "0"}, variable: "AGENT_PGBACKREST_REPOSITORY"},
		"repository past 256":     {changes: map[string]string{"AGENT_PGBACKREST_REPOSITORY": "257"}, variable: "AGENT_PGBACKREST_REPOSITORY"},
		"repository not a number": {changes: map[string]string{"AGENT_PGBACKREST_REPOSITORY": "repo1"}, variable: "AGENT_PGBACKREST_REPOSITORY"},
		"endpoint with a scheme":  {changes: map[string]string{"AGENT_PGBACKREST_ENDPOINT": "https://minio:9000"}, variable: "AGENT_PGBACKREST_ENDPOINT"},
		"endpoint with a path":    {changes: map[string]string{"AGENT_PGBACKREST_ENDPOINT": "minio:9000/x"}, variable: "AGENT_PGBACKREST_ENDPOINT"},
		"endpoint without self-resolution": {
			base:    validAgentEnvironment(accessPath, secretPath),
			changes: map[string]string{"AGENT_PGBACKREST_ENDPOINT": "minio:9000"}, variable: "AGENT_PGBACKREST_ENDPOINT",
		},
		"CA key without a Secret": {changes: map[string]string{"AGENT_PGBACKREST_ENDPOINT_CA_KEY": "ca.crt"}, variable: "AGENT_PGBACKREST_ENDPOINT_CA_SECRET"},
		"CA Secret without a key": {changes: map[string]string{"AGENT_PGBACKREST_ENDPOINT_CA_SECRET": "minio-ca"}, variable: "AGENT_PGBACKREST_ENDPOINT_CA_KEY"},
		"CA key with a slash": {changes: map[string]string{
			"AGENT_PGBACKREST_ENDPOINT_CA_SECRET": "minio-ca", "AGENT_PGBACKREST_ENDPOINT_CA_KEY": "../ca",
		}, variable: "AGENT_PGBACKREST_ENDPOINT_CA_KEY"},
		"CA Secret outside the allow-list": {changes: map[string]string{
			"AGENT_PGBACKREST_ENDPOINT_CA_SECRET": "other-ca", "AGENT_PGBACKREST_ENDPOINT_CA_KEY": "ca.crt",
		}, variable: "AGENT_PGBACKREST_ENDPOINT_CA_SECRET"},
		"repository without self-resolution": {
			base:    validAgentEnvironment(accessPath, secretPath),
			changes: map[string]string{"AGENT_PGBACKREST_REPOSITORY": "1"}, variable: "AGENT_PGBACKREST_REPOSITORY",
		},
	} {
		t.Run(name, func(t *testing.T) {
			environment := teamAgentEnvironment()
			if test.base != nil {
				environment = cloneMap(test.base)
			}
			for key, value := range test.changes {
				environment[key] = value
			}
			_, err := loadExact(environment)
			var configErr *Error
			if !errors.As(err, &configErr) || configErr.Variable != test.variable {
				t.Fatalf("Load() = %v, want an error naming %s", err, test.variable)
			}
		})
	}
}

// TestApplyResolvedPgBackRest: a resolved pgBackRest repository sets the
// format, the one stanza, the WAL stanza, the repository number and the
// cipher pass; a failure clears the cipher pass with the credentials.
func TestApplyResolvedPgBackRest(t *testing.T) {
	base, err := loadExact(teamAgentEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	resolved := ResolvedRepository{
		ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", DestinationPath: "s3://backups/orders", EndpointURL: "https://minio.storage.svc:9000",
		Keys: staticKeys{}, Format: FormatPGBackRest, Stanza: "orders", ArchiveStanza: "orders-wal", Repository: 2, CipherPass: []byte("synthetic-canary"),
		Retention: &PGBackRestRetention{Full: 7, FullType: "time", ArchiveType: "full"},
	}
	cfg := ApplyResolved(base, resolved, 5, nil)
	if cfg.PGBackRestRetention == nil || *cfg.PGBackRestRetention != *resolved.Retention || cfg.PGBackRestRetention == resolved.Retention {
		t.Fatalf("resolved retention = %+v", cfg.PGBackRestRetention)
	}
	if cfg.RepositoryError != nil || cfg.RepositoryFormat != FormatPGBackRest || cfg.PGBackRestStanza != "orders" || len(cfg.PGBackRestStanzas) != 0 || len(cfg.BarmanServerNames) != 0 ||
		cfg.PGBackRestArchiveStanza != "orders-wal" || cfg.PGBackRestRepository != 2 || string(cfg.PGBackRestCipher.value) != "synthetic-canary" ||
		cfg.DestinationBucket != "backups" || cfg.DestinationPrefix != "orders" || cfg.Credentials.AWSKeySource == nil {
		t.Fatalf("resolved config = %#v", cfg)
	}
	for name, change := range map[string]func(*ResolvedRepository){
		"stanza with a slash":      func(r *ResolvedRepository) { r.Stanza = "a/b" },
		"no stanza":                func(r *ResolvedRepository) { r.Stanza = "" },
		"WAL stanza with a slash":  func(r *ResolvedRepository) { r.ArchiveStanza = "a/b" },
		"repository out of range":  func(r *ResolvedRepository) { r.Repository = 0 },
		"retention type unknown":   func(r *ResolvedRepository) { r.Retention = &PGBackRestRetention{FullType: "days"} },
		"retention out of range":   func(r *ResolvedRepository) { r.Retention = &PGBackRestRetention{Full: 10_000_000} },
		"credential in the bucket": func(r *ResolvedRepository) { r.DestinationPath = "s3://AKIA:synthetic-canary@backups/x" },
	} {
		candidate := resolved
		change(&candidate)
		cfg := ApplyResolved(base, candidate, 6, nil)
		if !IsError(cfg.RepositoryError) || strings.Contains(cfg.RepositoryError.Error(), "canary") || len(cfg.PGBackRestCipher.value) != 0 ||
			cfg.Credentials.AWSKeySource != nil {
			t.Fatalf("%s: %#v", name, cfg)
		}
	}
}
