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
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestTruncatedPodListFailsClosed: the uncached console Pod lists are
// bounded (client.Limit); a list that comes back with a continue token
// grants no any-peer trust (ExposureTrustUnverified) and withdraws no
// NetworkPolicy (a Pod past the page may still trust every peer).
func TestTruncatedPodListFailsClosed(t *testing.T) {
	t.Parallel()
	c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a", UID: "u"}}
	c.Spec.Exposure.Type, c.Spec.Exposure.Hostname = pgtoolboxv1alpha1.ExposureTypeIngress, "console.example"
	c.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{From: []pgtoolboxv1alpha1.PgToolBoxConsolePolicyPeer{nginxSpecPeer}}
	policy := buildExposureNetworkPolicy(c, true)
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "console-0", Namespace: "team-a", Labels: selectorLabels(c)}}
	var limits []int64
	reader := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(policy, &pod).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := cl.List(ctx, list, opts...); err != nil {
				return err
			}
			if pods, ok := list.(*corev1.PodList); ok {
				var o client.ListOptions
				o.ApplyOptions(opts)
				limits = append(limits, o.Limit)
				pods.Continue = "more"
			}
			return nil
		},
	}).Build()
	r := &Reconciler{Client: reader, APIReader: reader}
	ctx := context.Background()

	found, err := r.exposureConfinement(ctx, c, policy.Spec.Ingress[0].From)
	if err != nil {
		t.Fatal(err)
	}
	if found.Unverified == "" {
		t.Fatalf("confinement = %+v from a truncated Pod list, want unverified", found)
	}
	if trust := effectiveProxyTrust(c, true, found); trust.EveryPeer {
		t.Fatal("any-peer trust granted from a truncated Pod list")
	}

	facts := &workloadFacts{Kind: "StatefulSet", Generation: 1, ObservedGeneration: 1, Desired: 1, Replicas: 1, Updated: 1, Available: 1}
	if _, _, err := r.withdrawExposurePolicy(ctx, c, facts); err != nil {
		t.Fatal(err)
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(policy), &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatalf("the policy was withdrawn on a truncated Pod list: %v", err)
	}
	for _, limit := range limits {
		if limit != podListLimit {
			t.Fatalf("console Pods listed with limit %d, want %d", limit, podListLimit)
		}
	}
	if len(limits) != 2 {
		t.Fatalf("%d Pod lists, want 2", len(limits))
	}
}
