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

package application

import (
	"context"
	"testing"

	barmanapi "github.com/cloudnative-pg/barman-cloud/pkg/api"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fyannk/pgToolBox/internal/backupresolve"
	"github.com/fyannk/pgToolBox/internal/evidence/config"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
)

type noKeys struct{}

func (noKeys) Keys(context.Context) ([]byte, []byte, []byte, error) { return nil, nil, nil, nil }

// TestPublishedIdentityMatchesTheResolver is the contract between the two
// sides of every provider: the fingerprint the resolver computes (the
// operator's EvidenceAgent status.fingerprint, which the console compares)
// equals the one the agent publishes once its configuration is built from
// that same composition, and the published provider is the composition's.
func TestPublishedIdentityMatchesTheResolver(t *testing.T) {
	secret := map[string]any{"name": "store", "key": "value"}
	for name, raw := range map[string]map[string]any{
		"S3": {
			"destinationPath": "s3://backups/team-a", "endpointURL": "https://minio.storage.svc:9000",
			"s3Credentials": map[string]any{"accessKeyId": secret, "secretAccessKey": secret},
		},
		"Azure public cloud": {
			"destinationPath":  "https://orders.blob.core.windows.net/backups/team%20a",
			"azureCredentials": map[string]any{"storageAccount": secret, "storageKey": secret},
		},
		"Azure emulator": {
			"destinationPath":  "http://azurite.storage.svc:10000/devstoreaccount1/backups",
			"azureCredentials": map[string]any{"connectionString": secret},
		},
		"GCS": {
			"destinationPath":   "gs://backups/team-a/orders",
			"googleCredentials": map[string]any{"applicationCredentials": secret},
		},
	} {
		t.Run(name, func(t *testing.T) {
			configuration := &barmanapi.BarmanObjectStoreConfiguration{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, configuration); err != nil {
				t.Fatal(err)
			}
			composition, whyNot := backupresolve.Compose(backupresolve.Origin{Cluster: "orders"}, "orders", configuration)
			if whyNot != nil {
				t.Fatal(whyNot.Message)
			}
			base, err := config.Load(func(key string) string {
				return map[string]string{
					"RUNTIME_MODE": "agent", "AGENT_AUTH": "sharedtoken", "AGENT_TOKEN_FILE": "/var/run/token", "AGENT_PLAIN_HTTP": "true",
					"AGENT_RESOLVE": "cluster", "AGENT_NAME": "orders-evidence", "AGENT_NAMESPACE": "team-a", "CNPG_CLUSTER_NAME": "orders",
					"AGENT_ALLOWED_SECRETS": "store",
				}[key]
			})
			if err != nil {
				t.Fatal(err)
			}
			resolved := config.ResolvedRepository{
				ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: composition.ServerName,
				Provider: config.Provider(composition.Provider), DestinationPath: composition.AgentDestination,
				EndpointURL: composition.EndpointURL, Region: composition.Region.Value, AzureAccount: composition.AzureAccount,
			}
			switch composition.Credential {
			case backupresolve.CredentialStaticKeys:
				resolved.Credential, resolved.Keys = config.CredentialKubernetesSecrets, noKeys{}
			case backupresolve.CredentialStorageKey:
				resolved.Credential, resolved.AzureAccountKey = config.CredentialAccountKeyFile, []byte("synthetic")
			case backupresolve.CredentialConnectionString:
				resolved.Credential, resolved.AzureConnectionString = config.CredentialConnectionFile,
					[]byte("AccountName=devstoreaccount1;AccountKey=synthetic;BlobEndpoint=http://azurite.storage.svc:10000/devstoreaccount1")
			case backupresolve.CredentialJSONKey:
				resolved.Credential, resolved.GoogleCredentialsJSON = config.CredentialJSONFile, []byte(`{"type":"service_account"}`)
			case backupresolve.CredentialSASToken, backupresolve.CredentialAmbient:
				// No mounted credential to stand in for.
			}
			cfg := config.ApplyResolved(base, resolved, 1, nil)
			if cfg.RepositoryError != nil {
				t.Fatal(cfg.RepositoryError)
			}
			engine, err := newEvidenceEngine(cfg, "1.2.3", inventory.Initial(barmancloud.New().Descriptor()))
			if err != nil {
				t.Fatal(err)
			}
			repository := engine.Snapshot().Identity.Repository
			if repository.DestinationFingerprint != composition.Fingerprint || repository.Provider != composition.Provider {
				t.Fatalf("published %s %s, resolver %s %s", repository.Provider, repository.DestinationFingerprint, composition.Provider, composition.Fingerprint)
			}
		})
	}
}
