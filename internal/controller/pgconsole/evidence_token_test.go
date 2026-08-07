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
	"context"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEvidenceTokenLifecycle(t *testing.T) {
	console := testConsole()
	r, c := newTestReconciler(t, console)
	ctx := context.Background()

	// First generation is minted under a deterministic name.
	name, err := r.reconcileEvidenceToken(ctx, console)
	if err != nil {
		t.Fatalf("first token: %v", err)
	}
	if name != "console-pgconsole-evidence-t1" {
		t.Fatalf("first token name = %q", name)
	}
	var first corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: "test", Name: name}, &first); err != nil {
		t.Fatalf("read token secret: %v", err)
	}
	if first.Immutable == nil || !*first.Immutable {
		t.Fatalf("the token secret must be immutable")
	}
	if len(first.Data[evidenceTokenKey]) != 43 {
		t.Fatalf("token length = %d, want 43 (32 bytes base64url)", len(first.Data[evidenceTokenKey]))
	}
	if first.Labels[pgtoolboxv1alpha1.EvidenceTokenLabelKey] != console.Name {
		t.Fatalf("token labels = %v", first.Labels)
	}

	// A no-op reconcile reuses the same secret: the console's status names
	// the active generation and the live object is valid.
	console.Status.Evidence.TokenSecretName = name
	again, err := r.reconcileEvidenceToken(ctx, console)
	if err != nil {
		t.Fatalf("steady-state token: %v", err)
	}
	if again != name {
		t.Fatalf("steady-state token name = %q, want %q", again, name)
	}

	// Rotation mints a successor under a NEW name and acknowledges the
	// annotation on the live object.
	console.Annotations = map[string]string{pgtoolboxv1alpha1.RotateEvidenceTokenAnnotation: "now"}
	rotated, err := r.reconcileEvidenceToken(ctx, console)
	if err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if rotated != "console-pgconsole-evidence-t2" {
		t.Fatalf("rotated token name = %q", rotated)
	}
	if rotated == name {
		t.Fatalf("rotation must never reuse the superseded name")
	}
	var live pgtoolboxv1alpha1.PgConsole
	if err := c.Get(ctx, client.ObjectKeyFromObject(console), &live); err != nil {
		t.Fatalf("read console: %v", err)
	}
	if _, present := live.Annotations[pgtoolboxv1alpha1.RotateEvidenceTokenAnnotation]; present {
		t.Fatalf("rotation annotation must be acknowledged once the successor exists")
	}
	// The superseded secret is still there: GC waits for the rollout.
	var superseded corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: "test", Name: name}, &superseded); err != nil {
		t.Fatalf("superseded token must outlive the rollout: %v", err)
	}
}

func TestEvidenceTokenSelfHealsUnderNewName(t *testing.T) {
	console := testConsole()
	r, _ := newTestReconciler(t, console)

	// Status names a generation whose secret is gone (deleted out of band):
	// the replacement must carry a new identity, never the old name.
	console.Status.Evidence.TokenSecretName = "console-pgconsole-evidence-t3"
	name, err := r.reconcileEvidenceToken(context.Background(), console)
	if err != nil {
		t.Fatalf("self-heal token: %v", err)
	}
	if name != "console-pgconsole-evidence-t4" {
		t.Fatalf("self-healed token name = %q, want the next identity", name)
	}
}

func TestTokenGenerationParsing(t *testing.T) {
	for input, want := range map[string]int{
		"":                               0,
		"console-pgconsole":              0,
		"console-pgconsole-evidence-t1":  1,
		"console-pgconsole-evidence-t12": 12,
		"console-pgconsole-evidence-tx":  0,
		"console-pgconsole-evidence-t0":  0,
	} {
		if got := tokenGeneration(input); got != want {
			t.Fatalf("tokenGeneration(%q) = %d, want %d", input, got, want)
		}
	}
}
