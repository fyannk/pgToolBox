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

package pgadmin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"maps"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

// testCertificatePEM is a self-signed CA certificate.
func testCertificatePEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Unix(0, 0), NotAfter: time.Unix(0, 0).Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestCertificatesPEMKeepsOnlyCertificates(t *testing.T) {
	ca := testCertificatePEM(t)
	key := "-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"
	broken := "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
	if got := certificatesPEM([]byte(key + "comment\n" + ca + broken)); got != ca {
		t.Fatalf("certificatesPEM kept %q", got)
	}
	if got := certificatesPEM([]byte(key)); got != "" {
		t.Fatalf("a key alone gave %q", got)
	}
}

func TestOfferCAs(t *testing.T) {
	ca := testCertificatePEM(t)
	caSecret := func(namespace, name, crt string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Data:       map[string][]byte{"ca.crt": []byte(crt), "ca.key": []byte("signing key")},
		}
	}
	doc := offer.Document{
		Servers: []offer.Server{
			{Cluster: "one", CASecretName: "one-ca"},
			{Cluster: "one", CASecretName: "one-ca"},
			{Cluster: "pay", CASecretName: "pay-ca", Namespace: "team-b"},
			{Cluster: "gone", CASecretName: "gone-ca"},
			{Cluster: "empty", CASecretName: "empty-ca"},
		},
	}
	gets := 0
	c := fake.NewClientBuilder().WithObjects(
		caSecret("team-a", "one-ca", ca), caSecret("team-b", "pay-ca", ca), caSecret("team-a", "empty-ca", ""),
	).WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		gets++
		return c.Get(ctx, key, obj, opts...)
	}}).Build()
	r := &Reconciler{APIReader: c}
	p := &pgtoolboxv1alpha1.PgAdmin{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "team"}}
	if err := r.offerCAs(context.Background(), p, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.CAs) != 2 || doc.CAs["one"] != ca || doc.CAs["team-b.pay"] != ca {
		t.Fatalf("CAs = %v, want one and team-b.pay only (a missing or empty CA Secret is left out)", doc.CAs)
	}
	if gets != 4 {
		t.Fatalf("%d reads, want one per CA Secret", gets)
	}
	for _, crt := range doc.CAs {
		if strings.Contains(crt, "signing key") {
			t.Fatal("ca.key reached the offer")
		}
	}

	// Large bundles stop at the budget: the Clusters past it are left
	// out, the document stays encodable, and the choice follows the
	// servers' order.
	bundle := strings.Repeat(ca, caBudget/len(ca)/3+1)
	var large offer.Document
	names := []string{"a", "b", "c", "d", "e"}
	secrets := make([]client.Object, 0, len(names))
	for _, name := range names {
		large.Servers = append(large.Servers, offer.Server{Cluster: name, CASecretName: name + "-ca"})
		secrets = append(secrets, caSecret("team-a", name+"-ca", bundle))
	}
	r.APIReader = fake.NewClientBuilder().WithObjects(secrets...).Build()
	if err := r.offerCAs(context.Background(), p, &large); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(large.CAs)); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("CAs for %v, want a and b only (c would pass the budget)", got)
	}

	failing := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return errors.New("etcd is away")
	}}).Build()
	r.APIReader = failing
	if err := r.offerCAs(context.Background(), p, &offer.Document{Servers: doc.Servers[:1]}); err == nil {
		t.Fatal("a failed read other than NotFound must fail the loop")
	}
}
