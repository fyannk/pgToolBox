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

package specs

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func TestHashIsStableAndSensitive(t *testing.T) {
	a := map[string]string{"x": "1", "y": "2"}
	b := map[string]string{"y": "2", "x": "1"}
	if Hash(a) != Hash(b) {
		t.Fatal("map order changed the hash")
	}
	if Hash(a) == Hash(map[string]string{"x": "1", "y": "3"}) {
		t.Fatal("different content, same hash")
	}
	if len(Hash(a)) != 16 {
		t.Fatalf("hash length %d", len(Hash(a)))
	}
}

func TestResolveImage(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name       string
		spec       *pgtoolboxv1alpha1.ImageSpec
		fallback   string
		wantRef    string
		wantPolicy corev1.PullPolicy
		wantPinned bool
	}{
		{name: "default pinned", fallback: "r/p:1@" + digest, wantRef: "r/p:1@" + digest, wantPolicy: corev1.PullIfNotPresent, wantPinned: true},
		{name: "default tag only", fallback: "registry:5000/p:1", wantRef: "registry:5000/p:1", wantPolicy: corev1.PullIfNotPresent},
		{name: "tag only", spec: &pgtoolboxv1alpha1.ImageSpec{Repository: "r/p", Tag: "9"}, wantRef: "r/p:9", wantPolicy: corev1.PullAlways},
		{name: "digest", spec: &pgtoolboxv1alpha1.ImageSpec{Repository: "r/p", Tag: "9", Digest: digest}, wantRef: "r/p:9@" + digest, wantPolicy: corev1.PullIfNotPresent, wantPinned: true},
		{name: "explicit policy", spec: &pgtoolboxv1alpha1.ImageSpec{Repository: "r/p", Digest: digest, PullPolicy: corev1.PullNever}, wantRef: "r/p@" + digest, wantPolicy: corev1.PullNever, wantPinned: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveImage(tc.spec, tc.fallback)
			if got.Reference != tc.wantRef || got.PullPolicy != tc.wantPolicy || got.Pinned != tc.wantPinned {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestSecurityContexts(t *testing.T) {
	sc := ContainerSecurityContext()
	if *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot || sc.Capabilities.Drop[0] != "ALL" || sc.RunAsUser != nil {
		t.Fatalf("container context not restricted: %+v", sc)
	}
	if pod := PodSecurityContext(true, 5050); pod.FSGroup != nil || pod.RunAsUser != nil {
		t.Fatalf("OpenShift pod context fixes an id: %+v", pod)
	}
	if pod := PodSecurityContext(false, 5050); pod.FSGroup == nil || *pod.FSGroup != 5050 || pod.RunAsUser != nil {
		t.Fatalf("pod context: %+v", pod)
	}
}

func TestImageAllowed(t *testing.T) {
	defaultImage := "registry.example/pgtoolbox/pgtoolbox-evidence-agent:1.0@sha256:" + strings.Repeat("0", 64)
	cases := []struct {
		repository string
		allowed    []string
		want       bool
	}{
		{"registry.example/pgtoolbox/pgtoolbox-evidence-agent", nil, true},
		{"registry.example/pgtoolbox/other", nil, false},
		{"attacker.example/exfil", nil, false},
		{"registry.example/pgtoolbox/other", []string{"registry.example/pgtoolbox/"}, true},
		{"registry.example/pgtoolboxx/other", []string{"registry.example/pgtoolbox/"}, false},
		{"registry.example/pgtoolbox", []string{"registry.example/pgtoolbox/"}, false},
		{"mirror.example/pgtoolbox-evidence-agent", []string{"mirror.example/pgtoolbox-evidence-agent"}, true},
		{"mirror.example/pgtoolbox-evidence-agent-evil", []string{"mirror.example/pgtoolbox-evidence-agent"}, false},
		{"localhost:5000/pgtoolbox-evidence-agent", nil, false},
	}
	for _, tc := range cases {
		if got := ImageAllowed(tc.repository, tc.allowed, defaultImage); got != tc.want {
			t.Errorf("ImageAllowed(%q, %v) = %v", tc.repository, tc.allowed, got)
		}
	}
	if ImageAllowed("anything", nil, "") {
		t.Error("with neither a list nor a default image, nothing may run")
	}
	for reference, want := range map[string]string{
		"r/a": "r/a", "r/a:1": "r/a", "r/a@sha256:x": "r/a", "host:5000/r/a:1@sha256:x": "host:5000/r/a", "host:5000/r/a": "host:5000/r/a",
	} {
		if got := ImageRepository(reference); got != want {
			t.Errorf("ImageRepository(%q) = %q, want %q", reference, got, want)
		}
	}
}
