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

package sweep

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const packExposition = `cnpg_backends_total{datname="app",usename="app",application_name="",state="active"} 2
cnpg_pgtoolbox_pack_info{version="1"} 1
cnpg_pgtoolbox_backends_lock_waiting 1
cnpg_pgtoolbox_blocking_xact_age_seconds{pid="10",blocked_by="",datname="app",usename="app",application_name="api",state="idle in transaction",wait_event_type="Client",wait_event="ClientRead",backend_type="client backend",queryid=""} 120
cnpg_pgtoolbox_blocking_wait_seconds{pid="11",blocked_by="10",datname="app",usename="app",application_name="api",state="active",wait_event_type="Lock",wait_event="transactionid",backend_type="client backend",queryid="42"} 30
`

// exporterServer serves /pg/status and /metrics on one verified TLS
// port; primary decides isPrimary, metrics counts the exposition reads.
func exporterServer(t *testing.T, p pki, primary bool, body func() string) (port string, metricsHits *atomic.Int32) {
	t.Helper()
	metricsHits = &atomic.Int32{}
	srv, _ := statusServer(t, p, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pg/status":
			_, _ = fmt.Fprintf(w, report, primary, "0/3000000", 0)
		case "/metrics":
			metricsHits.Add(1)
			_, _ = w.Write([]byte(body()))
		default:
			http.NotFound(w, r)
		}
	})
	return portOf(t, srv.Listener.Addr().String()), metricsHits
}

func portOf(t *testing.T, addr string) string {
	t.Helper()
	i := strings.LastIndexByte(addr, ':')
	return addr[i+1:]
}

func insightSweeper(roster Roster, bundle *CABundle, p string, now func() time.Time) *Sweeper {
	return New(roster, Options{
		Interval: 5 * time.Second, Timeout: 2 * time.Second, Concurrency: 4, Retention: time.Hour, Metrics: true,
		Bundle: bundle, StatusPort: p, MetricsPort: p, Now: now,
	})
}

func TestSweepReadsTheInsightPack(t *testing.T) {
	p := newPKI(t, "orders-rw.team-a.svc")
	port, hits := exporterServer(t, p, true, func() string { return packExposition })
	roster := staticRoster{{
		Key: key, ServerName: "orders-rw.team-a.svc", MetricsTLS: true,
		Pods: []PodTarget{{Name: "orders-1", UID: "u1", IP: "127.0.0.1", StatusPortTLS: true}},
	}}
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	s := insightSweeper(roster, bundleFile(t, p.caPEM), port, func() time.Time { return now })
	s.SweepOnce(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("the pack must come from the sweep's one exposition read, got %d reads", hits.Load())
	}
	snap, ok := s.Insight(key)
	if !ok || !snap.Present() || snap.Primary != "orders-1" {
		t.Fatalf("snapshot = %+v", snap)
	}
	r, _ := snap.PrimaryReading()
	if len(r.Reading.Sessions) != 2 || !r.At.Equal(now) {
		t.Fatalf("reading = %+v", r)
	}
	// The catalog series of the pack land in the metrics store too.
	if v := s.Metrics(key).InstantReadings(); v == nil {
		t.Fatal("no metrics store")
	}
	if times, _ := s.Metrics(key).Range("insight-lock-waiting", 0); len(times) != 1 {
		t.Fatalf("insight-lock-waiting not stored: %v", times)
	}
}

func TestRefreshInsightIsRateLimited(t *testing.T) {
	p := newPKI(t, "orders-rw.team-a.svc")
	var waiting atomic.Int32
	port, hits := exporterServer(t, p, true, func() string {
		return strings.Replace(packExposition, "lock_waiting 1", fmt.Sprintf("lock_waiting %d", waiting.Load()), 1)
	})
	roster := staticRoster{{
		Key: key, ServerName: "orders-rw.team-a.svc", MetricsTLS: true,
		Pods: []PodTarget{{Name: "orders-1", UID: "u1", IP: "127.0.0.1", StatusPortTLS: true}},
	}}
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	s := insightSweeper(roster, bundleFile(t, p.caPEM), port, clock)
	if _, err := s.RefreshInsight(context.Background(), key); err == nil {
		t.Fatal("an unswept Cluster has no known primary to read")
	}
	s.SweepOnce(context.Background())
	waiting.Store(7)
	now = now.Add(2 * time.Second)
	at, err := s.RefreshInsight(context.Background(), key)
	if err != nil || !at.Equal(now) || hits.Load() != 2 {
		t.Fatalf("refresh: at=%v err=%v hits=%d", at, err, hits.Load())
	}
	snap, _ := s.Insight(key)
	if r, _ := snap.PrimaryReading(); *r.Reading.Backends.LockWaiting != 7 {
		t.Fatalf("refresh not observed: %+v", r.Reading.Backends)
	}
	// Within the second: no read, the last read's time.
	now = now.Add(500 * time.Millisecond)
	if again, err := s.RefreshInsight(context.Background(), key); err != nil || !again.Equal(at) || hits.Load() != 2 {
		t.Fatalf("second refresh read again: at=%v err=%v hits=%d", again, err, hits.Load())
	}
	now = now.Add(time.Second)
	if _, err := s.RefreshInsight(context.Background(), key); err != nil || hits.Load() != 3 {
		t.Fatalf("refresh after the second: err=%v hits=%d", err, hits.Load())
	}
}

func TestRefreshInsightReadsOnlyAPrimary(t *testing.T) {
	p := newPKI(t, "orders-rw.team-a.svc")
	port, hits := exporterServer(t, p, false, func() string { return packExposition })
	roster := staticRoster{{
		Key: key, ServerName: "orders-rw.team-a.svc", MetricsTLS: true,
		Pods: []PodTarget{{Name: "orders-2", UID: "u2", IP: "127.0.0.1", StatusPortTLS: true}},
	}}
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	s := insightSweeper(roster, bundleFile(t, p.caPEM), port, func() time.Time { return now })
	s.SweepOnce(context.Background())
	if _, err := s.RefreshInsight(context.Background(), key); err == nil || hits.Load() != 1 {
		t.Fatalf("a replica-only roster must not be read on demand: err=%v hits=%d", err, hits.Load())
	}
	if snap, _ := s.Insight(key); snap.Primary != "" || !snap.Present() {
		t.Fatalf("a replica's reading is kept but is not the primary's: %+v", snap)
	}
}

func TestTruncatedExpositionIsSaid(t *testing.T) {
	p := newPKI(t, "orders-rw.team-a.svc")
	big := packExposition + strings.Repeat("# padding padding padding padding padding padding padding padding\n", (maxMetricsBody/64)+10)
	port, _ := exporterServer(t, p, true, func() string { return big })
	roster := staticRoster{{
		Key: key, ServerName: "orders-rw.team-a.svc", MetricsTLS: true,
		Pods: []PodTarget{{Name: "orders-1", UID: "u1", IP: "127.0.0.1", StatusPortTLS: true}},
	}}
	s := insightSweeper(roster, bundleFile(t, p.caPEM), port, time.Now)
	s.SweepOnce(context.Background())
	snap, _ := s.Insight(key)
	r, ok := snap.PrimaryReading()
	if !ok || len(r.Reading.Truncated) != 1 || r.Reading.Truncated[0] != "exposition" {
		t.Fatalf("truncation not reported: %+v", r.Reading.Truncated)
	}
}

func TestCutAtLine(t *testing.T) {
	for in, want := range map[string]string{
		"a 1\nb 1234": "a 1\n", "a 1\n": "a 1\n", "no newline at all": "",
	} {
		if got := string(cutAtLine([]byte(in))); got != want {
			t.Errorf("cutAtLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCutSampleIsDropped: the bound falls inside a sample; cut there, the
// value 1234567 would read 12. The line is dropped instead, the cut is
// said, and the next read, the pack having been seen, takes the larger
// bound and reads it whole.
func TestCutSampleIsDropped(t *testing.T) {
	p := newPKI(t, "orders-rw.team-a.svc")
	head := "cnpg_pgtoolbox_pack_info{version=\"1\"} 1\n"
	line := "cnpg_pgtoolbox_backends_lock_waiting 1234567\n"
	pad := maxMetricsBody - len(head) - len("cnpg_pgtoolbox_backends_lock_waiting 12")
	comment := "# " + strings.Repeat("p", 61) + "\n" // 64 bytes
	last := pad % len(comment)
	body := head + strings.Repeat(comment, pad/len(comment)) + "#" + strings.Repeat("p", last-2) + "\n" + line
	port, _ := exporterServer(t, p, true, func() string { return body })
	roster := staticRoster{{
		Key: key, ServerName: "orders-rw.team-a.svc", MetricsTLS: true,
		Pods: []PodTarget{{Name: "orders-1", UID: "u1", IP: "127.0.0.1", StatusPortTLS: true}},
	}}
	s := insightSweeper(roster, bundleFile(t, p.caPEM), port, time.Now)
	s.SweepOnce(context.Background())
	snap, _ := s.Insight(key)
	r, _ := snap.PrimaryReading()
	if !r.Reading.ExpositionCut() || r.Reading.Backends != nil {
		t.Fatalf("cut = %v, backends = %+v: a cut sample must not be read", r.Reading.Truncated, r.Reading.Backends)
	}
	s.SweepOnce(context.Background())
	snap, _ = s.Insight(key)
	r, _ = snap.PrimaryReading()
	if r.Reading.ExpositionCut() || r.Reading.Backends == nil || *r.Reading.Backends.LockWaiting != 1234567 {
		t.Fatalf("second read: cut = %v, backends = %+v", r.Reading.Truncated, r.Reading.Backends)
	}
}

// TestRefreshInsightSharesReadSlots: on-demand reads take the sweep's
// read slots, so they are bounded across every Cluster and user.
func TestRefreshInsightSharesReadSlots(t *testing.T) {
	p := newPKI(t, "orders-rw.team-a.svc")
	port, hits := exporterServer(t, p, true, func() string { return packExposition })
	roster := staticRoster{{
		Key: key, ServerName: "orders-rw.team-a.svc", MetricsTLS: true,
		Pods: []PodTarget{{Name: "orders-1", UID: "u1", IP: "127.0.0.1", StatusPortTLS: true}},
	}}
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	s := insightSweeper(roster, bundleFile(t, p.caPEM), port, func() time.Time { return now })
	s.SweepOnce(context.Background())
	for range cap(s.reads) {
		s.reads <- struct{}{}
	}
	now = now.Add(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.RefreshInsight(ctx, key); !errors.Is(err, ErrReadsBusy) || hits.Load() != 1 {
		t.Fatalf("err = %v, reads = %d: a refresh must wait for a read slot", err, hits.Load())
	}
	<-s.reads
	now = now.Add(2 * time.Second)
	if _, err := s.RefreshInsight(context.Background(), key); err != nil || hits.Load() != 2 {
		t.Fatalf("err = %v, reads = %d", err, hits.Load())
	}
}

// TestCutExpositionLeavesSumsOut is the negative of a truncated read:
// a sum whose label sets straddle the cut is not recorded at the part
// before it (a low connections count, a counter that reads as reset),
// while a gauge folded by Max is kept.
func TestCutExpositionLeavesSumsOut(t *testing.T) {
	p := newPKI(t, "orders-rw.team-a.svc")
	padding := strings.Repeat("# padding padding padding padding padding padding padding padding\n", (maxMetricsBody/64)+10)
	body := func(cut bool) string {
		head := "cnpg_backends_total{datname=\"a\"} 5\ncnpg_backends_max_tx_duration_seconds 3\n"
		tail := "cnpg_backends_total{datname=\"b\"} 7\n"
		if cut {
			return head + padding + tail
		}
		return head + tail
	}
	for _, tc := range []struct {
		name            string
		cut             bool
		wantConnections *float64
	}{
		{"whole exposition", false, ptrTo(12.0)},
		{"cut exposition", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, _ := exporterServer(t, p, true, func() string { return body(tc.cut) })
			roster := staticRoster{{
				Key: key, ServerName: "orders-rw.team-a.svc", MetricsTLS: true,
				Pods: []PodTarget{{Name: "orders-1", UID: "u1", IP: "127.0.0.1", StatusPortTLS: true}},
			}}
			now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
			s := insightSweeper(roster, bundleFile(t, p.caPEM), port, func() time.Time { return now })
			s.SweepOnce(context.Background())
			last := func(series string) *float64 {
				_, byInstance := s.Metrics(key).Range(series, 0)
				values := byInstance["orders-1"]
				if len(values) == 0 {
					return nil
				}
				return values[len(values)-1]
			}
			got := last("connections")
			switch {
			case tc.wantConnections == nil && got != nil:
				t.Fatalf("connections recorded %v from a cut exposition", *got)
			case tc.wantConnections != nil && (got == nil || *got != *tc.wantConnections):
				t.Fatalf("connections = %v, want %v", got, *tc.wantConnections)
			}
			if v := last("max-tx-duration"); v == nil || *v != 3 {
				t.Fatalf("max-tx-duration = %v, want 3 (a Max gauge survives the cut)", v)
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }
