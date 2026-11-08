//go:build e2echart

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

package chart

import (
	"fmt"
	"net/http"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

var clusterGVR = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}

// saToken mints a short-lived API-server token for a ServiceAccount.
func saToken(t *testing.T, name string) string {
	t.Helper()
	const ns = "team-a"
	tr, err := clientset.CoreV1().ServiceAccounts(ns).CreateToken(ctx(), name, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: new(int64(3600))},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("token for %s/%s: %v", ns, name, err)
	}
	return tr.Status.Token
}

// seedClusters creates the Clusters, the stand-in instance and the people's RBAC
// the SSO scenarios read. There is no CloudNativePG operator in this cluster, only
// its CRDs, so status is written by hand: the console reads what is there.
//
//	team-a/orders   healthy status, no pods
//	team-b/billing  one instance Pod (a stand-in that has the status-port flag but
//	                 serves nothing); team B has no CA bundle, so it is "not verifiable"
//	team-b/audit    healthy status
//	team-c/ledger   fenced: cnpg.io/fencedInstances=["*"]
//
// RBAC: dba gets clusters in team-a (and may patch orders) and only billing in team-b; ops gets every
// cluster of team-b and team-c; stranger gets nothing. A ServiceAccount
// "breakglass" in team-a reads team-a's Clusters, and "scraper" may get /metrics.
func seedClusters(t *testing.T) {
	t.Helper()
	dyn := newDynamic(t)
	mkCluster := func(ns, name string, annotations map[string]any) types.UID {
		return createCluster(t, dyn, ns, name, annotations)
	}
	mkCluster("team-a", "orders", nil)
	billingUID := mkCluster("team-b", "billing", nil)
	mkCluster("team-b", "audit", nil)
	mkCluster("team-c", "ledger", map[string]any{"cnpg.io/fencedInstances": `["*"]`})

	// The instance Pod of billing. The postgres container carries the flag that
	// says its status port serves TLS; nothing listens there.
	f := false
	_, err := clientset.CoreV1().Pods("team-b").Create(ctx(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "billing-1", Namespace: "team-b", Labels: map[string]string{
				"cnpg.io/cluster": "billing", "cnpg.io/podRole": "instance", "cnpg.io/instanceName": "billing-1", "role": "primary",
			},
			// The console counts a Pod as a member only when the Cluster controls it.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "billing", UID: billingUID, Controller: new(true),
			}},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &f,
			Containers: []corev1.Container{{
				Name: "postgres", Image: probeImage, ImagePullPolicy: corev1.PullNever,
				Command:         []string{"sh", "-c", "exec sleep 86400", "--status-port-tls"},
				SecurityContext: restrictedContext(),
			}},
			TerminationGracePeriodSeconds: new(int64),
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create the stand-in instance: %v", err)
	}

	for _, sa := range [][2]string{{"team-a", "breakglass"}, {"team-a", "scraper"}} {
		if _, err := clientset.CoreV1().ServiceAccounts(sa[0]).Create(ctx(), &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: sa[1], Namespace: sa[0]},
		}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	grantVerbs := func(ns, name string, verbs, resourceNames []string, subject rbacv1.Subject) {
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters"},
				Verbs: verbs, ResourceNames: resourceNames,
			}},
		}
		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name},
			Subjects:   []rbacv1.Subject{subject},
		}
		if _, err := clientset.RbacV1().Roles(ns).Create(ctx(), role, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := clientset.RbacV1().RoleBindings(ns).Create(ctx(), binding, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	grant := func(ns, name string, resourceNames []string, subject rbacv1.Subject) {
		grantVerbs(ns, name, []string{"get", "list", "watch"}, resourceNames, subject)
	}
	user := func(n string) rbacv1.Subject {
		return rbacv1.Subject{APIGroup: "rbac.authorization.k8s.io", Kind: "User", Name: "oidc:" + n + "@example.test"}
	}
	grant("team-a", "e2e-view", nil, user("dba"))
	// dba may also patch orders: reload and restart (the MCP write tools).
	grantVerbs("team-a", "e2e-operate-orders", []string{"patch"}, []string{"orders"}, user("dba"))
	grant("team-b", "e2e-view-billing", []string{"billing"}, user("dba"))
	grant("team-b", "e2e-view-all", nil, user("ops"))
	grant("team-c", "e2e-view-all", nil, user("ops"))
	grant("team-a", "e2e-view-breakglass", nil, rbacv1.Subject{Kind: "ServiceAccount", Name: "breakglass", Namespace: "team-a"})

	reader := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-metrics-reader"},
		Rules:      []rbacv1.PolicyRule{{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}}},
	}
	bind := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-metrics-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "e2e-metrics-reader"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "scraper", Namespace: "team-a"}},
	}
	for _, create := range []func() error{
		func() error {
			_, e := clientset.RbacV1().ClusterRoles().Create(ctx(), reader, metav1.CreateOptions{})
			return e
		},
		func() error {
			_, e := clientset.RbacV1().ClusterRoleBindings().Create(ctx(), bind, metav1.CreateOptions{})
			return e
		},
	} {
		if err := create(); err != nil {
			t.Fatal(fmt.Errorf("metrics reader: %w", err))
		}
	}
	t.Cleanup(func() {
		_ = clientset.RbacV1().ClusterRoleBindings().Delete(ctx(), "e2e-metrics-reader", metav1.DeleteOptions{})
		_ = clientset.RbacV1().ClusterRoles().Delete(ctx(), "e2e-metrics-reader", metav1.DeleteOptions{})
	})
}

func newDynamic(t *testing.T) dynamic.Interface {
	t.Helper()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return retryTransport{next: rt} })
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return dyn
}

// createCluster creates a one-instance Cluster and writes a healthy status.
func createCluster(t *testing.T, dyn dynamic.Interface, ns, name string, annotations map[string]any) types.UID {
	t.Helper()
	meta := map[string]any{"name": name, "namespace": ns}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": meta,
		"spec": map[string]any{"instances": int64(1), "storage": map[string]any{"size": "1Gi"}},
	}}
	created, err := dyn.Resource(clusterGVR).Namespace(ns).Create(ctx(), obj, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create Cluster %s/%s: %v", ns, name, err)
	}
	created.Object["status"] = map[string]any{
		"phase":           "Cluster in healthy state",
		"instances":       int64(1),
		"readyInstances":  int64(1),
		"currentPrimary":  name + "-1",
		"targetPrimary":   name + "-1",
		"instancesStatus": map[string]any{"healthy": []any{name + "-1"}},
	}
	if _, err := dyn.Resource(clusterGVR).Namespace(ns).UpdateStatus(ctx(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write the status of Cluster %s/%s: %v", ns, name, err)
	}
	return created.GetUID()
}
