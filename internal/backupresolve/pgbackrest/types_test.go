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

package pgbackrest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// load reads one example manifest as the resolver receives it: an
// unstructured object.
func load(t *testing.T, path string) (*unstructured.Unstructured, error) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", path))
	if err != nil {
		t.Fatal(err)
	}
	object := map[string]any{}
	if err := yaml.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: object}, nil
}

func selector(name, key string) *SecretKeySelector { return &SecretKeySelector{Name: name, Key: key} }

// TestDecodeDaliboExamples decodes every Stanza example of cnpg-i-pgbackrest
// v0.0.3 and checks the fields the resolver reads.
func TestDecodeDaliboExamples(t *testing.T) {
	keys := &S3SecretRef{AccessKeyIDReference: selector("minio", "ACCESS_KEY_ID"), SecretAccessKeyReference: selector("minio", "ACCESS_SECRET_KEY")}
	thirty := int32(30)
	// Every S3 example sets the same retention on repo1; the second
	// repository of stanza_multi_repositories.yaml keeps less.
	repo1Retention := Retention{Full: 7, FullType: "count", Diff: 14, Archive: 2, ArchiveType: "full", History: &thirty}
	repo := func(bucket, path string) S3Repository {
		return S3Repository{
			Bucket: bucket, Endpoint: "https://172.18.0.2:9000", Region: "fr-par", RepoPath: path, UriStyle: "path", SecretRef: keys,
			RetentionPolicy: repo1Retention,
		}
	}
	repo2 := repo("bucket-02", "/repo-02")
	repo2.RetentionPolicy = Retention{Full: 2, FullType: "count", Diff: 7, Archive: 1, ArchiveType: "full", History: &thirty}
	cases := []struct {
		file string
		want StanzaConfiguration
	}{
		{"stanza.yaml", StanzaConfiguration{Name: "main", S3Repositories: []S3Repository{repo("bucket-01", "/repo-01")}}},
		{"stanza_async.yaml", StanzaConfiguration{Name: "main", S3Repositories: []S3Repository{repo("bucket-01", "/repo-01")}}},
		{"stanza_multi_repositories.yaml", StanzaConfiguration{Name: "main", S3Repositories: []S3Repository{
			repo("bucket-01", "/repo-01"), repo2,
		}}},
		{"stanza_azure.yaml", StanzaConfiguration{Name: "main", AzureRepositories: []AzureRepository{{
			Account: "sts-sample", Container: "backups", Endpoint: "blob.core.windows.net", RepoPath: "/cluster-sample", KeyType: "shared",
			SecretRef: &AzureSecretRef{KeyReference: selector("azure", "KEY")},
		}}}},
	}
	for _, test := range cases {
		t.Run(test.file, func(t *testing.T) {
			object, err := load(t, filepath.Join("dalibo-v0.0.3", test.file))
			if err != nil {
				t.Fatal(err)
			}
			if object.GetAPIVersion() != "pgbackrest.dalibo.com/v1" || object.GetKind() != "Stanza" {
				t.Fatalf("example is a %s %s", object.GetAPIVersion(), object.GetKind())
			}
			stanza, err := DecodeStanza(object)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stanza.Spec.Configuration, test.want) {
				t.Fatalf("decoded %+v\nwant    %+v", stanza.Spec.Configuration, test.want)
			}
		})
	}

	// The customEnvVar example is broken YAML at v0.0.3 (README); the day
	// it parses, it must decode its override map and join the table above.
	if _, err := load(t, filepath.Join("dalibo-v0.0.3", "stanza_custom.yaml")); err == nil {
		t.Fatal("stanza_custom.yaml parses now: decode it and assert customEnvVar and cipherConfig")
	}

	for file, want := range map[string]map[string]string{
		"cluster.yaml":                    {"stanzaRef": "stanza-sample", "pluginConfigRef": "sample-pluginconfig"},
		"cluster_multi_repositories.yaml": {"stanzaRef": "stanza-multi-repositories"},
	} {
		object, err := load(t, filepath.Join("dalibo-v0.0.3", file))
		if err != nil {
			t.Fatal(err)
		}
		if got := pluginParameters(t, object, DaliboPluginName); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: parameters %v, want %v", file, got, want)
		}
		if _, ok := want[DaliboStanzaParameter]; !ok {
			t.Fatalf("%s names no %s", file, DaliboStanzaParameter)
		}
	}
}

// TestDecodeOperaExamples decodes the Archive example of
// cnpg-plugin-pgbackrest v0.9.0 and its Clusters' parameters.
func TestDecodeOperaExamples(t *testing.T) {
	object, err := load(t, filepath.Join("opera-v0.9.0", "minio-archive.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if object.GetAPIVersion() != "pgbackrest.cnpg.opera.com/v1" || object.GetKind() != "Archive" {
		t.Fatalf("example is a %s %s", object.GetAPIVersion(), object.GetKind())
	}
	archive, err := DecodeArchive(object)
	if err != nil {
		t.Fatal(err)
	}
	want := ArchiveConfiguration{
		Repositories: []Repository{{
			DestinationPath: "/", Bucket: "backups", EndpointURL: "minio:9000", DisableVerifyTLS: true,
			Encryption: "aes-256-cbc", EncryptionKey: selector("minio", "ENCRYPTION_KEY"),
			Retention: &Retention{Full: 7, FullType: "time"},
			AWS: &S3Credentials{
				Region: "dummy", AccessKeyIDReference: selector("minio", "ACCESS_KEY_ID"), SecretAccessKeyReference: selector("minio", "ACCESS_SECRET_KEY"),
			},
		}},
		Wal:  &WalBackupConfiguration{},
		Data: &DataBackupConfiguration{AdditionalCommandArgs: []string{"--buffer-size=5MB", "--io-timeout=60"}},
	}
	if !reflect.DeepEqual(archive.Spec.Configuration, want) {
		t.Fatalf("decoded %+v\nwant    %+v", archive.Spec.Configuration, want)
	}
	if archive.Spec.InstanceSidecarConfiguration.Env != nil {
		t.Fatalf("sidecar env = %v", archive.Spec.InstanceSidecarConfiguration.Env)
	}

	cluster, err := load(t, filepath.Join("opera-v0.9.0", "cluster-example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := pluginParameters(t, cluster, OperaPluginName); !reflect.DeepEqual(got, map[string]string{OperaObjectParameter: "minio-store"}) {
		t.Fatalf("parameters = %v", got)
	}
	restore, err := load(t, filepath.Join("opera-v0.9.0", "cluster-restore-archive.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := pluginParameters(t, restore, OperaPluginName); !reflect.DeepEqual(got, map[string]string{OperaObjectParameter: "minio-store-bis"}) {
		t.Fatalf("parameters = %v", got)
	}
	external, _, _ := unstructured.NestedSlice(restore.Object, "spec", "externalClusters")
	parameters, _, _ := unstructured.NestedStringMap(external[0].(map[string]any), "plugin", "parameters")
	if parameters[OperaStanzaParameter] != "cluster-example" || parameters[OperaObjectParameter] != "minio-store" {
		t.Fatalf("external cluster parameters = %v", parameters)
	}
}

// pluginParameters returns the parameters of the Cluster's plugin entry.
func pluginParameters(t *testing.T, cluster *unstructured.Unstructured, name string) map[string]string {
	t.Helper()
	plugins, _, err := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range plugins {
		plugin, _ := entry.(map[string]any)
		if plugin["name"] != name {
			continue
		}
		parameters, _, err := unstructured.NestedStringMap(plugin, "parameters")
		if err != nil {
			t.Fatal(err)
		}
		return parameters
	}
	t.Fatalf("no %s plugin", name)
	return nil
}

func TestDecodeRefusesMistypedFields(t *testing.T) {
	stanza := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"stanzaConfiguration": map[string]any{
		"name": "main", "s3Repositories": []any{map[string]any{"verifyTLS": "no"}},
	}}}}
	if _, err := DecodeStanza(stanza); err == nil {
		t.Fatal("a string verifyTLS decoded")
	}
	archive := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"configuration": map[string]any{"repositories": "repo1"}}}}
	if _, err := DecodeArchive(archive); err == nil {
		t.Fatal("a string repositories decoded")
	}
}
