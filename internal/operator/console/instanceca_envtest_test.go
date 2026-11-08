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

package console

import (
	"context"
	"reflect"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestInstanceCAMirrorOnEnvtest runs the instance CA mirror as the
// generated manager ClusterRole: the Secret reads, the server-side apply of
// the ConfigMap, renewal, Clusters coming and going, a foreign ConfigMap
// under the mirror's name, and an explicit instanceCABundleRef.
//
//nolint:gocognit,cyclop,funlen // one scenario, step by step.
func TestInstanceCAMirrorOnEnvtest(t *testing.T) {
	s := newEnvtestSuite(t)
	ctx := context.Background()
	s.installShippedRoles(ctx, t)
	alpha, beta := testCA(t, "alpha"), testCA(t, "beta")
	newCluster := func(name, serverCASecret string) {
		t.Helper()
		cluster := &cnpgv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: suiteNamespace, Labels: map[string]string{"team": "a"}},
			Spec:       cnpgv1.ClusterSpec{Instances: 1, StorageConfiguration: cnpgv1.StorageConfiguration{Size: "1Gi"}},
		}
		if serverCASecret != "" {
			cluster.Spec.Certificates = &cnpgv1.CertificatesConfiguration{ServerCASecret: serverCASecret}
		}
		must(t, s.admin.Create(ctx, cluster))
	}
	newCluster("pg-a", "")
	newCluster("pg-b", "shared-ca")
	newCluster("pg-c", "shared-ca")
	newCluster("pg-d", "")
	must(t, s.admin.Create(ctx, caSecret("pg-a-ca", map[string][]byte{"ca.crt": alpha, "ca.key": []byte("SIGNING KEY")})))
	must(t, s.admin.Create(ctx, caSecret("shared-ca", map[string][]byte{"ca.crt": beta, "ca.key": []byte("SIGNING KEY")})))
	console := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: suiteNamespace}}
	console.Spec.ClusterSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}
	must(t, s.admin.Create(ctx, console))

	bundle := func(t *testing.T) *corev1.ConfigMap {
		t.Helper()
		cm := &corev1.ConfigMap{}
		s.get(ctx, t, "console-instance-ca", cm)
		return cm
	}

	t.Run("the mirror holds each distinct CA once and never a key; a missing one is reported, not fatal", func(t *testing.T) {
		s.reconcile(ctx, t, "console")
		cm := bundle(t)
		if cm.Data[caKey] != string(alpha)+string(beta) || len(cm.Data) != 1 || len(cm.BinaryData) != 0 {
			t.Fatalf("data = %v", cm.Data)
		}
		if owner := metav1.GetControllerOf(cm); owner == nil || owner.Kind != "PgToolBoxConsole" || owner.Name != "console" {
			t.Fatalf("controller = %+v", owner)
		}
		if cm.Labels[pgtoolboxv1alpha1.LabelManagedBy] != pgtoolboxv1alpha1.ManagedByValue {
			t.Fatalf("labels = %v: the operator's ConfigMap informer would not see it", cm.Labels)
		}
		got := s.console(ctx, t, "console")
		assertCondition(t, got, pgtoolboxv1alpha1.ConditionInstanceCABundleComplete, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonInstanceCAMissing)
		assertCondition(t, got, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		want := &pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCABundleStatus{
			ConfigMap: "console-instance-ca", KeptBy: pgtoolboxv1alpha1.InstanceCAKeptByOperator, Clusters: 3,
			Missing: []pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCAMissing{{Cluster: "pg-d", Secret: "pg-d-ca", Reason: pgtoolboxv1alpha1.ReasonSecretNotFound}},
		}
		if !reflect.DeepEqual(got.Status.InstanceCABundle, want) {
			t.Fatalf("status.instanceCABundle = %+v", got.Status.InstanceCABundle)
		}
		set := &appsv1.StatefulSet{}
		s.get(ctx, t, "console", set)
		if !mountsBundle(&set.Spec.Template, "console-instance-ca") {
			t.Fatal("the StatefulSet does not mount the mirror")
		}
	})

	t.Run("the missing CA appears; a second loop writes nothing", func(t *testing.T) {
		must(t, s.admin.Create(ctx, caSecret("pg-d-ca", map[string][]byte{"ca.crt": alpha})))
		s.reconcile(ctx, t, "console")
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionInstanceCABundleComplete, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		before := bundle(t)
		s.reconcile(ctx, t, "console")
		if after := bundle(t); after.ResourceVersion != before.ResourceVersion {
			t.Fatal("an unchanged bundle was rewritten")
		}
	})

	t.Run("a renewed CA replaces the old one", func(t *testing.T) {
		renewed := testCA(t, "alpha-renewed")
		for _, name := range []string{"pg-a-ca", "pg-d-ca"} {
			secret := &corev1.Secret{}
			s.get(ctx, t, name, secret)
			secret.Data["ca.crt"] = renewed
			must(t, s.admin.Update(ctx, secret))
		}
		s.reconcile(ctx, t, "console")
		if got := bundle(t).Data[caKey]; got != string(renewed)+string(beta) {
			t.Fatalf("ca.crt after renewal = %q", got)
		}
	})

	t.Run("a Cluster removed or no longer selected leaves the bundle", func(t *testing.T) {
		for _, name := range []string{"pg-a", "pg-d"} {
			must(t, s.admin.Delete(ctx, &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: suiteNamespace}}))
		}
		s.reconcile(ctx, t, "console")
		if got := bundle(t).Data[caKey]; got != string(beta) {
			t.Fatalf("ca.crt = %q, want the shared CA only", got)
		}
		for _, name := range []string{"pg-b", "pg-c"} {
			cluster := &cnpgv1.Cluster{}
			s.get(ctx, t, name, cluster)
			cluster.Labels["team"] = "b"
			must(t, s.admin.Update(ctx, cluster))
		}
		s.reconcile(ctx, t, "console")
		if got := bundle(t).Data[caKey]; got != "" {
			t.Fatalf("ca.crt = %q, want empty", got)
		}
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionInstanceCABundleComplete, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	})

	t.Run("an explicit instanceCABundleRef mounts that ConfigMap and drops the condition", func(t *testing.T) {
		got := s.console(ctx, t, "console")
		got.Spec.InstanceCABundleRef = &pgtoolboxv1alpha1.LocalObjectReference{Name: "my-bundle"}
		must(t, s.admin.Update(ctx, got))
		before := bundle(t)
		// Named but absent: a stall, not a console reading every Cluster as
		// "not verifiable".
		s.reconcile(ctx, t, "console")
		assertCondition(t, s.console(ctx, t, "console"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonConfigMapNotFound)
		must(t, s.admin.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "my-bundle", Namespace: suiteNamespace}}))
		s.reconcile(ctx, t, "console")
		got = s.console(ctx, t, "console")
		if b := got.Status.InstanceCABundle; b == nil || b.ConfigMap != "my-bundle" || b.KeptBy != pgtoolboxv1alpha1.InstanceCAReferenced || b.Clusters != 0 {
			t.Fatalf("status.instanceCABundle = %+v", b)
		}
		for _, c := range got.Status.Conditions {
			if c.Type == pgtoolboxv1alpha1.ConditionInstanceCABundleComplete {
				t.Fatalf("condition %s kept with a referenced bundle", c.Type)
			}
		}
		set := &appsv1.StatefulSet{}
		s.get(ctx, t, "console", set)
		if !mountsBundle(&set.Spec.Template, "my-bundle") {
			t.Fatal("the StatefulSet does not mount the referenced bundle")
		}
		mine := &corev1.ConfigMap{}
		s.get(ctx, t, "my-bundle", mine)
		if len(mine.OwnerReferences) != 0 || len(mine.Data) != 0 {
			t.Fatal("the operator wrote the referenced bundle")
		}
		if after := bundle(t); after.ResourceVersion != before.ResourceVersion {
			t.Fatal("the operator wrote the mirror of a console that references its own bundle")
		}
	})

	t.Run("someone else's ConfigMap under the mirror's name is reported and left alone", func(t *testing.T) {
		must(t, s.admin.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "other-instance-ca", Namespace: suiteNamespace},
			Data:       map[string]string{"ca.crt": "hand copy"},
		}))
		must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: suiteNamespace}}))
		s.reconcile(ctx, t, "other")
		got := s.console(ctx, t, "other")
		assertCondition(t, got, pgtoolboxv1alpha1.ConditionInstanceCABundleComplete, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonResourceNotOwned)
		assertCondition(t, got, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		s.get(ctx, t, "other-console", &appsv1.StatefulSet{})
		cm := &corev1.ConfigMap{}
		s.get(ctx, t, "other-instance-ca", cm)
		if cm.Data["ca.crt"] != "hand copy" {
			t.Fatal("the operator wrote someone else's ConfigMap")
		}
	})
}

// TestInstanceCAMirrorFollowsWatchesOnEnvtest runs the controller in a
// manager wired as the operator binary wires it (label-filtered caches,
// Secret and ConfigMap content never cached) and never calls Reconcile by
// hand: a renewed CA and a deleted Cluster must reach the mirror through
// the Secret and Cluster watches alone.
func TestPgToolBoxConsoleFollowsWatchesOnEnvtest(t *testing.T) {
	s := newEnvtestSuite(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.installShippedRoles(ctx, t)
	s.startManager(ctx, t)

	alpha, beta, renewed := testCA(t, "alpha"), testCA(t, "beta"), testCA(t, "alpha-renewed")
	for _, name := range []string{"pg-a", "pg-b"} {
		must(t, s.admin.Create(ctx, &cnpgv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: suiteNamespace},
			Spec:       cnpgv1.ClusterSpec{Instances: 1, StorageConfiguration: cnpgv1.StorageConfiguration{Size: "1Gi"}},
		}))
	}
	must(t, s.admin.Create(ctx, caSecret("pg-a-ca", map[string][]byte{"ca.crt": alpha})))
	must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: suiteNamespace}}))

	waitForBundle := func(t *testing.T, want string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		var got string
		for time.Now().Before(deadline) {
			cm := &corev1.ConfigMap{}
			if err := s.admin.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: "console-instance-ca"}, cm); err == nil {
				if got = cm.Data[caKey]; got == want {
					return
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("ca.crt = %q, want %q", got, want)
	}

	t.Run("the first loop mirrors what exists", func(t *testing.T) {
		waitForBundle(t, string(alpha))
	})
	t.Run("a CA Secret created later is picked up", func(t *testing.T) {
		must(t, s.admin.Create(ctx, caSecret("pg-b-ca", map[string][]byte{"ca.crt": beta})))
		waitForBundle(t, string(alpha)+string(beta))
	})
	t.Run("a renewal is picked up", func(t *testing.T) {
		secret := &corev1.Secret{}
		s.get(ctx, t, "pg-a-ca", secret)
		secret.Data["ca.crt"] = renewed
		must(t, s.admin.Update(ctx, secret))
		waitForBundle(t, string(renewed)+string(beta))
	})
	t.Run("a deleted Cluster leaves the bundle", func(t *testing.T) {
		must(t, s.admin.Delete(ctx, &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "pg-a", Namespace: suiteNamespace}}))
		waitForBundle(t, string(beta))
	})
	t.Run("a mounted Secret created later releases the stall, through the Secret watch", func(t *testing.T) {
		console := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "digest", Namespace: suiteNamespace}}
		console.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{
			ChatWebhookSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "late-chat", Key: "url"},
		}
		must(t, s.admin.Create(ctx, console))
		waitForConfigurationValid(ctx, t, s, "digest", pgtoolboxv1alpha1.ReasonSecretNotFound)
		// No requeue: only the watch on the referenced Secret brings it back.
		must(t, s.admin.Create(ctx, caSecret("late-chat", map[string][]byte{"url": []byte("test-only")})))
		waitForConfigurationValid(ctx, t, s, "digest", pgtoolboxv1alpha1.ReasonAsExpected)
	})
}

// waitForConfigurationValid waits for the console's ConfigurationValid to
// carry reason.
func waitForConfigurationValid(ctx context.Context, t *testing.T, s *envtestSuite, name string, reason pgtoolboxv1alpha1.Reason) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got *metav1.Condition
	for time.Now().Before(deadline) {
		console := &pgtoolboxv1alpha1.PgToolBoxConsole{}
		if err := s.admin.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: name}, console); err == nil {
			if got = meta.FindStatusCondition(console.Status.Conditions, pgtoolboxv1alpha1.ConditionConfigurationValid); got != nil && got.Reason == reason.String() {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("ConfigurationValid = %+v, want reason %s", got, reason)
}

// startManager runs a reconciler under a manager with the operator's cache
// options, as the operator identity, until the test ends.
func (s *envtestSuite) startManager(ctx context.Context, t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	options := ctrl.Options{
		Scheme:                 s.reconciler.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: new(true)},
	}
	options.Cache.ByObject = CacheByObject(Features{})
	options.Cache.DefaultTransform = cache.TransformStripManagedFields()
	options.Client.Cache = &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}, &corev1.ConfigMap{}}}
	mgr, err := ctrl.NewManager(s.operatorConfig, options)
	must(t, err)
	reconciler := &Reconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(),
		Recorder: events.NewFakeRecorder(256), DefaultImage: s.reconciler.DefaultImage,
	}
	must(t, reconciler.SetupWithManager(ctx, mgr))
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
}

// mountsBundle reports whether the console Pod's instance-ca volume is the
// named ConfigMap.
func mountsBundle(template *corev1.PodTemplateSpec, name string) bool {
	for _, volume := range template.Spec.Volumes {
		if volume.Name == "instance-ca" {
			return volume.ConfigMap != nil && volume.ConfigMap.Name == name
		}
	}
	return false
}
