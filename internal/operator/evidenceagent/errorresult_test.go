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

package evidenceagent

import (
	"context"
	"errors"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestFailingReconcileReturnsAZeroResultWithTheError: a loop that converged
// and asked to come back at the certificate's renewal, but whose status
// write failed, returns the error alone. controller-runtime ignores (and
// warns of) a result beside an error, and the back-off, not the renewal
// timer, must drive the retry.
func TestFailingReconcileReturnsAZeroResultWithTheError(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, cnpgv1.AddToScheme, pgtoolboxv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	configuration := &cnpgv1.BarmanObjectStoreConfiguration{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(s3Configuration(nil), configuration); err != nil {
		t.Fatal(err)
	}
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a", UID: "uid-1"},
		Spec:       cnpgv1.ClusterSpec{Backup: &cnpgv1.BackupConfiguration{BarmanObjectStore: configuration}},
	}
	agent := testAgent()
	agent.Spec.AllowedSecrets = []string{"minio"}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	run := func(t *testing.T, failStatus bool) (ctrl.Result, error) {
		t.Helper()
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&pgtoolboxv1alpha1.EvidenceAgent{}).
			WithObjects(cluster.DeepCopy(), agent.DeepCopy()).
			WithInterceptorFuncs(interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if failStatus {
						return apierrors.NewServiceUnavailable("etcd is away")
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			}).Build()
		r := &Reconciler{
			Client: c, APIReader: c, Recorder: events.NewFakeRecorder(64),
			DefaultImage: "registry.example/pgobjectstoreviewer:1.0.0", Now: func() time.Time { return now },
		}
		return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
	}

	// Control: the same loop with a working status write asks for the
	// certificate renewal timer, so the failing case below has a result to lose.
	result, err := run(t, false)
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("control loop = %+v, %v; want a renewal requeue and no error", result, err)
	}

	result, err = run(t, true)
	var status apierrors.APIStatus
	if err == nil || !errors.As(err, &status) || status.Status().Reason != metav1.StatusReasonServiceUnavailable {
		t.Fatalf("failed status write returned %v, want the API error", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("result = %+v beside an error, want the zero result", result)
	}
}
