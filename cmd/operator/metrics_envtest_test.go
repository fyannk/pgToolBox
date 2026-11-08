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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestEnvtestMetricsEndpoint proves against a real API server what the
// options promise: /metrics answers 401 without a token, 403 to a
// ServiceAccount that may not get /metrics, and 200 to one bound to a
// metrics-reader ClusterRole — and the manager doing the reviews holds
// only create on tokenreviews and subjectaccessreviews.
//
// Needs KUBEBUILDER_ASSETS (make test-envtest-manager).
func TestEnvtestMetricsEndpoint(t *testing.T) {
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

	// The manager's identity: the two review verbs and nothing else.
	mustCreate(t, func() error {
		_, err := admin.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "reviews"},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
				{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}},
			},
		}, metav1.CreateOptions{})
		return err
	})
	mustCreate(t, func() error {
		_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "reviews"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "reviews"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "pgtoolbox-operator"}},
		}, metav1.CreateOptions{})
		return err
	})
	operator, err := env.AddUser(envtest.User{Name: "pgtoolbox-operator"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	address := freeAddress(t)
	certDir, roots := servingCertificate(t)
	mgr, err := ctrl.NewManager(operator.Config(), ctrl.Options{
		Scheme: scheme, Metrics: metricsOptions(address, certDir), HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	// Two scrapers: one bound to a metrics-reader ClusterRole, one not.
	mustCreate(t, func() error {
		_, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}}, metav1.CreateOptions{})
		return err
	})
	for _, name := range []string{"prometheus", "stranger"} {
		mustCreate(t, func() error {
			_, err := admin.CoreV1().ServiceAccounts("monitoring").Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
			return err
		})
	}
	mustCreate(t, func() error {
		_, err := admin.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: "metrics-reader"},
			Rules:      []rbacv1.PolicyRule{{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}}},
		}, metav1.CreateOptions{})
		return err
	})
	mustCreate(t, func() error {
		_, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "metrics-reader"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "metrics-reader"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: "monitoring", Name: "prometheus"}},
		}, metav1.CreateOptions{})
		return err
	})
	token := func(name string) string {
		request, err := admin.CoreV1().ServiceAccounts("monitoring").CreateToken(ctx, name, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return request.Status.Token
	}

	// The certificate from --metrics-cert-dir is verified; the bearer
	// token is what decides.
	httpClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
	scrape := func(bearer string) int {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+address+"/metrics", nil)
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := httpClient.Do(request)
		if err != nil {
			return 0
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	plainServes := func() bool {
		response, err := httpClient.Do(getRequest(t, "http://"+address+"/metrics"))
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	}

	deadline := time.Now().Add(30 * time.Second)
	for scrape("") == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	for _, check := range []struct {
		name   string
		bearer string
		want   int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"token without get /metrics", token("stranger"), http.StatusForbidden},
		{"metrics reader", token("prometheus"), http.StatusOK},
	} {
		if got := scrape(check.bearer); got != check.want {
			t.Errorf("%s: HTTP %d, want %d", check.name, got, check.want)
		}
	}
	// A malformed token never reads metrics. The API server reports it as a
	// TokenReview error rather than "not authenticated", which the
	// apiserver webhook authenticator turns into a 500: refused either way.
	if got := scrape("not-a-token"); got == http.StatusOK || got == 0 {
		t.Errorf("malformed token: HTTP %d", got)
	}
	if plainServes() {
		t.Error("/metrics answered 200 over plain HTTP")
	}
}

func mustCreate(t *testing.T, create func() error) {
	t.Helper()
	if err := create(); err != nil {
		t.Fatal(err)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String()
}

// servingCertificate writes a self-signed tls.crt/tls.key for 127.0.0.1
// into a directory, as a mounted certificate Secret would be, and returns
// the pool that trusts it.
func servingCertificate(t *testing.T) (string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pgtoolbox-operator-metrics"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return dir, roots
}
