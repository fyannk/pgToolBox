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

package azure

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/fyannk/pgToolBox/internal/evidence/provider/providertest"
)

// azuriteKey is Azurite's published development account key.
const azuriteKey = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="

// TestAzuritePGBackRestJourney has two phases, both driven by the script
// test-pgbackrest-providers.sh of hack/evidence, over TLS (SSL_CERT_FILE holds
// the journey's CA): "setup" creates the container pgBackRest writes into
// (it creates none itself); "verify" reads what real pgBackRest 2.60.0
// wrote, through the Azure adapter with a shared key.
func TestAzuritePGBackRestJourney(t *testing.T) {
	endpoint := os.Getenv("EVIDENCE_AGENT_PGBACKREST_AZURE_ENDPOINT")
	phase := os.Getenv("EVIDENCE_AGENT_PGBACKREST_AZURE_PHASE")
	if endpoint == "" || phase == "" {
		t.Skip("the pgBackRest Azurite journey environment is not configured")
	}
	const container, account = "pgbr-proof", "devstoreaccount1"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if phase == "setup" {
		connection := fmt.Sprintf("DefaultEndpointsProtocol=https;AccountName=%s;AccountKey=%s;BlobEndpoint=%s/%s;", account, azuriteKey, endpoint, account)
		admin, err := azblob.NewClientFromConnectionString(connection, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.CreateContainer(ctx, container, nil); err != nil {
			t.Fatal(err)
		}
		return
	}
	adapter := func(t *testing.T, prefix string) *Store {
		t.Helper()
		store, err := New(ctx, Options{
			Container: container, Prefix: prefix, Account: account, Endpoint: endpoint,
			AccountKey: []byte(azuriteKey), RequestTimeout: 10 * time.Second,
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
}
