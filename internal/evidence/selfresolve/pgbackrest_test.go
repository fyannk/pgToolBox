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
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/fyannk/pgToolBox/internal/backupresolve"
	"github.com/fyannk/pgToolBox/internal/evidence/config"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// The synthetic cipher pass: never a real one, and checked absent from
// every message.
const cipherCanary = "cipher-canary-value"

func pgBackRestCluster(plugin, parameter, object string) *cnpgv1.Cluster {
	cluster := testCluster()
	cluster.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: plugin, Parameters: map[string]string{parameter: object}}}
	return cluster
}

// operaArchive is an Archive with one S3 repository encrypted by the minio
// Secret's ENCRYPTION_KEY, and the WAL under its own stanza.
func operaArchive(endpointCA bool) *unstructured.Unstructured {
	repository := map[string]any{
		"destinationPath": "/orders", "bucket": "backups", "endpointURL": "minio.storage.svc:9000",
		"encryption": "aes-256-cbc", "encryptionKey": map[string]any{"name": "minio", "key": "ENCRYPTION_KEY"},
		"s3Credentials": map[string]any{
			"region":          "us-east-1",
			"accessKeyId":     map[string]any{"name": "minio", "key": "ACCESS_KEY_ID"},
			"secretAccessKey": map[string]any{"name": "minio", "key": "ACCESS_SECRET_KEY"},
		},
	}
	if endpointCA {
		repository["endpointCA"] = map[string]any{"name": "minio-ca", "key": "ca.crt"}
	}
	repository["retention"] = map[string]any{"full": int64(7), "fullType": "time", "archive": int64(3)}
	archive := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"configuration": map[string]any{
		"stanza": "orders-wal", "repositories": []any{repository},
	}}}}
	archive.SetGroupVersionKind(discovery.ArchiveGVK)
	archive.SetNamespace(namespace)
	archive.SetName("orders-archive")
	return archive
}

// daliboStanza is a Stanza with an S3 repository then an Azure one
// (repo1 and repo2).
func daliboStanza() *unstructured.Unstructured {
	stanza := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"stanzaConfiguration": map[string]any{
		"name": "main",
		"s3Repositories": []any{map[string]any{
			"bucket": "bucket-01", "endpoint": "https://minio.storage.svc:9000", "region": "fr-par", "repoPath": "/repo-01", "uriStyle": "path",
			"secretRef": map[string]any{
				"accessKeyId":     map[string]any{"name": "minio", "key": "ACCESS_KEY_ID"},
				"secretAccessKey": map[string]any{"name": "minio", "key": "ACCESS_SECRET_KEY"},
			},
		}},
		"azureRepositories": []any{map[string]any{
			"account": "stsample", "container": "backups", "endpoint": "blob.core.windows.net", "repoPath": "/cluster-sample", "keyType": "shared",
			"secretRef": map[string]any{"keyReference": map[string]any{"name": "azure", "key": "KEY"}},
		}},
	}}}}
	stanza.SetGroupVersionKind(discovery.StanzaGVK)
	stanza.SetNamespace(namespace)
	stanza.SetName("orders-stanza")
	return stanza
}

// azuriteStanza is daliboStanza with its Azure repository on an emulator,
// path-style.
func azuriteStanza() *unstructured.Unstructured {
	stanza := daliboStanza()
	repositories, _, _ := unstructured.NestedSlice(stanza.Object, "spec", "stanzaConfiguration", "azureRepositories")
	repository := repositories[0].(map[string]any)
	repository["endpoint"], repository["uriStyle"] = "azurite.storage.svc:10000", "path"
	_ = unstructured.SetNestedSlice(stanza.Object, repositories, "spec", "stanzaConfiguration", "azureRepositories")
	return stanza
}

func pgBackRestSecrets() []*corev1.Secret {
	minio := testSecret()
	minio.Data["ENCRYPTION_KEY"] = []byte(cipherCanary)
	return []*corev1.Secret{
		minio,
		{ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: namespace}, Data: map[string][]byte{"KEY": []byte("YXp1cmUta2V5")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "minio-ca", Namespace: namespace}, Data: map[string][]byte{"ca.crt": []byte("opera-ca")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "own-ca", Namespace: namespace}, Data: map[string][]byte{"ca.crt": []byte("own-ca")}},
	}
}

func pgBackRestResolver(t *testing.T, cluster *cnpgv1.Cluster, object *unstructured.Unstructured, settings *backupresolve.PgBackRestSettings, allowed ...string) *Resolver {
	t.Helper()
	return pinnedPgBackRestResolver(t, cluster, object, settings, "", allowed...)
}

func pinnedPgBackRestResolver(t *testing.T, cluster *cnpgv1.Cluster, object *unstructured.Unstructured, settings *backupresolve.PgBackRestSettings, pin string, allowed ...string) *Resolver {
	t.Helper()
	reads := 0
	resolver, err := New(Options{
		Reader: readerOf(t, cluster, object, false, &reads, pgBackRestSecrets()...), Namespace: namespace, Cluster: "orders",
		AllowedSecrets: allowed, PgBackRest: settings, PgBackRestEndpoint: pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

// TestResolvePgBackRest: each plugin's object resolves to one stanza of
// one repository, with the cipher pass and the CA read only through the
// allow-list, and the watched object recorded.
func TestResolvePgBackRest(t *testing.T) {
	opera := pgBackRestCluster("pgbackrest.cnpg.opera.com", "pgbackrestObjectName", "orders-archive")
	dalibo := pgBackRestCluster("pgbackrest.dalibo.com", "stanzaRef", "orders-stanza")
	ownCA := &backupresolve.PgBackRestSettings{EndpointCA: &backupresolve.SecretKeyRef{Name: "own-ca", Key: "ca.crt"}}
	for _, test := range []struct {
		name     string
		resolver *Resolver
		check    func(Outcome) bool
		failure  string
	}{
		{
			"Opera, split stanza, cipher pass and the Archive's CA",
			pgBackRestResolver(t, opera, operaArchive(true), nil, "minio", "minio-ca"),
			func(o Outcome) bool {
				r := o.Repository
				return r.Format == config.FormatPGBackRest && r.Stanza == "orders" && r.ArchiveStanza == "orders-wal" && r.Repository == 1 &&
					r.DestinationPath == "s3://backups/orders" && r.EndpointURL == "https://minio.storage.svc:9000" && r.Region == "us-east-1" &&
					string(r.CipherPass) == cipherCanary && string(r.EndpointCABundle) == "opera-ca" && r.Keys != nil &&
					o.PgBackRest != nil && o.PgBackRest.Kind == "Archive" && o.PgBackRest.ObjectName == "orders-archive" && o.ObjectStoreName == "" &&
					r.Retention != nil && *r.Retention == config.PGBackRestRetention{Full: 7, FullType: "time", Archive: 3, ArchiveType: "full"}
			}, "",
		},
		{
			"the agent's own endpointCA wins over the Archive's",
			pgBackRestResolver(t, opera, operaArchive(true), ownCA, "minio", "own-ca"),
			func(o Outcome) bool { return string(o.Repository.EndpointCABundle) == "own-ca" }, "",
		},
		{
			"Dalibo repo2 is the Azure repository",
			pgBackRestResolver(t, dalibo, daliboStanza(), &backupresolve.PgBackRestSettings{Repository: 2}, "azure"),
			func(o Outcome) bool {
				r := o.Repository
				return r.Format == config.FormatPGBackRest && r.Provider == "azure" && r.Stanza == "main" && r.ArchiveStanza == "" && r.Repository == 2 &&
					r.DestinationPath == "azure://backups/cluster-sample" && r.AzureAccount == "stsample" && string(r.AzureAccountKey) == "YXp1cmUta2V5" &&
					r.CipherPass == nil && o.PgBackRest.Kind == "Stanza" &&
					// No retentionPolicy on the Azure repository: unset, not absent.
					r.Retention != nil && *r.Retention == config.PGBackRestRetention{FullType: "count", ArchiveType: "full"}
			}, "",
		},
		{
			"the Archive's Secrets outside the allow-list are refused by name, unread",
			pgBackRestResolver(t, opera, operaArchive(false), nil, "minio-ca"),
			nil, "Archive orders-archive references Secret minio, which this agent may not read",
		},
		{
			"a repository past the Stanza's",
			pgBackRestResolver(t, dalibo, daliboStanza(), &backupresolve.PgBackRestSettings{Repository: 3}, "minio", "azure"),
			nil, "repo3",
		},
		{
			"the endpoint pin admits the Archive's endpoint",
			pinnedPgBackRestResolver(t, opera, operaArchive(false), nil, "minio.storage.svc:9000", "minio"),
			func(o Outcome) bool { return o.Repository.EndpointURL == "https://minio.storage.svc:9000" }, "",
		},
		{
			// Whoever edits the Archive does not choose
			// where the credentials go.
			"the endpoint pin refuses another endpoint",
			pinnedPgBackRestResolver(t, opera, operaArchive(false), nil, "minio.storage.svc:9443", "minio"),
			nil, "is not the pinned endpoint minio.storage.svc:9443",
		},
		{
			"an Azure key path-style off the Azure Blob hosts, unpinned",
			pgBackRestResolver(t, dalibo, azuriteStanza(), &backupresolve.PgBackRestSettings{Repository: 2}, "azure"),
			nil, "not an Azure Blob host",
		},
		{
			"an Azure key path-style, pinned",
			pinnedPgBackRestResolver(t, dalibo, azuriteStanza(), &backupresolve.PgBackRestSettings{Repository: 2}, "azurite.storage.svc:10000", "azure"),
			func(o Outcome) bool {
				return o.Repository.Provider == "azure" && o.Repository.AzureAccount == "stsample"
			}, "",
		},
		{
			"a missing Archive",
			pgBackRestResolver(t, opera, nil, nil, "minio"),
			nil, "Archive orders-archive was not found",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome := test.resolver.Resolve(context.Background())
			if test.failure != "" {
				if outcome.Failure == nil || !strings.Contains(outcome.Failure.Error(), test.failure) {
					t.Fatalf("failure = %v, want %q", outcome.Failure, test.failure)
				}
				if strings.Contains(outcome.Failure.Error(), cipherCanary) {
					t.Fatal("the failure quotes the cipher pass")
				}
				if outcome.PgBackRest == nil {
					t.Fatal("a failed resolution of a pgBackRest Cluster watches no object")
				}
				return
			}
			if outcome.Failure != nil {
				t.Fatal(outcome.Failure)
			}
			if !test.check(outcome) {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}

// TestPgBackRestStoreReaderAgreesWithTheAPI: the watch resolves the same
// Archive to the same hash, a change of it changes the hash, and another
// kind of the same name is not answered from memory.
func TestPgBackRestStoreReaderAgreesWithTheAPI(t *testing.T) {
	cluster := pgBackRestCluster("pgbackrest.cnpg.opera.com", "pgbackrestObjectName", "orders-archive")
	archive := operaArchive(false)
	resolver := pgBackRestResolver(t, cluster, archive, nil, "minio")
	outcome := resolver.Resolve(context.Background())
	if outcome.Failure != nil {
		t.Fatal(outcome.Failure)
	}
	unstructuredCluster, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
	if err != nil {
		t.Fatal(err)
	}
	clusters, archives := cache.NewStore(cache.MetaNamespaceKeyFunc), cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := clusters.Add(&unstructured.Unstructured{Object: unstructuredCluster}); err != nil {
		t.Fatal(err)
	}
	if err := archives.Add(archive.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	resource, gvk, name, ok := outcome.watchedObject()
	if !ok || resource != ArchiveResource || name != "orders-archive" {
		t.Fatalf("watched object = %v %s %v", resource, name, ok)
	}
	stores := storeReader{namespace: namespace, clusters: clusters, objectStores: archives, objectGVK: gvk, objectStoreName: name, objectResource: resource}
	if current, err := resolver.locate(context.Background(), stores); err != nil || current.hash() != outcome.Hash {
		t.Fatalf("store hash differs from API hash (err %v)", err)
	}
	changed := archive.DeepCopy()
	changed.Object["spec"].(map[string]any)["configuration"].(map[string]any)["stanza"] = "elsewhere"
	if err := archives.Update(changed); err != nil {
		t.Fatal(err)
	}
	if current, err := resolver.locate(context.Background(), stores); err != nil || current.hash() == outcome.Hash {
		t.Fatalf("a changed WAL stanza kept the hash (err %v)", err)
	}
	stanzaStores := stores
	stanzaStores.objectGVK = discovery.StanzaGVK
	if _, err := resolver.locate(context.Background(), stanzaStores); !errors.Is(err, errNotWatched) {
		t.Fatalf("a Stanza store answered an Archive read: %v", err)
	}
}

// TestWaitReturnsWhenTheArchiveChanges drives the watch on a fake dynamic
// client: a change of the Archive the agent reads returns a reason.
func TestWaitReturnsWhenTheArchiveChanges(t *testing.T) {
	cluster := pgBackRestCluster("pgbackrest.cnpg.opera.com", "pgbackrestObjectName", "orders-archive")
	archive := operaArchive(false)
	resolver := pgBackRestResolver(t, cluster, archive, nil, "minio")
	outcome := resolver.Resolve(context.Background())
	if outcome.Failure != nil {
		t.Fatal(outcome.Failure)
	}
	unstructuredCluster, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
	if err != nil {
		t.Fatal(err)
	}
	clusterObject := &unstructured.Unstructured{Object: unstructuredCluster}
	clusterObject.SetAPIVersion("postgresql.cnpg.io/v1")
	clusterObject.SetKind("Cluster")
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		ClusterResource: "ClusterList", ArchiveResource: "ArchiveList",
	}, clusterObject, archive.DeepCopy())
	watcher, err := NewWatcher(dynamicClient, resolver)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	reasons := make(chan string, 1)
	go func() { reasons <- watcher.Wait(ctx, outcome) }()

	changed := archive.DeepCopy()
	changed.Object["spec"].(map[string]any)["configuration"].(map[string]any)["stanza"] = "elsewhere"
	// Before or after the informer's first list: either way the resolution
	// from the stores differs from outcome.
	if _, err := dynamicClient.Resource(ArchiveResource).Namespace(namespace).Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-reasons:
		if reason == "" {
			t.Fatal("Wait returned no reason")
		}
	case <-ctx.Done():
		t.Fatal("Wait did not see the Archive change")
	}
}
