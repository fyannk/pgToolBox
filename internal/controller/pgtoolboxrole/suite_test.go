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

package pgtoolboxrole

import (
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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

func newTestReconciler(t *testing.T, objects ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxRole{}).
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

func testConsole() *pgtoolboxv1alpha1.PgConsole {
	return &pgtoolboxv1alpha1.PgConsole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "console",
			Namespace: "test",
			UID:       types.UID("uid-console"),
		},
		Spec: pgtoolboxv1alpha1.PgConsoleSpec{
			CNPGClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "cluster-1"},
		},
	}
}

func testRoleProfile() *pgtoolboxv1alpha1.PgToolBoxRole {
	return &pgtoolboxv1alpha1.PgToolBoxRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "monitor-role",
			Namespace: "test",
			UID:       types.UID("uid-role"),
		},
		Spec: pgtoolboxv1alpha1.PgToolBoxRoleSpec{
			PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "console"},
			Level:        pgtoolboxv1alpha1.RoleLevelView,
			PostgresRole: pgtoolboxv1alpha1.PostgresRoleSpec{
				Profile: pgtoolboxv1alpha1.PostgresRoleProfileMonitor,
			},
		},
	}
}

func testRoleRef() *pgtoolboxv1alpha1.PgToolBoxRole {
	role := testRoleProfile()
	role.Name = "byoref-role"
	role.UID = types.UID("uid-role-ref")
	role.Spec.PostgresRole = pgtoolboxv1alpha1.PostgresRoleSpec{
		DatabaseRoleRef: &pgtoolboxv1alpha1.LocalObjectReference{Name: "existing-role"},
	}
	return role
}

func conditionOf(role *pgtoolboxv1alpha1.PgToolBoxRole, conditionType string) *metav1.Condition {
	for i := range role.Status.Conditions {
		if role.Status.Conditions[i].Type == conditionType {
			return &role.Status.Conditions[i]
		}
	}
	return nil
}
