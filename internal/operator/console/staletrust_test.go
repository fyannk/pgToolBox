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

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// grantedSet is the console's StatefulSet whose template carries the
// any-peer grant, its generation observed.
func grantedSet(c *pgtoolboxv1alpha1.PgToolBoxConsole, grant bool) *appsv1.StatefulSet {
	set := owned(c, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: c.WorkloadName(), Namespace: c.Namespace, Generation: 1}})
	if grant {
		set.Spec.Template.Annotations = map[string]string{annotationTrustsEveryPeer: "true"}
	}
	set.Status.ObservedGeneration = 1
	return set
}

// widened is a console whose from adds a peer to the ipBlock one, its
// desired policy, and the narrower policy applied before it.
func widened() (*pgtoolboxv1alpha1.PgToolBoxConsole, *networkingv1.NetworkPolicy, *networkingv1.NetworkPolicy) {
	c := ipBlockConsole("192.168.10.0/24")
	c.Spec.Exposure.NetworkPolicy.From = append(c.Spec.Exposure.NetworkPolicy.From, nginxSpecPeer)
	desired := buildExposureNetworkPolicy(c, true)
	narrow := owned(c, buildExposureNetworkPolicy(ipBlockConsole("192.168.10.0/24"), true))
	narrow.Name = desired.Name
	return c, desired, narrow
}

// TestHeldWideningStaleWideCache: the cache still shows an older, wider
// policy equal to the desired one, while the API server holds the
// narrower one this operator applied last, and the template trusts every
// peer: the widening is held (the round-8 probe TestR8ProbeStaleWideCache).
func TestHeldWideningStaleWideCache(t *testing.T) {
	t.Parallel()
	c, desired, narrow := widened()
	set := grantedSet(c, true)
	cache := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(owned(c, desired.DeepCopy()), set.DeepCopy()).Build()
	live := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow.DeepCopy(), set.DeepCopy()).Build()
	r := &Reconciler{Client: cache, APIReader: live}
	held, _, err := r.heldWidening(context.Background(), c, desired)
	if err != nil || held == "" {
		t.Fatalf("heldWidening = %q, %v: the API server has the narrower policy and the template trusts every peer, want it held", held, err)
	}
}

// TestHeldWideningReadsTheWorkloadPastCache: the template rendered with
// the grant a moment ago is not in the cache yet (it shows the template
// without), and no Pod carries the grant yet: the widening is still held,
// as the API server's template carries it.
func TestHeldWideningReadsTheWorkloadPastCache(t *testing.T) {
	t.Parallel()
	c, desired, narrow := widened()
	cache := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow.DeepCopy(), grantedSet(c, false)).Build()
	live := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow.DeepCopy(), grantedSet(c, true)).Build()
	r := &Reconciler{Client: cache, APIReader: live}
	held, _, err := r.heldWidening(context.Background(), c, desired)
	if err != nil || held == "" {
		t.Fatalf("heldWidening = %q, %v with the grant in the API server's template, want it held", held, err)
	}
	// Neither carries it: the widening goes through.
	live = fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(narrow.DeepCopy(), grantedSet(c, false)).Build()
	r = &Reconciler{Client: cache, APIReader: live}
	if held, _, err = r.heldWidening(context.Background(), c, desired); err != nil || held != "" {
		t.Fatalf("heldWidening = %q, %v without any grant, want nothing held", held, err)
	}
}
