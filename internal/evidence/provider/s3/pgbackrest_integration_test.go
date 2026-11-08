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

//go:build integration

package s3

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/provider/providertest"
)

// TestS3PGBackRestJourney reads what real pgBackRest 2.60.0 wrote to MinIO
// over TLS (hack/evidence/test-pgbackrest-providers.sh): the bucket root, to
// prove the repo-path to key mapping, then the plain and the encrypted
// repositories through the S3 adapter rooted at their prefixes.
func TestS3PGBackRestJourney(t *testing.T) {
	endpointValue := os.Getenv("EVIDENCE_AGENT_PGBACKREST_S3_ENDPOINT")
	caFile := os.Getenv("EVIDENCE_AGENT_PGBACKREST_S3_CA_FILE")
	bucket := os.Getenv("EVIDENCE_AGENT_PGBACKREST_S3_BUCKET")
	accessKey := os.Getenv("EVIDENCE_AGENT_PGBACKREST_S3_ACCESS_KEY")
	secretKey := os.Getenv("EVIDENCE_AGENT_PGBACKREST_S3_SECRET_KEY")
	if endpointValue == "" || caFile == "" || bucket == "" || accessKey == "" || secretKey == "" {
		t.Skip("the pgBackRest MinIO journey environment is not configured")
	}
	endpoint, err := url.Parse(endpointValue)
	if err != nil {
		t.Fatal(err)
	}
	caBundle, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	adapter := func(t *testing.T, prefix string) *Store {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		store, err := New(ctx, Options{
			Bucket: bucket, Prefix: prefix, Endpoint: endpoint, CABundle: caBundle, Region: "us-east-1",
			AccessKeyID: []byte(accessKey), SecretAccessKey: []byte(secretKey), RequestTimeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	t.Run("key mapping", func(t *testing.T) {
		providertest.AssertPGBackRestKeyMapping(t, adapter(t, ""))
	})
	t.Run("plain", func(t *testing.T) {
		result := providertest.PGBackRestJourney(t, adapter(t, providertest.PGBackRestRepositoryPrefix), nil)
		providertest.WritePGBackRestJourneyResult(t, result)
	})
	t.Run("a leading slash names the same repository", func(t *testing.T) {
		providertest.PGBackRestJourney(t, adapter(t, "/"+providertest.PGBackRestRepositoryPrefix), nil)
	})
	t.Run("encrypted", func(t *testing.T) {
		result := providertest.PGBackRestJourney(t, adapter(t, providertest.PGBackRestEncryptedPrefix), []byte(providertest.PGBackRestPassphrase))
		if !result.Encrypted {
			t.Fatal("the encrypted repository was not reported encrypted")
		}
	})
}
