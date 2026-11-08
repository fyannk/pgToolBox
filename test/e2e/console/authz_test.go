//go:build e2econsole

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

// Package console holds the console scenarios that need a real API server but no
// kubelet: the RBAC and access review semantics the console builds its
// whole authorization on, driven through the console's own packages
// (internal/console/authz and internal/console/auth) against envtest. Everything
// the unit tests prove against a fake API server is proved here against the real
// authorizer: a 403-worthy Cluster, list filtering, the identity a token has,
// revocation, and the identity behind /metrics.
//
// Run with `make test-e2e-console-authz`; it skips without KUBEBUILDER_ASSETS.
package console

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
)

type fixture struct {
	admin  kubernetes.Interface
	az     *authz.Authorizer
	review *auth.SelfSubjectReviewer
	env    *envtest.Environment
}

func start(t *testing.T) *fixture {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset; run through make test-e2e-console-authz")
	}
	logf.SetLogger(logr.Discard())
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	admin, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"t-a", "t-b"} {
		if _, err := admin.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	factory, err := auth.NewClientFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		admin:  admin,
		az:     authz.NewAuthorizer(&subjectReviewer{env: env, cfg: cfg, clients: map[string]kubernetes.Interface{}}, authz.Options{}),
		review: auth.NewSelfSubjectReviewer(factory),
		env:    env,
	}
}

// subjectReviewer answers each question with a SelfSubjectAccessReview sent
// as the subject itself: an envtest user with that name and those groups,
// the way auth.SelfReviewer asks with the subject's token in the console.
// The console holds no review grant, so nothing here asks as
// the console.
type subjectReviewer struct {
	env     *envtest.Environment
	cfg     *rest.Config
	mu      sync.Mutex
	clients map[string]kubernetes.Interface
}

func (r *subjectReviewer) Review(ctx context.Context, sar *authorizationv1.SubjectAccessReview) (*authorizationv1.SubjectAccessReview, error) {
	key := sar.Spec.User + "\x00" + strings.Join(sar.Spec.Groups, "\x00")
	r.mu.Lock()
	client, ok := r.clients[key]
	if !ok {
		user, err := r.env.AddUser(envtest.User{Name: sar.Spec.User, Groups: sar.Spec.Groups}, r.cfg)
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		if client, err = kubernetes.NewForConfig(user.Config()); err != nil {
			r.mu.Unlock()
			return nil, err
		}
		r.clients[key] = client
	}
	r.mu.Unlock()
	res, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes:    sar.Spec.ResourceAttributes,
		NonResourceAttributes: sar.Spec.NonResourceAttributes,
	}}, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	out := sar.DeepCopy()
	out.Status = res.Status
	return out, nil
}

func mustCreate(t *testing.T, f func() error) {
	t.Helper()
	if err := f(); err != nil {
		t.Fatal(err)
	}
}

// grant binds subject to a Role with the given rules in ns.
func (f *fixture) grant(ctx context.Context, t *testing.T, ns, name string, rules []rbacv1.PolicyRule, subject rbacv1.Subject) {
	t.Helper()
	mustCreate(t, func() error {
		_, err := f.admin.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Rules: rules}, metav1.CreateOptions{})
		return err
	})
	mustCreate(t, func() error {
		_, err := f.admin.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name},
			Subjects:   []rbacv1.Subject{subject},
		}, metav1.CreateOptions{})
		return err
	})
}

func readClusters(names ...string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{APIGroups: []string{authz.CNPGGroup}, Resources: []string{"clusters"}, Verbs: []string{"get", "list", "watch"}, ResourceNames: names}}
}

func user(name string) rbacv1.Subject {
	return rbacv1.Subject{APIGroup: "rbac.authorization.k8s.io", Kind: "User", Name: name}
}

func refs(rs ...string) []authz.ObjectRef {
	out := make([]authz.ObjectRef, 0, len(rs))
	for _, r := range rs {
		ns, name, _ := strings.Cut(r, "/")
		out = append(out, authz.ObjectRef{Namespace: ns, Name: name})
	}
	return out
}

func names(rs []authz.ObjectRef) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Namespace+"/"+r.Name)
	}
	return out
}

// TestSubjectAccessReviewSemantics: what the real RBAC authorizer says about the
// questions the console asks, through the console's own Authorizer.
func TestSubjectAccessReviewSemantics(t *testing.T) {
	f := start(t)
	ctx := context.Background()
	f.grant(ctx, t, "t-a", "view-all", readClusters(), user("alice"))
	f.grant(ctx, t, "t-b", "view-billing", readClusters("billing"), user("alice"))
	f.grant(ctx, t, "t-a", "view-by-group", readClusters(), rbacv1.Subject{APIGroup: "rbac.authorization.k8s.io", Kind: "Group", Name: "dbas"})

	alice := authz.User{Name: "alice"}
	all := refs("t-a/orders", "t-a/ledger", "t-b/billing", "t-b/audit", "t-c/ghost")

	t.Run("a Cluster the caller may not get is not visible, and not undecided", func(t *testing.T) {
		visible, undecided, err := f.az.VisibleClusters(ctx, alice, all)
		if err != nil || len(undecided) != 0 {
			t.Fatalf("err=%v undecided=%v: a denial is an answer, not an outage", err, undecided)
		}
		// Input order is kept; resourceNames is honoured down to the name; a
		// namespace nobody granted anything in shows nothing.
		if got, want := names(visible), []string{"t-a/orders", "t-a/ledger", "t-b/billing"}; !slices.Equal(got, want) {
			t.Fatalf("visible = %v, want %v", got, want)
		}
	})

	t.Run("a stranger sees nothing, and nothing leaks through capabilities", func(t *testing.T) {
		stranger := authz.User{Name: "stranger"}
		visible, undecided, err := f.az.VisibleClusters(ctx, stranger, all)
		if len(visible) != 0 || len(undecided) != 0 || err != nil {
			t.Fatalf("visible=%v undecided=%v err=%v", visible, undecided, err)
		}
		if c := f.az.ClusterCapabilities(ctx, stranger, "t-a", "orders"); c.View || c.Logs || c.Operations || len(c.Undecided) != 0 {
			t.Fatalf("capabilities of a stranger: %+v", c)
		}
	})

	t.Run("capabilities follow the verbs the user holds, not the read grant", func(t *testing.T) {
		c := f.az.ClusterCapabilities(ctx, alice, "t-a", "orders")
		if !c.View || c.Logs || c.Operations || len(c.Undecided) != 0 {
			t.Fatalf("alice has read only: %+v", c)
		}
		f.grant(ctx, t, "t-a", "operate", []rbacv1.PolicyRule{
			{APIGroups: []string{authz.CNPGGroup}, Resources: []string{"clusters", "clusters/status"}, Verbs: []string{"patch"}},
			{APIGroups: []string{authz.CNPGGroup}, Resources: []string{"backups"}, Verbs: []string{"create"}},
			{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
		}, user("operator"))
		// A different user: the cache is keyed by subject.
		op := authz.User{Name: "operator"}
		if c := f.az.ClusterCapabilities(ctx, op, "t-a", "orders"); c.View || !c.Logs || !c.Operations {
			t.Fatalf("operator holds patch+backup+log but no get: %+v (Operations needs all three, View needs get)", c)
		}
	})

	t.Run("groups count only when the API server reported them", func(t *testing.T) {
		member := authz.User{Name: "carol", Groups: []string{"dbas"}}
		if d := f.az.Allowed(ctx, member, authz.ClusterView("t-a", "orders")); !d.Allowed {
			t.Errorf("a member of dbas, as reported by SelfSubjectReview, was refused: %+v", d)
		}
		if d := f.az.Allowed(ctx, authz.User{Name: "carol"}, authz.ClusterView("t-a", "orders")); d.Allowed {
			t.Errorf("a user without the group was admitted")
		}
	})

	t.Run("an empty subject is refused before any review", func(t *testing.T) {
		if d := f.az.Allowed(ctx, authz.User{}, authz.ClusterView("t-a", "orders")); d.Allowed || !errors.Is(d.Err, authz.ErrNoUser) {
			t.Fatalf("%+v", d)
		}
	})
}

// TestRevocationTakesEffect: a revoked RoleBinding denies writes at once (the
// console sends them with the user's own token, so the API server decides) and
// views once the positive SAR cache expires (10 s).
func TestRevocationTakesEffect(t *testing.T) {
	f := start(t)
	ctx := context.Background()
	f.grant(ctx, t, "t-a", "view-and-patch", append(readClusters(), rbacv1.PolicyRule{
		APIGroups: []string{authz.CNPGGroup}, Resources: []string{"clusters"}, Verbs: []string{"patch"},
	}), user("dave"))
	dave := authz.User{Name: "dave"}
	davesClient := func() kubernetes.Interface {
		u, err := f.env.AddUser(envtest.User{Name: "dave"}, f.env.Config)
		if err != nil {
			t.Fatal(err)
		}
		c, err := kubernetes.NewForConfig(u.Config())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}()
	// This API server has no CNPG CRDs: a request the authorizer admits ends in
	// 404, one it refuses in 403, so the two are told apart without a Cluster.
	write := func() int {
		err := davesClient.CoreV1().RESTClient().Patch(types.MergePatchType).
			AbsPath("/apis/" + authz.CNPGGroup + "/v1/namespaces/t-a/clusters/orders").Body([]byte(`{}`)).Do(ctx).Error()
		var st apierrors.APIStatus
		if errors.As(err, &st) {
			return int(st.Status().Code)
		}
		return 0
	}

	if d := f.az.Allowed(ctx, dave, authz.ClusterView("t-a", "orders")); !d.Allowed {
		t.Fatalf("before the revocation: %+v", d)
	}
	if got := write(); got != http.StatusNotFound {
		t.Fatalf("before the revocation the write reaches the (absent) resource: HTTP %d, want 404", got)
	}

	revoked := time.Now()
	if err := f.admin.RbacV1().RoleBindings("t-a").Delete(ctx, "view-and-patch", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	// Writes: immediate. Retry only for the authorizer's own informer to see the
	// deletion (milliseconds), never for the SAR cache.
	deadline := time.Now().Add(3 * time.Second)
	for write() != http.StatusForbidden {
		if time.Now().After(deadline) {
			t.Fatalf("a write after the RoleBinding was deleted is still admitted")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("writes denied %s after the deletion", time.Since(revoked).Round(time.Millisecond))

	// Views: the cached positive answer may live for the TTL and no longer.
	for f.az.Allowed(ctx, dave, authz.ClusterView("t-a", "orders")).Allowed {
		if time.Since(revoked) > authz.DefaultTTL+3*time.Second {
			t.Fatalf("the view is still allowed %s after the revocation (TTL %s)", time.Since(revoked), authz.DefaultTTL)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("views denied %s after the deletion (TTL %s)", time.Since(revoked).Round(100*time.Millisecond), authz.DefaultTTL)

	// A grant is visible on the very next question: a denial is never cached.
	f.grant(ctx, t, "t-a", "view-again", readClusters(), user("dave"))
	deadline = time.Now().Add(3 * time.Second)
	for !f.az.Allowed(ctx, dave, authz.ClusterView("t-a", "orders")).Allowed {
		if time.Now().After(deadline) {
			t.Fatalf("a new grant is not seen; denials must not be cached")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestIdentityBehindTheToken: the console never trusts a claim of its own; who
// the user is, is what the API server says about their token.
func TestIdentityBehindTheToken(t *testing.T) {
	f := start(t)
	ctx := context.Background()
	mint := func(ns, sa string) string {
		t.Helper()
		if _, err := f.admin.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		exp := int64(3600)
		tr, err := f.admin.CoreV1().ServiceAccounts(ns).CreateToken(ctx, sa, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &exp}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("TokenRequest (does this envtest API server sign service account tokens?): %v", err)
		}
		return tr.Status.Token
	}

	t.Run("a ServiceAccount token is the ServiceAccount", func(t *testing.T) {
		u, err := f.review.Review(ctx, mint("t-a", "sa1"))
		if err != nil {
			t.Fatal(err)
		}
		if u.Name != "system:serviceaccount:t-a:sa1" || !slices.Contains(u.Groups, "system:serviceaccounts:t-a") || !slices.Contains(u.Groups, "system:authenticated") {
			t.Fatalf("identity = %+v", u)
		}
	})

	t.Run("a token the API server does not accept never becomes an identity", func(t *testing.T) {
		for name, token := range map[string]string{"empty": "", "garbage": "not-a-token", "jwt-shaped": "eyJhbGciOiJSUzI1NiJ9.e30.c2ln"} {
			if _, err := f.review.Review(ctx, token); !errors.Is(err, auth.ErrTokenRejected) {
				t.Errorf("%s: err = %v, want ErrTokenRejected", name, err)
			}
		}
	})
}

// TestMetricsIdentity: /metrics is protected by SelfSubjectReview plus a
// SubjectAccessReview for `get /metrics`, the same two steps the handler takes.
func TestMetricsIdentity(t *testing.T) {
	f := start(t)
	ctx := context.Background()
	mustCreate(t, func() error {
		_, err := f.admin.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "metrics-reader"},
			Rules:      []rbacv1.PolicyRule{{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}}},
		}, metav1.CreateOptions{})
		return err
	})
	mustCreate(t, func() error {
		_, err := f.admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "metrics-reader"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "metrics-reader"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "scraper", Namespace: "t-a"}},
		}, metav1.CreateOptions{})
		return err
	})
	token := func(sa string) string {
		if _, err := f.admin.CoreV1().ServiceAccounts("t-a").Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: "t-a"}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		exp := int64(3600)
		tr, err := f.admin.CoreV1().ServiceAccounts("t-a").CreateToken(ctx, sa, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &exp}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return tr.Status.Token
	}
	may := func(tok string) (bool, error) {
		u, err := f.review.Review(ctx, tok)
		if err != nil {
			return false, err
		}
		return f.az.Allowed(ctx, u, authz.Attributes{Verb: "get", Path: "/metrics"}).Allowed, nil
	}
	if ok, err := may(token("scraper")); err != nil || !ok {
		t.Errorf("the scraper bound to the reader ClusterRole: allowed=%v err=%v", ok, err)
	}
	if ok, err := may(token("bystander")); err != nil || ok {
		t.Errorf("a ServiceAccount without the grant: allowed=%v err=%v", ok, err)
	}
	if _, err := may("nope"); !errors.Is(err, auth.ErrTokenRejected) {
		t.Errorf("an unauthenticated caller: err=%v, want ErrTokenRejected (a 401, not a 403)", err)
	}
}
