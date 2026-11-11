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

package pgadmin

import (
	"context"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func hostnamePgAdmin(hostname string) *pgtoolboxv1alpha1.PgAdmin {
	p := oauth2PgAdmin()
	p.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: hostname}
	return p
}

func warnings(events []string, reason pgtoolboxv1alpha1.Reason) int {
	n := 0
	for _, e := range events {
		if strings.HasPrefix(e, "Warning "+reason.String()) {
			n++
		}
	}
	return n
}

// TestHostnameFixedWhileAnotherStallHolds: once the hostname is allowed, a
// loop that stalls on something else before the exposure (a refused image)
// no longer reports HostnameNotAllowed on ExposureReady (the round-11
// finding: it kept the refusal until the image was fixed too); the next
// loop is a no-op, and no second HostnameNotAllowed Warning is emitted.
func TestHostnameFixedWhileAnotherStallHolds(t *testing.T) {
	p := hostnamePgAdmin("login.corp.example.com")
	p.Spec.Image = &pgtoolboxv1alpha1.ImageSpec{Repository: "evil.example/pgadmin", Tag: "1"}
	h := newHarness(t, nil, baseObjects(p)...)
	h.r.Config.PgAdminImageRepositories = []string{"registry.example/pgadmin"}
	h.r.Config.ExposureAllowedHostSuffixes = []string{"apps.example.com"}
	if err := h.reconcile(p.Name); err != nil {
		t.Fatal(err)
	}
	refused := h.pgadmin(p.Name)
	expectCondition(t, refused, "ExposureReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
	expectCondition(t, refused, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
	if n := warnings(h.events(), pgtoolboxv1alpha1.ReasonHostnameNotAllowed); n != 1 {
		t.Fatalf("HostnameNotAllowed Warnings = %d, want 1", n)
	}

	// The installer allows the name; the image is still refused.
	h.r.Config.ExposureAllowedHostSuffixes = []string{"corp.example.com"}
	if err := h.reconcile(p.Name); err != nil {
		t.Fatal(err)
	}
	got := h.pgadmin(p.Name)
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonImageNotAllowed)
	expectCondition(t, got, "Stalled", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonImageNotAllowed)
	expectCondition(t, got, "ExposureReady", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciling)
	if c := condition(got, "Ready"); c.Reason == pgtoolboxv1alpha1.ReasonHostnameNotAllowed.String() {
		t.Errorf("Ready still says the hostname is refused: %+v", c)
	}
	events := h.events()
	if n := warnings(events, pgtoolboxv1alpha1.ReasonHostnameNotAllowed); n != 0 {
		t.Errorf("HostnameNotAllowed Warnings after the fix = %d, want 0 (%v)", n, events)
	}
	if n := warnings(events, pgtoolboxv1alpha1.ReasonImageNotAllowed); n != 1 {
		t.Errorf("ImageNotAllowed Warnings = %d, want 1 (%v)", n, events)
	}
	if err := h.reconcile(p.Name); err != nil {
		t.Fatal(err)
	}
	if again := h.pgadmin(p.Name); again.ResourceVersion != got.ResourceVersion {
		t.Fatal("the loop after the fix is not a no-op")
	}
	if events := h.events(); len(events) != 0 {
		t.Errorf("a no-op loop emitted %v", events)
	}

	// The image fixed too: published under the allowed name.
	h.r.Config.PgAdminImageRepositories = []string{"evil.example/pgadmin"}
	if err := h.reconcile(p.Name); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: p.Name + "-pgadmin"}, &networkingv1.Ingress{}); err != nil {
		t.Fatalf("the Ingress is not published once both are fixed: %v", err)
	}
	if c := condition(h.pgadmin(p.Name), "ExposureReady"); c.Reason == pgtoolboxv1alpha1.ReasonReconciling.String() && c.Message == "" {
		t.Errorf("ExposureReady not read from the Ingress: %+v", c)
	}
}

// TestPausedPgAdminWithdrawsRefusedHostname: a paused PgAdmin
// (reconcile=skip) does not keep an Ingress published under a hostname
// the installer refuses: it is withdrawn, ExposureReady and status.url say
// so, the stall is published, and the next loop is a no-op.
func TestPausedPgAdminWithdrawsRefusedHostname(t *testing.T) {
	p := hostnamePgAdmin("login.corp.example.com")
	h := newHarness(t, nil, baseObjects(p)...)
	if err := h.reconcile(p.Name); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: testNamespace, Name: p.Name + "-pgadmin"}
	if err := h.client.Get(context.Background(), key, &networkingv1.Ingress{}); err != nil {
		t.Fatalf("no Ingress before the pause: %v", err)
	}
	paused := h.pgadmin(p.Name)
	paused.Annotations = map[string]string{pgtoolboxv1alpha1.AnnotationReconcile: pgtoolboxv1alpha1.ReconcileSkip}
	if err := h.client.Update(context.Background(), paused); err != nil {
		t.Fatal(err)
	}
	h.events()
	h.r.Config.ExposureAllowedHostSuffixes = []string{"apps.example.com"}
	if err := h.reconcile(p.Name); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Get(context.Background(), key, &networkingv1.Ingress{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a paused PgAdmin keeps the Ingress under a refused hostname: %v", err)
	}
	got := h.pgadmin(p.Name)
	expectCondition(t, got, "ExposureReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
	expectCondition(t, got, "ConfigurationValid", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
	expectCondition(t, got, "Stalled", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonHostnameNotAllowed)
	expectCondition(t, got, "Ready", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciliationPaused)
	if got.Status.URL != "" {
		t.Errorf("status.url = %q while nothing is published", got.Status.URL)
	}
	if n := warnings(h.events(), pgtoolboxv1alpha1.ReasonHostnameNotAllowed); n != 1 {
		t.Errorf("HostnameNotAllowed Warnings = %d, want 1", n)
	}
	if err := h.reconcile(p.Name); err != nil {
		t.Fatal(err)
	}
	if again := h.pgadmin(p.Name); again.ResourceVersion != got.ResourceVersion {
		t.Fatal("a paused, refused PgAdmin's status is rewritten on every loop")
	}
}

// TestPgAdminPauseAllowUnpause: a PgAdmin paused while its hostname is
// refused, whose hostname the installer then allows while it is still
// paused, stops reporting the refusal and the stall at once, each paused
// loop after that is a no-op, and unpausing it publishes the Ingress again
// under the allowed name, with nothing left saying HostnameNotAllowed.
func TestPgAdminPauseAllowUnpause(t *testing.T) {
	p := hostnamePgAdmin("login.corp.example.com")
	p.Annotations = map[string]string{pgtoolboxv1alpha1.AnnotationReconcile: pgtoolboxv1alpha1.ReconcileSkip}
	h := newHarness(t, nil, baseObjects(p)...)
	key := client.ObjectKey{Namespace: testNamespace, Name: p.Name + "-pgadmin"}
	type want struct {
		conditionType string
		status        metav1.ConditionStatus
		reason        pgtoolboxv1alpha1.Reason
	}
	steps := []struct {
		name        string
		allowed     []string
		unpause     bool
		want        []want
		wantNoop    bool
		wantIngress bool
	}{
		{
			name: "paused, hostname refused", allowed: []string{"apps.example.com"},
			want: []want{
				{"ExposureReady", metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonHostnameNotAllowed},
				{"Stalled", metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonHostnameNotAllowed},
				{"Ready", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciliationPaused},
			},
		},
		{
			name: "paused, hostname allowed", allowed: []string{"corp.example.com"}, wantNoop: true,
			want: []want{
				{"ExposureReady", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciling},
				{"ConfigurationValid", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciling},
				{"Ready", metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciliationPaused},
			},
		},
		{name: "unpaused", allowed: []string{"corp.example.com"}, unpause: true, wantIngress: true},
	}
	for _, step := range steps {
		h.r.Config.ExposureAllowedHostSuffixes = step.allowed
		if step.unpause {
			current := h.pgadmin(p.Name)
			current.Annotations = nil
			if err := h.client.Update(context.Background(), current); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.reconcile(p.Name); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		got := h.pgadmin(p.Name)
		for _, w := range step.want {
			expectCondition(t, got, w.conditionType, w.status, w.reason)
		}
		if step.name != "paused, hostname refused" {
			for _, c := range got.Status.Conditions {
				if c.Reason == pgtoolboxv1alpha1.ReasonHostnameNotAllowed.String() {
					t.Errorf("%s: %s still says HostnameNotAllowed: %q", step.name, c.Type, c.Message)
				}
			}
			if s := condition(got, "Stalled"); s.Status == metav1.ConditionTrue {
				t.Errorf("%s: Stalled = %+v", step.name, s)
			}
		}
		err := h.client.Get(context.Background(), key, &networkingv1.Ingress{})
		if step.wantIngress && err != nil {
			t.Errorf("%s: the Ingress is not published under the allowed hostname: %v", step.name, err)
		}
		if !step.wantIngress && !apierrors.IsNotFound(err) {
			t.Errorf("%s: an Ingress is published while paused: %v", step.name, err)
		}
		if step.wantNoop {
			if err := h.reconcile(p.Name); err != nil {
				t.Fatal(err)
			}
			if again := h.pgadmin(p.Name); again.ResourceVersion != got.ResourceVersion {
				t.Errorf("%s: the next loop is not a no-op", step.name)
			}
		}
	}
}
