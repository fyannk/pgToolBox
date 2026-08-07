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
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/fyannk/pgtoolbox/internal/adminsync"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestGenerateAdminSyncMaterial(t *testing.T) {
	data, err := generateAdminSyncMaterial()
	if err != nil {
		t.Fatalf("generateAdminSyncMaterial() error = %v", err)
	}
	if len(data[adminsync.SidecarSecretTokenKey]) < 32 {
		t.Errorf("token length = %d, want >= 32", len(data[adminsync.SidecarSecretTokenKey]))
	}
	block, _ := pem.Decode(data[adminsync.SidecarSecretCertKey])
	if block == nil {
		t.Fatal("tls.crt is not PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse generated certificate: %v", err)
	}
	if len(certificate.DNSNames) != 1 || certificate.DNSNames[0] != adminsync.SidecarServerName {
		t.Errorf("certificate SANs = %v, want [%s]", certificate.DNSNames, adminsync.SidecarServerName)
	}
}

func TestReconcileAdminSyncSecretCreatesAndReuses(t *testing.T) {
	console := testConsole()
	r, c := newTestReconciler(t, console, testCluster())

	rev1, err := r.reconcileAdminSyncSecret(context.Background(), console)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if rev1 == "" {
		t.Fatal("expected non-empty resource version on creation")
	}

	var secret corev1.Secret
	key := types.NamespacedName{Namespace: console.Namespace, Name: adminsync.SidecarSecretName(console.Name)}
	if err := c.Get(context.Background(), key, &secret); err != nil {
		t.Fatalf("get admin-sync secret: %v", err)
	}
	if !adminSyncSecretUsable(&secret) {
		t.Fatal("generated secret is not usable")
	}

	rev2, err := r.reconcileAdminSyncSecret(context.Background(), console)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if rev2 != rev1 {
		t.Errorf("resource version changed on steady-state reconcile: %s -> %s", rev1, rev2)
	}
}
