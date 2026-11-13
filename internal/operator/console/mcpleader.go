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
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
)

// The MCP leader, as CloudNativePG's -rw Service follows the primary: the
// console's replicas elect one on the MCP Lease, and the operator labels
// the holder's Pod LabelMCPLeader, which the -mcp Service selects. The
// console holds nothing on Pods.

// mcpHolderPod is the Pod named by a Lease that is held and not expired at
// now: its holderIdentity is "<pod name>_<suffix>" (the console's
// leader.CandidateIdentity). Empty for none.
func mcpHolderPod(lease *coordinationv1.Lease, now time.Time) string {
	if lease == nil {
		return ""
	}
	spec := lease.Spec
	if spec.HolderIdentity == nil || spec.RenewTime == nil || spec.LeaseDurationSeconds == nil {
		return ""
	}
	if now.After(spec.RenewTime.Add(time.Duration(*spec.LeaseDurationSeconds) * time.Second)) {
		return ""
	}
	pod, _, found := strings.Cut(*spec.HolderIdentity, "_")
	if !found {
		return ""
	}
	return pod
}

// mcpLabelChanges are the Pods whose label must change for holder alone
// to carry it: true to set it, false to remove it.
func mcpLabelChanges(pods []corev1.Pod, holder string) map[string]bool {
	changes := map[string]bool{}
	for i := range pods {
		has := pods[i].Labels[pgtoolboxv1alpha1.LabelMCPLeader] == pgtoolboxv1alpha1.MCPLeaderLabelValue
		_, present := pods[i].Labels[pgtoolboxv1alpha1.LabelMCPLeader]
		switch want := holder != "" && pods[i].Name == holder; {
		case want && !has:
			changes[pods[i].Name] = true
		case !want && present:
			changes[pods[i].Name] = false
		}
	}
	return changes
}

// labelMCPLeader puts LabelMCPLeader on the MCP Lease holder's Pod and
// takes it off every other Pod of this console (all of them with MCP off,
// with no holder or an expired Lease). Pods already right are not written.
func (r *Reconciler) labelMCPLeader(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) error {
	holder := ""
	if console.MCPEnabled() {
		lease := &coordinationv1.Lease{}
		switch err := r.Client.Get(ctx, client.ObjectKey{Namespace: console.Namespace, Name: console.MCPLeaseName()}, lease); {
		case err == nil:
			holder = mcpHolderPod(lease, time.Now())
		case !apierrors.IsNotFound(err):
			return fmt.Errorf("read Lease %s: %w", console.MCPLeaseName(), err)
		}
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(console.Namespace), client.MatchingLabels(selectorLabels(console))); err != nil {
		return fmt.Errorf("list console Pods: %w", err)
	}
	for name, set := range mcpLabelChanges(pods.Items, holder) {
		value := "null"
		if set {
			value = `"` + pgtoolboxv1alpha1.MCPLeaderLabelValue + `"`
		}
		patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%s}}}`, pgtoolboxv1alpha1.LabelMCPLeader, value)
		pod := &corev1.Pod{}
		pod.Namespace, pod.Name = console.Namespace, name
		if err := r.Client.Patch(ctx, pod, client.RawPatch(types.MergePatchType, []byte(patch)), client.FieldOwner(reconcile.FieldOwner)); err != nil &&
			!apierrors.IsNotFound(err) {
			return fmt.Errorf("label Pod %s: %w", name, err)
		}
	}
	return nil
}

// leaseHolderChanged passes the Lease events that can move the label: a
// Lease created or deleted, or another holder. Renewals (every two
// seconds) are not the operator's business.
func leaseHolderChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok := e.ObjectOld.(*coordinationv1.Lease)
			after, ok2 := e.ObjectNew.(*coordinationv1.Lease)
			if !ok || !ok2 {
				return true
			}
			return holderOf(before) != holderOf(after)
		},
	}
}

func holderOf(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}
