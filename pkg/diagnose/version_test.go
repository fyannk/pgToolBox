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

package diagnose

import (
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// TestSatisfiesPinsTheConstraintGrammar covers each operator, the
// clause conjunction, and the closed failure mode: anything unreadable
// rules the version out rather than in.
func TestSatisfiesPinsTheConstraintGrammar(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version, constraint string
		want                bool
	}{
		{"1.25.1", ">=1.24 <1.26", true},
		{"1.26", ">=1.24 <1.26", false},
		{"1.26.0", "<=1.26", true},
		{"17", "<14", false},
		{"13", "<14", true},
		{"17", "=17", true},
		{"17.5", "17", false},       // bare version means equality, and 17.5 != 17.0
		{"17", "17", true},          // operatorless clause reads as "="
		{"1.25", "!=1.25.0", false}, // missing fields compare as zero
		{"1.25.1", ">1.25 <2", true},
		{"anything", "", false},      // unparseable version fails closed
		{"1.2.3", "", true},          // empty constraint holds
		{"1.2.3", ">=latest", false}, // unparseable clause fails closed
	} {
		if got := satisfies(tc.version, tc.constraint); got != tc.want {
			t.Errorf("satisfies(%q, %q) = %v, want %v", tc.version, tc.constraint, got, tc.want)
		}
	}
}

// TestImageTagVersionReadsOnlyTheVersionPart proves the parser takes
// the leading numerals of a tag and nothing else: not a digest, not a
// registry port, not a build-variant suffix.
func TestImageTagVersionReadsOnlyTheVersionPart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		image, want string
		ok          bool
	}{
		{"ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar:v0.6.0", "0.6.0", true},
		{"ghcr.io/cloudnative-pg/postgresql:17.5-standard-bookworm", "17.5", true},
		{"ghcr.io/x/y:latest", "", false},
		{"ghcr.io/x/y@sha256:abcd", "", false},
		{"registry:5000/x/y", "", false}, // the colon is a port, not a tag
		{"registry:5000/x/y:1.2", "1.2", true},
		{"bare-image", "", false},
	} {
		got, ok := imageTagVersion(tc.image)
		if got != tc.want || ok != tc.ok {
			t.Errorf("imageTagVersion(%q) = %q, %v, want %q, %v", tc.image, got, ok, tc.want, tc.ok)
		}
	}
}

// TestVersionFactsAreObservedNeverAssumed proves each derivable fact
// appears only when its snapshot reports it, and carries provenance a
// finding can quote.
func TestVersionFactsAreObservedNeverAssumed(t *testing.T) {
	t.Parallel()

	if facts := versionFacts(Input{Now: now}); len(facts) != 0 {
		t.Errorf("versions derived from nothing observed: %+v", facts)
	}

	major := 17
	in := Input{
		Now:        now,
		HasCluster: true,
		Cluster: observe.Snapshot{Cluster: observe.ClusterFacts{
			Present:              true,
			PostgresMajorVersion: &major,
		}},
		HasPods: true,
		Pods: observe.PodsSnapshot{Pods: []observe.PodFacts{{
			Name: "orders-1",
			Containers: []observe.ContainerFacts{
				{Name: "bootstrap-controller", Init: true, Image: "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0"},
				{Name: "postgres", Image: "ghcr.io/cloudnative-pg/postgresql:17.5"},
				{Name: "plugin-barman-cloud", Image: "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar:v0.6.0"},
			},
		}}},
	}
	facts := versionFacts(in)

	postgres, ok := facts[ComponentPostgreSQL]
	if !ok || postgres.Version != "17" {
		t.Fatalf("PostgreSQL version = %+v, want 17 from the operator's status", postgres)
	}
	if postgres.Origin != "operator-reported" {
		t.Errorf("PostgreSQL origin = %q", postgres.Origin)
	}

	barman, ok := facts[ComponentBarman]
	if !ok || barman.Version != "0.6.0" {
		t.Fatalf("Barman version = %+v, want 0.6.0 from the sidecar image", barman)
	}
	if barman.Detail == "" || barman.Object == "" {
		t.Errorf("Barman fact carries no provenance: %+v", barman)
	}

	// The operator injects its own image as the bootstrap init
	// container, which is where its version is observed.
	cnpg, ok := facts[ComponentCNPG]
	if !ok || cnpg.Version != "1.30.0" {
		t.Fatalf("CloudNativePG version = %+v, want 1.30.0 from the bootstrap image", cnpg)
	}
	if !strings.Contains(cnpg.Object, "bootstrap-controller") {
		t.Errorf("CloudNativePG provenance does not name the init container: %+v", cnpg)
	}

	// Kubernetes stays unknown until the /version poller has reported.
	if _, ok := facts[ComponentKubernetes]; ok {
		t.Error("Kubernetes version asserted without an observation")
	}
	in.HasKubeVersion = true
	in.KubeVersion = observe.KubeVersionSnapshot{GitVersion: "v1.33.1+k3s1"}
	kube, ok := versionFacts(in)[ComponentKubernetes]
	if !ok || kube.Version != "1.33.1" {
		t.Fatalf("Kubernetes version = %+v, want 1.33.1 from /version", kube)
	}
	if kube.Origin != "Kubernetes-reported" || !strings.Contains(kube.Detail, "v1.33.1+k3s1") {
		t.Errorf("Kubernetes fact does not quote the server's own report: %+v", kube)
	}

	// A stale observation stays usable but says so.
	in.KubeVersion.Stale = true
	if kube := versionFacts(in)[ComponentKubernetes]; !strings.Contains(kube.Detail, "stale") {
		t.Errorf("stale observation not marked in the provenance: %+v", kube)
	}
}

// TestCNPGVersionFallsBackToTheBootstrapJob covers the cluster that
// broke before its first instance pod: the bootstrap Job carries the
// same injected operator image, and it is the source of last resort —
// consulted only when no instance pod answers.
func TestCNPGVersionFallsBackToTheBootstrapJob(t *testing.T) {
	t.Parallel()
	in := Input{
		Now:               now,
		HasPods:           true, // observed, and empty: initdb never succeeded
		Pods:              observe.PodsSnapshot{},
		HasInfrastructure: true,
		Infrastructure: observe.InfrastructureSnapshot{Children: []observe.ChildFacts{
			{
				Kind: "Job", Name: "oom-1-initdb",
				BootstrapImage: "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0",
			},
		}},
	}
	cnpg, ok := versionFacts(in)[ComponentCNPG]
	if !ok || cnpg.Version != "1.30.0" {
		t.Fatalf("CloudNativePG version = %+v, want 1.30.0 from the bootstrap Job", cnpg)
	}
	if !strings.Contains(cnpg.Object, "Job/oom-1-initdb") {
		t.Errorf("provenance does not name the Job: %+v", cnpg)
	}

	// With an instance pod present, the pod wins: it is the running
	// workload, not a relic of bootstrap.
	in.Pods = observe.PodsSnapshot{Pods: []observe.PodFacts{{
		Name: "oom-1",
		Containers: []observe.ContainerFacts{{
			Name: "bootstrap-controller", Init: true,
			Image: "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1",
		}},
	}}}
	if cnpg := versionFacts(in)[ComponentCNPG]; cnpg.Version != "1.30.1" ||
		!strings.Contains(cnpg.Object, "Pod/") {
		t.Errorf("instance pod did not take precedence: %+v", cnpg)
	}
}

// TestCNPGVersionOutlivesThePods proves the version of last resort: with
// no pod and no bootstrap Job — a hibernated cluster — the image the
// pod store retained from the last instance pod still names the
// operator version, labelled as retained rather than observed now.
func TestCNPGVersionOutlivesThePods(t *testing.T) {
	t.Parallel()
	seen := now.Add(-time.Hour)
	in := Input{
		Now: now, HasPods: true, Pods: observe.PodsSnapshot{
			LastOperatorImage: "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0", LastOperatorImagePod: "orders-1",
			LastOperatorImageSeenAt: seen, LastOperatorImageOwner: "uid-orders",
		},
		HasCluster: true, Cluster: observe.Snapshot{Cluster: observe.ClusterFacts{Present: true, UID: "uid-orders"}},
	}
	cnpg, ok := versionFacts(in)[ComponentCNPG]
	if !ok || cnpg.Version != "1.30.0" {
		t.Fatalf("CloudNativePG version = %+v, want 1.30.0 retained from the last pod", cnpg)
	}
	if !strings.Contains(cnpg.Origin, "retained") || !strings.Contains(cnpg.Detail, "no instance pod carries it now") {
		t.Errorf("provenance does not say the image is retained: %+v", cnpg)
	}
	in.Cluster.Cluster.UID = "uid-recreated"
	if _, ok := versionFacts(in)[ComponentCNPG]; ok {
		t.Error("a recreated Cluster inherited the previous one's retained image")
	}
	in.Cluster.Cluster.UID = "uid-orders"
	in.Pods.LastOperatorImage = ""
	if _, ok := versionFacts(in)[ComponentCNPG]; ok {
		t.Error("a version was invented with nothing to read it from")
	}
}
