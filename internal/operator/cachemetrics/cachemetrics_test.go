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

package cachemetrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

var managedLabels = map[string]string{pgtoolboxv1alpha1.LabelManagedBy: pgtoolboxv1alpha1.ManagedByValue}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := pgtoolboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// TestCollectorCountsPerKindAndManaged: typed and metadata-only kinds are
// counted, split on the managed-by label, with an explicit zero for the
// side that has none (a dashboard can alert on managed="false" > 0 for an
// owned kind), and a kind the scheme cannot list is left out, not zero.
func TestCollectorCountsPerKindAndManaged(t *testing.T) {
	scheme := testScheme(t)
	objects := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "team", Labels: managedLabels}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "team", Labels: managedLabels}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "generated", Namespace: "team", Labels: managedLabels}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "user", Namespace: "team"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team"}},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.ServiceList); ok {
				return errors.New("the cache is not started")
			}
			return c.List(ctx, list, opts...)
		},
	}).Build()
	collector := Collector{Reader: reader, Scheme: scheme, Kinds: []client.Object{
		&appsv1.Deployment{},
		&pgtoolboxv1alpha1.PgAdmin{},
		Metadata(corev1.SchemeGroupVersion.WithKind("Secret")),
		&corev1.Service{},
		&unregistered{},
	}}
	expected := `
# HELP pgtoolbox_cached_objects Objects held in the operator's cache, per kind, split on whether pgtoolbox manages them (app.kubernetes.io/managed-by=pgtoolbox).
# TYPE pgtoolbox_cached_objects gauge
pgtoolbox_cached_objects{kind="Deployment",managed="false"} 0
pgtoolbox_cached_objects{kind="Deployment",managed="true"} 2
pgtoolbox_cached_objects{kind="PgAdmin",managed="false"} 0
pgtoolbox_cached_objects{kind="PgAdmin",managed="true"} 0
pgtoolbox_cached_objects{kind="Secret",managed="false"} 2
pgtoolbox_cached_objects{kind="Secret",managed="true"} 1
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

// TestUnionKeepsOneSeriesPairPerKind: two controllers watching the same
// kind share one informer, so the registry must not see the kind twice
// (a duplicate series fails the whole scrape).
func TestUnionKeepsOneSeriesPairPerKind(t *testing.T) {
	scheme := testScheme(t)
	secret := Metadata(corev1.SchemeGroupVersion.WithKind("Secret"))
	got := Union(scheme,
		[]client.Object{&appsv1.Deployment{}, secret},
		[]client.Object{&appsv1.Deployment{}, Metadata(corev1.SchemeGroupVersion.WithKind("Secret")), &corev1.Service{}},
	)
	if len(got) != 3 {
		t.Fatalf("union = %d kinds, want 3 (Deployment, Secret, Service)", len(got))
	}
	if got[1] != secret {
		t.Fatal("union does not keep the first appearance")
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	if problems, err := testutil.CollectAndLint(Collector{Reader: reader, Scheme: scheme, Kinds: got}); err != nil || len(problems) > 0 {
		t.Fatalf("lint: %v %v", problems, err)
	}
}

// unregistered is a kind no scheme knows.
type unregistered struct{ corev1.ConfigMap }

func (u *unregistered) DeepCopyObject() runtime.Object {
	return &unregistered{ConfigMap: *u.ConfigMap.DeepCopy()}
}
