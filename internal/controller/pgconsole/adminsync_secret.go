/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package pgconsole

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// reconcileAdminSyncSecret generates the per-console material of the
// admin-sync sidecar API: a self-signed serving certificate with the fixed
// DNS SAN the operator pins, and the shared bearer token. The resource
// version participates in the workload checksum so regeneration rolls the
// Deployment.
func (r *Reconciler) reconcileAdminSyncSecret(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) (string, error) {
	key := client.ObjectKey{
		Namespace: console.Namespace,
		Name:      adminsync.SidecarSecretName(console.Name),
	}
	var existing corev1.Secret
	err := r.APIReader.Get(ctx, key, &existing)
	if err == nil {
		if adminSyncSecretUsable(&existing) {
			return existing.ResourceVersion, nil
		}
		return "", fmt.Errorf("admin-sync secret %s/%s is invalid; delete it to regenerate", key.Namespace, key.Name)
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}

	data, err := generateAdminSyncMaterial()
	if err != nil {
		return "", err
	}
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := controllerutil.SetControllerReference(console, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		// The prior live read makes AlreadyExists a concurrent-reconcile
		// race only; surface it and let the next reconcile pick the
		// existing object up.
		return "", fmt.Errorf("create %s/%s: %w", key.Namespace, key.Name, err)
	}
	return secret.ResourceVersion, nil
}

// adminSyncSecretUsable reports whether an existing admin-sync Secret
// holds the full certificate, key and token material; the caller never
// overwrites a partial Secret and instead asks for its manual deletion.
func adminSyncSecretUsable(secret *corev1.Secret) bool {
	return len(secret.Data[adminsync.SidecarSecretCAKey]) > 0 &&
		len(secret.Data[adminsync.SidecarSecretCertKey]) > 0 &&
		len(secret.Data[adminsync.SidecarSecretKeyKey]) > 0 &&
		len(secret.Data[adminsync.SidecarSecretTokenKey]) >= 32
}

// generateAdminSyncMaterial mints the self-signed serving certificate —
// pinned to the fixed sidecar DNS SAN — and the random bearer token for
// the admin-sync API. The output is random, so it is generated once per
// instance and reused thereafter; regenerating it would roll the
// Deployment.
func generateAdminSyncMaterial() (map[string][]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate admin-sync key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate admin-sync serial: %w", err)
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: adminsync.SidecarServerName},
		DNSNames:              []string{adminsync.SidecarServerName},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certificate, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create admin-sync certificate: %w", err)
	}
	privateKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode admin-sync key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKey})

	token := make([]byte, 36)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("generate admin-sync token: %w", err)
	}

	return map[string][]byte{
		adminsync.SidecarSecretCAKey:    certPEM,
		adminsync.SidecarSecretCertKey:  certPEM,
		adminsync.SidecarSecretKeyKey:   keyPEM,
		adminsync.SidecarSecretTokenKey: []byte(base64.RawURLEncoding.EncodeToString(token)),
	}, nil
}
