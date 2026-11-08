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
	"fmt"
	"io"
	"slices"
	"sort"
	"time"
)

// sample is the outcome of one request.
type sample struct {
	d      time.Duration
	status int // 0: transport error
}

// epStats accumulates the samples of one endpoint. It is not safe for
// concurrent use: every worker owns its own and they are merged at the end.
type epStats struct {
	durations []time.Duration
	errors    int
	statuses  map[int]int
}

func (e *epStats) add(s sample) {
	if e.statuses == nil {
		e.statuses = map[int]int{}
	}
	e.statuses[s.status]++
	if s.status < 200 || s.status > 299 {
		e.errors++
		return
	}
	e.durations = append(e.durations, s.d)
}

func (e *epStats) merge(o *epStats) {
	e.durations = append(e.durations, o.durations...)
	e.errors += o.errors
	if e.statuses == nil {
		e.statuses = map[int]int{}
	}
	for code, n := range o.statuses {
		e.statuses[code] += n
	}
}

func (e *epStats) count() int {
	n := 0
	for _, c := range e.statuses {
		n += c
	}
	return n
}

// percentile returns the p-th percentile (0..100) of the successful
// durations by the nearest-rank method; zero when there are none.
func percentile(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	rank := int(p/100*float64(len(sorted)) + 0.999999)
	rank = min(max(rank, 1), len(sorted))
	return sorted[rank-1]
}

// statusSummary renders the non-2xx outcomes: "429x3 0x1" (0 is a
// transport error).
func (e *epStats) statusSummary() string {
	codes := make([]int, 0, len(e.statuses))
	for code := range e.statuses {
		if code < 200 || code > 299 {
			codes = append(codes, code)
		}
	}
	sort.Ints(codes)
	out := ""
	for _, code := range codes {
		if out != "" {
			out += " "
		}
		out += fmt.Sprintf("%dx%d", code, e.statuses[code])
	}
	if out == "" {
		return "-"
	}
	return out
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond)) }

// printEndpointTable writes one row per endpoint: count, errors, rate and
// the latency percentiles of the successful requests, in milliseconds.
func printEndpointTable(w io.Writer, stats map[string]*epStats, elapsed time.Duration) {
	names := make([]string, 0, len(stats))
	for name := range stats {
		names = append(names, name)
	}
	sort.Strings(names)
	_, _ = fmt.Fprintf(w, "%-22s %8s %6s %8s %9s %9s %9s %9s  %s\n", "endpoint", "count", "errs", "req/s", "p50(ms)", "p95(ms)", "p99(ms)", "max(ms)", "non-2xx")
	for _, name := range names {
		e := stats[name]
		var maxD time.Duration
		for _, d := range e.durations {
			maxD = max(maxD, d)
		}
		rate := 0.0
		if elapsed > 0 {
			rate = float64(e.count()) / elapsed.Seconds()
		}
		_, _ = fmt.Fprintf(w, "%-22s %8d %6d %8.1f %9s %9s %9s %9s  %s\n", name, e.count(), e.errors, rate,
			ms(percentile(e.durations, 50)), ms(percentile(e.durations, 95)), ms(percentile(e.durations, 99)), ms(maxD), e.statusSummary())
	}
}
