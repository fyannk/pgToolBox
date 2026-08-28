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

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestReconcileExposureLabels covers the label overlay on exposure objects:
// user-supplied labels land on the object (router shards select routes by
// label, so annotations alone cannot steer admission), operator-reserved
// keys are dropped, and the identity labels always win over a spec that
// tries to override them.
func TestReconcileExposureLabels(t *testing.T) {
	console := testConsole()
	console.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{
		Type:     pgtoolboxv1alpha1.ExposureTypeIngress,
		Hostname: "pgconsole.apps.example.com",
		Labels: map[string]string{
			"network":                      "gin",
			"app.kubernetes.io/name":       "impostor",
			"pgtoolbox.fyannk.dev/planted": "dropped",
		},
		Annotations: map[string]string{
			"route.openshift.io/termination": "edge",
		},
	}
	r, c := newTestReconciler(t, console, testCluster())

	reconcileToSteadyState(t, r)

	var ingress networkingv1.Ingress
	getObject(t, c, client.ObjectKey{Namespace: "test", Name: "console-pgconsole"}, &ingress)

	if got := ingress.Labels["network"]; got != "gin" {
		t.Fatalf("user exposure label network = %q, want %q", got, "gin")
	}
	if got := ingress.Labels["app.kubernetes.io/name"]; got != "pgconsole" {
		t.Fatalf("identity label must win over the spec, app.kubernetes.io/name = %q", got)
	}
	if _, ok := ingress.Labels["pgtoolbox.fyannk.dev/planted"]; ok {
		t.Fatalf("operator-reserved label key must be dropped, labels = %v", ingress.Labels)
	}
	if got := ingress.Annotations["route.openshift.io/termination"]; got != "edge" {
		t.Fatalf("exposure annotation = %q, want %q", got, "edge")
	}
}
