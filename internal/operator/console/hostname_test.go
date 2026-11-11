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
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestDisallowedHostnameWithdrawnWhateverTheImage: a hostname outside
// EXPOSURE_ALLOWED_HOST_SUFFIXES withdraws the published Ingress even when
// the image is refused too (or no image resolves), and the status names
// both problems under the hostname's reason.
func TestDisallowedHostnameWithdrawnWhateverTheImage(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		image        *pgtoolboxv1alpha1.ImageSpec
		defaultImage string
		imageText    string
	}{
		"image outside the allow-list": {
			image:        &pgtoolboxv1alpha1.ImageSpec{Repository: "evil.example/pgtoolbox-console", Tag: "1"},
			defaultImage: "registry.example/pgtoolbox-console:1",
			imageText:    "evil.example/pgtoolbox-console is not in the operator's allow-list",
		},
		"no image at all": {imageText: "spec.image is not set"},
		"image allowed":   {defaultImage: "registry.example/pgtoolbox-console:1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a", UID: "u", Generation: 1}}
			c.Spec.Image = tc.image
			c.Spec.Exposure.Type, c.Spec.Exposure.Hostname = pgtoolboxv1alpha1.ExposureTypeIngress, "login.corp.example.com"
			// Published before the allow-list existed.
			c.Status.URL = "https://login.corp.example.com"
			c.Status.Conditions = []metav1.Condition{{
				Type: pgtoolboxv1alpha1.ConditionExposureReady, Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonAsExpected.String(),
				ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
			}}
			published := owned(c, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: c.WorkloadName(), Namespace: c.Namespace}})
			cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(c, published).
				WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxConsole{}).Build()
			r := &Reconciler{
				Client: cl, APIReader: cl, DefaultImage: tc.defaultImage,
				AllowedImageRepositories: []string{"registry.example/pgtoolbox-console"},
				AllowedHostSuffixes:      []string{"apps.example.com"},
			}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
				t.Fatal(err)
			}
			err := cl.Get(context.Background(), client.ObjectKeyFromObject(published), &networkingv1.Ingress{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("the Ingress under the disallowed hostname is still published (get: %v)", err)
			}
			got := &pgtoolboxv1alpha1.PgToolBoxConsole{}
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
				t.Fatal(err)
			}
			valid := meta.FindStatusCondition(got.Status.Conditions, pgtoolboxv1alpha1.ConditionConfigurationValid)
			if valid == nil || valid.Status != metav1.ConditionFalse || valid.Reason != pgtoolboxv1alpha1.ReasonHostnameNotAllowed.String() {
				t.Fatalf("ConfigurationValid = %+v, want False/HostnameNotAllowed", valid)
			}
			if !strings.Contains(valid.Message, "login.corp.example.com") || !strings.Contains(valid.Message, tc.imageText) {
				t.Fatalf("ConfigurationValid message %q does not name the hostname and %q", valid.Message, tc.imageText)
			}
			if tc.imageText == "" && strings.Contains(valid.Message, "also") {
				t.Fatalf("ConfigurationValid message %q names an image problem there is none of", valid.Message)
			}
			// Nothing stale says the console is still reachable.
			if ready := meta.FindStatusCondition(got.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureReady); ready == nil ||
				ready.Status != metav1.ConditionFalse || ready.Reason != pgtoolboxv1alpha1.ReasonHostnameNotAllowed.String() {
				t.Fatalf("ExposureReady = %+v, want False/HostnameNotAllowed", ready)
			}
			if got.Status.URL != "" {
				t.Fatalf("status.url = %q while nothing is published", got.Status.URL)
			}
		})
	}
}

// hostnameHarness is a console published under login.corp.example.com
// before the allow-list existed, with a fake client and a recorder.
func hostnameHarness(t *testing.T, annotations map[string]string, image *pgtoolboxv1alpha1.ImageSpec) (*Reconciler, client.Client, *pgtoolboxv1alpha1.PgToolBoxConsole, *networkingv1.Ingress, *events.FakeRecorder) {
	t.Helper()
	c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a", UID: "u", Generation: 1, Annotations: annotations}}
	c.Spec.Image = image
	c.Spec.Exposure.Type, c.Spec.Exposure.Hostname = pgtoolboxv1alpha1.ExposureTypeIngress, "login.corp.example.com"
	c.Status.URL = "https://login.corp.example.com"
	published := owned(c, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: c.WorkloadName(), Namespace: c.Namespace}})
	cl := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(c, published).
		WithStatusSubresource(&pgtoolboxv1alpha1.PgToolBoxConsole{}).Build()
	recorder := events.NewFakeRecorder(100)
	r := &Reconciler{
		Client: cl, APIReader: cl, Recorder: recorder, DefaultImage: "registry.example/pgtoolbox-console:1",
		AllowedImageRepositories: []string{"registry.example/pgtoolbox-console"},
		AllowedHostSuffixes:      []string{"apps.example.com"},
	}
	return r, cl, c, published, recorder
}

func reconcileConsole(t *testing.T, r *Reconciler, cl client.Client, c *pgtoolboxv1alpha1.PgToolBoxConsole) *pgtoolboxv1alpha1.PgToolBoxConsole {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	got := &pgtoolboxv1alpha1.PgToolBoxConsole{}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(c), got); err != nil {
		t.Fatal(err)
	}
	return got
}

func drainEvents(recorder *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func countWarnings(evs []string, reason pgtoolboxv1alpha1.Reason) int {
	n := 0
	for _, e := range evs {
		if strings.HasPrefix(e, "Warning "+reason.String()) {
			n++
		}
	}
	return n
}

func expectConsoleCondition(t *testing.T, c *pgtoolboxv1alpha1.PgToolBoxConsole, conditionType string, status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason) {
	t.Helper()
	got := meta.FindStatusCondition(c.Status.Conditions, conditionType)
	if got == nil || got.Status != status || got.Reason != reason.String() {
		t.Fatalf("%s = %+v, want %s/%s", conditionType, got, status, reason)
	}
}

// TestConsoleHostnameFixedWhileAnotherStallHolds: once the hostname is
// allowed, a loop that stalls on something else (a refused image) no longer
// reports HostnameNotAllowed on ExposureReady; the next loop is a no-op and
// no second HostnameNotAllowed Warning is emitted.
func TestConsoleHostnameFixedWhileAnotherStallHolds(t *testing.T) {
	t.Parallel()
	r, cl, c, _, recorder := hostnameHarness(t, nil, &pgtoolboxv1alpha1.ImageSpec{Repository: "evil.example/pgtoolbox-console", Tag: "1"})
	refused := reconcileConsole(t, r, cl, c)
	expectConsoleCondition(t, refused, pgtoolboxv1alpha1.ConditionExposureReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
	if n := countWarnings(drainEvents(recorder), pgtoolboxv1alpha1.ReasonHostnameNotAllowed); n != 1 {
		t.Fatalf("HostnameNotAllowed Warnings = %d, want 1", n)
	}

	r.AllowedHostSuffixes = []string{"corp.example.com"}
	got := reconcileConsole(t, r, cl, c)
	expectConsoleCondition(t, got, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonImageNotAllowed)
	expectConsoleCondition(t, got, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonImageNotAllowed)
	expectConsoleCondition(t, got, pgtoolboxv1alpha1.ConditionExposureReady, metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciling)
	evs := drainEvents(recorder)
	if n := countWarnings(evs, pgtoolboxv1alpha1.ReasonHostnameNotAllowed); n != 0 {
		t.Errorf("HostnameNotAllowed Warnings after the fix = %d (%v)", n, evs)
	}
	if n := countWarnings(evs, pgtoolboxv1alpha1.ReasonImageNotAllowed); n != 1 {
		t.Errorf("ImageNotAllowed Warnings = %d, want 1 (%v)", n, evs)
	}
	if again := reconcileConsole(t, r, cl, c); again.ResourceVersion != got.ResourceVersion {
		t.Fatal("the loop after the fix is not a no-op")
	}
	if evs := drainEvents(recorder); len(evs) != 0 {
		t.Errorf("a no-op loop emitted %v", evs)
	}
}

// TestPausedConsoleWithdrawsRefusedHostname: a paused console
// (reconcile=skip) does not keep an Ingress published under a refused
// hostname; the stall is published and the next loop is a no-op.
func TestPausedConsoleWithdrawsRefusedHostname(t *testing.T) {
	t.Parallel()
	r, cl, c, published, recorder := hostnameHarness(t,
		map[string]string{pgtoolboxv1alpha1.AnnotationReconcile: pgtoolboxv1alpha1.ReconcileSkip}, nil)
	got := reconcileConsole(t, r, cl, c)
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(published), &networkingv1.Ingress{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a paused console keeps the Ingress under a refused hostname (get: %v)", err)
	}
	expectConsoleCondition(t, got, pgtoolboxv1alpha1.ConditionExposureReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
	expectConsoleCondition(t, got, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
	expectConsoleCondition(t, got, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
	expectConsoleCondition(t, got, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciliationPaused)
	if got.Status.URL != "" {
		t.Errorf("status.url = %q while nothing is published", got.Status.URL)
	}
	if n := countWarnings(drainEvents(recorder), pgtoolboxv1alpha1.ReasonHostnameNotAllowed); n != 1 {
		t.Errorf("HostnameNotAllowed Warnings = %d, want 1", n)
	}
	if again := reconcileConsole(t, r, cl, c); again.ResourceVersion != got.ResourceVersion {
		t.Fatal("a paused, refused console's status is rewritten on every loop")
	}
	// A paused console with an allowed hostname touches nothing.
	r.AllowedHostSuffixes = nil
	if allowed := reconcileConsole(t, r, cl, c); meta.FindStatusCondition(allowed.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureReady).Reason ==
		pgtoolboxv1alpha1.ReasonHostnameNotAllowed.String() {
		t.Error("ExposureReady still says HostnameNotAllowed once the hostname is allowed")
	}
}
