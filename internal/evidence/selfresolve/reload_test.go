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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testCA = "-----BEGIN CERTIFICATE-----\nsynthetic\n-----END CERTIFICATE-----"

// storeWithCA is the S3 ObjectStore with an endpoint CA and a region
// Secret, the two values read once at start.
func storeWithCA() *unstructured.Unstructured {
	store := testObjectStore("minio")
	configuration := store.Object["spec"].(map[string]any)["configuration"].(map[string]any)
	configuration["endpointCA"] = map[string]any{"name": "minio-ca", "key": "ca.crt"}
	configuration["s3Credentials"].(map[string]any)["region"] = map[string]any{"name": "minio", "key": "REGION"}
	return store
}

func secretsWithCA() []*corev1.Secret {
	keys := testSecret()
	keys.Data["REGION"] = []byte("eu-west-3")
	return []*corev1.Secret{keys, {
		ObjectMeta: metav1.ObjectMeta{Name: "minio-ca", Namespace: namespace},
		Data:       map[string][]byte{"ca.crt": []byte(testCA)},
	}}
}

// unanswered fails every Secret read as an API that does not answer.
type unanswered struct{ client.Reader }

func (u unanswered) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Secret); ok {
		return errors.New("connection refused")
	}
	return u.Reader.Get(ctx, key, object, options...)
}

// TestSecretsChanged covers every Secret value the process is built with:
// a change or a disappearance is seen, the S3 key pair (followed at run
// time) and an API that does not answer are not changes.
func TestSecretsChanged(t *testing.T) {
	for name, test := range map[string]struct {
		mutate     func(context.Context, client.Client) error
		unanswered bool
		changed    bool
	}{
		"nothing changed": {mutate: func(context.Context, client.Client) error { return nil }},
		"the endpoint CA changed": {changed: true, mutate: func(ctx context.Context, api client.Client) error {
			return patchSecret(ctx, api, "minio-ca", "ca.crt", []byte(testCA+"\n"+testCA))
		}},
		"the region changed": {changed: true, mutate: func(ctx context.Context, api client.Client) error {
			return patchSecret(ctx, api, "minio", "REGION", []byte("eu-west-1"))
		}},
		"the CA key was removed": {changed: true, mutate: func(ctx context.Context, api client.Client) error {
			return patchSecret(ctx, api, "minio-ca", "ca.crt", nil)
		}},
		"the CA Secret was deleted": {changed: true, mutate: func(ctx context.Context, api client.Client) error {
			return api.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "minio-ca", Namespace: namespace}})
		}},
		"only the key pair rotated": {mutate: func(ctx context.Context, api client.Client) error {
			return patchSecret(ctx, api, "minio", "ACCESS_SECRET_KEY", []byte("rotated"))
		}},
		"the API does not answer": {unanswered: true, mutate: func(context.Context, client.Client) error { return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			reads := 0
			api := reader(t, storeWithCA(), false, &reads, secretsWithCA()...)
			resolver := newResolver(t, api, "minio", "minio-ca")
			outcome := resolver.Resolve(ctx)
			if outcome.Failure != nil {
				t.Fatal(outcome.Failure)
			}
			if len(outcome.secrets) != 2 || outcome.Repository.Region != "eu-west-3" || string(outcome.Repository.EndpointCABundle) != testCA {
				t.Fatalf("recorded %v, region %q", outcome.secrets, outcome.Repository.Region)
			}
			if bytes.Contains(outcome.secretDigest, []byte(testCA)) {
				t.Fatal("the digest holds a value")
			}
			if err := test.mutate(ctx, api); err != nil {
				t.Fatal(err)
			}
			if test.unanswered {
				resolver.reader = unanswered{resolver.reader}
			}
			changed, err := resolver.secretsChanged(ctx, outcome)
			if changed != test.changed {
				t.Fatalf("changed = %v, want %v (err %v)", changed, test.changed, err)
			}
			if test.unanswered != (err != nil) {
				t.Fatalf("err = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), testCA) {
				t.Fatal("the error holds a value")
			}
		})
	}
}

func patchSecret(ctx context.Context, api client.Client, name, key string, value []byte) error {
	var secret corev1.Secret
	if err := api.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret); err != nil {
		return err
	}
	if value == nil {
		delete(secret.Data, key)
	} else {
		secret.Data[key] = value
	}
	return api.Update(ctx, &secret)
}

// TestDigestIsKeyedPerProcess: two resolvers never produce the same digest
// of the same values, so a digest is useless outside its process.
func TestDigestIsKeyedPerProcess(t *testing.T) {
	reads := 0
	api := reader(t, storeWithCA(), false, &reads, secretsWithCA()...)
	first := newResolver(t, api, "minio", "minio-ca").Resolve(context.Background())
	second := newResolver(t, api, "minio", "minio-ca").Resolve(context.Background())
	if len(first.secretDigest) == 0 || bytes.Equal(first.secretDigest, second.secretDigest) {
		t.Fatal("the digest is not keyed per process")
	}
}

// TestWaitReturnsWhenASecretChanges drives the recheck through Wait.
func TestWaitReturnsWhenASecretChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	reads := 0
	api := reader(t, storeWithCA(), false, &reads, secretsWithCA()...)
	resolver := newResolver(t, api, "minio", "minio-ca")
	outcome := resolver.Resolve(ctx)
	if outcome.Failure != nil {
		t.Fatal(outcome.Failure)
	}
	cluster, err := runtime.DefaultUnstructuredConverter.ToUnstructured(testCluster())
	if err != nil {
		t.Fatal(err)
	}
	clusterObject := &unstructured.Unstructured{Object: cluster}
	clusterObject.SetAPIVersion("postgresql.cnpg.io/v1")
	clusterObject.SetKind("Cluster")
	watcher, err := NewWatcher(dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		ClusterResource: "ClusterList", ObjectStoreResource: "ObjectStoreList",
	}, clusterObject, storeWithCA()), resolver)
	if err != nil {
		t.Fatal(err)
	}
	watcher.recheck = 50 * time.Millisecond
	reasons := make(chan string, 1)
	go func() { reasons <- watcher.Wait(ctx, outcome) }()
	select {
	case reason := <-reasons:
		t.Fatalf("Wait returned %q with no change", reason)
	case <-time.After(300 * time.Millisecond):
	}
	if err := patchSecret(ctx, api, "minio-ca", "ca.crt", []byte(testCA+"\n")); err == nil {
		// A trailing newline is trimmed: still the same value.
		select {
		case reason := <-reasons:
			t.Fatalf("Wait returned %q on a trimmed newline", reason)
		case <-time.After(300 * time.Millisecond):
		}
	} else {
		t.Fatal(err)
	}
	if err := patchSecret(ctx, api, "minio-ca", "ca.crt", []byte(testCA+"\n"+testCA)); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-reasons:
		if !strings.Contains(reason, "Secret") {
			t.Fatalf("reason = %q", reason)
		}
	case <-ctx.Done():
		t.Fatal("Wait did not see the CA change")
	}
}
