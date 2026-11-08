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
	"context"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	barmanapi "github.com/cloudnative-pg/barman-cloud/pkg/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// objectStoreConfiguration decodes a spec.configuration exactly as the
// reconciler does from the unstructured ObjectStore.
func objectStoreConfiguration(t *testing.T, raw map[string]any) *barmanapi.BarmanObjectStoreConfiguration {
	t.Helper()
	configuration := &barmanapi.BarmanObjectStoreConfiguration{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, configuration); err != nil {
		t.Fatal(err)
	}
	return configuration
}

func s3Configuration(overrides map[string]any) map[string]any {
	raw := map[string]any{
		"destinationPath": "s3://backups/team-a",
		"endpointURL":     "https://minio.storage.svc:9000",
		"s3Credentials": map[string]any{
			"accessKeyId":     map[string]any{"name": "minio", "key": "ACCESS_KEY_ID"},
			"secretAccessKey": map[string]any{"name": "minio", "key": "ACCESS_SECRET_KEY"},
		},
	}
	for key, value := range overrides {
		if value == nil {
			delete(raw, key)
			continue
		}
		raw[key] = value
	}
	return raw
}

func TestComposeFromConfiguration(t *testing.T) {
	resolved, whyNot := Compose(Origin{Cluster: "orders", ObjectStore: "store"}, "orders", objectStoreConfiguration(t, s3Configuration(nil)))
	if whyNot != nil {
		t.Fatal(whyNot.Message)
	}
	want, err := evidencev1alpha1.FingerprintS3(evidencev1alpha1.S3FingerprintInput{
		Endpoint: "https://minio.storage.svc:9000", Bucket: "backups", Prefix: "team-a",
		Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders",
	})
	if err != nil || resolved.Fingerprint != want {
		t.Fatalf("fingerprint = %q, want %q (%v)", resolved.Fingerprint, want, err)
	}
	if *resolved.AccessKeyID != (SecretKeyRef{Name: "minio", Key: "ACCESS_KEY_ID"}) || resolved.Region.SecretRef != nil || resolved.Region.Value != "" {
		t.Fatalf("composition = %+v", resolved)
	}
	if got := resolved.ReferencedSecrets(); len(got) != 1 || got[0] != "minio" {
		t.Fatalf("referenced Secrets = %v", got)
	}

	aws, whyNot := Compose(Origin{Cluster: "orders", ObjectStore: "store"}, "orders", objectStoreConfiguration(t, s3Configuration(map[string]any{
		"endpointURL": "https://s3.eu-west-3.amazonaws.com",
	})))
	if whyNot != nil || aws.Region.Value != "eu-west-3" {
		t.Fatalf("AWS endpoint region = %+v %v", aws, whyNot)
	}

	// SSE-C (barman-cloud API v0.7.0, s3Credentials.sseCustomerKey): the
	// key is one more Secret reference beside the key pair, and is listed
	// for the allow-list like every other one.
	sseC, whyNot := Compose(Origin{Cluster: "orders", ObjectStore: "store"}, "orders", objectStoreConfiguration(t, s3Configuration(map[string]any{
		"s3Credentials": map[string]any{
			"accessKeyId":     map[string]any{"name": "minio", "key": "ACCESS_KEY_ID"},
			"secretAccessKey": map[string]any{"name": "minio", "key": "ACCESS_SECRET_KEY"},
			"sseCustomerKey":  map[string]any{"name": "sse-c", "key": "key.b64"},
		},
	})))
	if whyNot != nil {
		t.Fatalf("an SSE-C repository was refused: %s", whyNot.Message)
	}
	if sseC.SSECustomerKey == nil || *sseC.SSECustomerKey != (SecretKeyRef{Name: "sse-c", Key: "key.b64"}) || sseC.Credential != CredentialStaticKeys || sseC.Fingerprint != want {
		t.Fatalf("SSE-C composition = %+v", sseC)
	}
	if got := sseC.ReferencedSecrets(); strings.Join(got, ",") != "minio,sse-c" {
		t.Fatalf("referenced Secrets = %v, want the SSE-C Secret too", got)
	}

	cases := []struct {
		name      string
		overrides map[string]any
		reason    pgtoolboxv1alpha1.Reason
		leak      string
	}{
		{"azure", map[string]any{"azureCredentials": map[string]any{"inheritFromAzureAD": true}}, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, ""},
		{"no s3 credentials", map[string]any{"s3Credentials": nil}, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, ""},
		{"secret key missing", map[string]any{"s3Credentials": map[string]any{"accessKeyId": map[string]any{"name": "m", "key": "k"}}}, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, ""},
		{"credential in the destination", map[string]any{"destinationPath": "s3://AKIA:canary@backups/x"}, pgtoolboxv1alpha1.ReasonDestinationInvalid, "canary"},
		{"presigned query", map[string]any{"destinationPath": "s3://backups/x?X-Amz-Signature=canary"}, pgtoolboxv1alpha1.ReasonDestinationInvalid, "canary"},
		{"not s3", map[string]any{"destinationPath": "https://backups/x"}, pgtoolboxv1alpha1.ReasonDestinationInvalid, ""},
		{"endpoint with a path", map[string]any{"endpointURL": "https://minio/path"}, pgtoolboxv1alpha1.ReasonDestinationInvalid, ""},
		{"AWS without a region", map[string]any{"endpointURL": nil}, pgtoolboxv1alpha1.ReasonRegionRequired, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, whyNot := Compose(Origin{Cluster: "orders", ObjectStore: "store"}, "orders", objectStoreConfiguration(t, s3Configuration(test.overrides)))
			if whyNot == nil || whyNot.Reason != test.reason {
				t.Fatalf("problem = %+v, want %s", whyNot, test.reason)
			}
			if test.leak != "" && strings.Contains(whyNot.Message, test.leak) {
				t.Fatal("the condition message discloses the rejected value")
			}
		})
	}
	if _, whyNot := Compose(Origin{Cluster: "orders", ObjectStore: "store"}, "a/b", objectStoreConfiguration(t, s3Configuration(nil))); whyNot == nil {
		t.Fatal("a server name with a slash was fingerprinted")
	}
}

func TestBarmanPluginParameters(t *testing.T) {
	enabled, disabled := true, false
	cluster := func(plugins ...cnpgv1.PluginConfiguration) *cnpgv1.Cluster {
		return &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{Plugins: plugins}}
	}
	parameters := map[string]string{"barmanObjectName": "store", "serverName": "orders-v2"}
	if got := BarmanPluginParameters(cluster(cnpgv1.PluginConfiguration{Name: BarmanPluginName, Parameters: parameters})); got["serverName"] != "orders-v2" {
		t.Fatalf("parameters = %v", got)
	}
	if got := BarmanPluginParameters(cluster(cnpgv1.PluginConfiguration{Name: BarmanPluginName, Enabled: &enabled, Parameters: parameters})); got == nil {
		t.Fatal("an explicitly enabled plugin was ignored")
	}
	if got := BarmanPluginParameters(cluster(cnpgv1.PluginConfiguration{Name: BarmanPluginName, Enabled: &disabled, Parameters: parameters})); got != nil {
		t.Fatal("a disabled plugin was used")
	}
	if got := BarmanPluginParameters(cluster(cnpgv1.PluginConfiguration{Name: "other", Parameters: parameters})); got != nil {
		t.Fatal("another plugin was used")
	}
}

func TestLocatePicksPluginOrInTree(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	inTree := func(serverName string) *cnpgv1.BackupConfiguration {
		return &cnpgv1.BackupConfiguration{BarmanObjectStore: &barmanapi.BarmanObjectStoreConfiguration{
			DestinationPath: "s3://backups/in-tree", ServerName: serverName,
		}}
	}
	plugin := cnpgv1.PluginConfiguration{Name: BarmanPluginName, Parameters: map[string]string{BarmanObjectParameter: "store", BarmanServerParameter: "orders-plugin"}}
	disabled := false
	disabledPlugin := plugin
	disabledPlugin.Enabled = &disabled
	cases := []struct {
		name        string
		spec        cnpgv1.ClusterSpec
		request     Request
		objectStore string
		inTree      bool
		server      string
		pgbackrest  *PgBackRestLocation
		reason      pgtoolboxv1alpha1.Reason
		message     string
	}{
		{name: "plugin", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{plugin}}, objectStore: "store", server: "orders-plugin"},
		{name: "in-tree, Cluster name", spec: cnpgv1.ClusterSpec{Backup: inTree("")}, inTree: true, server: "orders"},
		{name: "in-tree, serverName", spec: cnpgv1.ClusterSpec{Backup: inTree("orders-v2")}, inTree: true, server: "orders-v2"},
		{name: "in-tree, request overrides the server", spec: cnpgv1.ClusterSpec{Backup: inTree("orders-v2")}, request: Request{ServerName: "explicit"}, inTree: true, server: "explicit"},
		{name: "plugin wins over in-tree", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{plugin}, Backup: inTree("x")}, objectStore: "store", server: "orders-plugin"},
		{name: "disabled plugin falls back to in-tree", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{disabledPlugin}, Backup: inTree("")}, inTree: true, server: "orders"},
		{name: "explicit ObjectStore wins over in-tree", spec: cnpgv1.ClusterSpec{Backup: inTree("")}, request: Request{ObjectStoreName: "named"}, objectStore: "named", server: "orders"},
		{name: "backup without barmanObjectStore", spec: cnpgv1.ClusterSpec{Backup: &cnpgv1.BackupConfiguration{}}, reason: pgtoolboxv1alpha1.ReasonBarmanPluginNotConfigured},
		{name: "nothing", reason: pgtoolboxv1alpha1.ReasonBarmanPluginNotConfigured},
		{name: "Dalibo pgBackRest plugin", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: "pgbackrest.dalibo.com", Parameters: map[string]string{"stanzaRef": "orders-stanza"},
		}}}, pgbackrest: &PgBackRestLocation{Plugin: "pgbackrest.dalibo.com", Kind: "Stanza", ObjectName: "orders-stanza"}},
		{name: "Opera pgBackRest plugin", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: "pgbackrest.cnpg.opera.com", Parameters: map[string]string{"pgbackrestObjectName": "orders-archive", "stanza": "main"},
		}}}, pgbackrest: &PgBackRestLocation{Plugin: "pgbackrest.cnpg.opera.com", Kind: "Archive", ObjectName: "orders-archive", StanzaParameter: "main"}},
		{name: "Dalibo wins over Opera", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{
			{Name: "pgbackrest.cnpg.opera.com", Parameters: map[string]string{"pgbackrestObjectName": "a"}},
			{Name: "pgbackrest.dalibo.com", Parameters: map[string]string{"stanzaRef": "s"}},
		}}, pgbackrest: &PgBackRestLocation{Plugin: "pgbackrest.dalibo.com", Kind: "Stanza", ObjectName: "s"}},
		{name: "Dalibo pgBackRest plugin without stanzaRef", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: "pgbackrest.dalibo.com", Parameters: map[string]string{"pluginConfigRef": "x"},
		}}}, reason: pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, message: "without the stanzaRef parameter"},
		{name: "Opera pgBackRest plugin without pgbackrestObjectName", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: "pgbackrest.cnpg.opera.com",
		}}}, reason: pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, message: "without the pgbackrestObjectName parameter"},
		{name: "pgBackRest with a Barman server name", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: "pgbackrest.dalibo.com", Parameters: map[string]string{"stanzaRef": "s"},
		}}}, request: Request{ServerName: "orders"}, reason: pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, message: "remove spec.serverName"},
		{
			name: "pgBackRest settings on a barman-cloud Cluster", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{plugin}},
			request: Request{PgBackRest: &PgBackRestSettings{Repository: 2}}, reason: pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, message: "is read from ObjectStore store",
		},
		{name: "disabled pgBackRest plugin", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: "pgbackrest.dalibo.com", Enabled: &disabled,
		}}}, reason: pgtoolboxv1alpha1.ReasonBarmanPluginNotConfigured},
		{name: "barman-cloud wins over pgBackRest", spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{
			{Name: "pgbackrest.dalibo.com"}, plugin,
		}}, objectStore: "store", server: "orders-plugin"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cluster := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a", UID: "uid-1"}, Spec: test.spec}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
			request := test.request
			request.Namespace, request.ClusterName = "team-a", "orders"
			located, whyNot, err := Locate(context.Background(), reader, request)
			if err != nil {
				t.Fatal(err)
			}
			if test.reason != "" {
				if whyNot == nil || whyNot.Reason != test.reason {
					t.Fatalf("problem = %+v, want %s", whyNot, test.reason)
				}
				if !strings.Contains(whyNot.Message, test.message) {
					t.Fatalf("message %q does not say %q", whyNot.Message, test.message)
				}
				return
			}
			if whyNot != nil {
				t.Fatalf("problem = %+v", whyNot)
			}
			if test.pgbackrest != nil {
				if located.PgBackRest == nil || *located.PgBackRest != *test.pgbackrest || located.ServerName != "" || located.ObjectStoreName != "" {
					t.Fatalf("located = %+v (%+v)", located, located.PgBackRest)
				}
				if got, want := located.Origin().String(), test.pgbackrest.Kind+" "+test.pgbackrest.ObjectName; got != want {
					t.Fatalf("origin = %q, want %q", got, want)
				}
				if _, whyNot, _ := Configuration(context.Background(), reader, located); whyNot == nil {
					t.Fatal("a pgBackRest location yielded a Barman configuration")
				}
				return
			}
			if located.ObjectStoreName != test.objectStore || (located.InTree != nil) != test.inTree || located.ServerName != test.server {
				t.Fatalf("located = %+v", located)
			}
			if test.inTree {
				configuration, whyNot, err := Configuration(context.Background(), reader, located)
				if err != nil || whyNot != nil || configuration.DestinationPath != "s3://backups/in-tree" {
					t.Fatalf("in-tree configuration = %+v %+v %v", configuration, whyNot, err)
				}
				if got := located.Origin().String(); got != "Cluster orders spec.backup.barmanObjectStore" {
					t.Fatalf("origin = %q", got)
				}
			}
		})
	}
}

func TestComposeAzureAndGCS(t *testing.T) {
	secret := func(name string) map[string]any { return map[string]any{"name": name, "key": "value"} }
	origin := Origin{Cluster: "orders", ObjectStore: "store"}
	accepted := []struct {
		name        string
		raw         map[string]any
		provider    string
		destination string
		endpoint    string
		account     string
		credential  Credential
		secrets     []string
	}{
		{
			"Azure storage key",
			map[string]any{
				"destinationPath":  "https://orders.blob.core.windows.net/backups/team-a",
				"azureCredentials": map[string]any{"storageAccount": secret("account"), "storageKey": secret("key")},
			},
			evidencev1alpha1.ProviderAzure, "azure://backups/team-a", "", "orders", CredentialStorageKey,
			[]string{"key"},
		},
		{
			"Azure SAS token",
			map[string]any{
				"destinationPath":  "https://orders.blob.core.windows.net/backups",
				"azureCredentials": map[string]any{"storageAccount": secret("account"), "storageSasToken": secret("sas")},
			},
			evidencev1alpha1.ProviderAzure, "azure://backups", "", "orders", CredentialSASToken,
			[]string{"sas"},
		},
		{
			"Azure connection string",
			map[string]any{
				"destinationPath":  "https://orders.blob.core.windows.net/backups/a%20b",
				"azureCredentials": map[string]any{"connectionString": secret("conn")},
			},
			evidencev1alpha1.ProviderAzure, "azure://backups/a%20b", "", "orders", CredentialConnectionString,
			[]string{"conn"},
		},
		{
			"Azure emulator",
			map[string]any{
				"destinationPath":  "http://azurite:10000/devstoreaccount1/backups/x",
				"azureCredentials": map[string]any{"connectionString": secret("conn")},
			},
			evidencev1alpha1.ProviderAzure, "azure://backups/x", "http://azurite:10000", "devstoreaccount1", CredentialConnectionString,
			[]string{"conn"},
		},
		{
			// Azurite over TLS: barman-cloud trusts endpointCA for Azure
			// (REQUESTS_CA_BUNDLE); the CA Secret is read and allow-listed.
			"Azure emulator over TLS with an endpoint CA",
			map[string]any{
				"destinationPath":  "https://azurite:10000/devstoreaccount1/backups/x",
				"endpointCA":       secret("ca"),
				"azureCredentials": map[string]any{"connectionString": secret("conn")},
			},
			evidencev1alpha1.ProviderAzure, "azure://backups/x", "https://azurite:10000", "devstoreaccount1", CredentialConnectionString,
			[]string{"ca", "conn"},
		},
		{
			"S3 IAM role",
			map[string]any{
				"destinationPath": "s3://backups/team-a", "endpointURL": "https://s3.eu-west-3.amazonaws.com",
				"s3Credentials": map[string]any{"inheritFromIAMRole": true},
			},
			evidencev1alpha1.ProviderS3, "s3://backups/team-a", "https://s3.eu-west-3.amazonaws.com", "", CredentialAmbient, nil,
		},
		{
			"S3 IAM role, region from the Pod",
			map[string]any{
				"destinationPath": "s3://backups/team-a",
				"s3Credentials":   map[string]any{"inheritFromIAMRole": true},
			},
			evidencev1alpha1.ProviderS3, "s3://backups/team-a", "", "", CredentialAmbient, nil,
		},
		{
			"S3 IAM role, region Secret",
			map[string]any{
				"destinationPath": "s3://backups/team-a",
				"s3Credentials":   map[string]any{"inheritFromIAMRole": true, "region": secret("region")},
			},
			evidencev1alpha1.ProviderS3, "s3://backups/team-a", "", "", CredentialAmbient,
			[]string{"region"},
		},
		{
			"Azure AD",
			map[string]any{
				"destinationPath":  "https://orders.blob.core.windows.net/backups/team-a",
				"azureCredentials": map[string]any{"inheritFromAzureAD": true},
			},
			evidencev1alpha1.ProviderAzure, "azure://backups/team-a", "", "orders", CredentialAmbient, nil,
		},
		{
			"Azure default credentials",
			map[string]any{
				"destinationPath":  "https://orders.blob.core.windows.net/backups/team-a",
				"azureCredentials": map[string]any{"useDefaultAzureCredentials": true},
			},
			evidencev1alpha1.ProviderAzure, "azure://backups/team-a", "", "orders", CredentialAmbient, nil,
		},
		{
			"S3 customer key (SSE-C) with an IAM role",
			map[string]any{
				"destinationPath": "s3://backups/team-a", "endpointURL": "https://objects.example.net",
				"s3Credentials": map[string]any{"inheritFromIAMRole": true, "sseCustomerKey": secret("sse")},
			},
			evidencev1alpha1.ProviderS3, "s3://backups/team-a", "https://objects.example.net", "", CredentialAmbient,
			[]string{"sse"},
		},
		{
			"GCS GKE environment",
			map[string]any{
				"destinationPath":   "gs://backups/team-a",
				"googleCredentials": map[string]any{"gkeEnvironment": true},
			},
			evidencev1alpha1.ProviderGCS, "gs://backups/team-a", "", "", CredentialAmbient, nil,
		},
		{
			"GCS JSON key",
			map[string]any{
				"destinationPath":   "gs://backups/team-a",
				"googleCredentials": map[string]any{"applicationCredentials": secret("gcs")},
			},
			evidencev1alpha1.ProviderGCS, "gs://backups/team-a", "", "", CredentialJSONKey,
			[]string{"gcs"},
		},
		{
			// barman-cloud uses no CA (and no endpointURL) for GCS: the
			// repository is read where barman-cloud writes it, and the CA
			// Secret is not referenced.
			"GCS JSON key with an endpoint CA, not used",
			map[string]any{
				"destinationPath": "gs://backups/team-a", "endpointCA": secret("ca"), "endpointURL": "https://storage.example.test",
				"googleCredentials": map[string]any{"applicationCredentials": secret("gcs")},
			},
			evidencev1alpha1.ProviderGCS, "gs://backups/team-a", "", "", CredentialJSONKey,
			[]string{"gcs"},
		},
	}
	for _, test := range accepted {
		t.Run(test.name, func(t *testing.T) {
			resolved, whyNot := Compose(origin, "orders", objectStoreConfiguration(t, test.raw))
			if whyNot != nil {
				t.Fatal(whyNot.Message)
			}
			if resolved.Provider != test.provider || resolved.AgentDestination != test.destination || resolved.EndpointURL != test.endpoint ||
				resolved.AzureAccount != test.account || resolved.Credential != test.credential || resolved.DestinationPath != test.raw["destinationPath"] {
				t.Fatalf("composition = %+v", resolved)
			}
			if got := resolved.ReferencedSecrets(); strings.Join(got, ",") != strings.Join(test.secrets, ",") {
				t.Fatalf("referenced Secrets = %v, want %v (the storageAccount Secret is not read)", got, test.secrets)
			}
			bucket := "backups"
			prefix := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(test.destination, "azure://backups"), "gs://backups"), "s3://backups")
			if test.name == "Azure connection string" {
				prefix = "/a b"
			}
			want, err := evidencev1alpha1.FingerprintRepository(evidencev1alpha1.RepositoryFingerprintInput{
				Provider: test.provider, Endpoint: test.endpoint, Account: test.account, Bucket: bucket, Prefix: prefix,
				Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders",
			})
			if err != nil || resolved.Fingerprint != want {
				t.Fatalf("fingerprint = %q, want %q (%v)", resolved.Fingerprint, want, err)
			}
		})
	}

	refused := []struct {
		name   string
		raw    map[string]any
		reason pgtoolboxv1alpha1.Reason
		leak   string
	}{
		{"no provider", map[string]any{"destinationPath": "s3://backups"}, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, ""},
		{"two providers", map[string]any{
			"destinationPath": "gs://backups", "googleCredentials": map[string]any{"applicationCredentials": secret("g")},
			"azureCredentials": map[string]any{"connectionString": secret("c")},
		}, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, ""},
		{"Azure SAS in the destination", map[string]any{
			"destinationPath":  "https://orders.blob.core.windows.net/backups?sig=canary",
			"azureCredentials": map[string]any{"connectionString": secret("c")},
		}, pgtoolboxv1alpha1.ReasonDestinationInvalid, "canary"},
		{"Azure with an S3 destination", map[string]any{
			"destinationPath":  "s3://backups",
			"azureCredentials": map[string]any{"connectionString": secret("c")},
		}, pgtoolboxv1alpha1.ReasonDestinationInvalid, ""},
		{"Azure emulator with a key", map[string]any{
			"destinationPath":  "http://azurite:10000/devstoreaccount1/backups",
			"azureCredentials": map[string]any{"storageAccount": secret("a"), "storageKey": secret("k")},
		}, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, ""},
		{"Azure emulator with Azure AD", map[string]any{
			"destinationPath":  "http://azurite:10000/devstoreaccount1/backups",
			"azureCredentials": map[string]any{"inheritFromAzureAD": true},
		}, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, ""},
		{"Azure without a credential", map[string]any{
			"destinationPath":  "https://orders.blob.core.windows.net/backups",
			"azureCredentials": map[string]any{"storageAccount": secret("a")},
		}, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, ""},
		{"GCS userinfo", map[string]any{
			"destinationPath":   "gs://canary@backups/x",
			"googleCredentials": map[string]any{"applicationCredentials": secret("g")},
		}, pgtoolboxv1alpha1.ReasonDestinationInvalid, "canary"},
		{"GCS without a credential", map[string]any{
			"destinationPath":   "gs://backups",
			"googleCredentials": map[string]any{},
		}, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, ""},
	}
	for _, test := range refused {
		t.Run(test.name, func(t *testing.T) {
			_, whyNot := Compose(origin, "orders", objectStoreConfiguration(t, test.raw))
			if whyNot == nil || whyNot.Reason != test.reason {
				t.Fatalf("problem = %+v, want %s", whyNot, test.reason)
			}
			if test.leak != "" && strings.Contains(whyNot.Message, test.leak) {
				t.Fatal("the message discloses the rejected value")
			}
		})
	}
}
