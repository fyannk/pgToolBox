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

package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// crdDir is the vendored CRD set of one CNPG minor (CNPG_CRDS, default
// 1.30); make test-envtest-console runs the suite once per minor.
func crdDir(t *testing.T) (string, string) {
	t.Helper()
	dir := os.Getenv("CNPG_CRDS")
	if dir == "" {
		dir = "../../../test/crds/cnpg-1.30"
	}
	version, err := os.ReadFile(filepath.Join(dir, "VERSION"))
	if err != nil {
		t.Fatalf("reading %s/VERSION: %v", dir, err)
	}
	return dir, strings.TrimSpace(string(version))
}

type env struct {
	admin   client.Client
	console *rest.Config
	// servesDatabaseRoles: the CRD set has the DatabaseRole kind. Every
	// supported CloudNativePG (1.30 and later) serves it; a CNPG_CRDS
	// without it is an install that lacks the kind.
	servesDatabaseRoles bool
}

// start runs an API server with the CNPG CRDs, a watched namespace
// team-a, an unwatched team-b, and a "console" user holding a Role in
// team-a that grants reads on every console kind except poolers, and
// nothing on Secrets — the shape of the chart's Role, starved by one
// kind so the forbidden path is real RBAC, not a fake.
func start(t *testing.T) *env {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; run through make test-envtest-console")
	}
	dir, _ := crdDir(t)
	testEnv := &envtest.Environment{CRDDirectoryPaths: []string{dir}, ErrorIfCRDPathMissing: true}
	adminCfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(adminCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, ns := range []string{"team-a", "team-b"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	user, err := testEnv.AddUser(envtest.User{Name: "console"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	grant(ctx, t, admin, "console-read", []rbacv1.PolicyRule{
		{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{
			"clusters", "backups", "scheduledbackups", "databases", "databaseroles",
			"publications", "subscriptions", "imagecatalogs", "failoverquorums",
		}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods", "events", "persistentvolumeclaims"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"policy"}, Resources: []string{"poddisruptionbudgets"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"barmancloud.cnpg.io"}, Resources: []string{"objectstores"}, Verbs: []string{"get", "list", "watch"}},
	})
	_, statErr := os.Stat(filepath.Join(dir, "postgresql.cnpg.io_databaseroles.yaml"))
	return &env{admin: admin, console: user.Config(), servesDatabaseRoles: statErr == nil}
}

func grant(ctx context.Context, t *testing.T, admin client.Client, name string, rules []rbacv1.PolicyRule) {
	t.Helper()
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"}, Rules: rules}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "console"}},
	}
	if err := admin.Create(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
}

func cluster(ns, name string) *cnpgv1.Cluster {
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app": name}},
		Spec:       cnpgv1.ClusterSpec{Instances: 1, StorageConfiguration: cnpgv1.StorageConfiguration{Size: "1Gi"}},
	}
}

func consoleConfig(reprobe time.Duration) config.Console {
	return config.Console{
		WatchNamespaces:   []string{"team-a"},
		ClusterSelector:   labels.Everything(),
		APIRequestTimeout: 10 * time.Second,
		ResyncPeriod:      10 * time.Hour,
		ReprobeInterval:   reprobe,
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestManagerOnEnvtest(t *testing.T) {
	e := start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, c := range []*cnpgv1.Cluster{cluster("team-a", "orders"), cluster("team-b", "other")} {
		if err := e.admin.Create(ctx, c); err != nil {
			t.Fatalf("creating %s: %v", c.Name, err)
		}
	}

	m, err := New(Options{Config: consoleConfig(time.Second), RestConfig: e.console})
	if err != nil {
		t.Fatal(err)
	}
	sub := m.Subscribe()
	if err := m.Reader().List(ctx, &cnpgv1.ClusterList{}); !errors.Is(err, ErrNotReady) {
		t.Errorf("before sync: %v, want ErrNotReady", err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	eventually(t, "first sync", m.Ready)

	t.Run("only the watched namespace is cached", func(t *testing.T) {
		list := &cnpgv1.ClusterList{}
		if err := m.Reader().List(ctx, list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 1 || list.Items[0].Namespace != "team-a" {
			t.Fatalf("clusters = %+v", list.Items)
		}
		if list.Items[0].ManagedFields != nil {
			t.Error("managedFields not stripped")
		}
	})

	t.Run("source states", func(t *testing.T) {
		want := map[string]viewmodel.SourceStateKind{
			SourceClusters:     viewmodel.SourceLive,
			SourcePods:         viewmodel.SourceLive,
			SourceEvents:       viewmodel.SourceLive,
			SourceBackups:      viewmodel.SourceLive,
			SourcePoolers:      viewmodel.SourceForbidden,
			SourceObjectStores: viewmodel.SourceNotInstalled,
			// The second, raw watch of the same Clusters has a state of
			// its own: the shared GVK must not mix the two up.
			SourceClusterStorage: viewmodel.SourceLive,
		}
		if e.servesDatabaseRoles {
			want[SourceDatabaseRoles] = viewmodel.SourceLive
		} else {
			want[SourceDatabaseRoles] = viewmodel.SourceNotInstalled
		}
		for source, state := range want {
			if got := m.Source("team-a", source); got.State != state {
				t.Errorf("%s = %s (%s), want %s", source, got.State, got.Reason, state)
			}
		}
		if got := m.Source("team-a", SourcePoolers).Reason; !strings.Contains(got, "Pooler.postgresql.cnpg.io in team-a") {
			t.Errorf("forbidden reason %q", got)
		}
		if served := m.Served(SourceObjectStores); served == nil || *served {
			t.Errorf("objectStores served = %v, want false", served)
		}
		// Only the CNPG CRDs are installed: the shape of a team install
		// without the operator, where the access-request endpoints answer
		// 501 from this answer.
		if served := m.Served(SourceAccessRequests); served == nil || *served {
			t.Errorf("accessRequests served = %v, want false", served)
		}
	})

	t.Run("the raw Cluster view is stripped to its storage", func(t *testing.T) {
		raw := NewClusterStorage()
		if err := m.Reader().Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "orders"}, raw); err != nil {
			t.Fatal(err)
		}
		if _, found := raw.Object["status"]; found || raw.GetUID() == "" || len(raw.GetLabels()) != 0 {
			t.Errorf("raw Cluster = %v", raw.Object)
		}
		typed := &cnpgv1.Cluster{}
		if err := m.Reader().Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "orders"}, typed); err != nil || typed.Spec.Instances != 1 {
			t.Errorf("the typed Cluster after the raw read: %v, %+v", err, typed.Spec)
		}
	})

	t.Run("cached objects are counted per readable source", func(t *testing.T) {
		counts := m.Counts(ctx)
		if counts[SourceClusters] != 1 {
			t.Errorf("clusters counted %d, want 1 (team-b is not watched)", counts[SourceClusters])
		}
		if _, ok := counts[SourcePoolers]; ok {
			t.Error("a forbidden source is counted")
		}
		if _, ok := counts[SourcePgAdmins]; ok {
			t.Error("a not-installed source is counted")
		}
	})

	t.Run("secrets are never cached", func(t *testing.T) {
		err := m.Reader().List(ctx, &corev1.SecretList{}, client.InNamespace("team-a"))
		if err == nil {
			t.Fatal("listing secrets through the console cache succeeded")
		}
		err = m.Reader().Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "x"}, &corev1.ConfigMap{})
		if err == nil {
			t.Fatal("getting a configmap through the console cache succeeded")
		}
	})

	t.Run("a backup change names its cluster", func(t *testing.T) {
		sub.Drain()
		backup := &cnpgv1.Backup{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-1"},
			Spec:       cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: "orders"}},
		}
		if err := e.admin.Create(ctx, backup); err != nil {
			t.Fatal(err)
		}
		eventually(t, "change notification", func() bool {
			for _, key := range sub.Drain() {
				if key == (ClusterKey{Namespace: "team-a", Name: "orders"}) {
					return true
				}
			}
			return false
		})
		list := &cnpgv1.BackupList{}
		if err := m.Reader().List(ctx, list, client.InNamespace("team-a"), client.MatchingFields{IndexCluster: "orders"}); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 1 {
			t.Errorf("backups by cluster index = %d", len(list.Items))
		}
	})

	t.Run("a grant added after start is picked up by the re-probe", func(t *testing.T) {
		grant(ctx, t, e.admin, "console-poolers", []rbacv1.PolicyRule{
			{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"poolers"}, Verbs: []string{"get", "list", "watch"}},
		})
		eventually(t, "poolers live", func() bool { return m.Source("team-a", SourcePoolers).State == viewmodel.SourceLive })
		if err := m.Reader().List(ctx, &cnpgv1.PoolerList{}, client.InNamespace("team-a")); err != nil {
			t.Errorf("listing poolers after the rebuild: %v", err)
		}
	})

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}

// TestManagerFailsStartOnUnclassifiedProbeError: an API server that cannot
// be reached is not "feature off".
func TestManagerFailsStartOnUnreachableAPIServer(t *testing.T) {
	cfg := &rest.Config{Host: "https://127.0.0.1:1", Timeout: time.Second}
	m, err := New(Options{Config: consoleConfig(time.Minute), RestConfig: cfg})
	if err != nil {
		t.Skipf("mapper construction needs discovery on this client version: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Run(ctx); err == nil {
		t.Fatal("Run succeeded against an unreachable API server")
	}
}

// syncJournal is the journal fake, safe across informer goroutines.
type syncJournal struct {
	mu sync.Mutex
	journal
}

func (j *syncJournal) tap(k Kind, obj client.Object, deleted bool) {
	if k.Source != SourceClusters {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.journal.tap(k, obj, deleted)
}

func (j *syncJournal) seed(k Kind, namespaces []string, objs []client.Object) {
	if k.Source != SourceClusters {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.journal.seed(k, namespaces, objs)
}

func (j *syncJournal) snapshot() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.calls)
}

// TestManagerSeedsTheHistoryOnEveryEpoch: a source's initial list reaches
// the history as one seed per cache epoch (the first build and every
// rebuild), never as watch adds, and later changes as taps.
func TestManagerSeedsTheHistoryOnEveryEpoch(t *testing.T) {
	e := start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.admin.Create(ctx, cluster("team-a", "orders")); err != nil {
		t.Fatal(err)
	}
	j := &syncJournal{}
	m, err := New(Options{Config: consoleConfig(time.Second), RestConfig: e.console, Tap: j.tap, Seed: j.seed})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	seed := "seed clusters [team-a] [orders]"
	eventually(t, "the first seed", func() bool { return slices.Contains(j.snapshot(), seed) })

	if err := e.admin.Create(ctx, cluster("team-a", "billing")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the watch add", func() bool { return slices.Contains(j.snapshot(), "tap billing") })

	// A grant added after start rebuilds the cache: the new epoch seeds again.
	grant(ctx, t, e.admin, "console-poolers", []rbacv1.PolicyRule{
		{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"poolers"}, Verbs: []string{"get", "list", "watch"}},
	})
	eventually(t, "the rebuild's seed", func() bool {
		return slices.Contains(j.snapshot(), "seed clusters [team-a] [billing orders]") ||
			slices.Contains(j.snapshot(), "seed clusters [team-a] [orders billing]")
	})
	for _, call := range j.snapshot() {
		if call == "tap orders" {
			t.Errorf("an initial-list add reached the tap: %v", j.snapshot())
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}
