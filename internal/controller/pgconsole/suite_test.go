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
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testScheme registers every kind the reconciler touches.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		pgtoolboxv1alpha1.AddToScheme,
		cnpgv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("register scheme: %v", err)
		}
	}
	return scheme
}

// newTestReconciler builds a Reconciler backed by a fake client; the same
// client serves cached and live reads.
func newTestReconciler(t *testing.T, objects ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&pgtoolboxv1alpha1.PgConsole{}).
		WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxUser{}).
		WithStatusSubresource(&cnpgv1.DatabaseRole{}).
		Build()
	return &Reconciler{
		Runtime: shared.Runtime{
			Client:    fakeClient,
			APIReader: fakeClient,
			Scheme:    scheme,
			Recorder:  record.NewFakeRecorder(64),
		},
	}, fakeClient
}

// testConsole returns a minimal valid PgConsole in local authentication
// mode, with explicit images everywhere so no operator default is needed.
func testConsole() *pgtoolboxv1alpha1.PgConsole {
	return &pgtoolboxv1alpha1.PgConsole{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "console",
			Namespace:  "test",
			UID:        types.UID("uid-console"),
			Generation: 1,
		},
		Spec: pgtoolboxv1alpha1.PgConsoleSpec{
			CNPGClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "cluster-1"},
			Image: &pgtoolboxv1alpha1.ImageSpec{
				Repository: "example.com/pgconsole",
				Tag:        "1.0.0",
			},
			Proxy: pgtoolboxv1alpha1.ProxySpec{
				Image: &pgtoolboxv1alpha1.ImageSpec{
					Repository: "example.com/proxy",
					Tag:        "1.0.0",
				},
				Authentication: pgtoolboxv1alpha1.ProxyAuthenticationSpec{
					Mode: pgtoolboxv1alpha1.ProxyAuthenticationModeLocal,
				},
			},
			PgAdmin: pgtoolboxv1alpha1.PgAdminSpec{
				Image: &pgtoolboxv1alpha1.ImageSpec{
					Repository: "example.com/pgadmin",
					Tag:        "8.0",
				},
			},
		},
	}
}

// testCluster returns the CNPG Cluster testConsole attaches to.
func testCluster() *cnpgv1.Cluster {
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-1",
			Namespace: "test",
			UID:       types.UID("uid-cluster"),
		},
	}
}

// testPgToolBoxUser returns a user referencing the given role with a local
// password secret.
func testPgToolBoxUser(name string, level pgtoolboxv1alpha1.RoleLevel, passwordSecretName string) *pgtoolboxv1alpha1.PgToolBoxUser {
	return &pgtoolboxv1alpha1.PgToolBoxUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test",
			UID:       types.UID("uid-" + name),
		},
		Spec: pgtoolboxv1alpha1.PgToolBoxUserSpec{
			PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "console"},
			Subject:      name + "@example.com",
			Level:        level,
			LocalPasswordSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{
				Name: passwordSecretName,
			},
		},
	}
}

// testDatabaseRole returns a DatabaseRole with the password secret already
// applied.

// testLocalPasswordSecret returns a Secret holding a bcrypt hash.
func testLocalPasswordSecret(name, hash string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"},
		Data:       map[string][]byte{"password": []byte(hash)},
	}
}
