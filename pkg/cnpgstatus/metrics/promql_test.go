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

package metrics

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPromQL(t *testing.T) {
	t.Parallel()
	gauge := SeriesDef{Key: "connections", Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_backends_total"}}
	counter := SeriesDef{Key: "deadlocks", Kind: Counter, Aggregate: Sum, Names: []string{"cnpg_a_total", "cnpg_a"}}
	maxed := SeriesDef{Key: "lag", Kind: Gauge, Aggregate: Max, Names: []string{"cnpg_pg_replication_lag"}}
	matched := SeriesDef{Key: "wal", Kind: Gauge, Aggregate: Sum, Names: []string{"cnpg_collector_pg_wal"}, Match: map[string]string{"value": `si"ze`, "a": "b"}}
	tests := []struct {
		name, ns, cluster string
		def               SeriesDef
		want              string
		ok                bool
	}{
		{"gauge", "team-a", "orders", gauge, `sum by (pod) (cnpg_backends_total{namespace="team-a",pod=~"orders-[0-9]+"})`, true},
		{
			"counter alternates", "team-a", "orders", counter,
			`sum by (pod) (rate(cnpg_a_total{namespace="team-a",pod=~"orders-[0-9]+"}[300s])) or sum by (pod) (rate(cnpg_a{namespace="team-a",pod=~"orders-[0-9]+"}[300s]))`, true,
		},
		{"max", "team-a", "orders", maxed, `max by (pod) (cnpg_pg_replication_lag{namespace="team-a",pod=~"orders-[0-9]+"})`, true},
		{
			"match labels sorted and escaped", "team-a", "orders", matched,
			`sum by (pod) (cnpg_collector_pg_wal{namespace="team-a",pod=~"orders-[0-9]+",a="b",value="si\"ze"})`, true,
		},
		{"no metric name", "team-a", "orders", SeriesDef{Key: "x"}, "", false},
		{"namespace injection refused", `team-a"} or vector(1) #`, "orders", gauge, "", false},
		{"cluster regex refused", "team-a", "orders.*", gauge, "", false},
		{"uppercase refused", "Team", "orders", gauge, "", false},
	}
	for _, tt := range tests {
		got, ok := PromQL(tt.def, tt.ns, tt.cluster, 5*time.Minute)
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s:\n got %q %v\nwant %q %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestEveryInstanceSeriesHasAQuery(t *testing.T) {
	t.Parallel()
	for _, def := range Instance.Series {
		if _, ok := PromQL(def, "ns", "c", time.Minute); !ok {
			t.Errorf("series %s builds no query", def.Key)
		}
	}
}

func TestRangeStep(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rng    time.Duration
		points int
		floor  time.Duration
		want   time.Duration
	}{
		{time.Hour, 120, 15 * time.Second, 30 * time.Second},
		{10 * time.Minute, 120, 15 * time.Second, 15 * time.Second},
		{time.Hour, 0, 0, time.Hour},
		{time.Second, 1000, 0, time.Second},
	}
	for _, tt := range tests {
		if got := RangeStep(tt.rng, tt.points, tt.floor); got != tt.want {
			t.Errorf("RangeStep(%v, %d, %v) = %v, want %v", tt.rng, tt.points, tt.floor, got, tt.want)
		}
	}
}

const promOK = `{"status":"success","data":{"resultType":"matrix","result":[
 {"metric":{"pod":"orders-2"},"values":[[1730282400,"1"],[1730282460,"NaN"],[1730282520,"3.5"]]},
 {"metric":{"pod":"orders-1"},"values":[[1730282460.000,"2"],[1730282400,"+Inf"],[1730282490,"9"]]},
 {"metric":{},"values":[[1730282400,"7"]]},
 {"metric":{"pod":"Bad Pod"},"values":[[1730282400,"7"]]}
]}}`

func TestParseRangeAndAlign(t *testing.T) {
	t.Parallel()
	series, err := ParseRange([]byte(promOK))
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 || series[0].Pod != "orders-1" || series[1].Pod != "orders-2" {
		t.Fatalf("series = %+v, want orders-1 then orders-2 only", series)
	}
	start := time.Unix(1730282400, 0).UTC()
	times, lines := AlignRange(series, start, start.Add(2*time.Minute), time.Minute)
	if len(times) != 3 || !times[2].Equal(start.Add(2*time.Minute)) {
		t.Fatalf("times = %v", times)
	}
	show := func(v []*float64) string {
		parts := make([]string, len(v))
		for i, p := range v {
			if p == nil {
				parts[i] = "-"
			} else {
				parts[i] = strconv.FormatFloat(*p, 'g', -1, 64)
			}
		}
		return strings.Join(parts, " ")
	}
	// orders-1: +Inf dropped, the off-grid sample at :90 dropped.
	if got := show(lines["orders-1"]); got != "- 2 -" {
		t.Errorf("orders-1 = %s", got)
	}
	// orders-2: NaN is a gap, never a zero.
	if got := show(lines["orders-2"]); got != "1 - 3.5" {
		t.Errorf("orders-2 = %s", got)
	}
}

func TestParseRangeRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body string
		want       error
	}{
		{"not json", `<html>`, ErrPromMalformed},
		{"error status", `{"status":"error","errorType":"bad_data","error":"x"}`, ErrPromStatus},
		{"vector", `{"status":"success","data":{"resultType":"vector","result":[]}}`, ErrPromResult},
		{"wrong value shape", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"a"},"values":"x"}]}}`, ErrPromMalformed},
	}
	for _, tt := range tests {
		if _, err := ParseRange([]byte(tt.body)); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestParseRangeBounds(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString(`{"status":"success","data":{"resultType":"matrix","result":[`)
	for i := range MaxPromSeries + 10 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"metric":{"pod":"p-` + strconv.Itoa(i) + `"},"values":[`)
		for j := range 3 {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString(`[1730282400,"1"]`)
		}
		b.WriteString("]}")
	}
	b.WriteString("]}}")
	series, err := ParseRange([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != MaxPromSeries {
		t.Errorf("kept %d series, want the bound %d", len(series), MaxPromSeries)
	}
}

// FuzzParseRange holds the parser's invariants on any body: no panic, and
// whatever it accepts is bounded, sorted, finite and labelled by a
// plausible pod name.
func FuzzParseRange(f *testing.F) {
	f.Add([]byte(promOK))
	f.Add([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"a"},"values":[[1e300,"1"],[-1,"2"],["x","3"]]}]}}`))
	f.Add([]byte(`{"status":"success","data":{"resultType":"matrix","result":null}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		series, err := ParseRange(body)
		if err != nil {
			if series != nil {
				t.Fatal("series returned with an error")
			}
			return
		}
		if len(series) > MaxPromSeries {
			t.Fatalf("%d series exceed the bound", len(series))
		}
		for i, s := range series {
			if !dnsSubdomainLike(s.Pod) {
				t.Fatalf("pod %q accepted", s.Pod)
			}
			if i > 0 && series[i-1].Pod >= s.Pod {
				t.Fatalf("pods not sorted and unique: %q then %q", series[i-1].Pod, s.Pod)
			}
			if len(s.Samples) > MaxRangePoints {
				t.Fatalf("%d samples exceed the bound", len(s.Samples))
			}
			for _, p := range s.Samples {
				if math.IsNaN(p.Value) || math.IsInf(p.Value, 0) {
					t.Fatalf("non-finite value kept: %v", p.Value)
				}
			}
		}
		start := time.Unix(1730282400, 0)
		times, lines := AlignRange(series, start, start.Add(time.Hour), time.Minute)
		for pod, values := range lines {
			if len(values) != len(times) {
				t.Fatalf("pod %s has %d values for %d instants", pod, len(values), len(times))
			}
		}
	})
}
