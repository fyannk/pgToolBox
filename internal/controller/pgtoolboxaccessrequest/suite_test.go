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

package pgtoolboxaccessrequest

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
		WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxAccessRequest{}).
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

func testAccessRequest(state pgtoolboxv1alpha1.AccessRequestState, level pgtoolboxv1alpha1.RoleLevel) *pgtoolboxv1alpha1.PgToolBoxAccessRequest {
	req := &pgtoolboxv1alpha1.PgToolBoxAccessRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "req-1",
			Namespace: "test",
			UID:       types.UID("uid-req"),
		},
		Spec: pgtoolboxv1alpha1.PgToolBoxAccessRequestSpec{
			PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "console"},
			Subject:      "alice@example.com",
		},
		Status: pgtoolboxv1alpha1.PgToolBoxAccessRequestStatus{
			State: state,
		},
	}
	req.Status.RequestedLevel = level
	return req
}

func conditionOf(req *pgtoolboxv1alpha1.PgToolBoxAccessRequest, conditionType string) *metav1.Condition {
	for i := range req.Status.Conditions {
		if req.Status.Conditions[i].Type == conditionType {
			return &req.Status.Conditions[i]
		}
	}
	return nil
}
