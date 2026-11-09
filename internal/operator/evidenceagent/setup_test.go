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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func agentPod(agent *pgtoolboxv1alpha1.EvidenceAgent, mutate func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: agent.WorkloadName() + "-7c9d-x2k", Namespace: agent.Namespace, Labels: commonLabels(agent)}}
	if mutate != nil {
		mutate(pod)
	}
	return pod
}

func TestAgentForPod(t *testing.T) {
	agent := testAgent()
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want []reconcile.Request
	}{
		{name: "agent Pod", pod: agentPod(agent, nil), want: []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(agent)}}},
		{name: "a pgAdmin Pod", pod: agentPod(agent, func(p *corev1.Pod) {
			p.Labels = map[string]string{"app.kubernetes.io/name": "pgadmin", pgtoolboxv1alpha1.LabelManagedBy: pgtoolboxv1alpha1.ManagedByValue}
		})},
		{name: "agent label on a foreign Pod", pod: agentPod(agent, func(p *corev1.Pod) {
			p.Labels = map[string]string{pgtoolboxv1alpha1.LabelEvidenceAgent: "orders", "app.kubernetes.io/name": "mine"}
		})},
		{name: "no labels", pod: agentPod(agent, func(p *corev1.Pod) { p.Labels = nil })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := agentForPod(context.Background(), tc.pod)
			if len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
				t.Fatalf("agentForPod = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCacheByObjectScopesPods: the agent Pods enter the cache, the
// namespace's other Pods do not.
func TestCacheByObjectScopesPods(t *testing.T) {
	agent := testAgent()
	var selector labels.Selector
	for object, byObject := range CacheByObject() {
		if _, ok := object.(*corev1.Pod); ok {
			selector = byObject.Label
		}
	}
	if selector == nil || selector.Empty() {
		t.Fatal("the Pod informer is not label-filtered")
	}
	if !selector.Matches(labels.Set(agentPod(agent, nil).Labels)) {
		t.Fatal("the agent Pod is filtered out of the cache")
	}
	if selector.Matches(labels.Set{"app": "postgres", "cnpg.io/cluster": "orders"}) {
		t.Fatal("a foreign Pod enters the cache")
	}
	found := false
	for _, kind := range CachedKinds(OptionalAPIs{}) {
		_, found = kind.(*corev1.Pod)
		if found {
			break
		}
	}
	if !found {
		t.Fatal("pgtoolbox_cached_objects does not count the Pod informer")
	}
}

// TestPodFactsReadsTheWatchedCache: the Pod facts come from the client
// whose informer the controller watches, never from a live List.
func TestPodFactsReadsTheWatchedCache(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := testAgent()
	for _, tc := range []struct {
		name   string
		pod    *corev1.Pod
		reason pgtoolboxv1alpha1.Reason
	}{
		{name: "image pull back-off", reason: pgtoolboxv1alpha1.ReasonImagePullFailed, pod: agentPod(agent, func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"},
			}}}
		})},
		{name: "crash loop", reason: pgtoolboxv1alpha1.ReasonCrashLoopBackOff, pod: agentPod(agent, func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", RestartCount: 4, State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
			}}}
		})},
		{name: "another agent's broken Pod", pod: agentPod(&pgtoolboxv1alpha1.EvidenceAgent{ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: agent.Namespace}}, func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"},
			}}}
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cached := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Pod{}).WithObjects(tc.pod).Build()
			live := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					return errors.New("a live List of Pods")
				},
			}).Build()
			r := &Reconciler{Client: cached, APIReader: live}
			reason, message, err := r.podFacts(context.Background(), agent)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.reason {
				t.Fatalf("reason = %q (%s), want %q", reason, message, tc.reason)
			}
		})
	}
}
