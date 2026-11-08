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

package backupresolve

import (
	"slices"
	"strings"
	"testing"
)

func TestAdmit(t *testing.T) {
	store := Origin{Cluster: "orders", ObjectStore: "store"}
	inTree := Origin{Cluster: "orders"}
	keys := func(origin Origin, endpoint string) *Composition {
		return &Composition{
			Origin: origin, Credential: CredentialStaticKeys, EndpointURL: endpoint,
			AccessKeyID: &SecretKeyRef{Name: "minio", Key: "ACCESS_KEY_ID"}, SecretAccessKey: &SecretKeyRef{Name: "minio", Key: "ACCESS_SECRET_KEY"},
		}
	}
	ambient := &Composition{Origin: inTree, Credential: CredentialAmbient}
	swapped := keys(store, "https://minio:9000")
	swapped.AccessKeyID = &SecretKeyRef{Name: "minio", Key: "ACCESS_SECRET_KEY"}
	regionSwap := keys(store, "https://minio:9000")
	regionSwap.Region.SecretRef = &SecretKeyRef{Name: "minio", Key: "ACCESS_SECRET_KEY"}
	sseSwap := keys(store, "https://minio:9000")
	sseSwap.SSECustomerKey = &SecretKeyRef{Name: "minio", Key: "ACCESS_SECRET_KEY"}
	sseKey := keys(inTree, "https://minio:9000")
	sseKey.SSECustomerKey = &SecretKeyRef{Name: "sse-c", Key: "key.b64"}
	listed := Policy{AllowedSecrets: []string{"minio"}}
	pgbr := func(endpoint string) *Composition {
		composition := keys(inTree, endpoint)
		composition.Format, composition.Provider = FormatPgBackRest, "s3"
		return composition
	}
	pinned := func(pin string) Policy {
		return Policy{AllowedSecrets: []string{"minio"}, AllowInsecureEndpoint: true, PgBackRestEndpoint: pin}
	}
	azure := func(endpoint, account string, credential Credential) *Composition {
		return &Composition{
			Origin: inTree, Format: FormatPgBackRest, Provider: "azure", Credential: credential,
			EndpointURL: endpoint, AzureAccount: account, AzureSecret: &SecretKeyRef{Name: "minio", Key: "k"},
		}
	}
	for _, test := range []struct {
		name        string
		composition *Composition
		policy      Policy
		notAllowed  []string
		refusal     string
	}{
		{name: "listed", composition: keys(inTree, "https://minio:9000"), policy: listed},
		{name: "not listed", composition: keys(inTree, ""), policy: Policy{AllowedSecrets: []string{"other"}}, notAllowed: []string{"minio"}},
		{name: "plugin ObjectStore, unlisted, http", composition: keys(store, "http://garage:3900"), policy: Policy{Unlisted: true}},
		{name: "in-tree, unlisted", composition: keys(inTree, ""), policy: Policy{Unlisted: true}, refusal: "spec.allowedSecrets"},
		{
			name: "in-tree, unlisted, no Secret", composition: &Composition{Origin: inTree, Credential: CredentialAmbient},
			policy: Policy{Unlisted: true, WorkloadIdentity: true},
		},
		{name: "http", composition: keys(inTree, "http://garage:3900"), policy: listed, refusal: "allowInsecureEndpoint"},
		{name: "HTTP upper case is still http", composition: keys(inTree, "HTTP://garage:3900"), policy: listed, refusal: "allowInsecureEndpoint"},
		{name: "http admitted", composition: keys(inTree, "http://garage:3900"), policy: Policy{AllowedSecrets: []string{"minio"}, AllowInsecureEndpoint: true}},
		{name: "Azure emulator over http", composition: &Composition{
			Origin: inTree, Credential: CredentialConnectionString,
			EndpointURL: "http://azurite:10000", AzureSecret: &SecretKeyRef{Name: "minio", Key: "c"},
		}, policy: listed, refusal: "allowInsecureEndpoint"},
		{name: "ambient without opt-in", composition: ambient, policy: listed, refusal: "workloadIdentity"},
		{name: "ambient with opt-in", composition: ambient, policy: Policy{WorkloadIdentity: true}},
		{name: "access key names the secret key", composition: swapped, policy: Policy{Unlisted: true}, refusal: "for both accessKeyId and secretAccessKey"},
		{name: "region names the secret key", composition: regionSwap, policy: listed, refusal: "for both secretAccessKey and region"},
		{name: "Azure key and endpoint CA on one Secret key", composition: &Composition{
			Origin: inTree, Credential: CredentialStorageKey, AzureSecret: &SecretKeyRef{Name: "minio", Key: "k"}, EndpointCA: &SecretKeyRef{Name: "minio", Key: "k"},
		}, policy: listed, refusal: "for both endpointCA and azureCredentials"},
		{name: "SSE-C key names the secret key", composition: sseSwap, policy: listed, refusal: "for both secretAccessKey and sseCustomerKey"},
		{name: "SSE-C key outside the allow-list", composition: sseKey, policy: listed, notAllowed: []string{"sse-c"}},
		{name: "SSE-C key listed", composition: sseKey, policy: Policy{AllowedSecrets: []string{"minio", "sse-c"}}},
		{name: "SSE-C key, in-tree, unlisted", composition: sseKey, policy: Policy{Unlisted: true}, refusal: "spec.allowedSecrets"},
		// The pin keeps the endpoint with the agent's author.
		{name: "pin matches", composition: pgbr("https://minio.storage.svc:9000"), policy: pinned("minio.storage.svc:9000")},
		{name: "pin matches without case", composition: pgbr("https://MinIO.storage.svc:9000"), policy: pinned("minio.STORAGE.svc:9000")},
		{name: "pin without a port is the default port", composition: pgbr("https://s3.fr-par.scw.cloud"), policy: pinned("s3.fr-par.scw.cloud")},
		{name: "pin without a port, http", composition: pgbr("http://garage"), policy: pinned("garage")},
		{name: "pin without a port, explicit 443", composition: pgbr("https://minio:443"), policy: pinned("minio")},
		{name: "pin refuses another host", composition: pgbr("https://attacker.example:9000"), policy: pinned("minio.storage.svc:9000"), refusal: "is not the pinned endpoint minio.storage.svc:9000"},
		{name: "pin refuses another port", composition: pgbr("https://minio.storage.svc:9001"), policy: pinned("minio.storage.svc:9000"), refusal: "is not the pinned endpoint"},
		{name: "pin refuses a port when it names none", composition: pgbr("https://minio:9000"), policy: pinned("minio"), refusal: "is not the pinned endpoint"},
		{name: "pin refuses AWS's default endpoint", composition: pgbr(""), policy: pinned("minio:9000"), refusal: "AWS's default"},
		{name: "pin refuses a suffix match", composition: pgbr("https://minio.storage.svc.attacker.example"), policy: pinned("minio.storage.svc"), refusal: "is not the pinned endpoint"},
		{name: "pin IPv6", composition: pgbr("https://[fd00::1]:9000"), policy: pinned("[FD00::1]:9000")},
		{name: "invalid pin", composition: pgbr("https://minio:9000"), policy: pinned("https://minio:9000"), refusal: "not a host[:port]"},
		{name: "pin ignores barman-cloud", composition: keys(inTree, "https://minio:9000"), policy: pinned("other:1")},
		{name: "pin applies to Azure host-style", composition: azure("", "stssample", CredentialSASToken), policy: pinned("stssample.blob.core.windows.net")},
		{name: "pin refuses another Azure account", composition: azure("", "attacker", CredentialStorageKey), policy: pinned("stssample.blob.core.windows.net"), refusal: "attacker.blob.core.windows.net is not the pinned endpoint"},
		{name: "Azure key on the public cloud", composition: azure("", "stssample", CredentialStorageKey), policy: listed},
		{name: "Azure SAS on a national cloud", composition: azure("https://stssample.blob.core.usgovcloudapi.net", "stssample", CredentialSASToken), policy: listed},
		{name: "Azure key off the cloud", composition: azure("https://attacker.example:10000", "stssample", CredentialStorageKey), policy: listed, refusal: "to attacker.example:10000, which is not an Azure Blob host"},
		{name: "Azure SAS off the cloud", composition: azure("https://azurite:10000", "stssample", CredentialSASToken), policy: listed, refusal: "not an Azure Blob host"},
		{name: "Azure key off the cloud, pinned", composition: azure("https://azurite.storage.svc:10000", "stssample", CredentialStorageKey), policy: pinned("azurite.storage.svc:10000")},
		{name: "Azure identity off the cloud", composition: azure("https://azurite:10000", "stssample", CredentialAmbient), policy: Policy{AllowedSecrets: []string{"minio"}, WorkloadIdentity: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			notAllowed, whyNot := test.composition.Admit(test.policy)
			if !slices.Equal(notAllowed, test.notAllowed) {
				t.Fatalf("notAllowed = %v, want %v", notAllowed, test.notAllowed)
			}
			switch {
			case test.refusal == "" && whyNot != nil:
				t.Fatalf("refused: %s", whyNot.Message)
			case test.refusal != "" && (whyNot == nil || !strings.Contains(whyNot.Message, test.refusal)):
				t.Fatalf("problem = %+v, want %q", whyNot, test.refusal)
			}
		})
	}
}

func TestParseEndpointPin(t *testing.T) {
	for pin, want := range map[string][2]string{
		"minio.storage.svc:9000": {"minio.storage.svc", "9000"},
		"S3.Example.com":         {"s3.example.com", ""},
		"10.0.0.1:443":           {"10.0.0.1", "443"},
		"[fd00::1]:9000":         {"fd00::1", "9000"},
		"[fd00::1]":              {"fd00::1", ""},
	} {
		host, port, ok := ParseEndpointPin(pin)
		if !ok || host != want[0] || port != want[1] {
			t.Errorf("ParseEndpointPin(%q) = %q, %q, %v; want %v", pin, host, port, ok, want)
		}
	}
	for _, pin := range []string{
		"", "https://minio", "minio/", "minio:0", "minio:65536", "minio:123456", "minio:", "user@minio", "minio?x", "-minio",
		"minio..svc", "[not-ip]:1", "fd00::1", "mi nio", "minio%2f", strings.Repeat("a", 262), "minio:9000#",
	} {
		if _, _, ok := ParseEndpointPin(pin); ok {
			t.Errorf("ParseEndpointPin(%q) accepted", pin)
		}
	}
}
