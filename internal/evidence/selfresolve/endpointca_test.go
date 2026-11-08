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

package selfresolve

import (
	"bytes"
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestResolveAzureEndpointCA: Azurite over TLS. The CA Secret is
// allow-listed like every other, and only the key endpointCA names is read
// (the same Secret may hold a private key: it never leaves the API read).
func TestResolveAzureEndpointCA(t *testing.T) {
	store := testObjectStore("unused")
	store.Object["spec"] = map[string]any{"configuration": map[string]any{
		"destinationPath":  "https://azurite.storage.svc:10000/devstoreaccount1/backups/team-a",
		"endpointCA":       map[string]any{"name": "azurite-tls", "key": "ca.crt"},
		"azureCredentials": map[string]any{"connectionString": map[string]any{"name": "azurite", "key": "AZURE_CONNECTION_STRING"}},
	}}
	connection := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "azurite", Namespace: namespace},
		Data:       map[string][]byte{"AZURE_CONNECTION_STRING": []byte("BlobEndpoint=https://azurite.storage.svc:10000/devstoreaccount1;SharedAccessSignature=sig=synthetic")},
	}
	tlsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "azurite-tls", Namespace: namespace},
		Data:       map[string][]byte{"ca.crt": []byte(testCA), "tls.key": []byte("synthetic-canary")},
	}
	reads := 0
	outcome := newResolver(t, reader(t, store, false, &reads, connection, tlsSecret), "azurite", "azurite-tls").Resolve(context.Background())
	if outcome.Failure != nil || outcome.Repository.Provider != "azure" || outcome.Repository.EndpointURL != "https://azurite.storage.svc:10000" ||
		string(outcome.Repository.EndpointCABundle) != testCA || len(outcome.secrets) != 2 || reads != 2 {
		t.Fatalf("outcome = %+v (%d Secret reads)", outcome, reads)
	}
	if bytes.Contains(outcome.Repository.EndpointCABundle, []byte("canary")) || bytes.Contains(outcome.Repository.AzureConnectionString, []byte("canary")) {
		t.Fatal("a key other than the named one was read")
	}

	reads = 0
	refused := newResolver(t, reader(t, store, false, &reads, connection, tlsSecret), "azurite").Resolve(context.Background())
	var notAllowed *NotAllowedError
	if !errors.As(refused.Failure, &notAllowed) || len(notAllowed.Secrets) != 1 || notAllowed.Secrets[0] != "azurite-tls" || reads != 0 {
		t.Fatalf("allow-list: %v (%d Secret reads)", refused.Failure, reads)
	}
}
