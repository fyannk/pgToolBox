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

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func TestErrorReason(t *testing.T) {
	gr := schema.GroupResource{Resource: "deployments"}
	cases := []struct {
		name string
		err  error
		want pgtoolboxv1alpha1.Reason
	}{
		{"nil", nil, ""},
		{"next loop", NextLoop("pod not ready"), ""},
		{"canceled", context.Canceled, ""},
		{"conflict", apierrors.NewConflict(gr, "x", errors.New("c")), ""},
		{"terminal", ctrlreconcile.TerminalError(errors.New("bad input")), ""},
		{"quota", apierrors.NewForbidden(gr, "x", errors.New("exceeded quota: q")), pgtoolboxv1alpha1.ReasonQuotaExceeded},
		{"psa", apierrors.NewForbidden(gr, "x", errors.New("violates PodSecurity \"restricted\"")), pgtoolboxv1alpha1.ReasonAdmissionDenied},
		{"webhook", apierrors.NewForbidden(gr, "x", errors.New("admission webhook denied the request")), pgtoolboxv1alpha1.ReasonAdmissionDenied},
		{"other forbidden", apierrors.NewForbidden(gr, "x", errors.New("no")), pgtoolboxv1alpha1.ReasonReconciliationFailed},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "x", nil), pgtoolboxv1alpha1.ReasonAdmissionDenied},
		{"stalled", Stall(pgtoolboxv1alpha1.ReasonSecretNotFound, "no Secret"), ""},
		{"wrapped stall", fmt.Errorf("resolve: %w", Stall(pgtoolboxv1alpha1.ReasonSecretNotFound, "no Secret")), ""},
		{"wrapped other", fmt.Errorf("apply: %w", errors.New("boom")), pgtoolboxv1alpha1.ReasonReconciliationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorReason(tc.err); got != tc.want {
				t.Fatalf("ErrorReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBoundedMessage(t *testing.T) {
	long := strings.Repeat("é", MaxMessageLength)
	got := BoundedMessage(long)
	if len(got) > MaxMessageLength || !strings.HasSuffix(got, "...") {
		t.Fatalf("len %d", len(got))
	}
	if BoundedMessage("short") != "short" {
		t.Fatal("short message changed")
	}
}

func TestControls(t *testing.T) {
	owner := metav1.OwnerReference{APIVersion: "v1", Kind: "X", Name: "o", UID: "u1", Controller: new(true)}
	other := owner
	other.UID = "u2"
	labelled := map[string]string{pgtoolboxv1alpha1.LabelManagedBy: "pgtoolbox", pgtoolboxv1alpha1.PgAdminLabelKey: "team"}
	cases := []struct {
		name             string
		existing, wanted metav1.ObjectMeta
		adopt            bool
		want             bool
	}{
		{"same controller", metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{owner}}, metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{owner}}, false, true},
		{"other controller", metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{other}}, metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{owner}}, true, false},
		{"planted object with our labels is not adopted", metav1.ObjectMeta{Labels: labelled}, metav1.ObjectMeta{Labels: labelled, OwnerReferences: []metav1.OwnerReference{owner}}, false, false},
		{"retained volume with our labels is adopted", metav1.ObjectMeta{Labels: labelled}, metav1.ObjectMeta{Labels: labelled, OwnerReferences: []metav1.OwnerReference{owner}}, true, true},
		{"retained volume desired without controller", metav1.ObjectMeta{Labels: labelled}, metav1.ObjectMeta{Labels: labelled}, true, true},
		{"foreign object without labels", metav1.ObjectMeta{}, metav1.ObjectMeta{Labels: labelled, OwnerReferences: []metav1.OwnerReference{owner}}, true, false},
		{"labels of another PgAdmin", metav1.ObjectMeta{Labels: map[string]string{pgtoolboxv1alpha1.LabelManagedBy: "pgtoolbox", pgtoolboxv1alpha1.PgAdminLabelKey: "other"}}, metav1.ObjectMeta{Labels: labelled}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Controls(&tc.existing, &tc.wanted, tc.adopt); got != tc.want {
				t.Fatalf("Controls = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplierRefusesForeignObjectOutsideTheCache(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "team-pgadmin", Namespace: "ns"}}
	// The cache is label-filtered: model it as a client that has nothing,
	// and the API server as one that has the foreign object.
	cached := fake.NewClientBuilder().WithScheme(scheme).Build()
	live := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
	applier := Applier{Client: cached, APIReader: live, Scheme: scheme}
	desired := &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: "team-pgadmin", Namespace: "ns", Labels: map[string]string{pgtoolboxv1alpha1.LabelManagedBy: "pgtoolbox"}},
	}
	err := applier.Apply(context.Background(), desired)
	var notOwned *NotOwnedError
	if !errors.As(err, &notOwned) {
		t.Fatalf("Apply = %v, want NotOwnedError", err)
	}
}

func TestApplierCreatesAndIsIdempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	applier := Applier{Client: c, APIReader: c, Scheme: scheme}
	desired := func() *appsv1.Deployment {
		return &appsv1.Deployment{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
			ObjectMeta: metav1.ObjectMeta{Name: "d", Namespace: "ns", Labels: map[string]string{pgtoolboxv1alpha1.LabelManagedBy: "pgtoolbox"}},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "b"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}},
				},
			},
		}
	}
	if err := applier.Apply(context.Background(), desired()); err != nil {
		t.Fatal(err)
	}
	var got appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "d"}, &got); err != nil {
		t.Fatal(err)
	}
	// A second apply must succeed; that it writes nothing is proven
	// against a real API server (the PgAdmin envtest suite), the fake
	// client's apply semantics do not model managedFields faithfully.
	if err := applier.Apply(context.Background(), desired()); err != nil {
		t.Fatal(err)
	}
}

func TestNotOwnedErrorNamesScope(t *testing.T) {
	namespaced := (&NotOwnedError{Kind: "Service", Namespace: "a", Name: "b"}).Error()
	if !strings.HasPrefix(namespaced, "Service a/b exists and is not controlled by this resource") {
		t.Errorf("namespaced: %q", namespaced)
	}
	cluster := (&NotOwnedError{Kind: "ClusterRole", Name: "r"}).Error()
	if !strings.HasPrefix(cluster, "ClusterRole r exists and is not managed by pgtoolbox") {
		t.Errorf("cluster-scoped: %q", cluster)
	}
}

func TestStallErrorIsErrStalled(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", Stall(pgtoolboxv1alpha1.ReasonResourceNotOwned, "%s taken", "Service x"))
	if !errors.Is(err, ErrStalled) {
		t.Fatal("a wrapped StallError is not ErrStalled")
	}
	var stall *StallError
	if !errors.As(err, &stall) || stall.Reason != pgtoolboxv1alpha1.ReasonResourceNotOwned || stall.Message != "Service x taken" {
		t.Fatalf("stall = %+v", stall)
	}
}
