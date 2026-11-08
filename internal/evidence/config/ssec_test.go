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
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syntheticSSECKey is a 256-bit key in the Secret's form (base64), with
// the trailing newline a Secret written by hand usually has.
var (
	syntheticSSECRaw = bytes.Repeat([]byte{0xa5}, 32)
	syntheticSSECKey = base64.StdEncoding.EncodeToString(syntheticSSECRaw) + "\n"
)

func TestParseSSECustomerKey(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		ok    bool
	}{
		{"base64 of 32 bytes", syntheticSSECKey, true},
		{"surrounding white space, as barman strips it", "  " + strings.TrimSpace(syntheticSSECKey) + " \r\n", true},
		{"raw 32 bytes, not base64", string(syntheticSSECRaw), false},
		{"base64 of 16 bytes", base64.StdEncoding.EncodeToString(make([]byte, 16)), false},
		{"base64 of 33 bytes", base64.StdEncoding.EncodeToString(make([]byte, 33)), false},
		{"URL-safe alphabet", base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 32)), false},
		{"missing padding", base64.RawStdEncoding.EncodeToString(syntheticSSECRaw), false},
		{"empty", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			key, err := parseSSECustomerKey("S3_SSE_CUSTOMER_KEY_FILE", []byte(test.value))
			if test.ok != (err == nil) {
				t.Fatalf("parse = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), strings.TrimSpace(test.value)) && test.value != "" {
				t.Fatal("the error discloses the value")
			}
			if test.ok && !bytes.Equal(key, syntheticSSECRaw) {
				t.Fatalf("key = %x", key)
			}
		})
	}
}

func TestLoadSSECustomerKeyFile(t *testing.T) {
	accessPath, secretPath := agentCredentialFiles(t)
	keyPath := filepath.Join(t.TempDir(), "sse-customer-key")
	if err := os.WriteFile(keyPath, []byte(syntheticSSECKey), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := validAgentEnvironment(accessPath, secretPath)
	environment["S3_SSE_CUSTOMER_KEY_FILE"] = keyPath
	cfg, err := Load(mapEnvironment(environment))
	if err != nil || cfg.RepositoryError != nil {
		t.Fatalf("Load() = %v, repository error %v", err, cfg.RepositoryError)
	}
	if !bytes.Equal(cfg.Credentials.S3SSECustomerKey.Bytes(), syntheticSSECRaw) {
		t.Fatal("the SSE-C key was not loaded")
	}
	redacted := false
	for _, value := range cfg.SecretValues() {
		if string(value) == strings.TrimSpace(syntheticSSECKey) {
			redacted = true
		}
	}
	if !redacted {
		t.Fatal("the base64 form of the SSE-C key does not reach the redactor")
	}

	// A bad key is a repository error in the agent: unknown evidence that
	// names the variable, never the value, and no credential reaches a store.
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, []byte("synthetic-canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment["S3_SSE_CUSTOMER_KEY_FILE"] = bad
	cfg, err = Load(mapEnvironment(environment))
	var configErr *Error
	if err != nil || !errors.As(cfg.RepositoryError, &configErr) || configErr.Variable != "S3_SSE_CUSTOMER_KEY_FILE" ||
		strings.Contains(cfg.RepositoryError.Error(), "canary") || !cfg.Credentials.S3SSECustomerKey.Empty() {
		t.Fatalf("bad key: %v, %#v", err, cfg.RepositoryError)
	}

	// Only S3 has SSE-C.
	gcs := map[string]string{
		"RUNTIME_MODE": "", "REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "gcs", "DESTINATION_PATH": "gs://backups/team-a",
		"BARMAN_SERVER_NAMES": "orders", "S3_SSE_CUSTOMER_KEY_FILE": keyPath,
	}
	if _, err := Load(mapEnvironment(gcs)); !errors.As(err, &configErr) || configErr.Variable != "S3_SSE_CUSTOMER_KEY_FILE" {
		t.Fatalf("SSE-C on GCS = %v", err)
	}
	// A self-resolving agent takes it from the configuration only.
	team := teamAgentEnvironment()
	team["S3_SSE_CUSTOMER_KEY_FILE"] = keyPath
	if _, err := loadExact(team); !errors.As(err, &configErr) || configErr.Variable != "S3_SSE_CUSTOMER_KEY_FILE" {
		t.Fatalf("SSE-C variable with AGENT_RESOLVE=cluster = %v", err)
	}
}

func TestApplyResolvedSSECustomerKey(t *testing.T) {
	base, err := loadExact(teamAgentEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	resolved := ResolvedRepository{
		ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders", DestinationPath: "s3://backups/team-a",
		EndpointURL: "https://objects.example.net", Keys: staticKeys{}, S3SSECustomerKey: []byte(strings.TrimSpace(syntheticSSECKey)),
	}
	cfg := ApplyResolved(base, resolved, 1, nil)
	if cfg.RepositoryError != nil || !bytes.Equal(cfg.Credentials.S3SSECustomerKey.Bytes(), syntheticSSECRaw) {
		t.Fatalf("SSE-C repository = %#v", cfg)
	}
	for name, candidate := range map[string]ResolvedRepository{
		"not base64": func() ResolvedRepository { r := resolved; r.S3SSECustomerKey = []byte("synthetic-canary"); return r }(),
		"128-bit key": func() ResolvedRepository {
			r := resolved
			r.S3SSECustomerKey = []byte(base64.StdEncoding.EncodeToString(make([]byte, 16)))
			return r
		}(),
		"on Azure": func() ResolvedRepository {
			return ResolvedRepository{
				ClusterUID: resolved.ClusterUID, ServerName: "orders", Provider: ProviderAzure, DestinationPath: "azure://backups/team-a",
				AzureAccount: "orders", Credential: CredentialSASTokenFile, AzureSASToken: []byte("sv=2024&sig=synthetic-canary"),
				S3SSECustomerKey: resolved.S3SSECustomerKey,
			}
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := ApplyResolved(base, candidate, 1, nil)
			var configErr *Error
			if !errors.As(cfg.RepositoryError, &configErr) || configErr.Variable != "S3_SSE_CUSTOMER_KEY_FILE" ||
				strings.Contains(cfg.RepositoryError.Error(), "canary") || len(cfg.SecretValues()) != 0 {
				t.Fatalf("%#v", cfg.RepositoryError)
			}
		})
	}
}
