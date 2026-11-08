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
	"strconv"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

func sizingIn(params map[string]string, limit int64, huge map[string]int64, major int) Input {
	s := observe.PostgresSizing{Read: true, Parameters: params, HugePages: huge}
	if limit > 0 {
		s.MemoryLimit = &limit
	}
	c := observe.ClusterFacts{Present: true, Sizing: s}
	if major > 0 {
		c.PostgresMajorVersion = &major
	}
	return Input{Now: now, HasCluster: true, Cluster: observe.Snapshot{Cluster: c}}
}

// withPods adds the instance Pods, each with the postgres container's
// memory limit as the Pod carries it (nil: none).
func withPods(in Input, limits ...*int64) Input {
	in.HasPods = true
	for i, l := range limits {
		in.Pods.Pods = append(in.Pods.Pods, observe.PodFacts{Name: "db-" + strconv.Itoa(i+1), MemoryLimit: l})
	}
	return in
}

// TestConfigAdvisor proves each advisor condition on the cases a DBA
// meets: defaults within a limit, no limit (nothing to compare), an
// unread spec and an unparsable value (could not run, never clear),
// huge pages taking the shared segment out of the memory limit, and the
// PostgreSQL 16 autovacuum cap.
func TestConfigAdvisor(t *testing.T) {
	t.Parallel()
	const gib = int64(1 << 30)
	memory := Rule{ID: "mem", Summary: "Memory.", When: MemoryOverLimit{}}
	shared := Rule{ID: "shared", Summary: "Shared.", When: SharedBuffersOverLimit{}}
	ecs := Rule{ID: "ecs", Summary: "ECS.", When: EffectiveCacheSizeOverLimit{}}
	missing := Rule{ID: "huge-missing", Summary: "Huge.", When: HugePages{Problem: HugePagesMissing}}
	unused := Rule{ID: "huge-unused", Summary: "Huge.", When: HugePages{Problem: HugePagesUnused}}
	unparsable := Rule{ID: "bad", Summary: "Bad.", When: ParametersUnparsable{}}

	cases := []struct {
		name string
		rule Rule
		in   Input
		want CheckOutcome
		// says is a substring of the first finding's summary or evidence.
		says string
	}{
		{"no cluster", memory, Input{Now: now}, CheckUnavailable, ""},
		{"spec not read", memory, Input{Now: now, HasCluster: true, Cluster: observe.Snapshot{Cluster: observe.ClusterFacts{Present: true}}}, CheckUnavailable, ""},
		// Defaults: 128 + 4 + 100×4 + 3×64 = 724 MiB.
		{"defaults within 1 GiB", memory, sizingIn(nil, gib, nil, 17), CheckClear, ""},
		{"defaults over 512 MiB", memory, sizingIn(nil, 512<<20, nil, 17), CheckMatched, "724 MiB"},
		// No limit in the spec: only the Pods tell whether a LimitRange set one.
		{"no limit, Pods not observed", memory, sizingIn(map[string]string{"work_mem": "1GB"}, 0, nil, 17), CheckUnavailable, ""},
		{"no limit anywhere", memory, withPods(sizingIn(map[string]string{"work_mem": "1GB"}, 0, nil, 17), nil, nil), CheckClear, ""},
		{"limit from a LimitRange, on the Pods", memory, withPods(sizingIn(map[string]string{"work_mem": "1GB"}, 0, nil, 17), bytesPtr(2*gib), bytesPtr(gib)), CheckMatched, "LimitRange"},
		{"no Pod yet", ecs, withPods(sizingIn(nil, 0, nil, 17)), CheckUnavailable, ""},
		{"work_mem × connections", memory, sizingIn(map[string]string{"shared_buffers": "2GB", "work_mem": "64MB", "max_connections": "200"}, 8*gib, nil, 17), CheckMatched, "200 (declared \"200\") × work_mem 64 MiB"},
		{"unparsable work_mem", memory, sizingIn(map[string]string{"work_mem": "64mb"}, gib, nil, 17), CheckUnavailable, ""},
		// 3 × 4 GiB of maintenance_work_mem: 12 GiB on 17, capped at 3 GiB on 16.
		{"autovacuum on 17", memory, sizingIn(map[string]string{"maintenance_work_mem": "4GB"}, 8*gib, nil, 17), CheckMatched, ""},
		{"autovacuum capped on 16", memory, sizingIn(map[string]string{"maintenance_work_mem": "4GB"}, 8*gib, nil, 16), CheckClear, ""},
		// 6 GiB shared in huge pages leaves 0.5 GiB of connections and autovacuum for the 1 GiB limit.
		{"shared segment in huge pages", memory, sizingIn(map[string]string{"shared_buffers": "6GB", "huge_pages": "on"}, gib, map[string]int64{"hugepages-2Mi": 7 * gib}, 17), CheckClear, ""},
		{"shared over limit", shared, sizingIn(map[string]string{"shared_buffers": "1GB"}, gib, nil, 17), CheckMatched, "1 GiB"},
		{"shared within limit", shared, sizingIn(map[string]string{"shared_buffers": "256MB"}, gib, nil, 17), CheckClear, ""},
		{"shared in huge pages (on)", shared, sizingIn(map[string]string{"shared_buffers": "2GB", "huge_pages": "on"}, gib, map[string]int64{"hugepages-1Gi": 3 * gib}, 17), CheckClear, ""},
		// try may have fallen back to normal pages: not proven out of the limit.
		{"try is not proof of huge pages", shared, sizingIn(map[string]string{"shared_buffers": "2GB"}, gib, map[string]int64{"hugepages-1Gi": 3 * gib}, 17), CheckMatched, "2 GiB"},
		{"effective_cache_size default over 2 GiB", ecs, sizingIn(nil, 2*gib, nil, 17), CheckMatched, "not set: PostgreSQL's default"},
		{"effective_cache_size within", ecs, sizingIn(map[string]string{"effective_cache_size": "1536MB"}, 2*gib, nil, 17), CheckClear, ""},
		{"huge on without reservation", missing, sizingIn(map[string]string{"huge_pages": "on"}, gib, nil, 17), CheckMatched, "refuse to start"},
		{"huge on, reservation too small", missing, sizingIn(map[string]string{"huge_pages": "on", "shared_buffers": "1GB"}, 2*gib, map[string]int64{"hugepages-2Mi": 512 << 20}, 17), CheckMatched, "512 MiB"},
		{"huge on, enough", missing, sizingIn(map[string]string{"huge_pages": "on", "shared_buffers": "1GB"}, 2*gib, map[string]int64{"hugepages-2Mi": 2 * gib}, 17), CheckClear, ""},
		// 1 GiB + 16 MiB of wal_buffers needs 1040 MiB at least; the real segment is larger.
		{"huge on, reservation barely above the floor", missing, sizingIn(map[string]string{"huge_pages": "on", "shared_buffers": "1GB"}, 2*gib, map[string]int64{"hugepages-2Mi": 1040 << 20}, 17), CheckUnavailable, ""},
		{"huge try, reservation barely above the floor", unused, sizingIn(map[string]string{"shared_buffers": "1GB"}, 2*gib, map[string]int64{"hugepages-2Mi": 1040 << 20}, 17), CheckUnavailable, ""},
		{"huge try without reservation", missing, sizingIn(nil, gib, nil, 17), CheckClear, ""},
		{"huge off with reservation", unused, sizingIn(map[string]string{"huge_pages": "off"}, gib, map[string]int64{"hugepages-2Mi": gib}, 17), CheckMatched, "huge_pages is off"},
		{"huge try, reservation too small", unused, sizingIn(map[string]string{"shared_buffers": "1GB"}, 2*gib, map[string]int64{"hugepages-2Mi": 256 << 20}, 17), CheckMatched, "falls back"},
		{"huge try, enough", unused, sizingIn(nil, gib, map[string]int64{"hugepages-2Mi": gib}, 17), CheckClear, ""},
		{"unparsable named", unparsable, sizingIn(map[string]string{"work_mem": "4 mb", "huge_pages": "maybe"}, gib, nil, 17), CheckMatched, "huge_pages, work_mem"},
		// 300000 connections is past MAX_BACKENDS: PostgreSQL would not start, the estimate does not run.
		{"max_connections out of range", memory, sizingIn(map[string]string{"max_connections": "300000", "work_mem": "2000000MB"}, gib, nil, 17), CheckUnavailable, ""},
		{"max_connections out of range named", unparsable, sizingIn(map[string]string{"max_connections": "300000"}, gib, nil, 17), CheckMatched, "max_connections"},
		{"too long to read: could not run", memory, func() Input {
			in := sizingIn(nil, gib, nil, 17)
			in.Cluster.Cluster.Sizing.TooLong = []string{"work_mem"}
			return in
		}(), CheckUnavailable, ""},
		{"parsable", unparsable, sizingIn(map[string]string{"work_mem": "0x400"}, gib, nil, 17), CheckClear, ""},
	}
	for _, c := range cases {
		check, findings := evaluateRule(c.rule, c.in)
		if check.Outcome != c.want {
			t.Errorf("%s: outcome = %v (%s), want %v", c.name, check.Outcome, check.Because, c.want)
			continue
		}
		if c.says == "" {
			continue
		}
		var text strings.Builder
		text.WriteString(findings[0].Summary)
		for _, e := range findings[0].Evidence {
			text.WriteString(" | " + e.Detail)
		}
		if !strings.Contains(text.String(), c.says) {
			t.Errorf("%s: finding does not say %q: %s", c.name, c.says, text.String())
		}
	}
}

func TestIEC(t *testing.T) {
	t.Parallel()
	for b, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1 KiB", 1536 << 20: "1.5 GiB", 724 << 20: "724 MiB", 1 << 40: "1 TiB"} {
		if got := iec(b); got != want {
			t.Errorf("iec(%d) = %q, want %q", b, got, want)
		}
	}
}

func bytesPtr(v int64) *int64 { return &v }
