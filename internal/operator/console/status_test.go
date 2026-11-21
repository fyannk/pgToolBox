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
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/conditions"
)

// TestPublishAfterTheLoopTimedOut: ReconciliationTimeout cancels the
// loop's context; the status write still lands (a client honouring its
// context refuses calls on a done one), so a loop that ran out of time
// publishes Ready=False instead of leaving the previous Ready=True.
func TestPublishAfterTheLoopTimedOut(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := pgtoolboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	stored := stateConsole()
	initializeConditions(stored)
	for _, aspect := range aspectConditions {
		setCondition(stored, aspect, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "")
	}
	setCondition(stored, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stored).WithStatusSubresource(stored).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	r := &Reconciler{Client: c, APIReader: c, Recorder: events.NewFakeRecorder(16)}
	console := &pgtoolboxv1alpha1.PgToolBoxConsole{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stored), console); err != nil {
		t.Fatal(err)
	}
	original := console.DeepCopy()
	loop, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.publish(loop, console, original, conditions.Outcome{Err: loop.Err()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("publish returned %v, want the loop's error", err)
	}
	written := &pgtoolboxv1alpha1.PgToolBoxConsole{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stored), written); err != nil {
		t.Fatal(err)
	}
	if ready := meta.FindStatusCondition(written.Status.Conditions, pgtoolboxv1alpha1.ConditionReady); ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("stored Ready = %+v, want False after a timed-out loop", ready)
	}
}
