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
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/pkg/discovery"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// canary is a value no message may repeat: the resolver reads names, and
// an override's value is never quoted.
const canary = "canary-value-7f3a"

func secretRef(name, key string) map[string]any { return map[string]any{"name": name, "key": key} }

func daliboS3(overrides map[string]any) map[string]any {
	repository := map[string]any{
		"bucket": "bucket-01", "endpoint": "https://172.18.0.2:9000", "region": "fr-par", "repoPath": "/repo-01", "uriStyle": "path",
		"verifyTLS": false,
		"secretRef": map[string]any{"accessKeyId": secretRef("minio", "ACCESS_KEY_ID"), "secretAccessKey": secretRef("minio", "ACCESS_SECRET_KEY")},
	}
	for key, value := range overrides {
		if value == nil {
			delete(repository, key)
			continue
		}
		repository[key] = value
	}
	return repository
}

func daliboAzure(overrides map[string]any) map[string]any {
	repository := map[string]any{
		"account": "stssample", "container": "backups", "endpoint": "blob.core.windows.net", "repoPath": "/cluster-sample", "keyType": "shared",
		"secretRef": map[string]any{"keyReference": secretRef("azure", "KEY")},
	}
	for key, value := range overrides {
		if value == nil {
			delete(repository, key)
			continue
		}
		repository[key] = value
	}
	return repository
}

func stanzaObject(configuration map[string]any) *unstructured.Unstructured {
	if _, ok := configuration["name"]; !ok {
		configuration["name"] = "main"
	}
	object := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"stanzaConfiguration": configuration}}}
	object.SetGroupVersionKind(discovery.StanzaGVK)
	object.SetNamespace("team-a")
	object.SetName("orders-stanza")
	return object
}

func operaRepository(overrides map[string]any) map[string]any {
	repository := map[string]any{
		"destinationPath": "/", "bucket": "backups", "endpointURL": "minio:9000", "disableVerifyTLS": true,
		"encryption": "aes-256-cbc", "encryptionKey": secretRef("minio", "ENCRYPTION_KEY"),
		"s3Credentials": map[string]any{
			"region": "dummy", "accessKeyId": secretRef("minio", "ACCESS_KEY_ID"), "secretAccessKey": secretRef("minio", "ACCESS_SECRET_KEY"),
		},
	}
	for key, value := range overrides {
		if value == nil {
			delete(repository, key)
			continue
		}
		repository[key] = value
	}
	return repository
}

func archiveObject(spec map[string]any) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	object.SetGroupVersionKind(discovery.ArchiveGVK)
	object.SetNamespace("team-a")
	object.SetName("orders-archive")
	return object
}

func archiveSpec(configuration map[string]any) map[string]any {
	if _, ok := configuration["repositories"]; !ok {
		configuration["repositories"] = []any{operaRepository(nil)}
	}
	return map[string]any{"configuration": configuration}
}

func daliboLocated() *Located {
	return &Located{
		Cluster:    &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a"}},
		PgBackRest: &PgBackRestLocation{Plugin: "pgbackrest.dalibo.com", Kind: PgBackRestKindStanza, ObjectName: "orders-stanza"},
	}
}

func operaLocated(stanzaParameter string) *Located {
	return &Located{
		Cluster: &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a"}},
		PgBackRest: &PgBackRestLocation{
			Plugin: "pgbackrest.cnpg.opera.com", Kind: PgBackRestKindArchive, ObjectName: "orders-archive", StanzaParameter: stanzaParameter,
		},
	}
}

// compose decodes the object as ReadPgBackRest does and composes it.
func compose(t *testing.T, located *Located, object *unstructured.Unstructured, settings *PgBackRestSettings) (*Composition, *Problem) {
	t.Helper()
	reader := fake.NewClientBuilder().WithObjects(object).Build()
	decoded, whyNot, err := ReadPgBackRest(context.Background(), reader, "team-a", located.PgBackRest)
	if err != nil || whyNot != nil {
		t.Fatalf("read = %+v, %v", whyNot, err)
	}
	return ComposePgBackRest(located, decoded, settings)
}

// expectedFingerprint is the contract's pgbackrest identity.
func expectedFingerprint(t *testing.T, input evidencev1alpha1.RepositoryFingerprintInput) string {
	t.Helper()
	input.Format, input.ScopeKind = FormatPgBackRest, evidencev1alpha1.ScopeKindStanza
	fingerprint, err := evidencev1alpha1.FingerprintRepository(input)
	if err != nil {
		t.Fatalf("the contract refuses %+v: %v", input, err)
	}
	return fingerprint
}

func TestComposePgBackRestShapes(t *testing.T) {
	ref := func(name, key string) *SecretKeyRef { return &SecretKeyRef{Name: name, Key: key} }
	keys := [2]*SecretKeyRef{ref("minio", "ACCESS_KEY_ID"), ref("minio", "ACCESS_SECRET_KEY")}
	cases := []struct {
		name     string
		located  *Located
		object   *unstructured.Unstructured
		settings *PgBackRestSettings
		want     Composition
		secrets  string
	}{
		{
			name: "Dalibo S3, the example's https origin", located: daliboLocated(),
			object: stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(nil)}}),
			want: Composition{
				Provider: "s3", AgentDestination: "s3://bucket-01/repo-01", EndpointURL: "https://172.18.0.2:9000", Region: RegionSource{Value: "fr-par"},
				Stanza: "main", Repository: 1, URIStyle: "path", Credential: CredentialStaticKeys, AccessKeyID: keys[0], SecretAccessKey: keys[1],
			},
			secrets: "minio",
		},
		{
			name: "Dalibo S3, a bare host and a cipher pass", located: daliboLocated(),
			object: stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{
				"endpoint": "s3.minio.svc.cluster.local", "region": "us-east-1", "repoPath": "/cluster-demo",
				"cipherConfig": map[string]any{"encryptionPass": secretRef("cipher", "ENCRYPTION_PASS")},
			})}}),
			want: Composition{
				Provider: "s3", AgentDestination: "s3://bucket-01/cluster-demo", EndpointURL: "https://s3.minio.svc.cluster.local", Region: RegionSource{Value: "us-east-1"},
				Stanza: "main", Repository: 1, URIStyle: "path", Credential: CredentialStaticKeys, AccessKeyID: keys[0], SecretAccessKey: keys[1],
				CipherPass: ref("cipher", "ENCRYPTION_PASS"),
			},
			secrets: "cipher,minio",
		},
		{
			name: "Dalibo, repo2 of two S3 repositories, the author's CA", located: daliboLocated(),
			object:   stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(nil), daliboS3(map[string]any{"bucket": "bucket-02", "repoPath": "/repo-02/nested/"})}}),
			settings: &PgBackRestSettings{Repository: 2, EndpointCA: ref("minio-ca", "ca.crt")},
			want: Composition{
				Provider: "s3", AgentDestination: "s3://bucket-02/repo-02/nested", EndpointURL: "https://172.18.0.2:9000", Region: RegionSource{Value: "fr-par"},
				Stanza: "main", Repository: 2, URIStyle: "path", Credential: CredentialStaticKeys, AccessKeyID: keys[0], SecretAccessKey: keys[1],
				EndpointCA: ref("minio-ca", "ca.crt"),
			},
			secrets: "minio,minio-ca",
		},
		{
			name: "Dalibo Azure, public cloud, shared key", located: daliboLocated(),
			object: stanzaObject(map[string]any{"azureRepositories": []any{daliboAzure(nil)}}),
			want: Composition{
				Provider: "azure", AgentDestination: "azure://backups/cluster-sample", AzureAccount: "stssample",
				Stanza: "main", Repository: 1, Credential: CredentialStorageKey, AzureSecret: ref("azure", "KEY"),
			},
			secrets: "azure",
		},
		{
			name: "Dalibo Azure after one S3 repository is repo2, SAS token, national cloud", located: daliboLocated(),
			object: stanzaObject(map[string]any{
				"s3Repositories":    []any{daliboS3(nil)},
				"azureRepositories": []any{daliboAzure(map[string]any{"keyType": "sas", "endpoint": "blob.core.usgovcloudapi.net", "uriStyle": "host"})},
			}),
			settings: &PgBackRestSettings{Repository: 2},
			want: Composition{
				Provider: "azure", AgentDestination: "azure://backups/cluster-sample", AzureAccount: "stssample", EndpointURL: "https://stssample.blob.core.usgovcloudapi.net",
				Stanza: "main", Repository: 2, URIStyle: "host", Credential: CredentialSASToken, AzureSecret: ref("azure", "KEY"),
			},
			secrets: "azure",
		},
		{
			name: "Dalibo Azure emulator, path style, the author's CA", located: daliboLocated(),
			object: stanzaObject(map[string]any{"azureRepositories": []any{daliboAzure(map[string]any{
				"endpoint": "azurite.storage.svc:10000", "uriStyle": "path", "repoPath": "/",
			})}}),
			settings: &PgBackRestSettings{EndpointCA: ref("azurite-ca", "ca.crt")},
			want: Composition{
				Provider: "azure", AgentDestination: "azure://backups", AzureAccount: "stssample", EndpointURL: "https://azurite.storage.svc:10000",
				Stanza: "main", Repository: 1, URIStyle: "path", Credential: CredentialStorageKey, AzureSecret: ref("azure", "KEY"),
				EndpointCA: ref("azurite-ca", "ca.crt"),
			},
			secrets: "azure,azurite-ca",
		},
		{
			name: "Dalibo Azure, managed identity", located: daliboLocated(),
			object: stanzaObject(map[string]any{"azureRepositories": []any{daliboAzure(map[string]any{"keyType": "auto", "secretRef": nil})}}),
			want: Composition{
				Provider: "azure", AgentDestination: "azure://backups/cluster-sample", AzureAccount: "stssample",
				Stanza: "main", Repository: 1, Credential: CredentialAmbient,
			},
		},
		{
			name: "Dalibo, an override that leaves the files in place", located: daliboLocated(),
			object: stanzaObject(map[string]any{
				"s3Repositories": []any{daliboS3(nil)},
				"customEnvVar":   map[string]any{"PGBACKREST_LOG_LEVEL_CONSOLE": "info", "PGBACKREST_PROCESS_MAX": "4"},
			}),
			want: Composition{
				Provider: "s3", AgentDestination: "s3://bucket-01/repo-01", EndpointURL: "https://172.18.0.2:9000", Region: RegionSource{Value: "fr-par"},
				Stanza: "main", Repository: 1, URIStyle: "path", Credential: CredentialStaticKeys, AccessKeyID: keys[0], SecretAccessKey: keys[1],
			},
			secrets: "minio",
		},
		{
			name: "Opera, the example Archive", located: operaLocated(""),
			object: archiveObject(archiveSpec(map[string]any{
				"data": map[string]any{"additionalCommandArgs": []any{"--buffer-size=5MB", "--io-timeout=60"}},
			})),
			want: Composition{
				Provider: "s3", AgentDestination: "s3://backups", EndpointURL: "https://minio:9000", Region: RegionSource{Value: "dummy"},
				Stanza: "orders", Repository: 1, Credential: CredentialStaticKeys, AccessKeyID: keys[0], SecretAccessKey: keys[1],
				CipherPass: ref("minio", "ENCRYPTION_KEY"),
			},
			secrets: "minio",
		},
		{
			name: "Opera, split stanza and endpointCA", located: operaLocated("main"),
			object: archiveObject(archiveSpec(map[string]any{
				"stanza":       "wal",
				"repositories": []any{operaRepository(map[string]any{"endpointCA": secretRef("minio-ca", "ca.crt"), "destinationPath": "/team/a"})},
			})),
			want: Composition{
				Provider: "s3", AgentDestination: "s3://backups/team/a", EndpointURL: "https://minio:9000", Region: RegionSource{Value: "dummy"},
				Stanza: "main", ArchiveStanza: "wal", Repository: 1, Credential: CredentialStaticKeys, AccessKeyID: keys[0], SecretAccessKey: keys[1],
				CipherPass: ref("minio", "ENCRYPTION_KEY"), EndpointCA: ref("minio-ca", "ca.crt"),
			},
			secrets: "minio,minio-ca",
		},
		{
			name: "Opera, configuration.stanza equal to the backup stanza is no split; the author's CA wins", located: operaLocated(""),
			object: archiveObject(archiveSpec(map[string]any{
				"stanza":       "orders",
				"repositories": []any{operaRepository(map[string]any{"endpointCA": secretRef("minio-ca", "ca.crt"), "encryption": nil, "encryptionKey": secretRef("unused", "k")})},
			})),
			settings: &PgBackRestSettings{EndpointCA: ref("own-ca", "ca.crt")},
			want: Composition{
				Provider: "s3", AgentDestination: "s3://backups", EndpointURL: "https://minio:9000", Region: RegionSource{Value: "dummy"},
				Stanza: "orders", Repository: 1, Credential: CredentialStaticKeys, AccessKeyID: keys[0], SecretAccessKey: keys[1],
				EndpointCA: ref("own-ca", "ca.crt"),
			},
			secrets: "minio,own-ca",
		},
		{
			name: "Opera on AWS, no endpoint, repo2, web identity", located: operaLocated(""),
			object: archiveObject(archiveSpec(map[string]any{"repositories": []any{
				operaRepository(nil),
				operaRepository(map[string]any{"endpointURL": nil, "encryption": nil, "encryptionKey": nil, "s3Credentials": map[string]any{"keyType": "web-id", "region": "eu-west-3"}}),
			}})),
			settings: &PgBackRestSettings{Repository: 2},
			want: Composition{
				Provider: "s3", AgentDestination: "s3://backups", Region: RegionSource{Value: "eu-west-3"},
				Stanza: "orders", Repository: 2, Credential: CredentialAmbient,
			},
		},
		{
			name: "Opera on an AWS endpoint without a region", located: operaLocated(""),
			object: archiveObject(archiveSpec(map[string]any{"repositories": []any{operaRepository(map[string]any{
				"endpointURL": "s3.eu-west-1.amazonaws.com", "encryption": nil, "encryptionKey": nil,
				"s3Credentials": map[string]any{"keyType": "auto", "uriStyle": "host"},
			})}})),
			want: Composition{
				Provider: "s3", AgentDestination: "s3://backups", EndpointURL: "https://s3.eu-west-1.amazonaws.com", Region: RegionSource{Value: "eu-west-1"},
				Stanza: "orders", Repository: 1, URIStyle: "host", Credential: CredentialAmbient,
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, whyNot := compose(t, test.located, test.object, test.settings)
			if whyNot != nil {
				t.Fatalf("refused: %s", whyNot.Message)
			}
			want := test.want
			location := test.located.PgBackRest
			want.Format, want.Origin = FormatPgBackRest, test.located.Origin()
			want.PgBackRestPlugin, want.PgBackRestKind, want.PgBackRestObject = location.Plugin, location.Kind, location.ObjectName
			bucket, prefix := strings.TrimPrefix(want.AgentDestination, want.Provider+"://"), ""
			if want.Provider == "azure" {
				bucket = strings.TrimPrefix(want.AgentDestination, "azure://")
			}
			bucket, prefix, _ = strings.Cut(bucket, "/")
			want.Fingerprint = expectedFingerprint(t, evidencev1alpha1.RepositoryFingerprintInput{
				Provider: want.Provider, Endpoint: want.EndpointURL, Account: want.AzureAccount, Bucket: bucket, Prefix: prefix, ScopeName: want.Stanza,
			})
			// DestinationPath is the plugin's destination as the agent spells it.
			want.DestinationPath = got.DestinationPath
			if want.Retention == nil {
				// No retention in the object: pgBackRest's default types.
				want.Retention = &PgBackRestRetention{FullType: "count", ArchiveType: "full"}
			}
			if got.Provider == "s3" && got.DestinationPath != got.AgentDestination {
				t.Fatalf("S3 destination %q, agent destination %q", got.DestinationPath, got.AgentDestination)
			}
			if diff := compareCompositions(*got, want); diff != "" {
				t.Fatal(diff)
			}
			if names := strings.Join(got.ReferencedSecrets(), ","); names != test.secrets {
				t.Fatalf("referenced Secrets = %q, want %q", names, test.secrets)
			}
		})
	}
}

// compareCompositions names the first differing field.
func compareCompositions(got, want Composition) string {
	type field struct {
		name      string
		got, want any
	}
	deref := func(reference *SecretKeyRef) any {
		if reference == nil {
			return nil
		}
		return *reference
	}
	for _, f := range []field{
		{"Format", got.Format, want.Format},
		{"Origin", got.Origin, want.Origin},
		{"Provider", got.Provider, want.Provider},
		{"AgentDestination", got.AgentDestination, want.AgentDestination},
		{"DestinationPath", got.DestinationPath, want.DestinationPath},
		{"EndpointURL", got.EndpointURL, want.EndpointURL},
		{"AzureAccount", got.AzureAccount, want.AzureAccount},
		{"Region", got.Region.Value, want.Region.Value},
		{"RegionSecret", deref(got.Region.SecretRef), deref(want.Region.SecretRef)},
		{"Stanza", got.Stanza, want.Stanza},
		{"ArchiveStanza", got.ArchiveStanza, want.ArchiveStanza},
		{"Repository", got.Repository, want.Repository},
		{"URIStyle", got.URIStyle, want.URIStyle},
		{"Credential", got.Credential, want.Credential},
		{"AccessKeyID", deref(got.AccessKeyID), deref(want.AccessKeyID)},
		{"SecretAccessKey", deref(got.SecretAccessKey), deref(want.SecretAccessKey)},
		{"AzureSecret", deref(got.AzureSecret), deref(want.AzureSecret)},
		{"CipherPass", deref(got.CipherPass), deref(want.CipherPass)},
		{"EndpointCA", deref(got.EndpointCA), deref(want.EndpointCA)},
		{"Fingerprint", got.Fingerprint, want.Fingerprint},
		{"PgBackRestPlugin", got.PgBackRestPlugin, want.PgBackRestPlugin},
		{"PgBackRestKind", got.PgBackRestKind, want.PgBackRestKind},
		{"PgBackRestObject", got.PgBackRestObject, want.PgBackRestObject},
		{"ServerName", got.ServerName, want.ServerName},
		{"ObjectStoreName", got.ObjectStoreName, want.ObjectStoreName},
		{"Retention", fmt.Sprintf("%+v", got.Retention), fmt.Sprintf("%+v", want.Retention)},
	} {
		if f.got != f.want {
			return fmt.Sprintf("%s: got %#v, want %#v", f.name, f.got, f.want)
		}
	}
	return ""
}

// TestComposePgBackRestRetention maps each plugin's retention of the
// selected repository, and only that repository's, to the agent's
// expectation and its environment.
func TestComposePgBackRestRetention(t *testing.T) {
	daliboRetention := map[string]any{"full": int64(7), "fullType": "count", "diff": int64(14), "archive": int64(2), "archiveType": "full", "history": int64(30)}
	cases := []struct {
		name      string
		located   *Located
		object    *unstructured.Unstructured
		settings  *PgBackRestSettings
		want      PgBackRestRetention
		variables []Variable
	}{
		{
			name: "Dalibo S3, the example's retention (history is not carried)", located: daliboLocated(),
			object: stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{"retentionPolicy": daliboRetention})}}),
			want:   PgBackRestRetention{Full: 7, FullType: "count", Diff: 14, Archive: 2, ArchiveType: "full"},
			variables: []Variable{
				{"PGBACKREST_RETENTION_FULL", "7"},
				{"PGBACKREST_RETENTION_FULL_TYPE", "count"},
				{"PGBACKREST_RETENTION_DIFF", "14"},
				{"PGBACKREST_RETENTION_ARCHIVE", "2"},
				{"PGBACKREST_RETENTION_ARCHIVE_TYPE", "full"},
			},
		},
		{
			name: "Dalibo repo2 takes its own retention, not repo1's", located: daliboLocated(),
			object: stanzaObject(map[string]any{"s3Repositories": []any{
				daliboS3(map[string]any{"retentionPolicy": daliboRetention}),
				daliboS3(map[string]any{"bucket": "bucket-02", "retentionPolicy": map[string]any{"full": int64(30), "fullType": "time", "archiveType": "diff", "diff": int64(3)}}),
			}}),
			settings:  &PgBackRestSettings{Repository: 2},
			want:      PgBackRestRetention{Full: 30, FullType: "time", Diff: 3, ArchiveType: "diff"},
			variables: []Variable{{"PGBACKREST_RETENTION_FULL", "30"}, {"PGBACKREST_RETENTION_FULL_TYPE", "time"}, {"PGBACKREST_RETENTION_DIFF", "3"}, {"PGBACKREST_RETENTION_ARCHIVE_TYPE", "diff"}},
		},
		{
			name: "Dalibo Azure retention", located: daliboLocated(),
			object:    stanzaObject(map[string]any{"azureRepositories": []any{daliboAzure(map[string]any{"retentionPolicy": map[string]any{"full": int64(2)}})}}),
			want:      PgBackRestRetention{Full: 2, FullType: "count", ArchiveType: "full"},
			variables: []Variable{{"PGBACKREST_RETENTION_FULL", "2"}, {"PGBACKREST_RETENTION_FULL_TYPE", "count"}, {"PGBACKREST_RETENTION_ARCHIVE_TYPE", "full"}},
		},
		{
			name: "Dalibo without retentionPolicy: the types alone, repo-retention-full unset", located: daliboLocated(),
			object:    stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(nil)}}),
			want:      PgBackRestRetention{FullType: "count", ArchiveType: "full"},
			variables: []Variable{{"PGBACKREST_RETENTION_FULL_TYPE", "count"}, {"PGBACKREST_RETENTION_ARCHIVE_TYPE", "full"}},
		},
		{
			name: "Opera, the example's time-based retention", located: operaLocated(""),
			object:    archiveObject(archiveSpec(map[string]any{"repositories": []any{operaRepository(map[string]any{"retention": map[string]any{"full": int64(7), "fullType": "time"}})}})),
			want:      PgBackRestRetention{Full: 7, FullType: "time", ArchiveType: "full"},
			variables: []Variable{{"PGBACKREST_RETENTION_FULL", "7"}, {"PGBACKREST_RETENTION_FULL_TYPE", "time"}, {"PGBACKREST_RETENTION_ARCHIVE_TYPE", "full"}},
		},
		{
			name: "Opera without retention", located: operaLocated(""),
			object:    archiveObject(archiveSpec(map[string]any{})),
			want:      PgBackRestRetention{FullType: "count", ArchiveType: "full"},
			variables: []Variable{{"PGBACKREST_RETENTION_FULL_TYPE", "count"}, {"PGBACKREST_RETENTION_ARCHIVE_TYPE", "full"}},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, whyNot := compose(t, test.located, test.object, test.settings)
			if whyNot != nil {
				t.Fatalf("refused: %s", whyNot.Message)
			}
			if got.Retention == nil || *got.Retention != test.want {
				t.Fatalf("retention %+v, want %+v", got.Retention, test.want)
			}
			if variables := got.Retention.Variables(); !slices.Equal(variables, test.variables) {
				t.Fatalf("variables %+v, want %+v", variables, test.variables)
			}
		})
	}
	var none *PgBackRestRetention
	if none.Variables() != nil {
		t.Fatal("a Barman composition renders retention variables")
	}
	for name, retention := range map[string]map[string]any{
		"a type the CRD does not allow":          {"fullType": "days"},
		"an archive type the CRD does not allow": {"archiveType": "wal"},
		"a count above pgBackRest's bound":       {"full": int64(10_000_000)},
		"a negative count":                       {"diff": int64(-1)},
	} {
		t.Run(name, func(t *testing.T) {
			object := stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{"retentionPolicy": retention})}})
			_, whyNot := compose(t, daliboLocated(), object, nil)
			if whyNot == nil || whyNot.Reason != pgtoolboxv1alpha1.ReasonUnsupportedObjectStore || !strings.Contains(whyNot.Message, "retentionPolicy") {
				t.Fatalf("problem %+v", whyNot)
			}
		})
	}
}

func TestComposePgBackRestRefusals(t *testing.T) {
	cases := []struct {
		name     string
		located  *Located
		object   *unstructured.Unstructured
		settings *PgBackRestSettings
		reason   pgtoolboxv1alpha1.Reason
		message  string
	}{
		// Dalibo.
		{
			"Dalibo without a repository", daliboLocated(), stanzaObject(map[string]any{}), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "Stanza orders-stanza declares no S3 or Azure repository",
		},
		{
			"Dalibo, a repository number past the last", daliboLocated(),
			stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(nil)}, "azureRepositories": []any{daliboAzure(nil)}}), &PgBackRestSettings{Repository: 3},
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "declares 1 S3 and 1 Azure repositories (repo1 to repo2, S3 first): there is no repo3",
		},
		{
			"a repository number beyond pgBackRest's", daliboLocated(), stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(nil)}}), &PgBackRestSettings{Repository: 257},
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "repository 257 is not a pgBackRest repository number",
		},
		{
			"Dalibo without a stanza name", daliboLocated(), stanzaObject(map[string]any{"name": "", "s3Repositories": []any{daliboS3(nil)}}), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "the stanza from spec.stanzaConfiguration.name",
		},
		{
			// The agent reads at most 128 bytes; the
			// operator refuses past it, with the reason, rather than render
			// an agent that cannot start.
			"Dalibo, a stanza over the shared bound", daliboLocated(), stanzaObject(map[string]any{"name": strings.Repeat("s", 129), "s3Repositories": []any{daliboS3(nil)}}), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "the stanza from spec.stanzaConfiguration.name is empty, longer than 128 bytes",
		},
		{
			"Opera, an archive stanza over the shared bound", operaLocated(""), archiveObject(archiveSpec(map[string]any{"stanza": strings.Repeat("w", 129)})), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "the stanza from spec.configuration.stanza is empty, longer than 128 bytes",
		},
		{
			"Dalibo, a stanza with surrounding spaces", daliboLocated(), stanzaObject(map[string]any{"name": " main", "s3Repositories": []any{daliboS3(nil)}}), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "the stanza from spec.stanzaConfiguration.name",
		},
		{
			"Dalibo, a stanza with a slash", daliboLocated(), stanzaObject(map[string]any{"name": "a/b", "s3Repositories": []any{daliboS3(nil)}}), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "the stanza from spec.stanzaConfiguration.name",
		},
		{
			"Dalibo, customEnvVar moves the repository", daliboLocated(), stanzaObject(map[string]any{
				"s3Repositories": []any{daliboS3(nil)},
				"customEnvVar":   map[string]any{"PGBACKREST_REPO1_PATH": canary, "PGBACKREST_LOG_LEVEL_CONSOLE": "info", "PGBACKREST_REPO1_S3_BUCKET": canary},
			}), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
			"Stanza orders-stanza spec.stanzaConfiguration.customEnvVar sets PGBACKREST_REPO1_PATH, PGBACKREST_REPO1_S3_BUCKET, which can move",
		},
		{"Dalibo, customEnvVar names the stanza", daliboLocated(), stanzaObject(map[string]any{
			"s3Repositories": []any{daliboS3(nil)}, "customEnvVar": map[string]any{"PGBACKREST_STANZA": canary},
		}), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "customEnvVar sets PGBACKREST_STANZA,"},
		{"Dalibo, customEnvVar names a configuration file", daliboLocated(), stanzaObject(map[string]any{
			"s3Repositories": []any{daliboS3(nil)}, "customEnvVar": map[string]any{"PGBACKREST_CONFIG_INCLUDE_PATH": canary},
		}), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "customEnvVar sets PGBACKREST_CONFIG_INCLUDE_PATH,"},
		{
			"Dalibo S3 without a key pair", daliboLocated(), stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{"secretRef": nil})}}), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "Stanza orders-stanza repo1 names no static access key pair",
		},
		{
			"Dalibo cipherConfig without a pass", daliboLocated(), stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{"cipherConfig": map[string]any{}})}}), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "declares cipherConfig without cipherConfig.encryptionPass",
		},
		{"Dalibo, another cipher", daliboLocated(), stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{
			"cipherConfig": map[string]any{"type": "aes-128-gcm", "encryptionPass": secretRef("c", "k")},
		})}}), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, `cipherConfig.type "aes-128-gcm" is not aes-256-cbc`},
		{
			"an endpoint with a path", daliboLocated(), stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{"endpoint": "https://minio/" + canary})}}), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "repo1: the endpoint is not a host[:port]",
		},
		{
			"an endpoint with credentials", daliboLocated(), stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{"endpoint": "https://user:" + canary + "@minio"})}}), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "repo1: the endpoint is not a host[:port]",
		},
		{
			"an invalid bucket", daliboLocated(), stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{"bucket": "a b"})}}), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "the bucket or the repository path is not a valid S3 bucket",
		},
		{
			"a repository path with an empty segment", daliboLocated(), stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{"repoPath": "/a//b"})}}), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "the bucket or the repository path is not a valid S3 bucket",
		},
		{
			"an unknown uriStyle", daliboLocated(), stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(map[string]any{"uriStyle": "virtual"})}}), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, `uriStyle "virtual" is neither host nor path`,
		},
		{
			"Dalibo Azure, host style on a custom endpoint", daliboLocated(), stanzaObject(map[string]any{"azureRepositories": []any{daliboAzure(map[string]any{"endpoint": "azurite:10000"})}}), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "is not a public or national Azure Blob host",
		},
		{
			"Dalibo Azure, shared key without a Secret", daliboLocated(), stanzaObject(map[string]any{"azureRepositories": []any{daliboAzure(map[string]any{"secretRef": nil})}}), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "keyType shared names no secretRef.keyReference",
		},
		{
			"Dalibo Azure, an unknown key type", daliboLocated(), stanzaObject(map[string]any{"azureRepositories": []any{daliboAzure(map[string]any{"keyType": "oauth"})}}), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, `keyType "oauth" is not shared, sas or auto`,
		},
		{
			"Dalibo Azure, an invalid account", daliboLocated(), stanzaObject(map[string]any{"azureRepositories": []any{daliboAzure(map[string]any{"account": "Not_An_Account"})}}), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "is not a valid Azure Blob destination",
		},
		// Opera.
		{
			"Opera without a repository", operaLocated(""), archiveObject(archiveSpec(map[string]any{"repositories": []any{}})), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "Archive orders-archive declares no repository",
		},
		{
			"Opera, a repository number past the last", operaLocated(""), archiveObject(archiveSpec(map[string]any{})), &PgBackRestSettings{Repository: 2},
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "declares 1 repositories (repo1 to repo1): there is no repo2",
		},
		{
			"Opera without s3Credentials", operaLocated(""), archiveObject(archiveSpec(map[string]any{"repositories": []any{operaRepository(map[string]any{"s3Credentials": nil})}})), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "Archive orders-archive repo1 declares no s3Credentials",
		},
		{
			"Opera encryption without a key", operaLocated(""), archiveObject(archiveSpec(map[string]any{"repositories": []any{operaRepository(map[string]any{"encryptionKey": nil})}})), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "declares encryption aes-256-cbc without encryptionKey",
		},
		{
			"Opera, another cipher", operaLocated(""), archiveObject(archiveSpec(map[string]any{"repositories": []any{operaRepository(map[string]any{"encryption": "aes-128"})}})), nil,
			pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, `encryption "aes-128" is not aes-256-cbc`,
		},
		{"Opera, an unknown key type", operaLocated(""), archiveObject(archiveSpec(map[string]any{"repositories": []any{operaRepository(map[string]any{
			"s3Credentials": map[string]any{"keyType": "instance", "region": "r"},
		})}})), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, `s3Credentials.keyType "instance" is not shared, auto or web-id`},
		{"Opera shared keys missing", operaLocated(""), archiveObject(archiveSpec(map[string]any{"repositories": []any{operaRepository(map[string]any{
			"s3Credentials": map[string]any{"keyType": "shared", "region": "r", "accessKeyId": secretRef("a", "b")},
		})}})), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "names no static access key pair"},
		{"Opera on AWS without a region", operaLocated(""), archiveObject(archiveSpec(map[string]any{"repositories": []any{operaRepository(map[string]any{
			"endpointURL": nil, "s3Credentials": map[string]any{"accessKeyId": secretRef("a", "b"), "secretAccessKey": secretRef("a", "c")},
		})}})), nil, pgtoolboxv1alpha1.ReasonRegionRequired, "AWS S3 without an endpoint needs a region"},
		{"Opera, the sidecar env moves the repository", operaLocated(""), archiveObject(map[string]any{
			"configuration":                archiveSpec(map[string]any{})["configuration"],
			"instanceSidecarConfiguration": map[string]any{"env": []any{map[string]any{"name": "PGBACKREST_REPO1_S3_BUCKET", "value": canary}, map[string]any{"name": "TZ", "value": "UTC"}}},
		}), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "spec.instanceSidecarConfiguration.env sets PGBACKREST_REPO1_S3_BUCKET,"},
		{"Opera, an archive-push argument moves the repository", operaLocated(""), archiveObject(archiveSpec(map[string]any{
			"wal": map[string]any{"archiveAdditionalCommandArgs": []any{"--repo1-path=/" + canary}},
		})), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "spec.configuration.wal.archiveAdditionalCommandArgs passes --repo1-path,"},
		{"Opera, an archive-push argument resets the repository path", operaLocated(""), archiveObject(archiveSpec(map[string]any{
			"wal": map[string]any{"archiveAdditionalCommandArgs": []any{"--reset-repo1-path"}},
		})), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "archiveAdditionalCommandArgs passes --reset-repo1-path,"},
		{"Opera, an archive-get argument selects a repository", operaLocated(""), archiveObject(archiveSpec(map[string]any{
			"wal": map[string]any{"restoreAdditionalCommandArgs": []any{"--repo=2"}},
		})), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "restoreAdditionalCommandArgs passes --repo,"},
		{"Opera, a backup argument names the stanza", operaLocated(""), archiveObject(archiveSpec(map[string]any{
			"data": map[string]any{"additionalCommandArgs": []any{"--io-timeout=60", "--stanza=" + canary}},
		})), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "spec.configuration.data.additionalCommandArgs passes --stanza,"},
		{"Opera, a restore argument names a configuration", operaLocated(""), archiveObject(archiveSpec(map[string]any{
			"restore": map[string]any{"additionalCommandArgs": []any{"--config=/" + canary}},
		})), nil, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "spec.configuration.restore.additionalCommandArgs passes --config,"},
		{
			"Opera, an archive stanza with a slash", operaLocated(""), archiveObject(archiveSpec(map[string]any{"stanza": "a/b"})), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "the stanza from spec.configuration.stanza",
		},
		{
			"Opera, a stanza parameter with a backslash", operaLocated(`a\b`), archiveObject(archiveSpec(map[string]any{})), nil,
			pgtoolboxv1alpha1.ReasonDestinationInvalid, "the stanza from the Cluster's stanza parameter",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, whyNot := compose(t, test.located, test.object, test.settings)
			if whyNot == nil || whyNot.Reason != test.reason {
				t.Fatalf("problem = %+v, want %s", whyNot, test.reason)
			}
			if !strings.Contains(whyNot.Message, test.message) {
				t.Fatalf("message %q does not say %q", whyNot.Message, test.message)
			}
			if strings.Contains(whyNot.Message, canary) {
				t.Fatalf("message discloses a value: %s", whyNot.Message)
			}
		})
	}
}

// TestAdmitPgBackRest: the author's choices apply to pgBackRest exactly as
// to barman-cloud, without the deprecated empty allow-list.
func TestAdmitPgBackRest(t *testing.T) {
	located := operaLocated("")
	listed := Policy{AllowedSecrets: []string{"minio"}}
	cases := []struct {
		name       string
		repository map[string]any
		policy     Policy
		notAllowed string
		message    string
	}{
		{name: "listed", policy: listed},
		{name: "the empty allow-list never admits every Secret", policy: Policy{Unlisted: true}, message: "only an EvidenceAgent's spec.allowedSecrets may admit"},
		{name: "the cipher pass is a Secret like the others", repository: map[string]any{"encryptionKey": secretRef("cipher", "pass")}, policy: listed, notAllowed: "cipher"},
		{
			name: "the cipher pass on the secret key's slot", repository: map[string]any{"encryptionKey": secretRef("minio", "ACCESS_SECRET_KEY")}, policy: listed,
			message: "names Secret minio key ACCESS_SECRET_KEY for both secretAccessKey and cipherPass",
		},
		{name: "web identity without workloadIdentity", repository: map[string]any{
			"encryption": nil, "encryptionKey": nil, "s3Credentials": map[string]any{"keyType": "web-id", "region": "r"},
		}, policy: listed, message: "uses the Pod's cloud identity"},
		{name: "web identity with workloadIdentity", repository: map[string]any{
			"encryption": nil, "encryptionKey": nil, "s3Credentials": map[string]any{"keyType": "web-id", "region": "r"},
		}, policy: Policy{WorkloadIdentity: true}},
		{name: "plain http without the opt-in", repository: map[string]any{"endpointURL": "http://minio:9000"}, policy: listed, message: "names a plain-http endpoint"},
		{name: "plain http with the opt-in", repository: map[string]any{"endpointURL": "http://minio:9000"}, policy: Policy{AllowedSecrets: []string{"minio"}, AllowInsecureEndpoint: true}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			resolved, whyNot := compose(t, located, archiveObject(archiveSpec(map[string]any{"repositories": []any{operaRepository(test.repository)}})), nil)
			if whyNot != nil {
				t.Fatal(whyNot.Message)
			}
			notAllowed, whyNot := resolved.Admit(test.policy)
			switch {
			case test.message != "":
				if whyNot == nil || whyNot.Reason != pgtoolboxv1alpha1.ReasonUnsupportedObjectStore || !strings.Contains(whyNot.Message, test.message) {
					t.Fatalf("admit = %v %+v, want %q", notAllowed, whyNot, test.message)
				}
			case whyNot != nil || strings.Join(notAllowed, ",") != test.notAllowed:
				t.Fatalf("admit = %v %+v, want not allowed %q", notAllowed, whyNot, test.notAllowed)
			}
		})
	}
}

func TestReadPgBackRest(t *testing.T) {
	stanza := stanzaObject(map[string]any{"s3Repositories": []any{daliboS3(nil)}})
	undecodable := archiveObject(map[string]any{"configuration": map[string]any{"repositories": canary}})
	reader := fake.NewClientBuilder().WithObjects(stanza, undecodable).Build()
	location := daliboLocated().PgBackRest
	decoded, whyNot, err := ReadPgBackRest(context.Background(), reader, "team-a", location)
	if err != nil || whyNot != nil || decoded.Stanza == nil || decoded.Archive != nil || decoded.Stanza.Spec.Configuration.Name != "main" {
		t.Fatalf("read = %+v %+v %v", decoded, whyNot, err)
	}
	missing := *location
	missing.ObjectName = "absent"
	if _, whyNot, err := ReadPgBackRest(context.Background(), reader, "team-a", &missing); err != nil || whyNot == nil ||
		whyNot.Reason != pgtoolboxv1alpha1.ReasonObjectStoreNotFound || whyNot.Message != "Stanza absent was not found in namespace team-a" {
		t.Fatalf("missing = %+v %v", whyNot, err)
	}
	if _, whyNot, err := ReadPgBackRest(context.Background(), reader, "team-a", operaLocated("").PgBackRest); err != nil || whyNot == nil ||
		whyNot.Reason != pgtoolboxv1alpha1.ReasonUnsupportedObjectStore || strings.Contains(whyNot.Message, canary) {
		t.Fatalf("undecodable = %+v %v", whyNot, err)
	}

	unserved := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return &meta.NoKindMatchError{GroupKind: discovery.ArchiveGVK.GroupKind()}
		},
	}).Build()
	if _, whyNot, err := ReadPgBackRest(context.Background(), unserved, "team-a", operaLocated("").PgBackRest); err != nil || whyNot == nil ||
		whyNot.Reason != pgtoolboxv1alpha1.ReasonObjectStoreAPIUnavailable || !strings.Contains(whyNot.Message, "pgbackrest.cnpg.opera.com/v1 Archive API is not served") {
		t.Fatalf("unserved = %+v %v", whyNot, err)
	}
	failing := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("connection refused")
		},
	}).Build()
	if _, whyNot, err := ReadPgBackRest(context.Background(), failing, "team-a", location); err == nil || whyNot != nil {
		t.Fatalf("API failure = %+v %v", whyNot, err)
	}
}

// TestLocateThenComposePgBackRest walks the whole path on a fake API, as
// both consumers do.
func TestLocateThenComposePgBackRest(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a", UID: "uid-1"},
		Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: "pgbackrest.cnpg.opera.com", Parameters: map[string]string{"pgbackrestObjectName": "orders-archive"},
		}}},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster, archiveObject(archiveSpec(map[string]any{}))).Build()
	located, whyNot, err := Locate(context.Background(), reader, Request{Namespace: "team-a", ClusterName: "orders", PgBackRest: &PgBackRestSettings{}})
	if err != nil || whyNot != nil {
		t.Fatalf("locate = %+v %v", whyNot, err)
	}
	object, whyNot, err := ReadPgBackRest(context.Background(), reader, "team-a", located.PgBackRest)
	if err != nil || whyNot != nil {
		t.Fatalf("read = %+v %v", whyNot, err)
	}
	resolved, whyNot := ComposePgBackRest(located, object, nil)
	if whyNot != nil || resolved.Stanza != "orders" || resolved.Origin.String() != "Archive orders-archive" || resolved.ServerName != "" {
		t.Fatalf("compose = %+v %+v", resolved, whyNot)
	}
}

// TestStanzaBoundIsTheAgents: a stanza at the shared bound resolves.
func TestStanzaBoundIsTheAgents(t *testing.T) {
	var composition Composition
	at := strings.Repeat("s", evidencev1alpha1.MaxPGBackRestStanzaBytes)
	if whyNot := composition.setStanzas(Origin{Cluster: "orders"}, [2]string{"name", "stanza"}, at, at); whyNot != nil {
		t.Fatalf("a %d-byte stanza: %s", len(at), whyNot.Message)
	}
	if whyNot := composition.setStanzas(Origin{Cluster: "orders"}, [2]string{"name", "stanza"}, at+"s", ""); whyNot == nil || whyNot.Reason != pgtoolboxv1alpha1.ReasonDestinationInvalid {
		t.Fatalf("a %d-byte stanza: %+v", len(at)+1, whyNot)
	}
}

// TestMovingArguments pins the command-line forms pgBackRest resolves to
// an option that moves the repository, the stanza or the configuration
// (src/config/parse.c: the no- and reset- prefixes, unique abbreviations,
// the deprecated backup-* aliases), and the ones that do not.
func TestMovingArguments(t *testing.T) {
	cases := []struct {
		argument string
		moves    bool
	}{
		{"--repo1-path=/x", true},
		{"--repo=2", true},
		{"--reset-repo1-path", true},
		{"--no-repo1-storage-verify-tls", true},
		{"--RESET-Repo1-S3-Bucket", true},
		{"--reset-stanza", true},
		{"--no-config", true},
		{"--reset-config-include-path", true},
		{"--stan=other", true},
		{"--conf=/etc/x.conf", true},
		{"--config-p=/etc", true},
		{"--rep=2", true},
		{"--backup-host=elsewhere", true},
		{"--no-backup-config", true},
		{"--backup-ho=elsewhere", true},
		{"--io-timeout=60", false},
		{"--buffer-size=5MB", false},
		{"--no-compress", false},
		{"--reset-process-max", false},
		{"--backup-standby", false},
		{"--no-backup-standby", false},
		{"--start-fast", false},
		{"--pg1-path=/var/lib/postgresql", false},
		{"--log-level-console=info", false},
		{"repo1-path", false},
		{"--", false},
	}
	for _, test := range cases {
		t.Run(test.argument, func(t *testing.T) {
			if got := len(movingArguments([]string{test.argument})) > 0; got != test.moves {
				t.Fatalf("movingArguments(%q) moves = %v, want %v", test.argument, got, test.moves)
			}
		})
	}
}

// TestMovingVariables pins the environment forms. pgBackRest neither
// abbreviates an environment option nor honours a negate or reset form
// there (it warns and skips it), but it does resolve the deprecated
// backup-* aliases.
func TestMovingVariables(t *testing.T) {
	cases := []struct {
		name  string
		moves bool
	}{
		{"PGBACKREST_REPO1_PATH", true},
		{"PGBACKREST_REPO", true},
		{"pgbackrest_repo1_s3_bucket", true},
		{"PGBACKREST_STANZA", true},
		{"PGBACKREST_CONFIG", true},
		{"PGBACKREST_CONFIG_PATH", true},
		{"PGBACKREST_CONFIG_INCLUDE_PATH", true},
		{"PGBACKREST_BACKUP_HOST", true},
		{"PGBACKREST_BACKUP_CONFIG", true},
		{"PGBACKREST_BACKUP_STANDBY", false},
		{"PGBACKREST_STAN", false},
		{"PGBACKREST_RESET_REPO1_PATH", false},
		{"PGBACKREST_NO_CONFIG", false},
		{"PGBACKREST_LOG_LEVEL_CONSOLE", false},
		{"PGBACKREST_PROCESS_MAX", false},
		{"REPO1_PATH", false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := len(movingVariables([]string{test.name})) > 0; got != test.moves {
				t.Fatalf("movingVariables(%q) moves = %v, want %v", test.name, got, test.moves)
			}
		})
	}
}
