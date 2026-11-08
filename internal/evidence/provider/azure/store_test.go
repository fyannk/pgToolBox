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

package azure

import (
	"context"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
)

func TestSafeErrorRedactsAzureSDKDetails(t *testing.T) {
	t.Parallel()
	canary := "sas-sig-canary"
	for status, want := range map[int]fault.Category{401: fault.Authentication, 403: fault.Authorization, 404: fault.NotFound, 429: fault.Throttled, 500: fault.Unavailable} {
		err := safeError(context.Background(), &azcore.ResponseError{StatusCode: status, ErrorCode: canary})
		if fault.Categorize(err) != want || strings.Contains(err.Error(), canary) {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}

func TestServiceURL(t *testing.T) {
	for _, test := range []struct {
		options Options
		want    string
	}{
		{Options{Account: "orders"}, "https://orders.blob.core.windows.net/"},
		{Options{Account: "devstoreaccount1", Endpoint: "http://azurite:10000"}, "http://azurite:10000/devstoreaccount1/"},
		{Options{Account: "devstoreaccount1", Endpoint: "http://azurite:10000/"}, "http://azurite:10000/devstoreaccount1/"},
		{Options{Account: "orders", Endpoint: "https://orders.blob.core.chinacloudapi.cn"}, "https://orders.blob.core.chinacloudapi.cn/"},
	} {
		if got := test.options.serviceURL(); got != test.want {
			t.Errorf("serviceURL(%+v) = %q, want %q", test.options, got, test.want)
		}
	}
}

// TestAmbientCredentialIsWorkloadIdentityOnly: the agent's keyless Azure
// credential needs what the workload identity webhook injects; without it
// there is no credential, never the node's managed identity.
func TestAmbientCredentialIsWorkloadIdentityOnly(t *testing.T) {
	t.Setenv("AZURE_CLIENT_ID", "")
	t.Setenv("AZURE_TENANT_ID", "")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "")
	if _, err := ambientCredential(true); err == nil {
		t.Fatal("a workload identity credential was built without the webhook's variables")
	}
	t.Setenv("AZURE_CLIENT_ID", "00000000-0000-0000-0000-000000000001")
	t.Setenv("AZURE_TENANT_ID", "00000000-0000-0000-0000-000000000002")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "/var/run/secrets/azure/tokens/azure-identity-token")
	if _, err := ambientCredential(true); err != nil {
		t.Fatal(err)
	}
}
