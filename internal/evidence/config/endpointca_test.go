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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testCAPEM is a self-signed CA certificate, PEM-encoded.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic endpoint CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestEndpointCAForAzure: an endpoint CA applies to S3 and Azure (barman-cloud
// sets AWS_CA_BUNDLE and REQUESTS_CA_BUNDLE for them), never to GCS, for which
// barman-cloud uses none and which has no endpoint to verify.
func TestEndpointCAForAzure(t *testing.T) {
	bundle := testCAPEM(t)
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	azure := map[string]string{
		"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "azure", "DESTINATION_PATH": "azure://backups/team-a",
		"ENDPOINT_URL": "https://azurite.storage.svc:10000", "BARMAN_SERVER_NAMES": "orders", "ENDPOINT_CA_FILE": path,
	}
	cfg, err := Load(mapEnvironment(azure))
	if err != nil || !bytes.Equal(cfg.EndpointCABundle, bundle) {
		t.Fatalf("Azure endpoint CA = %v", err)
	}
	gcs := map[string]string{
		"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "gcs", "DESTINATION_PATH": "gs://backups/team-a",
		"BARMAN_SERVER_NAMES": "orders", "ENDPOINT_CA_FILE": path,
	}
	var configErr *Error
	if _, err := Load(mapEnvironment(gcs)); !errors.As(err, &configErr) || configErr.Variable != "ENDPOINT_CA_FILE" {
		t.Fatalf("GCS endpoint CA = %v", err)
	}

	base, err := loadExact(teamAgentEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	resolved := ResolvedRepository{
		ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders", Provider: ProviderAzure,
		DestinationPath: "azure://backups/team-a", EndpointURL: "https://azurite.storage.svc:10000", AzureAccount: "devstoreaccount1",
		Credential:            CredentialConnectionFile,
		AzureConnectionString: []byte("DefaultEndpointsProtocol=https;AccountName=devstoreaccount1;AccountKey=synthetic-canary;BlobEndpoint=https://azurite.storage.svc:10000/devstoreaccount1;"),
		EndpointCABundle:      bundle,
	}
	applied := ApplyResolved(base, resolved, 1, nil)
	if applied.RepositoryError != nil || !bytes.Equal(applied.EndpointCABundle, bundle) || applied.Credentials.AzureConnectionString.Empty() {
		t.Fatalf("self-resolved Azure endpoint CA = %v", applied.RepositoryError)
	}
}
