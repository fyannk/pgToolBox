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

package scrape

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

const exporterPayload = `# HELP cnpg_backends_total Number of backends
# TYPE cnpg_backends_total gauge
cnpg_backends_total{datname="app"} 5
cnpg_backends_total{datname="postgres"} 2
# TYPE cnpg_pg_replication_lag gauge
cnpg_pg_replication_lag{stream="a"} 0.5
cnpg_pg_replication_lag{stream="b"} 2.5
cnpg_pg_stat_database_xact_commit{datname="app"} 1000
not a metric line
cnpg_pg_stat_database_blks_hit 42 1700000000000
irrelevant_metric{x="y"} 9
# One name, four readings, told apart only by a label.
cnpg_collector_pg_wal{value="size"} 218103808
cnpg_collector_pg_wal{value="count"} 13
cnpg_collector_pg_wal{value="max"} 64
cnpg_collector_pg_wal{value="slots_max"} NaN
cnpg_collector_pg_wal_archive_status{value="ready"} 3
cnpg_collector_pg_wal_archive_status{value="done"} 1
cnpg_collector_postgres_version{cluster="orders",full="18.4"} 18.4
`

func TestParseFoldsTheCatalog(t *testing.T) {
	t.Parallel()
	values, _, err := Parse(strings.NewReader(exporterPayload))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := values["connections"]; got != 7 {
		t.Errorf("connections = %v, want the label sets summed to 7", got)
	}
	if got := values["replication-lag"]; got != 2.5 {
		t.Errorf("replication-lag = %v, want the worst stream 2.5", got)
	}
	if got := values["xact-commit"]; got != 1000 {
		t.Errorf("xact-commit = %v, want 1000 via the bare candidate name", got)
	}
	if got := values["blocks-hit"]; got != 42 {
		t.Errorf("blocks-hit = %v, want 42 with the timestamp ignored", got)
	}
	if _, ok := values["database-size"]; ok {
		t.Error("an absent metric produced a value")
	}
}

// One metric name carries several readings, separated only by a label.
// Without label matching every one of them would fold into the same
// number, so wal-disk-size would report the segment count added to the
// byte total — a wrong answer that still looks like a plausible one.
func TestParseSeparatesSeriesSharingAMetricNameByLabel(t *testing.T) {
	t.Parallel()
	values, instants, err := Parse(strings.NewReader(exporterPayload))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := values["wal-disk-size"]; got != 218103808 {
		t.Errorf("wal-disk-size = %v, want only the value=\"size\" label set", got)
	}
	if got := values["wal-archive-ready"]; got != 3 {
		t.Errorf("wal-archive-ready = %v, want only the value=\"ready\" label set", got)
	}
	if got := instants["wal-segments"]; got != 13 {
		t.Errorf("wal-segments = %v, want only the value=\"count\" label set", got)
	}
	if got := instants["wal-segments-max"]; got != 64 {
		t.Errorf("wal-segments-max = %v, want only the value=\"max\" label set", got)
	}
	if got := instants["archive-done"]; got != 1 {
		t.Errorf("archive-done = %v, want only the value=\"done\" label set", got)
	}
	if got := instants["postgres-version"]; got != 18.4 {
		t.Errorf("postgres-version = %v, want 18.4", got)
	}
	// The exporter reports NaN for "not applicable"; recording it as a
	// reading would draw a value where the instance claimed none.
	if _, ok := instants["wal-segments-min"]; ok {
		t.Error("a label set absent from the payload produced a reading")
	}
}

func TestParseSkipsHostileValues(t *testing.T) {
	t.Parallel()
	values, instants, err := Parse(strings.NewReader("cnpg_backends_total NaN\ncnpg_pg_replication_lag +Inf\ncnpg_collector_up{cluster=\"orders\"} NaN\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(values) != 0 || len(instants) != 0 {
		t.Fatalf("values = %v, instants = %v, want NaN and Inf refused", values, instants)
	}
}

// A label named "value" must not be matched by one named "othervalue",
// which a substring search would do.
func TestParseMatchesWholeLabelNames(t *testing.T) {
	t.Parallel()
	_, instants, err := Parse(strings.NewReader(`cnpg_collector_pg_wal{othervalue="count"} 99` + "\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := instants["wal-segments"]; ok {
		t.Error("a label whose name merely ends in the wanted one matched")
	}
}

// staticPods serves a fixed roster.
type staticPods struct{ snap observe.PodsSnapshot }

func (s staticPods) CurrentPods() (observe.PodsSnapshot, bool) { return s.snap, true }

// sweepClock fires the wait immediately a bounded number of times.
type sweepClock struct {
	mu     sync.Mutex
	sweeps int
	now    time.Time
}

func (c *sweepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(10 * time.Second)
	return c.now
}

func (c *sweepClock) Wait(ctx context.Context, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sweeps == 0 {
		return context.Canceled
	}
	c.sweeps--
	return ctx.Err()
}

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

func exporterServer(t *testing.T, payload string) (ip, port string, close func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	return host, port, srv.Close
}

func TestCollectorSweepsRunningPodsAndLogsOncePerStateChange(t *testing.T) {
	ip, port, closeSrv := exporterServer(t, exporterPayload)
	defer closeSrv()

	source := staticPods{snap: observe.PodsSnapshot{Pods: []observe.PodFacts{
		{Name: "orders-1", IP: ip},
		{Name: "orders-2", IP: "203.0.113.1"}, // unreachable, TEST-NET
		{Name: "orders-3"},                    // no IP yet: skipped silently
	}}}
	store := metrics.NewStore(metrics.Instance, metrics.Limits{Interval: 10 * time.Second})
	logger, logs := testLogger()
	c := New(source, store, InstancePort, 10*time.Second, &sweepClock{sweeps: 2, now: time.Unix(1700000000, 0)}, logger)
	c.port = port
	c.client.Timeout = 500 * time.Millisecond

	if err := c.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if got := store.Instances(); len(got) != 1 || got[0] != "orders-1" {
		t.Fatalf("instances = %v, want only the reachable orders-1", got)
	}
	times, byInstance := store.Range("connections", metrics.TierRaw)
	if len(times) != 2 || *byInstance["orders-1"][0] != 7 {
		t.Fatalf("stored range = %v %v", times, byInstance)
	}
	// Two failing sweeps for orders-2 log one line, not two.
	if got := strings.Count(logs.String(), "metrics scrape failed"); got != 1 {
		t.Fatalf("failure logged %d times across two sweeps, want once\n%s", got, logs.String())
	}
	if strings.Contains(logs.String(), "orders-3") {
		t.Fatal("a pod without an IP was treated as a failure")
	}
}

// TestParseReadsTheForkDiskSeriesPerVolume pins the cnpg_disk_* series
// (fork origin/dev/60, the sample of its docs/src/monitoring.md): one
// metric name per measure, the volume label telling the data volume from
// the WAL volume, tablespace volumes left to the storage screen.
func TestParseReadsTheForkDiskSeriesPerVolume(t *testing.T) {
	t.Parallel()
	const payload = `cnpg_disk_available_bytes{volume="pgdata"} 9.79345408e+08
cnpg_disk_available_bytes{volume="wal"} 9.33236736e+08
cnpg_disk_available_bytes{volume="tbs-hist"} 1
cnpg_disk_inodes{volume="pgdata"} 65536
cnpg_disk_inodes_free{volume="pgdata"} 64527
cnpg_disk_inodes_free{volume="wal"} 65517
cnpg_disk_total_bytes{volume="pgdata"} 1.04170496e+09
cnpg_disk_total_bytes{volume="wal"} 1.04170496e+09
cnpg_disk_used_bytes{volume="pgdata"} 6.2359552e+07
cnpg_disk_used_bytes{volume="wal"} 1.08468224e+08
`
	values, instants, err := Parse(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]float64{
		"disk-pgdata-available": 9.79345408e+08, "disk-wal-available": 9.33236736e+08,
		"disk-pgdata-used": 6.2359552e+07, "disk-wal-used": 1.08468224e+08,
		"disk-pgdata-inodes-free": 64527, "disk-wal-inodes-free": 65517,
	}
	for key, v := range want {
		if values[key] != v {
			t.Errorf("%s = %v, want %v", key, values[key], v)
		}
	}
	if instants["disk-pgdata-size"] != 1.04170496e+09 || instants["disk-pgdata-inodes"] != 65536 {
		t.Errorf("instants = %v", instants)
	}
	if _, ok := instants["disk-wal-inodes"]; ok {
		t.Error("an inode count the exporter did not report produced a value")
	}
}
