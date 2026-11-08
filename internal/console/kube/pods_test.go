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

package kube

import (
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// newTestClient builds a Client without a live connection, sufficient
// for conversion and membership tests, logging into the returned
// buffer.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	return &Client{
		opts:   target{Namespace: "payments", ClusterName: "orders"},
		logger: slog.New(slog.DiscardHandler),
	}
}

// ownedPod is an unstructured member pod with every consumed field set.
func ownedPod(name, uid, role string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "payments",
			"uid":       uid,
			"labels": map[string]any{
				"cnpg.io/cluster":      "orders",
				"cnpg.io/instanceRole": role,
			},
			"ownerReferences": []any{
				map[string]any{
					"apiVersion": "postgresql.cnpg.io/v1",
					"kind":       "Cluster",
					"name":       "orders",
					"uid":        "cluster-uid",
					"controller": true,
				},
			},
		},
		"spec": map[string]any{
			"nodeName": "node-a",
			"containers": []any{
				map[string]any{"name": "postgres", "image": "ghcr.io/cloudnative-pg/postgresql:16.4"},
			},
		},
		"status": map[string]any{
			"phase": "Running",
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True"},
			},
			"containerStatuses": []any{
				map[string]any{"name": "postgres", "restartCount": int64(2), "image": "x", "imageID": "y", "ready": true, "state": map[string]any{}},
			},
		},
	}
}

func TestConvertPodFullObject(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	facts, member, err := c.convertPod(ownedPod("orders-1", "u1", "primary"))
	if err != nil {
		t.Fatalf("convertPod: %v", err)
	}
	if !member {
		t.Fatal("owned, labeled pod not a member")
	}
	if facts.Name != "orders-1" || facts.Role != "primary" || facts.Phase != "Running" {
		t.Errorf("core facts wrong: %+v", facts)
	}
	if facts.Ready == nil || !*facts.Ready {
		t.Error("Ready not converted")
	}
	if facts.Restarts == nil || *facts.Restarts != 2 {
		t.Error("Restarts not converted")
	}
	if facts.Node != "node-a" || facts.Image != "ghcr.io/cloudnative-pg/postgresql:16.4" {
		t.Errorf("placement facts wrong: %+v", facts)
	}
}

// TestConvertPodMinimalObject proves unreported pod facts stay unknown.
func TestConvertPodMinimalObject(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	obj := ownedPod("orders-2", "u2", "")
	meta := obj["metadata"].(map[string]any)
	delete(meta["labels"].(map[string]any), "cnpg.io/instanceRole")
	obj["spec"] = map[string]any{}
	obj["status"] = map[string]any{}
	facts, member, err := c.convertPod(obj)
	if err != nil {
		t.Fatalf("convertPod: %v", err)
	}
	if !member {
		t.Fatal("ownership alone must establish membership")
	}
	if facts.Role != "" || facts.Node != "" || facts.Image != "" || facts.Phase != "" {
		t.Errorf("absent string facts must stay empty: %+v", facts)
	}
	if facts.Ready != nil || facts.Restarts != nil {
		t.Errorf("absent numeric facts must stay nil: %+v", facts)
	}
}

// TestPodMembershipVerification proves labels select but ownership
// decides: a labeled pod without the controller owner reference to the
// configured Cluster is excluded and logged with a stable reason.
func TestPodMembershipVerification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(obj map[string]any)
		member bool
	}{
		{"owned by the cluster", func(_ map[string]any) {}, true},
		{"no owner reference", func(obj map[string]any) {
			delete(obj["metadata"].(map[string]any), "ownerReferences")
		}, false},
		{"owned by another cluster", func(obj map[string]any) {
			ref := obj["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)
			ref["name"] = "other"
		}, false},
		{"owned by a different kind", func(obj map[string]any) {
			ref := obj["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)
			ref["kind"] = "StatefulSet"
			ref["apiVersion"] = "apps/v1"
		}, false},
		{"owner not the controller", func(obj map[string]any) {
			ref := obj["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)
			ref["controller"] = false
		}, false},
		{"wrong owner group", func(obj map[string]any) {
			ref := obj["metadata"].(map[string]any)["ownerReferences"].([]any)[0].(map[string]any)
			ref["apiVersion"] = "example.com/v1"
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t)
			obj := ownedPod("orders-1", "u1", "primary")
			tc.mutate(obj)
			_, member, err := c.convertPod(obj)
			if err != nil {
				t.Fatalf("convertPod: %v", err)
			}
			if member != tc.member {
				t.Errorf("member = %v, want %v", member, tc.member)
			}
		})
	}
}

// sidecarPod is an instance pod shaped the way CNPG-I produces one: an
// init container that has already completed, a healthy postgres
// container, and a plugin sidecar that is crash-looping.
func sidecarPod() map[string]any {
	pod := ownedPod("orders-1", "u1", "primary")
	spec := pod["spec"].(map[string]any)
	spec["initContainers"] = []any{
		map[string]any{"name": "bootstrap-controller", "image": "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0"},
	}
	spec["containers"] = []any{
		map[string]any{"name": "postgres", "image": "ghcr.io/cloudnative-pg/postgresql:16.4"},
		map[string]any{"name": "plugin-barman-cloud", "image": "ghcr.io/cloudnative-pg/plugin-barman-cloud:0.5.0"},
	}
	status := pod["status"].(map[string]any)
	status["initContainerStatuses"] = []any{
		map[string]any{
			"name": "bootstrap-controller", "restartCount": int64(0), "ready": true,
			"image": "x", "imageID": "y",
			"state": map[string]any{"terminated": map[string]any{"exitCode": int64(0), "reason": "Completed"}},
		},
	}
	status["containerStatuses"] = []any{
		map[string]any{
			"name": "postgres", "restartCount": int64(0), "ready": true,
			"image": "x", "imageID": "y",
			"state": map[string]any{"running": map[string]any{}},
		},
		map[string]any{
			"name": "plugin-barman-cloud", "restartCount": int64(14), "ready": false,
			"image": "x", "imageID": "y",
			"state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
		},
	}
	return pod
}

// TestConvertPodAttributesRestartsToPostgresNotTheSum proves a
// crash-looping sidecar does not read as an unstable instance. Summing
// across containers — which is what kubectl shows — would report 14
// restarts for a database that has never restarted.
func TestConvertPodAttributesRestartsToPostgresNotTheSum(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	facts, _, err := c.convertPod(sidecarPod())
	if err != nil {
		t.Fatalf("convertPod: %v", err)
	}
	if facts.Restarts == nil || *facts.Restarts != 0 {
		t.Errorf("Restarts = %v, want the postgres container's 0, not the summed 14", facts.Restarts)
	}
	if facts.Image != "ghcr.io/cloudnative-pg/postgresql:16.4" {
		t.Errorf("Image = %q, want the postgres container's", facts.Image)
	}
}

// TestConvertPodCarriesEveryContainerInOrder proves the whole pod is
// observable: init containers first, then regular ones in spec order,
// each with the reason that names what is wrong with it.
func TestConvertPodCarriesEveryContainerInOrder(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	facts, _, err := c.convertPod(sidecarPod())
	if err != nil {
		t.Fatalf("convertPod: %v", err)
	}
	if len(facts.Containers) != 3 {
		t.Fatalf("containers = %d, want 3: %+v", len(facts.Containers), facts.Containers)
	}
	want := []struct {
		name     string
		init     bool
		state    string
		reason   string
		restarts int
	}{
		{"bootstrap-controller", true, "terminated", "Completed", 0},
		{"postgres", false, "running", "", 0},
		{"plugin-barman-cloud", false, "waiting", "CrashLoopBackOff", 14},
	}
	for i, w := range want {
		got := facts.Containers[i]
		if got.Name != w.name || got.Init != w.init || got.State != w.state || got.Reason != w.reason {
			t.Errorf("container %d = %+v, want %+v", i, got, w)
		}
		if got.Restarts == nil || *got.Restarts != w.restarts {
			t.Errorf("container %d restarts = %v, want %d", i, got.Restarts, w.restarts)
		}
	}
	// The completed init container carries its exit code; the running
	// one carries none, because there is nothing to report.
	if facts.Containers[0].ExitCode == nil || *facts.Containers[0].ExitCode != 0 {
		t.Errorf("init exit code = %v, want 0", facts.Containers[0].ExitCode)
	}
	if facts.Containers[1].ExitCode != nil {
		t.Errorf("running container reported an exit code: %v", facts.Containers[1].ExitCode)
	}
}

// TestConvertPodKeepsContainersTheKubeletHasNotStarted proves a
// container with no status is listed as unknown rather than dropped.
// Dropping it would hide exactly the pod that cannot start: a container
// stuck before first run has no status at all.
func TestConvertPodKeepsContainersTheKubeletHasNotStarted(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	pod := sidecarPod()
	// The kubelet has reported nothing for the sidecar yet.
	status := pod["status"].(map[string]any)
	status["containerStatuses"] = []any{
		map[string]any{
			"name": "postgres", "restartCount": int64(0), "ready": true,
			"image": "x", "imageID": "y",
			"state": map[string]any{"running": map[string]any{}},
		},
	}
	facts, _, err := c.convertPod(pod)
	if err != nil {
		t.Fatalf("convertPod: %v", err)
	}
	if len(facts.Containers) != 3 {
		t.Fatalf("a container without status was dropped: %+v", facts.Containers)
	}
	sidecar := facts.Containers[2]
	if sidecar.Name != "plugin-barman-cloud" || sidecar.Image == "" {
		t.Errorf("sidecar lost its declared identity: %+v", sidecar)
	}
	if sidecar.State != "" || sidecar.Reason != "" || sidecar.Ready != nil || sidecar.Restarts != nil {
		t.Errorf("unreported container did not stay unknown: %+v", sidecar)
	}
}

// TestStatusSchemeFollowsThePodSpec is CloudNativePG's own rule: HTTPS
// probes or --status-port-tls mean TLS, anything else plain HTTP.
func TestStatusSchemeFollowsThePodSpec(t *testing.T) {
	t.Parallel()
	probe := func(scheme corev1.URIScheme) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Scheme: scheme}}}
	}
	cases := []struct {
		name      string
		container corev1.Container
		want      string
	}{
		{"1.31 HTTPS probes", corev1.Container{Name: "postgres", ReadinessProbe: probe(corev1.URISchemeHTTPS)}, "https"},
		{"1.30 TLS flag", corev1.Container{Name: "postgres", Command: []string{"/controller/manager", "instance", "run", "--status-port-tls"}}, "https"},
		{"pre-TLS", corev1.Container{Name: "postgres", LivenessProbe: probe(corev1.URISchemeHTTP)}, "http"},
		{"no probes", corev1.Container{Name: "postgres"}, "http"},
		{"not an instance", corev1.Container{Name: "pgbouncer", ReadinessProbe: probe(corev1.URISchemeHTTPS)}, ""},
	}
	for _, tc := range cases {
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{tc.container}}}
		if got := statusScheme(pod); got != tc.want {
			t.Errorf("%s: statusScheme = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestConvertPodReadsThePostgresMemoryLimit: the limit the container
// runs with, wherever it came from (a LimitRange fills in a limit the
// Cluster did not declare); a sidecar's limit is not PostgreSQL's.
func TestConvertPodReadsThePostgresMemoryLimit(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	obj := ownedPod("orders-1", "u1", "primary")
	obj["spec"].(map[string]any)["containers"] = []any{
		map[string]any{"name": "plugin-barman-cloud", "resources": map[string]any{"limits": map[string]any{"memory": "128Mi"}}},
		map[string]any{"name": "postgres", "resources": map[string]any{"limits": map[string]any{"memory": "2Gi"}}},
	}
	facts, _, err := c.convertPod(obj)
	if err != nil {
		t.Fatalf("convertPod: %v", err)
	}
	if facts.MemoryLimit == nil || *facts.MemoryLimit != 2<<30 {
		t.Errorf("MemoryLimit = %v, want 2 GiB", facts.MemoryLimit)
	}
	facts, _, _ = c.convertPod(ownedPod("orders-2", "u2", "replica"))
	if facts.MemoryLimit != nil {
		t.Errorf("no limit read as %d", *facts.MemoryLimit)
	}
}
