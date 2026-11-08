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
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
)

const promAnswer = `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"orders-1"},"values":[[1730282400,"5"]]}]}}`

// tlsProm serves handler over TLS and returns its URL and a CA file.
func tlsProm(t *testing.T, handler http.HandlerFunc) (string, string) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.crt")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv.URL, ca
}

func TestPrometheusClientQuery(t *testing.T) {
	t.Parallel()
	var seen atomic.Value
	url, ca := tlsProm(t, func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Clone(r.Context()))
		_, _ = w.Write([]byte(promAnswer))
	})
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("console-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewPrometheus(PrometheusOptions{URL: url + "/prom/", TokenFile: token, CAFile: ca, Namespaced: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1730282400, 0)
	series, err := c.QueryRange(context.Background(), "team-a", `up{a="b"}`, start, start.Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Pod != "orders-1" {
		t.Fatalf("series = %+v", series)
	}
	r, _ := seen.Load().(*http.Request)
	if r.URL.Path != "/prom/api/v1/query_range" {
		t.Errorf("path = %s", r.URL.Path)
	}
	q := r.URL.Query()
	if q.Get("query") != `up{a="b"}` || q.Get("start") != "1730282400" || q.Get("end") != "1730286000" || q.Get("step") != "60" || q.Get("namespace") != "team-a" {
		t.Errorf("query = %v", q)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer console-token" {
		t.Errorf("Authorization = %q, want the token file's content", got)
	}
}

func TestPrometheusClientFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		state   viewmodel.SourceStateKind
		reason  string
	}{
		{"forbidden", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }, viewmodel.SourceForbidden, "refused the console's identity"},
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }, viewmodel.SourceUnavailable, "HTTP 502"},
		{"redirect not followed", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example/steal", http.StatusFound)
		}, viewmodel.SourceUnavailable, "redirect"},
		{"error status", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
		}, viewmodel.SourceUnavailable, "error status"},
		{"oversized", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat(" ", maxPromBody+1)))
		}, viewmodel.SourceUnavailable, "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			url, ca := tlsProm(t, tt.handler)
			c, err := NewPrometheus(PrometheusOptions{URL: url, CAFile: ca})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.QueryRange(context.Background(), "team-a", "up", time.Unix(0, 0), time.Unix(60, 0), time.Minute)
			var pe *promError
			if !errors.As(err, &pe) || pe.state != tt.state || !strings.Contains(pe.reason, tt.reason) {
				t.Fatalf("err = %v, want %s containing %q", err, tt.state, tt.reason)
			}
			if strings.Contains(pe.reason, url) {
				t.Error("the reason names the URL")
			}
		})
	}
}

func TestPrometheusClientUntrustedCertificate(t *testing.T) {
	t.Parallel()
	url, _ := tlsProm(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(promAnswer)) })
	c, err := NewPrometheus(PrometheusOptions{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.QueryRange(context.Background(), "", "up", time.Unix(0, 0), time.Unix(60, 0), time.Minute); err == nil {
		t.Fatal("a server the CA does not vouch for was believed")
	}
}

func TestNewPrometheusRefuses(t *testing.T) {
	t.Parallel()
	for _, o := range []PrometheusOptions{
		{URL: "http://prometheus:9090", TokenFile: "/t"},
		{URL: "https://u:p@prometheus"},
		{URL: "prometheus:9090"},
		{URL: "https://prometheus/?q=1"},
	} {
		if _, err := NewPrometheus(o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

// fakeRanger answers every query with one series per pod, or err.
type fakeRanger struct {
	mu      sync.Mutex
	calls   int
	err     error
	empty   bool
	queries []string
}

func (f *fakeRanger) QueryRange(_ context.Context, ns, query string, start, _ time.Time, _ time.Duration) ([]metrics.PromSeries, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.queries = append(f.queries, ns+"|"+query)
	if f.err != nil {
		return nil, f.err
	}
	if f.empty {
		return nil, nil
	}
	return []metrics.PromSeries{{Pod: "orders-1", Samples: []metrics.PromSample{{At: start, Value: 1}}}}, nil
}

func TestMetricsFromPrometheus(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		ranger     *fakeRanger
		source     viewmodel.ChartSource
		state      viewmodel.SourceStateKind
		withCharts bool
	}{
		{"live", &fakeRanger{}, viewmodel.ChartSourcePrometheus, viewmodel.SourceLive, true},
		{"unreachable falls back to the ring", &fakeRanger{err: &promError{state: viewmodel.SourceUnavailable, reason: "Prometheus is unreachable from the console"}}, viewmodel.ChartSourceRing, viewmodel.SourceUnavailable, false},
		{"no series is said, not hidden", &fakeRanger{empty: true}, viewmodel.ChartSourceRing, viewmodel.SourceUnavailable, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newService(ctx, t, nil)
			svc.prom = newPromCharts(tt.ranger, time.Hour, func() time.Time { return now })
			m, err := svc.Metrics(ctx, "team-a", "orders")
			if err != nil {
				t.Fatal(err)
			}
			if m.ChartSource != tt.source || m.Prometheus == nil || m.Prometheus.State != tt.state {
				t.Fatalf("source %s prometheus %+v", m.ChartSource, m.Prometheus)
			}
			if tt.state != viewmodel.SourceLive && (m.Prometheus.Reason == "" || m.Prometheus.Since == nil) {
				t.Errorf("a failing source must say why and since when: %+v", m.Prometheus)
			}
			if (len(m.Charts) > 0) != tt.withCharts {
				t.Errorf("charts = %d", len(m.Charts))
			}
			if tt.withCharts {
				c := m.Charts[0]
				if len(c.Lines) != 1 || c.Lines[0].Instance != "orders-1" || len(c.Lines[0].Values) != len(c.Times) || m.StepSeconds != 30 || m.RangeSeconds != 3600 {
					t.Errorf("chart = %+v, step %d range %d", c, m.StepSeconds, m.RangeSeconds)
				}
			}
			if m.Monitoring.State == "" {
				t.Error("the link-out state is always reported")
			}
			for _, q := range tt.ranger.queries {
				if !strings.HasPrefix(q, `team-a|`) || !strings.Contains(q, `namespace="team-a",pod=~"orders-[0-9]+"`) {
					t.Fatalf("query not scoped to the Cluster: %s", q)
				}
			}
			calls := tt.ranger.calls
			if _, err := svc.Metrics(ctx, "team-a", "orders"); err != nil {
				t.Fatal(err)
			}
			if tt.ranger.calls != calls {
				t.Errorf("a refresh within the cache TTL re-ran %d queries", tt.ranger.calls-calls)
			}
		})
	}
}

func TestMetricsWithoutPrometheusIsTheRing(t *testing.T) {
	ctx := context.Background()
	svc := newService(ctx, t, nil)
	m, err := svc.Metrics(ctx, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if m.ChartSource != viewmodel.ChartSourceRing || m.Prometheus != nil {
		t.Errorf("source %s prometheus %+v", m.ChartSource, m.Prometheus)
	}
}
