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

	routev1 "github.com/openshift/api/route/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// TestRouteAPIDecidedPerNamespace: the Route API refused in one watched
// namespace while others allow it is unavailable to a console of that
// namespace only (ExposureReady names the namespace); a console of another
// namespace publishes its Route. Each namespace's RBAC decides there alone.
func TestRouteAPIDecidedPerNamespace(t *testing.T) {
	r := &Reconciler{Features: Features{Routes: discovery.Result{
		GVK: routev1.GroupVersion.WithKind("Route"), State: discovery.Available, RefusedIn: []string{"team-b"},
	}}}
	consoleIn := func(namespace string) *pgtoolboxv1alpha1.PgToolBoxConsole {
		c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: namespace, Generation: 1}}
		c.Spec.Exposure.Type = pgtoolboxv1alpha1.ExposureTypeRoute
		return c
	}
	if !r.exposureUsable(consoleIn("team-a")) {
		t.Error("a Route is not usable in team-a, which allows the Route API")
	}
	refused := consoleIn("team-b")
	if r.exposureUsable(refused) {
		t.Error("a Route is usable in team-b, which refuses the Route API")
	}
	r.observeExposure(context.Background(), refused)
	var ready *metav1.Condition
	for i := range refused.Status.Conditions {
		if refused.Status.Conditions[i].Type == pgtoolboxv1alpha1.ConditionExposureReady {
			ready = &refused.Status.Conditions[i]
		}
	}
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != string(pgtoolboxv1alpha1.ReasonRouteAPIUnavailable) ||
		!strings.Contains(ready.Message, "in team-b") {
		t.Fatalf("ExposureReady = %+v, want False RouteAPIUnavailable naming team-b", ready)
	}
}
