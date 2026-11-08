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
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	barmanapi "github.com/cloudnative-pg/barman-cloud/pkg/api"
	machineryapi "github.com/cloudnative-pg/machinery/pkg/api"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fyannk/pgToolBox/internal/backupresolve"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

const namespace = "team-a"

// secretReadsMutex guards the Secret read counters: the watch tests read
// Secrets from the Watcher's goroutine and the test's at once.
var secretReadsMutex sync.Mutex

func testCluster() *cnpgv1.Cluster {
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: namespace, UID: "uid-1"},
		Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: backupresolve.BarmanPluginName, Parameters: map[string]string{backupresolve.BarmanObjectParameter: "store"},
		}}},
	}
}

func testObjectStore(secret string) *unstructured.Unstructured {
	store := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"configuration": map[string]any{
		"destinationPath": "s3://backups/team-a",
		"endpointURL":     "https://minio.storage.svc:9000",
		"s3Credentials": map[string]any{
			"accessKeyId":     map[string]any{"name": secret, "key": "ACCESS_KEY_ID"},
			"secretAccessKey": map[string]any{"name": secret, "key": "ACCESS_SECRET_KEY"},
		},
	}}}}
	store.SetGroupVersionKind(discovery.ObjectStoreGVK)
	store.SetNamespace(namespace)
	store.SetName("store")
	return store
}

func testSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio", Namespace: namespace},
		Data:       map[string][]byte{"ACCESS_KEY_ID": []byte("access\n"), "ACCESS_SECRET_KEY": []byte("secret")},
	}
}

// reader builds a fake API with the Cluster, the ObjectStore and the given
// Secrets; it counts Secret reads and can refuse them.
func reader(t *testing.T, store *unstructured.Unstructured, forbidSecrets bool, secretReads *int, secrets ...*corev1.Secret) client.Client {
	t.Helper()
	return readerOf(t, testCluster(), store, forbidSecrets, secretReads, secrets...)
}

// readerOf is reader with another Cluster.
func readerOf(t *testing.T, cluster *cnpgv1.Cluster, store *unstructured.Unstructured, forbidSecrets bool, secretReads *int, secrets ...*corev1.Secret) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := make([]client.Object, 0, 1+len(secrets))
	objects = append(objects, cluster)
	for _, secret := range secrets {
		objects = append(objects, secret)
	}
	built := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, ok := object.(*corev1.Secret); ok {
				secretReadsMutex.Lock()
				*secretReads++
				secretReadsMutex.Unlock()
				if forbidSecrets {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("denied"))
				}
			}
			return c.Get(ctx, key, object, options...)
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			t.Error("the resolver listed objects")
			return errors.New("no list")
		},
	}).Build()
	if store != nil {
		if err := built.Create(context.Background(), store.DeepCopy()); err != nil {
			t.Fatal(err)
		}
	}
	return built
}

func newResolver(t *testing.T, api client.Reader, allowed ...string) *Resolver {
	t.Helper()
	resolver, err := New(Options{Reader: api, Namespace: namespace, Cluster: "orders", AllowedSecrets: allowed})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func TestResolveReadsTheRepositoryAndFollowsTheKeys(t *testing.T) {
	reads := 0
	secret := testSecret()
	api := reader(t, testObjectStore("minio"), false, &reads, secret)
	outcome := newResolver(t, api, "minio").Resolve(context.Background())
	if outcome.Failure != nil {
		t.Fatalf("Resolve failed: %v", outcome.Failure)
	}
	repository := outcome.Repository
	if repository.ClusterUID != "uid-1" || repository.ServerName != "orders" || repository.DestinationPath != "s3://backups/team-a" ||
		repository.EndpointURL != "https://minio.storage.svc:9000" || outcome.ObjectStoreName != "store" || outcome.RetryAfter != 0 || outcome.Hash == "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	access, secretKey, token, err := repository.Keys.Keys(context.Background())
	if err != nil || string(access) != "access" || string(secretKey) != "secret" || token != nil {
		t.Fatalf("keys = %q %q %q %v", access, secretKey, token, err)
	}
	// A rotated Secret is read on the next call, without a new resolution.
	secret.Data["ACCESS_KEY_ID"] = []byte("rotated")
	if err := api.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if access, _, _, err := repository.Keys.Keys(context.Background()); err != nil || string(access) != "rotated" {
		t.Fatalf("rotated key = %q, %v", access, err)
	}
}

// TestKeysReportRotationAndReadFailures: a rotation the agent follows, and
// a read it cannot make (the last keys stay in use), are both logged by
// Secret name only, so an operator can tell whether a rotation landed.
func TestKeysReportRotationAndReadFailures(t *testing.T) {
	reads := 0
	secret := testSecret()
	api := reader(t, testObjectStore("minio"), false, &reads, secret)
	var logs bytes.Buffer
	resolver, err := New(Options{Reader: api, Namespace: namespace, Cluster: "orders", AllowedSecrets: []string{"minio"}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	outcome := resolver.Resolve(context.Background())
	if outcome.Failure != nil {
		t.Fatal(outcome.Failure)
	}
	keys := outcome.Repository.Keys
	for range 2 {
		if _, _, _, err := keys.Keys(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(logs.String(), "changed") {
		t.Fatalf("unchanged keys were reported as rotated: %s", logs.String())
	}
	secret.Data["ACCESS_SECRET_KEY"] = []byte("rotated-secret-value")
	if err := api.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := keys.Keys(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), "the S3 keys changed in their Secrets") != 1 {
		t.Fatalf("the rotation was not reported once: %s", logs.String())
	}
	if err := api.Delete(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := keys.Keys(context.Background()); err == nil {
		t.Fatal("a deleted Secret was read")
	}
	if !strings.Contains(logs.String(), "the S3 keys could not be read again") || !strings.Contains(logs.String(), "minio") {
		t.Fatalf("the failed read was not reported by name: %s", logs.String())
	}
	if strings.Contains(logs.String(), "rotated-secret-value") || strings.Contains(logs.String(), "secret\"") {
		t.Fatalf("a key value reached the log: %s", logs.String())
	}
}

func TestResolveRefusesSecretsOutsideTheAllowList(t *testing.T) {
	reads := 0
	api := reader(t, testObjectStore("minio"), false, &reads, testSecret())
	outcome := newResolver(t, api, "other").Resolve(context.Background())
	var refused *NotAllowedError
	if !errors.As(outcome.Failure, &refused) {
		t.Fatalf("failure = %v, want NotAllowedError", outcome.Failure)
	}
	message := outcome.Failure.Error()
	if !strings.Contains(message, "Secret minio") || !strings.Contains(message, "evidence.agents[].secrets") || !strings.Contains(message, "AGENT_ALLOWED_SECRETS") {
		t.Fatalf("message %q does not name the Secret and the value to add", message)
	}
	if reads != 0 {
		t.Fatalf("the resolver read %d Secrets outside the allow-list", reads)
	}
	// The refusal heals only when the values change (a new Pod), or the
	// ObjectStore changes (the watch): no retry timer.
	if outcome.RetryAfter != 0 || outcome.ObjectStoreName != "store" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestResolveFailures(t *testing.T) {
	tests := map[string]struct {
		store     *unstructured.Unstructured
		secrets   []*corev1.Secret
		forbidden bool
		want      string
		retry     bool
	}{
		"missing Secret":      {store: testObjectStore("minio"), want: "the Secret minio was not found", retry: true},
		"forbidden Secret":    {store: testObjectStore("minio"), secrets: []*corev1.Secret{testSecret()}, forbidden: true, want: "the chart's Role must list it", retry: true},
		"missing key":         {store: testObjectStore("minio"), secrets: []*corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: "minio", Namespace: namespace}}}, want: "has no key ACCESS_KEY_ID", retry: true},
		"missing ObjectStore": {want: "store"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			reads := 0
			api := reader(t, test.store, test.forbidden, &reads, test.secrets...)
			outcome := newResolver(t, api, "minio").Resolve(context.Background())
			if outcome.Failure == nil || !strings.Contains(outcome.Failure.Error(), test.want) {
				t.Fatalf("failure = %v, want %q", outcome.Failure, test.want)
			}
			if (outcome.RetryAfter > 0) != test.retry {
				t.Fatalf("RetryAfter = %v, want retry %v", outcome.RetryAfter, test.retry)
			}
			if strings.Contains(outcome.Failure.Error(), "access") || strings.Contains(outcome.Failure.Error(), "secret\"") {
				t.Fatalf("failure %q leaks a value", outcome.Failure)
			}
		})
	}
}

func TestNewRejectsIncompleteOptions(t *testing.T) {
	if _, err := New(Options{Namespace: namespace, Cluster: "orders", AllowedSecrets: []string{"a"}}); err == nil {
		t.Fatal("New accepted a nil reader")
	}
	reads := 0
	if _, err := New(Options{Reader: reader(t, nil, false, &reads), Cluster: "orders"}); err == nil {
		t.Fatal("New accepted an empty namespace")
	}
}

func TestEpochIsPositiveAndMonotonic(t *testing.T) {
	if Epoch(time.Unix(0, 0)) != 1 || Epoch(time.Unix(-5, 0)) != 1 {
		t.Fatal("epoch before 1970 is not 1")
	}
	if Epoch(time.Unix(1_800_000_000, 0)) >= Epoch(time.Unix(1_800_000_001, 0)) {
		t.Fatal("epoch does not grow")
	}
}

// TestStoreReaderAgreesWithTheAPI pins the equivalence the watch relies on:
// the same objects resolve to the same hash from the API and from the
// informer stores, and a change of the ObjectStore changes the hash.
func TestStoreReaderAgreesWithTheAPI(t *testing.T) {
	reads := 0
	store := testObjectStore("minio")
	api := reader(t, store, false, &reads, testSecret())
	resolver := newResolver(t, api, "minio")
	outcome := resolver.Resolve(context.Background())
	if outcome.Failure != nil {
		t.Fatal(outcome.Failure)
	}

	cluster, err := runtime.DefaultUnstructuredConverter.ToUnstructured(testCluster())
	if err != nil {
		t.Fatal(err)
	}
	clusters := cache.NewStore(cache.MetaNamespaceKeyFunc)
	objectStores := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := clusters.Add(&unstructured.Unstructured{Object: cluster}); err != nil {
		t.Fatal(err)
	}
	if err := objectStores.Add(store.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	stores := storeReader{
		namespace: namespace, clusters: clusters, objectStores: objectStores, objectStoreName: "store",
		objectGVK: discovery.ObjectStoreGVK, objectResource: ObjectStoreResource,
	}
	current, err := resolver.locate(context.Background(), stores)
	if err != nil || current.hash() != outcome.Hash {
		t.Fatalf("store hash differs from API hash (err %v)", err)
	}

	changed := testObjectStore("minio")
	changed.Object["spec"].(map[string]any)["configuration"].(map[string]any)["destinationPath"] = "s3://backups/elsewhere"
	if err := objectStores.Update(changed); err != nil {
		t.Fatal(err)
	}
	if current, err := resolver.locate(context.Background(), stores); err != nil || current.hash() == outcome.Hash {
		t.Fatalf("a changed destination kept the hash (err %v)", err)
	}

	if err := objectStores.Delete(changed); err != nil {
		t.Fatal(err)
	}
	if current, err := resolver.locate(context.Background(), stores); err != nil || current.failure == nil || current.hash() == outcome.Hash {
		t.Fatalf("a deleted ObjectStore: %+v, %v", current, err)
	}

	// Another ObjectStore than the watched one is not answered from memory.
	other := storeReader{namespace: namespace, clusters: clusters}
	if _, err := resolver.locate(context.Background(), other); !errors.Is(err, errNotWatched) {
		t.Fatalf("unwatched ObjectStore: err = %v", err)
	}
}

// inTreeCluster backs up through the in-tree spec.backup.barmanObjectStore,
// without the plugin.
func inTreeCluster(secret string) *cnpgv1.Cluster {
	cluster := testCluster()
	cluster.Spec.Plugins = nil
	cluster.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{
		DestinationPath: "s3://backups/in-tree", EndpointURL: "https://minio.storage.svc:9000", ServerName: "orders-v2",
	}}
	cluster.Spec.Backup.BarmanObjectStore.AWS = &barmanapi.S3Credentials{
		AccessKeyIDReference:     &machineryapi.SecretKeySelector{LocalObjectReference: machineryapi.LocalObjectReference{Name: secret}, Key: "ACCESS_KEY_ID"},
		SecretAccessKeyReference: &machineryapi.SecretKeySelector{LocalObjectReference: machineryapi.LocalObjectReference{Name: secret}, Key: "ACCESS_SECRET_KEY"},
	}
	return cluster
}

func TestResolveInTreeBarmanObjectStore(t *testing.T) {
	reads := 0
	cluster := inTreeCluster("minio")
	resolver := newResolver(t, readerOf(t, cluster, nil, false, &reads, testSecret()), "minio")
	outcome := resolver.Resolve(context.Background())
	if outcome.Failure != nil {
		t.Fatal(outcome.Failure)
	}
	if outcome.ObjectStoreName != "" || outcome.Repository.ServerName != "orders-v2" || outcome.Repository.DestinationPath != "s3://backups/in-tree" {
		t.Fatalf("outcome = %+v", outcome)
	}

	// The watch sees the in-tree configuration through the Cluster alone.
	unstructuredCluster, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
	if err != nil {
		t.Fatal(err)
	}
	clusters := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := clusters.Add(&unstructured.Unstructured{Object: unstructuredCluster}); err != nil {
		t.Fatal(err)
	}
	stores := storeReader{namespace: namespace, clusters: clusters}
	if current, err := resolver.locate(context.Background(), stores); err != nil || current.hash() != outcome.Hash {
		t.Fatalf("store hash differs from API hash (err %v)", err)
	}
	changed := cluster.DeepCopy()
	changed.Spec.Backup.BarmanObjectStore.DestinationPath = "s3://backups/elsewhere"
	unstructuredChanged, err := runtime.DefaultUnstructuredConverter.ToUnstructured(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := clusters.Update(&unstructured.Unstructured{Object: unstructuredChanged}); err != nil {
		t.Fatal(err)
	}
	if current, err := resolver.locate(context.Background(), stores); err != nil || current.hash() == outcome.Hash {
		t.Fatalf("a changed in-tree destination kept the hash (err %v)", err)
	}

	// The allow-list applies to the in-tree references too, and names where
	// they were read.
	refused := newResolver(t, readerOf(t, inTreeCluster("minio"), nil, false, &reads, testSecret()), "other").Resolve(context.Background())
	if refused.Failure == nil || !strings.Contains(refused.Failure.Error(), "Cluster orders spec.backup.barmanObjectStore references Secret minio") {
		t.Fatalf("failure = %v", refused.Failure)
	}
}

func TestResolveAzureAndGCSReadTheirSecretOnce(t *testing.T) {
	for name, test := range map[string]struct {
		configuration map[string]any
		check         func(Outcome) bool
	}{
		"Azure SAS token": {map[string]any{
			"destinationPath":  "https://orders.blob.core.windows.net/backups/team-a",
			"azureCredentials": map[string]any{"storageAccount": map[string]any{"name": "azure", "key": "ACCOUNT"}, "storageSasToken": map[string]any{"name": "azure", "key": "SAS"}},
		}, func(o Outcome) bool {
			return o.Repository.Provider == "azure" && o.Repository.DestinationPath == "azure://backups/team-a" && o.Repository.AzureAccount == "orders" &&
				string(o.Repository.AzureSASToken) == "sv=1&sig=synthetic" && o.Repository.Keys == nil
		}},
		"GCS JSON key": {map[string]any{
			"destinationPath":   "gs://backups/team-a",
			"googleCredentials": map[string]any{"applicationCredentials": map[string]any{"name": "azure", "key": "JSON"}},
		}, func(o Outcome) bool {
			return o.Repository.Provider == "gcs" && string(o.Repository.GoogleCredentialsJSON) == "{}"
		}},
		// barman-cloud uses no CA for GCS: the CA Secret (not allow-listed
		// here) is neither refused nor read.
		"GCS JSON key with an unused endpoint CA": {map[string]any{
			"destinationPath":   "gs://backups/team-a",
			"endpointCA":        map[string]any{"name": "not-listed-ca", "key": "ca.crt"},
			"googleCredentials": map[string]any{"applicationCredentials": map[string]any{"name": "azure", "key": "JSON"}},
		}, func(o Outcome) bool {
			return o.Repository.Provider == "gcs" && len(o.Repository.EndpointCABundle) == 0 && len(o.secrets) == 1
		}},
	} {
		t.Run(name, func(t *testing.T) {
			store := testObjectStore("unused")
			store.Object["spec"] = map[string]any{"configuration": test.configuration}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "azure", Namespace: namespace},
				Data:       map[string][]byte{"SAS": []byte("sv=1&sig=synthetic\n"), "JSON": []byte("{}")},
			}
			reads := 0
			outcome := newResolver(t, reader(t, store, false, &reads, secret), "azure").Resolve(context.Background())
			if outcome.Failure != nil || !test.check(outcome) || reads != 1 {
				t.Fatalf("outcome = %+v (%d Secret reads)", outcome, reads)
			}
			refused := newResolver(t, reader(t, store, false, &reads, secret), "other").Resolve(context.Background())
			var notAllowed *NotAllowedError
			if !errors.As(refused.Failure, &notAllowed) || notAllowed.Secrets[0] != "azure" {
				t.Fatalf("allow-list: %v", refused.Failure)
			}
		})
	}
}

func TestResolveWorkloadIdentityReadsNoSecret(t *testing.T) {
	store := testObjectStore("unused")
	store.Object["spec"] = map[string]any{"configuration": map[string]any{
		"destinationPath":   "gs://backups/team-a",
		"googleCredentials": map[string]any{"gkeEnvironment": true},
	}}
	reads := 0
	// Without the chart entry's opt-in, the node's identity is never used.
	refused := newResolver(t, reader(t, store, false, &reads), "unrelated").Resolve(context.Background())
	if refused.Failure == nil || !strings.Contains(refused.Failure.Error(), "evidence.agents[].workloadIdentity") {
		t.Fatalf("no opt-in: %v", refused.Failure)
	}
	resolver, err := New(Options{
		Reader: reader(t, store, false, &reads), Namespace: namespace, Cluster: "orders", AllowedSecrets: []string{"unrelated"}, WorkloadIdentity: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome := resolver.Resolve(context.Background())
	if outcome.Failure != nil || outcome.Repository.Credential != "ambient" || outcome.Repository.Provider != "gcs" || reads != 0 {
		t.Fatalf("outcome = %+v (%d Secret reads)", outcome, reads)
	}
}

// TestResolveAppliesTheChartsChoices: a plain-http endpoint needs the
// chart entry's allowInsecureEndpoint, and no two credential settings may
// name the same Secret key (the access key would publish the secret key).
func TestResolveAppliesTheChartsChoices(t *testing.T) {
	plain := testObjectStore("minio")
	plain.Object["spec"].(map[string]any)["configuration"].(map[string]any)["endpointURL"] = "http://garage.storage.svc:3900"
	reads := 0
	refused := newResolver(t, reader(t, plain, false, &reads, testSecret()), "minio").Resolve(context.Background())
	if refused.Failure == nil || !strings.Contains(refused.Failure.Error(), "allowInsecureEndpoint") || reads != 0 {
		t.Fatalf("plain http: %v (%d Secret reads)", refused.Failure, reads)
	}
	resolver, err := New(Options{
		Reader: reader(t, plain, false, &reads, testSecret()), Namespace: namespace, Cluster: "orders",
		AllowedSecrets: []string{"minio"}, AllowInsecureEndpoint: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome := resolver.Resolve(context.Background()); outcome.Failure != nil {
		t.Fatalf("admitted plain http: %v", outcome.Failure)
	}

	swapped := testObjectStore("minio")
	swapped.Object["spec"].(map[string]any)["configuration"].(map[string]any)["s3Credentials"].(map[string]any)["accessKeyId"] = map[string]any{"name": "minio", "key": "ACCESS_SECRET_KEY"}
	reads = 0
	refused = newResolver(t, reader(t, swapped, false, &reads, testSecret()), "minio").Resolve(context.Background())
	if refused.Failure == nil || !strings.Contains(refused.Failure.Error(), "for both accessKeyId and secretAccessKey") || reads != 0 {
		t.Fatalf("key slot swap: %v (%d Secret reads)", refused.Failure, reads)
	}
}
