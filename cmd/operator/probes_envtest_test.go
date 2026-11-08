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

package main

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestEnvtestReadinessFollowsTheCache proves the readiness check on a
// real API server: an operator whose informer cannot list (RBAC-starved) is
// alive but not ready; once the API server lets it list, the informer
// syncs and /readyz turns 200 without a restart.
//
// Needs KUBEBUILDER_ASSETS (make test-envtest-manager).
func TestEnvtestReadinessFollowsTheCache(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-manager)")
	}
	env := &envtest.Environment{}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin := kubernetes.NewForConfigOrDie(config)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	operator, err := env.AddUser(envtest.User{Name: "starved-operator"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	address := freeAddress(t)
	mgr, err := ctrl.NewManager(operator.Config(), ctrl.Options{
		Scheme: scheme, Metrics: metricsOptions("0", ""), HealthProbeBindAddress: address,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := addProbes(mgr); err != nil {
		t.Fatal(err)
	}
	// An informer the operator may not list yet, as a watch would start.
	if _, err := mgr.GetCache().GetInformer(ctx, &corev1.ConfigMap{}, cache.BlockUntilSynced(false)); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	client := &http.Client{Timeout: 2 * time.Second}
	probe := func(path string) int {
		response, err := client.Do(getRequest(t, "http://"+address+path))
		if err != nil {
			return 0
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	waitFor := func(path string, want int, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		got := probe(path)
		for got != want && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			got = probe(path)
		}
		if got != want {
			t.Fatalf("%s: HTTP %d, want %d", path, got, want)
		}
	}

	waitFor("/healthz", http.StatusOK, 30*time.Second)
	// Alive, yet not ready, for as long as the list is refused.
	for range 5 {
		if got := probe("/readyz"); got == http.StatusOK {
			t.Fatal("/readyz is 200 while the informer cannot list")
		}
		time.Sleep(200 * time.Millisecond)
	}

	mustCreate(t, func() error {
		_, err := admin.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "configmaps"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"list", "watch"}}},
		}, metav1.CreateOptions{})
		return err
	})
	mustCreate(t, func() error {
		_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "configmaps"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "configmaps"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "starved-operator"}},
		}, metav1.CreateOptions{})
		return err
	})
	// The reflector retries with backoff (capped at 30 s by client-go).
	waitFor("/readyz", http.StatusOK, 90*time.Second)
	waitFor("/healthz", http.StatusOK, 5*time.Second)
}

// getRequest builds a GET bound to the test's context, so a request still in
// flight when the test ends is cancelled with it.
func getRequest(t testing.TB, url string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
