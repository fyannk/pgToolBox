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

package accessrequest

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestFailingReconcileReturnsAZeroResultWithTheError: an approved request
// with an expiry asks to come back when the grant ends, but when its status
// write fails the loop returns the error alone. controller-runtime ignores
// (and warns of) a result beside an error; the back-off drives the retry.
func TestFailingReconcileReturnsAZeroResultWithTheError(t *testing.T) {
	r := request(pgtoolboxv1alpha1.AccessPhaseApproved, expiresAt(testNow.Add(time.Hour)))
	binding := granted(r, testNow.Add(-time.Minute))

	run := func(failStatus bool) (ctrl.Result, error) {
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithStatusSubresource(&pgtoolboxv1alpha1.PgAccessRequest{}).
			WithObjects(r.DeepCopy(), binding.DeepCopy()).
			WithInterceptorFuncs(interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if failStatus {
						return apierrors.NewServiceUnavailable("etcd is away")
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			}).Build()
		reconciler := &Reconciler{Client: c, Recorder: events.NewFakeRecorder(16), Now: func() time.Time { return testNow }}
		return reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(r)})
	}

	// Control: with a working status write the loop asks for the expiry
	// timer, so the failing case has a result to lose.
	result, err := run(false)
	if err != nil || result.RequeueAfter != time.Hour {
		t.Fatalf("control loop = %+v, %v; want a requeue at the expiry and no error", result, err)
	}

	result, err = run(true)
	var status apierrors.APIStatus
	if err == nil || !errors.As(err, &status) || status.Status().Reason != metav1.StatusReasonServiceUnavailable {
		t.Fatalf("failed status write returned %v, want the API error", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("result = %+v beside an error, want the zero result", result)
	}
}
