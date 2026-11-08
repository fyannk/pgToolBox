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
	"encoding/base64"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/backupresolve"
	"github.com/fyannk/pgToolBox/internal/evidence/config"
)

// TestResolveSSECustomerKey: the SSE-C key is one more allow-listed Secret,
// read once at start and re-checked like every value the process is built
// with, and it reaches the configuration as the raw key.
func TestResolveSSECustomerKey(t *testing.T) {
	raw := bytes.Repeat([]byte{0x3c}, 32)
	encoded := base64.StdEncoding.EncodeToString(raw)
	store := testObjectStore("minio")
	credentials := store.Object["spec"].(map[string]any)["configuration"].(map[string]any)["s3Credentials"].(map[string]any)
	credentials["sseCustomerKey"] = map[string]any{"name": "sse-c", "key": "key.b64"}
	sseSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sse-c", Namespace: namespace},
		Data:       map[string][]byte{"key.b64": []byte(encoded + "\n"), "other": []byte("synthetic-canary")},
	}
	reads := 0
	api := reader(t, store, false, &reads, testSecret(), sseSecret)
	resolver := newResolver(t, api, "minio", "sse-c")
	outcome := resolver.Resolve(context.Background())
	if outcome.Failure != nil || string(outcome.Repository.S3SSECustomerKey) != encoded || outcome.Repository.Keys == nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if len(outcome.secrets) != 1 || outcome.secrets[0] != (backupresolve.SecretKeyRef{Name: "sse-c", Key: "key.b64"}) {
		t.Fatalf("re-checked values = %v, want the SSE-C key only", outcome.secrets)
	}
	base, err := config.Load(func(key string) string {
		return map[string]string{
			"RUNTIME_MODE": "agent", "AGENT_AUTH": "sharedtoken", "AGENT_TOKEN_FILE": "/var/run/token", "AGENT_PLAIN_HTTP": "true",
			"AGENT_RESOLVE": "cluster", "AGENT_NAME": "orders", "AGENT_NAMESPACE": namespace, "CNPG_CLUSTER_NAME": "orders",
			"AGENT_ALLOWED_SECRETS": "minio,sse-c",
		}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.ApplyResolved(base, outcome.Repository, 1, outcome.Failure)
	if cfg.RepositoryError != nil || !bytes.Equal(cfg.Credentials.S3SSECustomerKey.Bytes(), raw) {
		t.Fatalf("configuration = %v", cfg.RepositoryError)
	}

	// A rotated key changes what the process was built with: restart.
	sseSecret.Data["key.b64"] = []byte(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x3d}, 32)))
	if err := api.Update(context.Background(), sseSecret); err != nil {
		t.Fatal(err)
	}
	if changed, err := resolver.secretsChanged(context.Background(), outcome); err != nil || !changed {
		t.Fatalf("a changed SSE-C key was not noticed: %v %v", changed, err)
	}

	// The allow-list holds for it as for every Secret: nothing is read.
	reads = 0
	refused := newResolver(t, reader(t, store, false, &reads, testSecret(), sseSecret), "minio").Resolve(context.Background())
	var notAllowed *NotAllowedError
	if !errors.As(refused.Failure, &notAllowed) || len(notAllowed.Secrets) != 1 || notAllowed.Secrets[0] != "sse-c" || reads != 0 {
		t.Fatalf("allow-list: %v (%d Secret reads)", refused.Failure, reads)
	}
}
