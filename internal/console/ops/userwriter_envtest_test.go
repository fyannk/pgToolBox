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

package ops

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// The write path on a real API server: token passthrough (a context
// without a token writes nothing), RBAC deciding per verb exactly as the
// chart's pgtoolbox-console-viewer / pgtoolbox-console-operator ClusterRoles split them,
// the promote lock answering 409 on a stale resourceVersion, and the
// pinned objects passing the CNPG CRD schema.
func TestUserWriterOnEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; set it and CNPG_CRDS as make test-envtest-console does")
	}
	dir := os.Getenv("CNPG_CRDS")
	if dir == "" {
		dir = "../../../test/crds/cnpg-1.30"
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Clean(dir)}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme, err := NewUserScheme()
	if err != nil {
		t.Fatal(err)
	}
	_ = rbacv1.AddToScheme(scheme)
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	cs := kubernetes.NewForConfigOrDie(cfg)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}))
	c := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders"},
		Spec:       cnpgv1.ClusterSpec{Instances: 2, StorageConfiguration: cnpgv1.StorageConfiguration{Size: "1Gi"}},
	}
	must(admin.Create(ctx, c))
	c.Status = cnpgv1.ClusterStatus{CurrentPrimary: "orders-1", TargetPrimary: "orders-1", Phase: "Cluster in healthy state"}
	must(admin.Status().Update(ctx, c))

	viewer := []rbacv1.PolicyRule{
		{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters", "backups"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
	}
	operator := append(append([]rbacv1.PolicyRule{}, viewer...),
		rbacv1.PolicyRule{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"backups"}, Verbs: []string{"create"}},
		rbacv1.PolicyRule{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters", "clusters/status"}, Verbs: []string{"patch"}},
	)
	token := func(sa string, rules []rbacv1.PolicyRule) string {
		t.Helper()
		must(admin.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: sa}}))
		must(admin.Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: sa}, Rules: rules}))
		must(admin.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: sa},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: sa},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: "team-a"}},
		}))
		tr, err := cs.CoreV1().ServiceAccounts("team-a").CreateToken(ctx, sa, &authenticationv1.TokenRequest{
			Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: new(int64(3600))},
		}, metav1.CreateOptions{})
		must(err)
		return tr.Status.Token
	}
	dba, appTeam := token("dba", operator), token("app", viewer)

	uc, err := NewUserClient(auth.UserConfig(cfg))
	must(err)
	w := NewUserWriter(uc, 10*time.Second)
	as := func(tok string) context.Context { return auth.WithUserToken(ctx, tok) }

	t.Run("no token, no request", func(t *testing.T) {
		if _, err := w.AnnotateCluster(ctx, "team-a", "orders", RestartAnnotation, "x", ""); err == nil {
			t.Fatal("a write without a user token went through")
		}
		got := &cnpgv1.Cluster{}
		must(admin.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "orders"}, got))
		if _, ok := got.Annotations[RestartAnnotation]; ok {
			t.Fatal("annotation written without a token")
		}
	})

	t.Run("viewer is refused by the API server", func(t *testing.T) {
		_, err := w.AnnotateCluster(as(appTeam), "team-a", "orders", RestartAnnotation, "2026-10-29T12:00:00Z", "")
		var we *WriteError
		if !errors.As(err, &we) || we.Status != http.StatusForbidden {
			t.Fatalf("viewer restart: %v", err)
		}
		if _, _, err := w.CreateBackup(as(appTeam), BackupObject("team-a", "orders", "ondemand-", "plugin", "barman-cloud.cloudnative-pg.io")); !errors.As(err, &we) || we.Status != http.StatusForbidden {
			t.Fatalf("viewer backup: %v", err)
		}
	})

	t.Run("operator writes as themselves", func(t *testing.T) {
		if _, err := w.AnnotateCluster(as(dba), "team-a", "orders", RestartAnnotation, "2026-10-29T12:00:00Z", "1"); err == nil {
			t.Fatal("a restart locked to a stale resourceVersion was accepted")
		}
		current, err := w.ReadCluster(as(dba), "team-a", "orders")
		must(err)
		if _, err := w.AnnotateCluster(as(dba), "team-a", "orders", RestartAnnotation, "2026-10-29T12:00:00Z", current.ResourceVersion); err != nil {
			t.Fatal(err)
		}
		name, _, err := w.CreateBackup(as(dba), BackupObject("team-a", "orders", "ondemand-20261029120000-", "plugin", "barman-cloud.cloudnative-pg.io"))
		if err != nil || name == "" {
			t.Fatalf("backup: %q %v", name, err)
		}
		b := &cnpgv1.Backup{}
		must(admin.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: name}, b))
		if b.Spec.Method != "plugin" || b.Spec.PluginConfiguration == nil {
			t.Errorf("backup spec %+v", b.Spec)
		}
		fresh, err := w.ReadCluster(as(dba), "team-a", "orders")
		must(err)
		if fresh.Annotations[RestartAnnotation] != "2026-10-29T12:00:00Z" {
			t.Errorf("annotations %v", fresh.Annotations)
		}
		if _, err := w.Promote(as(dba), "team-a", "orders", "orders-2", "2026-10-29T12:00:00.000000Z", "1"); err == nil {
			t.Fatal("promote on a stale resourceVersion was accepted")
		} else if we := (*WriteError)(nil); !errors.As(err, &we) || we.Status != http.StatusConflict {
			t.Fatalf("stale promote: %v", err)
		}
		if _, err := w.Promote(as(dba), "team-a", "orders", "orders-2", "2026-10-29T12:00:00.000000Z", fresh.ResourceVersion); err != nil {
			t.Fatal(err)
		}
		got := &cnpgv1.Cluster{}
		must(admin.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "orders"}, got))
		if got.Status.TargetPrimary != "orders-2" || got.Status.Phase != SwitchoverPhase || got.Status.CurrentPrimary != "orders-1" {
			t.Errorf("status %+v", got.Status)
		}
	})
}
