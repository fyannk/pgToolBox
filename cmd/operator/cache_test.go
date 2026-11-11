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
	"maps"
	"os"
	"slices"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/console"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// TestSecretCacheOptions: whatever the controllers register, the Secret
// informer ends up with the metadata transform, and a label filter a
// controller sets on Secrets is kept.
func TestSecretCacheOptions(t *testing.T) {
	filtered := labels.SelectorFromSet(labels.Set{"a": "b"})
	for _, tc := range []struct {
		name     string
		existing map[client.Object]cache.ByObject
		label    labels.Selector
	}{
		{name: "no Secret entry"},
		{name: "a controller's Secret filter", existing: map[client.Object]cache.ByObject{&corev1.Secret{}: {Label: filtered}}, label: filtered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := &ctrl.Options{}
			options.Cache.ByObject = tc.existing
			secretCacheOptions(options)
			found := 0
			for object, byObject := range options.Cache.ByObject {
				if _, ok := object.(*corev1.Secret); !ok {
					continue
				}
				found++
				if byObject.Transform == nil {
					t.Error("Secret informer without a transform")
				}
				if selectorString(byObject.Label) != selectorString(tc.label) {
					t.Errorf("label = %v, want %v", byObject.Label, tc.label)
				}
			}
			if found != 1 {
				t.Fatalf("%d Secret entries, want 1", found)
			}
		})
	}
	options := &ctrl.Options{}
	managerCacheOptions(options, pgAdminPrepared{}, console.Features{})
	for object, byObject := range options.Cache.ByObject {
		if _, ok := object.(*corev1.Secret); ok && byObject.Transform != nil {
			return
		}
	}
	t.Fatal("managerCacheOptions leaves the Secret informer without its transform")
}

// TestEnvtestSecretMetadataCache proves against a real API server that the
// manager's cache, built with the operator's options, never holds a Secret's
// last-applied-configuration (kubectl apply copies the data there) nor its
// other annotations and labels, while it keeps the labels the operator reads.
//
// Needs KUBEBUILDER_ASSETS.
func TestEnvtestSecretMetadataCache(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set")
	}
	env := &envtest.Environment{}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	options := ctrl.Options{Scheme: scheme}
	managerCacheOptions(&options, pgAdminPrepared{}, console.Features{})
	options.Cache.Scheme = scheme
	informers, err := cache.New(config, options.Cache)
	if err != nil {
		t.Fatal(err)
	}
	secretMetadata := &metav1.PartialObjectMetadata{}
	secretMetadata.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	if _, err := informers.GetInformer(ctx, secretMetadata); err != nil {
		t.Fatal(err)
	}
	go func() { _ = informers.Start(ctx) }()
	if !informers.WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	admin, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const applied = `{"apiVersion":"v1","data":{"password":"aHVudGVyMg=="},"kind":"Secret","metadata":{"name":"superuser","namespace":"default"}}`
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "superuser", Namespace: "default",
			Annotations: map[string]string{lastAppliedAnnotation: applied, "team": "a"},
			Labels: map[string]string{
				pgtoolboxv1alpha1.LabelReload: pgtoolboxv1alpha1.ReloadLabelValue,
				"cnpg.io/cluster":             "pg",
			},
		},
		Data: map[string][]byte{"password": []byte("hunter2")},
	}
	if err := admin.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}

	cached := &metav1.PartialObjectMetadata{}
	cached.SetGroupVersionKind(secretMetadata.GroupVersionKind())
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		return informers.Get(ctx, client.ObjectKeyFromObject(secret), cached) == nil, nil
	}); err != nil {
		t.Fatalf("the Secret never reached the cache: %v", err)
	}
	if len(cached.Annotations) != 0 {
		t.Errorf("cached Secret annotations = %v, want none", cached.Annotations)
	}
	if len(cached.Labels) != 1 || cached.Labels[pgtoolboxv1alpha1.LabelReload] != pgtoolboxv1alpha1.ReloadLabelValue {
		t.Errorf("cached Secret labels = %v, want only the reload label", cached.Labels)
	}
	if cached.ManagedFields != nil {
		t.Error("cached Secret keeps managedFields")
	}

	// An update that adds the annotation later is stripped too.
	live := &corev1.Secret{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(secret), live); err != nil {
		t.Fatal(err)
	}
	live.Annotations[lastAppliedAnnotation] = applied + " "
	live.Labels["later"] = "x"
	if err := admin.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		err := informers.Get(ctx, client.ObjectKeyFromObject(secret), cached)
		return err == nil && cached.ResourceVersion == live.ResourceVersion, nil
	}); err != nil {
		t.Fatalf("the update never reached the cache: %v", err)
	}
	if len(cached.Annotations) != 0 || cached.Labels["later"] != "" {
		t.Errorf("after update: annotations %v, labels %v", cached.Annotations, cached.Labels)
	}
}

func selectorString(selector labels.Selector) string {
	if selector == nil {
		return "<nil>"
	}
	return selector.String()
}

// TestOptionalKindCacheOptions: an optional kind refused in some watched
// namespaces is cached only in the others, keeping the label filter a
// controller set on it; a kind with no refusal, or off everywhere, keeps
// the cache's default namespaces (no entry is added for it).
func TestOptionalKindCacheOptions(t *testing.T) {
	watched := []string{"team-a", "team-b", "team-c"}
	refusedIn := func(gvk schema.GroupVersionKind, namespaces ...string) discovery.Result {
		return discovery.Result{GVK: gvk, State: discovery.Available, RefusedIn: namespaces}
	}
	routeGVK := routev1.GroupVersion.WithKind("Route")
	poolerGVK := cnpgv1.SchemeGroupVersion.WithKind("Pooler")
	databaseGVK := cnpgv1.SchemeGroupVersion.WithKind("Database")
	prepared := pgAdminPrepared{
		Features: pgadmin.Features{Routes: refusedIn(routeGVK, "team-b")},
		OptionalKinds: []optionalKind{
			{Object: &routev1.Route{}, Result: refusedIn(routeGVK, "team-b")},
			{Object: &cnpgv1.Pooler{}, Result: refusedIn(poolerGVK, "team-a", "team-c")},
			{Object: &cnpgv1.Database{}, Result: refusedIn(databaseGVK)},
			{Object: metadataObject(discovery.StanzaGVK), Result: refusedIn(discovery.StanzaGVK, "team-c")},
			{Object: metadataObject(discovery.ArchiveGVK), Result: discovery.Result{GVK: discovery.ArchiveGVK, State: discovery.Forbidden}},
		},
	}
	prepared.Config.WatchNamespaces = watched
	options := &ctrl.Options{Scheme: scheme}
	managerCacheOptions(options, prepared, pgToolBoxConsoleFeatures(prepared))

	namespacesOf := func(gvk schema.GroupVersionKind) (cache.ByObject, bool) {
		for object, byObject := range options.Cache.ByObject {
			if got, err := apiutil.GVKForObject(object, scheme); err == nil && got == gvk {
				return byObject, true
			}
		}
		return cache.ByObject{}, false
	}
	for _, tc := range []struct {
		gvk       schema.GroupVersionKind
		want      []string // nil: no namespace restriction
		wantLabel bool
	}{
		{gvk: routeGVK, want: []string{"team-a", "team-c"}, wantLabel: true},
		{gvk: poolerGVK, want: []string{"team-b"}},
		{gvk: databaseGVK},
		{gvk: discovery.StanzaGVK, want: []string{"team-a", "team-b"}},
		{gvk: discovery.ArchiveGVK},
	} {
		byObject, found := namespacesOf(tc.gvk)
		got := slices.Sorted(maps.Keys(byObject.Namespaces))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s cached in %q (entry %t), want %q", tc.gvk.Kind, got, found, tc.want)
		}
		if tc.wantLabel && byObject.Label == nil {
			t.Errorf("%s lost its label filter", tc.gvk.Kind)
		}
	}
}
