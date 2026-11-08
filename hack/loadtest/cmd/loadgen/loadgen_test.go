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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

func TestPercentile(t *testing.T) {
	t.Parallel()
	d := make([]time.Duration, 0, 100)
	for i := 100; i >= 1; i-- { // unsorted on purpose
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{{50, 50 * time.Millisecond}, {95, 95 * time.Millisecond}, {99, 99 * time.Millisecond}, {100, 100 * time.Millisecond}} {
		if got := percentile(d, tc.p); got != tc.want {
			t.Errorf("p%.0f = %s, want %s", tc.p, got, tc.want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("empty = %s, want 0", got)
	}
}

func TestEpStatsCountsErrorsApart(t *testing.T) {
	t.Parallel()
	e := &epStats{}
	e.add(sample{d: time.Millisecond, status: 200})
	e.add(sample{d: time.Second, status: 500})
	e.add(sample{status: 0})
	if e.count() != 3 || e.errors != 2 || len(e.durations) != 1 {
		t.Fatalf("count %d errors %d durations %d, want 3 2 1", e.count(), e.errors, len(e.durations))
	}
	if got := e.statusSummary(); got != "0x1 500x1" {
		t.Errorf("statusSummary = %q", got)
	}
}

func TestParseMix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in           string
		list, detail int
		ok           bool
	}{{"1:4", 1, 4, true}, {"0:1", 0, 1, true}, {"0:0", 0, 0, false}, {"4", 0, 0, false}, {"a:b", 0, 0, false}, {"-1:2", 0, 0, false}} {
		l, d, err := parseMix(tc.in)
		if (err == nil) != tc.ok || l != tc.list || d != tc.detail {
			t.Errorf("parseMix(%q) = %d, %d, %v", tc.in, l, d, err)
		}
	}
}

func TestParseExposition(t *testing.T) {
	t.Parallel()
	m, err := parseExposition(strings.NewReader(`# HELP x y
# TYPE pgtoolbox_console_cached_objects gauge
pgtoolbox_console_cached_objects{source="pods"} 120
pgtoolbox_console_cached_objects{source="clusters"} 40
process_resident_memory_bytes 1.048576e+08
odd{a="b\"c",d="e"} 3 1700000000
broken{a="b
`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := m.sum("pgtoolbox_console_cached_objects"); got != 160 {
		t.Errorf("cached sum = %v, want 160", got)
	}
	if got := m.bySource("pgtoolbox_console_cached_objects", "source"); got != "clusters:40,pods:120" {
		t.Errorf("bySource = %q", got)
	}
	if got := m["odd"][0].labels["a"]; got != `b"c` {
		t.Errorf("escaped label = %q", got)
	}
	if got, _ := m.sum("process_resident_memory_bytes"); mib(got) != "100MiB" {
		t.Errorf("rss = %s", mib(got))
	}
	if _, ok := m["broken"]; ok {
		t.Error("a malformed line must be skipped")
	}
}

// TestBarmanServerLayout holds the S3 generator to what the evidence agent's
// own Barman reader accepts: every WAL key is a complete segment in its hash
// directory, every backup.info parses, and the object count is exact.
func TestBarmanServerLayout(t *testing.T) {
	t.Parallel()
	const server, total, backups = "srv-001", 700, 4
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	objs := barmanServer("path/in/bucket", server, total, backups, []byte("w"), now)
	if len(objs) != total {
		t.Fatalf("%d objects, want %d", len(objs), total)
	}
	seen := map[string]bool{}
	var wal, infos int
	for _, o := range objs {
		if seen[o.key] {
			t.Fatalf("duplicate key %s", o.key)
		}
		seen[o.key] = true
		rel := strings.TrimPrefix(o.key, "path/in/bucket/")
		switch {
		case strings.Contains(rel, "/wals/"):
			c, ok := barmancloud.ClassifyWALObject(store.Object{Key: rel})
			if !ok || c.Class != barmancloud.WALSegment || c.Server != server {
				t.Fatalf("%s classified as %+v", o.key, c)
			}
			wal++
		case strings.HasSuffix(rel, "/backup.info"):
			id := strings.Split(rel, "/")[2]
			b, err := barmancloud.ParseBackupInfo(o.body, server, id)
			if err != nil || b.Status != "DONE" || len(b.WithheldFields) != 0 {
				t.Fatalf("%s: %+v, %v", o.key, b, err)
			}
			if !b.EndAt.After(b.BeginAt) || b.BeginWAL == "" || b.EndWAL < b.BeginWAL {
				t.Fatalf("%s: implausible backup %+v", o.key, b)
			}
			infos++
		}
	}
	if wal != total-2*backups || infos != backups {
		t.Errorf("wal %d infos %d, want %d and %d", wal, infos, total-2*backups, backups)
	}
	if got := walName(256 + 3); got != "000000010000000100000003" {
		t.Errorf("walName(259) = %s", got)
	}
}

// TestFakeClusterIsWhatTheConsoleReads ties the generated objects to the
// console's own rules, so a change in either shows here rather than as an
// empty list on a 20-minute run.
func TestFakeClusterIsWhatTheConsoleReads(t *testing.T) {
	t.Parallel()
	c := newCluster("lt-01", "c-0001", 3)
	st := clusterStatus(c, 3, time.Now().UTC(), 0)
	if st.CurrentPrimary != "c-0001-1" || st.ReadyInstances != 3 || len(st.InstanceNames) != 3 {
		t.Errorf("status %+v", st)
	}
	if got := cnpgstatus.StatusServerName(c); got != "c-0001-rw.lt-01.svc" {
		t.Errorf("status server name = %s: instance-sim issues certificates for the name the console verifies", got)
	}
	_, _, pod := newInstance("lt-01", "c-0001", 1, "kwok-node-00", metav1.OwnerReference{})
	if !cnpgstatus.StatusPortTLS(pod) {
		t.Error("the instance pod must carry --status-port-tls or the console never reads it")
	}
	if pod.Labels[clusterLabel] != "c-0001" || pod.Labels["cnpg.io/instanceRole"] != "primary" {
		t.Errorf("labels %v", pod.Labels)
	}
	if a, b := clusterConditions(time.Now(), 1), clusterConditions(time.Now(), 2); a[0].Message == b[0].Message {
		t.Error("two revisions must differ, or churn writes are no-ops the API server collapses")
	}
}
