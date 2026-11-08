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

package main

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"
)

// root is the module root, from this package's directory.
var root = filepath.Join("..", "..")

func messages(findings []Finding) string {
	var b strings.Builder
	for _, f := range findings {
		b.WriteString(f.Message + "\n")
	}
	return b.String()
}

// The real tree is exactly the pinned surface.
func TestTheConsoleIsAsPinned(t *testing.T) {
	findings, err := run(root, nil, allowed, reaperClientUsedIn)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings on the real tree:\n%s", messages(findings))
	}
}

// With nothing allowed, every pinned site is reported, the drill teardown's
// Delete among them: the checker sees what the allowlist names.
func TestEveryPinnedSiteIsDetected(t *testing.T) {
	findings, err := run(root, nil, map[Site]int{}, reaperClientUsedIn)
	if err != nil {
		t.Fatal(err)
	}
	got := messages(findings)
	for site := range allowed {
		if !strings.Contains(got, site.Function+" calls "+site.Method+": not in the console's mutation allowlist") {
			t.Errorf("%s %s not reported; findings:\n%s", site.Function, site.Method, got)
		}
	}
}

// Planted violations, as overlay files in the console's packages.
func TestPlantedViolationsAreReported(t *testing.T) {
	abs := func(rel string) string {
		path, err := filepath.Abs(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	overlay := map[string][]byte{
		// A direct delete in a read-model package, and one through a local
		// interface that hides the client; a non-Kubernetes Delete (a
		// key-value store's) is not a mutation of the API server.
		abs("internal/console/status/zz_planted.go"): []byte(`package status

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type plantedDeleter interface {
	Delete(ctx context.Context, object client.Object) error
}

type plantedStore struct{}

func (plantedStore) Delete(key []byte) error { return nil }

func plantedDirect(ctx context.Context, c client.Client, object client.Object) error {
	return c.Delete(ctx, object)
}

func plantedWrapped(ctx context.Context, d plantedDeleter, object client.Object) error {
	return d.Delete(ctx, object)
}

func plantedStoreDelete() error { return plantedStore{}.Delete(nil) }
`),
		// The reaper client leaked in wiring, and built a second time.
		abs("internal/console/wiring/zz_planted.go"): []byte(`package wiring

import (
	"k8s.io/client-go/rest"

	"github.com/fyannk/pgToolBox/internal/console/ops"
)

func plantedLeak(cfg *rest.Config) any {
	c, _ := ops.NewDrillReaperClient(cfg)
	return c
}
`),
		// The reaper's client read by another method.
		abs("internal/console/ops/zz_planted.go"): []byte(`package ops

func (r *DrillReaper) plantedPeek() any { return r.c }
`),
		// client-go's own Lease lock: its writes happen inside client-go.
		abs("internal/console/leader/zz_planted.go"): []byte(`package leader

import (
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

func plantedLock(c kubernetes.Interface) (resourcelock.Interface, error) {
	return resourcelock.New(resourcelock.LeasesResourceLock, "ns", "name", c.CoreV1(), c.CoordinationV1(), resourcelock.ResourceLockConfig{})
}
`),
	}
	findings, err := run(root, overlay, maps.Clone(allowed), reaperClientUsedIn)
	if err != nil {
		t.Fatal(err)
	}
	got := messages(findings)
	for _, want := range []string{
		"internal/console/status.plantedDirect calls Delete: not in the console's mutation allowlist",
		"internal/console/status.plantedWrapped calls Delete: not in the console's mutation allowlist",
		"internal/console/wiring.plantedLeak uses the drill reaper's client other than to build the reaper",
		"internal/console/ops.NewDrillReaperClient is used 2 times, at most 1",
		"internal/console/ops.(*DrillReaper).plantedPeek uses the reaper's client (1 times): only the teardown may",
		"internal/console/leader.plantedLock uses resourcelock.New: client-go's locks write where this check cannot see",
		"internal/console/leader.plantedLock uses resourcelock.LeasesResourceLock",
		"internal/console/leader.plantedLock uses resourcelock.ResourceLockConfig",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q; findings:\n%s", want, got)
		}
	}
	if strings.Contains(got, "plantedStoreDelete") {
		t.Errorf("a key-value store's Delete reported as a mutation:\n%s", got)
	}
	if len(findings) != 8 {
		t.Errorf("%d findings, want exactly the 8 planted:\n%s", len(findings), got)
	}
}

// The reaper client outside wiring is reported.
func TestReaperClientOutsideWiring(t *testing.T) {
	findings, err := run(root, nil, allowed, "internal/console/elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(messages(findings), "uses internal/console/ops.NewDrillReaperClient: the console's own-identity writer is built in internal/console/elsewhere only") {
		t.Fatalf("findings:\n%s", messages(findings))
	}
}
