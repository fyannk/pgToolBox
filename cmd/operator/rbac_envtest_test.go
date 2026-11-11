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
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/configuration"
	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// TestEnvtestWatchNamespacesRBAC applies the RBAC the Helm chart renders
// with operator.config.watchNamespaces (team-a, and team-gone, a namespace
// deleted since the install) to a real API server and acts as the
// operator's ServiceAccount: it reads its own ConfigMap and Secret in the
// release namespace, which is not watched, and takes its Lease; it starts
// with team-gone left out; and it cannot leave its namespaces through RBAC:
// no ClusterRoleBinding to the console roles, no other Secret of its own
// namespace, while the RoleBinding it does create in a watched namespace
// still passes the escalation check.
//
// Needs KUBEBUILDER_ASSETS and helm (make test-envtest-manager).
//
//nolint:funlen // one API server, the operator's identity, the cases in order.
func TestEnvtestWatchNamespacesRBAC(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-manager)")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	root := filepath.Join("..", "..")
	out, err := exec.CommandContext(t.Context(), "helm", "template", "pgtoolbox", filepath.Join(root, "charts", "pgtoolbox"), // #nosec G204 -- fixed arguments
		"--namespace", "pgtoolbox", "--set", "operator.enabled=true",
		"--set", "operator.config.watchNamespaces={team-a,team-gone,team-narrowed}").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var rbac []client.Object
	for _, doc := range bytes.Split(out, []byte("\n---")) {
		kind := struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		}{}
		if yaml.Unmarshal(doc, &kind) != nil {
			continue
		}
		var object client.Object
		switch kind.Kind {
		case "ClusterRole":
			object = &rbacv1.ClusterRole{}
		case "ClusterRoleBinding":
			object = &rbacv1.ClusterRoleBinding{}
		case "Role":
			object = &rbacv1.Role{}
		case "RoleBinding":
			if kind.Metadata.Namespace == "team-gone" {
				continue // deleted with its namespace
			}
			if kind.Metadata.Namespace == "team-narrowed" {
				continue // replaced by its administrator, below
			}
			object = &rbacv1.RoleBinding{}
		default:
			continue
		}
		if err := yaml.UnmarshalStrict(doc, object); err != nil {
			t.Fatalf("%s: %v", kind.Kind, err)
		}
		rbac = append(rbac, object)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "test", "crds", "cnpg-1.30"), filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	admin, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx := t.Context()
	c, err := client.New(admin, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-narrowed"}},
		// team-narrowed's administrator bound the operator to a Role of
		// their own: Clusters, nothing else.
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "narrow", Namespace: "team-narrowed"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters"}, Verbs: []string{"get", "list", "watch"}}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "narrow", Namespace: "team-narrowed"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "narrow"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "pgtoolbox", Namespace: "pgtoolbox"}},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-operator-config", Namespace: "pgtoolbox"},
			Data:       map[string]string{"UPGRADE_BACKUPS_KEPT": "7"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-operator-config", Namespace: "pgtoolbox"},
			Data:       map[string][]byte{"MAX_CONCURRENT_RECONCILES": []byte("3")},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "someone-else", Namespace: "pgtoolbox"}},
	}
	for _, object := range append(objects, rbac...) {
		if err := c.Create(ctx, object); err != nil {
			t.Fatalf("create %T %s: %v", object, object.GetName(), err)
		}
	}

	config := rest.CopyConfig(admin)
	config.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:pgtoolbox:pgtoolbox",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:pgtoolbox", "system:authenticated"},
	}
	operator, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("reads its configuration in its own, unwatched namespace", func(t *testing.T) {
		loaded, err := configuration.Load(ctx, operator, func(key string) (string, bool) {
			if key == "OPERATOR_IMAGE" {
				return "registry.example.org/pgtoolbox-operator:test", true
			}
			return "", false
		}, configuration.Objects{
			Namespace: "pgtoolbox", ConfigMap: "pgtoolbox-operator-config", Secret: "pgtoolbox-operator-config",
		})
		if err != nil {
			t.Fatalf("the operator cannot read its configuration: %v", err)
		}
		if loaded.Config.UpgradeBackupsKept != 7 || loaded.Config.MaxConcurrentReconciles != 3 {
			t.Fatalf("configuration = %+v, want the ConfigMap and Secret values", loaded.Config)
		}
	})
	t.Run("takes its leader-election Lease", func(t *testing.T) {
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox.pgtoolbox.fyannk.dev", Namespace: "pgtoolbox"}}
		if err := operator.Create(ctx, lease); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("starts with the deleted namespace left out", func(t *testing.T) {
		got, _, err := reachableNamespaces(ctx, operator, []string{"team-a", "team-gone"})
		if err != nil || !slices.Equal(got, []string{"team-a"}) {
			t.Fatalf("watched = %q, %v; want [team-a]", got, err)
		}
		if _, err := probeOptionalAPIs(ctx, operator, got); err != nil {
			t.Fatalf("probeOptionalAPIs: %v", err)
		}
	})
	t.Run("the optional-API probe decides a narrowed namespace on its own", func(t *testing.T) {
		// probeOptionalAPIs alone: team-narrowed also refuses required kinds,
		// which permittedNamespaces leaves out (the subtest below).
		got, _, err := reachableNamespaces(ctx, operator, []string{"team-a", "team-narrowed"})
		if err != nil || !slices.Equal(got, []string{"team-a", "team-narrowed"}) {
			t.Fatalf("reachable = %q, %v; want both (Clusters are listable in each)", got, err)
		}
		probed, err := probeOptionalAPIs(ctx, operator, got)
		if err != nil {
			t.Fatalf("probeOptionalAPIs: %v", err)
		}
		if !slices.Equal(probed.Namespaces, []string{"team-a", "team-narrowed"}) {
			t.Fatalf("watched %q, want both", probed.Namespaces)
		}
		for name, r := range map[string]discovery.Result{
			"databaseRoles": probed.Features.DatabaseRoles, "poolers": probed.Features.Poolers, "databases": probed.Features.Databases,
		} {
			if !r.In("team-a").Usable() {
				t.Errorf("%s turned off in team-a by team-narrowed: %+v", name, r)
			}
			if r.In("team-narrowed").Usable() {
				t.Errorf("%s on in team-narrowed, whose Role refuses it: %+v", name, r)
			}
		}
		assertCacheSyncs(ctx, t, config, probed, &cnpgv1.Pooler{})
	})
	t.Run("a widened namespace leaves every other namespace watched", func(t *testing.T) {
		// The installer refuses Poolers in every watched namespace (the
		// documented way to turn a feature off); team-widened's
		// administrator grants them there. Round 13, P2: this used to
		// leave every other namespace out of the watch.
		widened := rest.CopyConfig(admin)
		widened.Impersonate = rest.ImpersonationConfig{
			UserName: "system:serviceaccount:pgtoolbox:widened",
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:pgtoolbox", "system:authenticated"},
		}
		namespaces := []string{"team-w1", "team-w2", "team-widened", "team-w3"}
		subject := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "widened", Namespace: "pgtoolbox"}}
		objects := make([]client.Object, 0, 3+2*len(namespaces))
		objects = append(objects, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "widened-base"},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters", "databaseroles", "databases"},
				Verbs: []string{"get", "list", "watch"},
			}},
		})
		for _, namespace := range namespaces {
			objects = append(objects,
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
				&rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: "widened-base", Namespace: namespace},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "widened-base"},
					Subjects:   subject,
				})
		}
		objects = append(objects,
			&rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: "poolers", Namespace: "team-widened"},
				Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"poolers"}, Verbs: []string{"get", "list", "watch"}}},
			},
			&rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "poolers", Namespace: "team-widened"},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "poolers"},
				Subjects:   subject,
			})
		for _, object := range objects {
			if err := c.Create(ctx, object); err != nil {
				t.Fatalf("create %T %s: %v", object, object.GetName(), err)
			}
		}
		reader, err := client.New(widened, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := reachableNamespaces(ctx, reader, namespaces)
		if err != nil || !slices.Equal(got, namespaces) {
			t.Fatalf("reachable = %q, %v; want every namespace", got, err)
		}
		probed, err := probeOptionalAPIs(ctx, reader, got)
		if err != nil {
			t.Fatalf("probeOptionalAPIs: %v", err)
		}
		if !slices.Equal(probed.Namespaces, namespaces) {
			t.Fatalf("watched %q, want every namespace %q", probed.Namespaces, namespaces)
		}
		for _, namespace := range namespaces {
			if on := probed.Features.Poolers.In(namespace).Usable(); on != (namespace == "team-widened") {
				t.Errorf("Poolers in %s usable = %t, want it on in team-widened only", namespace, on)
			}
			if !probed.Features.DatabaseRoles.In(namespace).Usable() {
				t.Errorf("DatabaseRoles off in %s, which allows them", namespace)
			}
		}
		assertCacheSyncs(ctx, t, widened, probed, &cnpgv1.Pooler{})
	})
	t.Run("a namespace refusing a required kind is left out, the others start and reconcile", func(t *testing.T) {
		// Round 14, K8s P2-1. team-narrowed's Role keeps Clusters and drops
		// everything else (Deployments, Secrets, ...); team-nowatch's
		// administrator copied the manager role but lets Clusters be listed,
		// not watched; team-nopoolers's refuses only an optional kind.
		rules, err := managerrole.Rules()
		if err != nil {
			t.Fatal(err)
		}
		var noWatch, noPoolers []rbacv1.PolicyRule
		for _, r := range rules {
			if slices.Contains(r.Resources, "clusters") && r.APIGroups[0] == "postgresql.cnpg.io" {
				noWatch = append(noWatch, rbacv1.PolicyRule{APIGroups: r.APIGroups, Resources: []string{"clusters"}, Verbs: []string{"get", "list"}})
				noPoolers = append(noPoolers, rbacv1.PolicyRule{APIGroups: r.APIGroups, Resources: []string{"clusters"}, Verbs: r.Verbs})
				continue
			}
			noWatch, noPoolers = append(noWatch, r), append(noPoolers, r)
		}
		for namespace, rules := range map[string][]rbacv1.PolicyRule{"team-nowatch": noWatch, "team-nopoolers": noPoolers} {
			for _, object := range []client.Object{
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}},
				&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "own", Namespace: namespace}, Rules: rules},
				&rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: "own", Namespace: namespace},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "own"},
					Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "pgtoolbox", Namespace: "pgtoolbox"}},
				},
			} {
				if err := c.Create(ctx, object); err != nil {
					t.Fatalf("create %T %s: %v", object, object.GetName(), err)
				}
			}
		}
		configured := []string{"team-a", "team-narrowed", "team-nowatch", "team-nopoolers"}
		reachable, _, err := reachableNamespaces(ctx, operator, configured)
		if err != nil || !slices.Equal(reachable, configured) {
			t.Fatalf("reachable = %q, %v; want every namespace (each lets Clusters be listed)", reachable, err)
		}

		// Without the check, the start fails for every namespace: the
		// Deployment informer never syncs in team-narrowed.
		unchecked := map[string]cache.Config{}
		for _, namespace := range reachable {
			unchecked[namespace] = cache.Config{}
		}
		assertInformerSyncs(ctx, t, config, unchecked, &appsv1.Deployment{}, false)

		watched, dropped, err := permittedNamespaces(ctx, operator, reachable)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"team-a", "team-nopoolers"}; !slices.Equal(watched, want) {
			t.Fatalf("watched = %q, want %q", watched, want)
		}
		assertDroppedReported(ctx, t, c, operator, dropped)
		probed, err := probeOptionalAPIs(ctx, operator, watched)
		if err != nil {
			t.Fatal(err)
		}
		if probed.Features.Poolers.In("team-nopoolers").Usable() || !probed.Features.Poolers.In("team-a").Usable() {
			t.Fatalf("Poolers: %+v, want off in team-nopoolers only", probed.Features.Poolers)
		}

		// The manager as main builds it, as the operator's identity, on the
		// namespaces kept: every informer syncs and a controller reconciles.
		prepared := pgAdminPrepared{Config: configuration.Defaults(), Features: probed.Features, EvidenceAgentAPIs: probed.EvidenceAgent, OptionalKinds: probed.Kinds}
		prepared.Config.WatchNamespaces = probed.Namespaces
		consoleFeatures := pgToolBoxConsoleFeatures(prepared)
		options := ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"}
		managerCacheOptions(&options, prepared, consoleFeatures)
		mgr, _, err := buildManager(ctx, newStartupBudget(20*time.Second, time.Minute), func() (ctrl.Manager, error) {
			return ctrl.NewManager(config, options)
		}, managerSetups(prepared, consoleFeatures))
		if err != nil {
			t.Fatal(err)
		}
		mgrCtx, stop := context.WithCancel(ctx)
		stopped := make(chan error, 1)
		go func() { stopped <- mgr.Start(mgrCtx) }()
		t.Cleanup(func() {
			stop()
			if err := <-stopped; err != nil {
				t.Errorf("the manager stopped with %v", err)
			}
		})
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if !mgr.GetCache().WaitForCacheSync(syncCtx) {
			t.Fatal("the manager's cache did not sync on the namespaces kept")
		}
		request := &pgtoolboxv1alpha1.PgAccessRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "victor", Namespace: "team-a"},
			Spec: pgtoolboxv1alpha1.PgAccessRequestSpec{
				Subject:    pgtoolboxv1alpha1.AccessSubject{Kind: pgtoolboxv1alpha1.AccessSubjectUser, Name: "victor"},
				Capability: pgtoolboxv1alpha1.AccessCapabilityOperator, Justification: "INC-1",
				ExpiresAt: &metav1.Time{Time: time.Now().Add(time.Hour)},
			},
		}
		if err := c.Create(ctx, request); err != nil {
			t.Fatal(err)
		}
		for request.Status.Phase == "" {
			select {
			case err := <-stopped:
				t.Fatalf("the manager stopped: %v", err)
			case <-syncCtx.Done():
				t.Fatal("the PgAccessRequest in team-a was not reconciled")
			case <-time.After(200 * time.Millisecond):
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(request), request); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("reads no other Secret of its namespace", func(t *testing.T) {
		err := operator.Get(ctx, client.ObjectKey{Namespace: "pgtoolbox", Name: "someone-else"}, &corev1.Secret{})
		if !apierrors.IsForbidden(err) {
			t.Fatalf("get another Secret: %v, want Forbidden", err)
		}
	})
	t.Run("cannot bind a console role cluster-wide", func(t *testing.T) {
		for _, role := range []string{"pgtoolbox-console-restore-drills", "pgtoolbox-console-pod-logs", "pgtoolbox-console"} {
			err := operator.Create(ctx, &rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "escape-" + role},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
				Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "pgtoolbox", Namespace: "pgtoolbox"}},
			})
			if !apierrors.IsForbidden(err) {
				t.Fatalf("ClusterRoleBinding to %s: %v, want Forbidden", role, err)
			}
		}
	})
	t.Run("binds the console roles in a watched namespace", func(t *testing.T) {
		err := operator.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "orders-console-restore-drills", Namespace: "team-a"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "pgtoolbox-console-restore-drills"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "orders-console", Namespace: "team-a"}},
		}, client.DryRunAll)
		if err != nil {
			t.Fatalf("RoleBinding in team-a: %v", err)
		}
	})
}

// assertCacheSyncs builds the manager's cache as main does, from the probe,
// as the operator's identity, and checks that the informer of object (an
// optional kind refused in some watched namespaces) syncs: watched only
// where it is listable, it never retries a refused List forever.
func assertCacheSyncs(ctx context.Context, t *testing.T, config *rest.Config, probed optionalAPIs, object client.Object) {
	t.Helper()
	prepared := pgAdminPrepared{Features: probed.Features, EvidenceAgentAPIs: probed.EvidenceAgent, OptionalKinds: probed.Kinds}
	prepared.Config.WatchNamespaces = probed.Namespaces
	options := ctrl.Options{Scheme: scheme}
	managerCacheOptions(&options, prepared, pgToolBoxConsoleFeatures(prepared))
	byObjects := map[client.Object]cache.ByObject{}
	for key, byObject := range options.Cache.ByObject {
		if reflect.TypeOf(key) == reflect.TypeOf(object) {
			byObjects[key] = byObject
			if len(byObject.Namespaces) == 0 || len(byObject.Namespaces) == len(probed.Namespaces) {
				t.Fatalf("the %T informer is not restricted to the namespaces it is listable in: %v", object, byObject.Namespaces)
			}
		}
	}
	if len(byObjects) != 1 {
		t.Fatalf("%d cache entries for %T, want 1", len(byObjects), object)
	}
	informers, err := cache.New(config, cache.Options{Scheme: scheme, DefaultNamespaces: options.Cache.DefaultNamespaces, ByObject: byObjects})
	if err != nil {
		t.Fatal(err)
	}
	syncCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := informers.GetInformer(syncCtx, object, cache.BlockUntilSynced(false)); err != nil {
		t.Fatal(err)
	}
	go func() { _ = informers.Start(syncCtx) }()
	if !informers.WaitForCacheSync(syncCtx) {
		t.Fatalf("the %T informer did not sync in the namespaces it is listable in", object)
	}
}

// assertInformerSyncs starts one informer of object on namespaces as the
// identity of config and checks whether it syncs within a few seconds.
func assertInformerSyncs(ctx context.Context, t *testing.T, config *rest.Config, namespaces map[string]cache.Config, object client.Object, want bool) {
	t.Helper()
	informers, err := cache.New(config, cache.Options{Scheme: scheme, DefaultNamespaces: namespaces})
	if err != nil {
		t.Fatal(err)
	}
	syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := informers.GetInformer(syncCtx, object, cache.BlockUntilSynced(false)); err != nil {
		t.Fatal(err)
	}
	go func() { _ = informers.Start(syncCtx) }()
	if got := informers.WaitForCacheSync(syncCtx); got != want {
		t.Fatalf("the %T informer on %d namespaces synced = %t, want %t", object, len(namespaces), got, want)
	}
}

// assertDroppedReported: round 15, DBA P2-2, on a real API server as the
// operator's identity. team-nowatch keeps the manager role but for watch
// on Clusters: its PgAdmin, Ready=True from before, turns Ready=Unknown
// (NamespaceNotWatched) and gets one Warning Event the API server accepts.
// team-narrowed grants Clusters only: nothing is written there (nor
// attempted: the review said so).
func assertDroppedReported(ctx context.Context, t *testing.T, admin, operator client.Client, dropped []droppedNamespace) {
	t.Helper()
	pgadmins := map[string]*pgtoolboxv1alpha1.PgAdmin{}
	for _, namespace := range []string{"team-nowatch", "team-narrowed"} {
		p := &pgtoolboxv1alpha1.PgAdmin{
			ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: namespace},
			Spec: pgtoolboxv1alpha1.PgAdminSpec{
				ClusterRefs: []pgtoolboxv1alpha1.LocalObjectReference{{Name: "orders"}},
				Auth: pgtoolboxv1alpha1.PgAdminAuthSpec{OAuth2: &pgtoolboxv1alpha1.PgAdminOAuth2Spec{
					IssuerURL: "https://idp.example.com/realms/corp/", ClientID: "pgadmin",
					ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "pgadmin-oidc"},
					RequiredClaims:  map[string]pgtoolboxv1alpha1.ClaimValues{"groups": {"dba"}},
				}},
				Admins: []string{"alice@example.com"},
			},
		}
		if err := admin.Create(ctx, p); err != nil {
			t.Fatalf("create the PgAdmin of %s: %v", namespace, err)
		}
		p.Status.Conditions = []metav1.Condition{{
			Type: pgtoolboxv1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonAsExpected.String(),
			Message: "every aspect is as expected", LastTransitionTime: metav1.Now(), ObservedGeneration: p.Generation,
		}}
		if err := admin.Status().Update(ctx, p); err != nil {
			t.Fatalf("set the PgAdmin of %s Ready: %v", namespace, err)
		}
		pgadmins[namespace] = p
	}
	reportDroppedNamespaces(ctx, operator, dropped, time.Now)
	for namespace, want := range map[string]metav1.ConditionStatus{"team-nowatch": metav1.ConditionUnknown, "team-narrowed": metav1.ConditionTrue} {
		p := &pgtoolboxv1alpha1.PgAdmin{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(pgadmins[namespace]), p); err != nil {
			t.Fatal(err)
		}
		ready := meta.FindStatusCondition(p.Status.Conditions, pgtoolboxv1alpha1.ConditionReady)
		if ready == nil || ready.Status != want {
			t.Errorf("%s: Ready = %+v, want %s", namespace, ready, want)
		}
		events := &eventsv1.EventList{}
		if err := admin.List(ctx, events, client.InNamespace(namespace)); err != nil {
			t.Fatal(err)
		}
		wantEvents := 0
		if namespace == "team-nowatch" {
			wantEvents = 1
			if ready.Reason != pgtoolboxv1alpha1.ReasonNamespaceNotWatched.String() || !strings.Contains(ready.Message, "watch clusters.postgresql.cnpg.io") {
				t.Errorf("Ready = %+v, want NamespaceNotWatched naming the missing watch on Clusters", ready)
			}
		}
		if len(events.Items) != wantEvents {
			t.Errorf("%s: %d Events, want %d", namespace, len(events.Items), wantEvents)
		}
	}
}
