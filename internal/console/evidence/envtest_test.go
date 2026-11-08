//go:build envtest

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

package evidence_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/evidence"
)

// TestStatusWriterOnEnvtest runs the console's real cache against an API
// server: the EvidenceAgent informer keeps the status-subresource
// managedFields (and nothing else), a status the operator's field manager
// wrote is trusted, a status another manager rewrote is not — on the
// API server's own ownership semantics, not a fake's.
func TestStatusWriterOnEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; run through make test-envtest-security")
	}
	environment := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "..", "test", "crds", "cnpg-1.30"),
		},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err := environment.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = environment.Stop() })
	scheme, err := cache.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	must(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}))
	agent := &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a"},
		Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}},
	}
	must(t, admin.Create(ctx, agent, client.FieldOwner("pgtoolbox")))

	// The operator's status write: a merge patch on the status
	// subresource under its status field manager (internal/operator/
	// evidenceagent/status.go).
	writeStatus := func(manager, endpoint string) {
		t.Helper()
		current := &pgtoolboxv1alpha1.EvidenceAgent{}
		must(t, admin.Get(ctx, client.ObjectKeyFromObject(agent), current))
		original := current.DeepCopy()
		current.Status.Endpoint = endpoint
		current.Status.CABundle = []byte("-----BEGIN CERTIFICATE-----\nMA==\n-----END CERTIFICATE-----\n")
		must(t, admin.Status().Patch(ctx, current, client.MergeFrom(original), client.FieldOwner(manager)))
	}
	writeStatus(evidence.OperatorStatusManager, "https://orders-evidence.team-a.svc:8443")

	manager, err := cache.New(cache.Options{
		Config: config.Console{
			WatchNamespaces: []string{"team-a"}, ClusterSelector: labels.Everything(),
			APIRequestTimeout: 10 * time.Second, ResyncPeriod: 10 * time.Hour, ReprobeInterval: time.Second,
		},
		RestConfig: restConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = manager.Run(ctx) }()

	cached := func() *pgtoolboxv1alpha1.EvidenceAgent {
		var list pgtoolboxv1alpha1.EvidenceAgentList
		if err := manager.Reader().List(ctx, &list, client.InNamespace("team-a")); err != nil || len(list.Items) != 1 {
			return nil
		}
		return &list.Items[0]
	}
	eventually(t, "the operator-written status is trusted from the cache", func() bool {
		item := cached()
		return item != nil && evidence.StatusWrittenByOperator(item)
	})
	for _, entry := range cached().ManagedFields {
		if entry.Subresource != "status" {
			t.Fatalf("the cache kept a non-status managedFields entry: %+v", entry)
		}
	}

	// Someone with patch on evidenceagents/status points the endpoint
	// elsewhere under their own field manager: the API server moves
	// ownership of status.endpoint to them, and the console stops trusting
	// the status.
	writeStatus("kubectl-edit", "https://attacker.team-a.svc:8443")
	eventually(t, "a status rewritten by another manager is not trusted", func() bool {
		item := cached()
		return item != nil && item.Status.Endpoint == "https://attacker.team-a.svc:8443" && !evidence.StatusWrittenByOperator(item)
	})

	// The documented limit: a writer that names itself after the operator
	// is trusted again. The per-namespace token audience bounds what that
	// buys to this namespace's agents.
	writeStatus(evidence.OperatorStatusManager, "https://impersonated.team-a.svc:8443")
	eventually(t, "a writer claiming the operator's manager name is indistinguishable", func() bool {
		item := cached()
		return item != nil && item.Status.Endpoint == "https://impersonated.team-a.svc:8443" && evidence.StatusWrittenByOperator(item)
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}
