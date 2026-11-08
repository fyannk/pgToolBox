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

package status

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

func TestCompareGolden(t *testing.T) {
	ctx := context.Background()
	svc := newService(ctx, t, nil)
	cmp, err := svc.Compare(ctx, "team-a", "orders", "team-a", "billing")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "compare-orders-billing", cmp)
	if _, err := svc.Compare(ctx, "team-a", "orders", "team-a", "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("compare with a missing Cluster: %v, want ErrNotFound", err)
	}
}

// TestCompareRows proves what "the same" means: equal text; a memory
// parameter spelled differently but equal in bytes (with a note); equal
// quantities however written; never an unknown side.
func TestCompareRows(t *testing.T) {
	t.Parallel()
	mk := func(params map[string]string, mem string) *gathered {
		c := &cnpgv1.Cluster{}
		c.Spec.PostgresConfiguration.Parameters = params
		c.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(mem)}
		return &gathered{cluster: c, readable: map[string]bool{}}
	}
	l := mk(map[string]string{"shared_buffers": "1GB", "work_mem": "4MB", "jit": "on"}, "1Gi")
	r := mk(map[string]string{"shared_buffers": "131072", "work_mem": "8MB", "max_connections": "200"}, "1024Mi")

	sec := parameterSection(l, r)
	want := map[string]bool{"jit": false, "max_connections": false, "shared_buffers": true, "work_mem": false, "shared_preload_libraries": true}
	if len(sec.Rows) != len(want) {
		t.Fatalf("rows = %+v", sec.Rows)
	}
	for _, row := range sec.Rows {
		if row.Same != want[row.Key] {
			t.Errorf("%s: same = %v, want %v (%+v)", row.Key, row.Same, want[row.Key], row)
		}
		if row.Key == "shared_buffers" && row.Note == "" {
			t.Errorf("shared_buffers equal in bytes carries no note")
		}
		if row.Key == "jit" && row.Right.State != "unset" {
			t.Errorf("jit right = %+v, want unset", row.Right)
		}
	}
	if sec.Differences != 3 {
		t.Errorf("differences = %d, want 3", sec.Differences)
	}

	res := section("resources", viewmodel.OriginOperator, "", l, r, resourceFields(l, r))
	for _, row := range res.Rows {
		if row.Key == "limits.memory" && !row.Same {
			t.Errorf("1Gi and 1024Mi differ: %+v", row)
		}
	}

	var unknownSec viewmodel.CompareSection
	sectionAdd(&unknownSec, "x", unknownValue(), unknownValue(), "")
	if unknownSec.Rows[0].Same || unknownSec.Differences != 1 {
		t.Errorf("two unknowns read as the same: %+v", unknownSec)
	}
}

// TestCompareNeverCallsTheUnseenTheSame: values that differ only past
// the display bound differ; parameter names match case-insensitively;
// rows left out by the bound are named; a source whose read failed is
// unknown, not "nothing declared".
func TestCompareNeverCallsTheUnseenTheSame(t *testing.T) {
	t.Parallel()
	mk := func(params map[string]string) *gathered {
		c := &cnpgv1.Cluster{}
		c.Spec.PostgresConfiguration.Parameters = params
		return &gathered{cluster: c, readable: map[string]bool{}}
	}
	long := strings.Repeat("x", 2*maxText)
	l := mk(map[string]string{"search_path": long + "a", "Work_Mem": "4MB"})
	r := mk(map[string]string{"search_path": long + "b", "work_mem": "4MB"})
	sec := parameterSection(l, r)
	rows := map[string]viewmodel.CompareRow{}
	for _, row := range sec.Rows {
		rows[strings.ToLower(row.Key)] = row
	}
	if row := rows["search_path"]; row.Same || len(row.Left.Value) > maxText+len("…") {
		t.Errorf("long values: same = %v, shown %d bytes", row.Same, len(row.Left.Value))
	}
	if row, ok := rows["work_mem"]; !ok || !row.Same || row.Note == "" || len(sec.Rows) != 3 {
		t.Errorf("Work_Mem and work_mem: %+v (rows %d)", row, len(sec.Rows))
	}

	many := map[string]string{}
	for i := range maxCompareParameters + 7 {
		many[fmt.Sprintf("custom.p%04d", i)] = "1"
	}
	if got := parametersBeyondBound(mk(many), mk(nil)); got != 7 {
		t.Errorf("beyond bound = %d, want 7", got)
	}
	if got := len(parameterSection(mk(many), mk(nil)).Rows); got != maxCompareParameters+1 {
		t.Errorf("rows = %d, want the bound plus shared_preload_libraries", got)
	}

	failed := mk(nil)
	failed.readable[cache.SourcePoolers] = true
	failed.readFailed(cache.SourcePoolers, errors.New("cache not synced"))
	empty := mk(nil)
	empty.readable[cache.SourcePoolers] = true
	empty.readFailed(cache.SourcePoolers, apierrors.NewNotFound(schema.GroupResource{}, "x"))
	for _, f := range topologyFields {
		if f.key != "poolers" {
			continue
		}
		if v := f.read(failed); v.State != "unknown" {
			t.Errorf("failed list reads %+v, want unknown", v)
		}
		if v := f.read(empty); v.State != "unset" {
			t.Errorf("listed, none: %+v, want unset", v)
		}
	}
}
