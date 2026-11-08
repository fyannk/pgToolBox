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

// The PromQL side of the catalog: with a Prometheus URL configured the
// console queries PromQL for charts. The console never forwards a
// caller's query: it builds one fixed query per catalogued series, scoped
// to one Cluster's instance Pods, and parses the answer of the Prometheus
// HTTP API (GET /api/v1/query_range, resultType "matrix") into the same
// shape the ring produces: shared timestamps, one line per instance, a nil
// value where Prometheus had no sample.
//
// The scope assumes the scrape keeps the target labels the Prometheus
// Operator's PodMonitor (CloudNativePG's monitoring.enablePodMonitor, or
// one the site writes) and OpenShift's user-workload monitoring attach to
// every sample: namespace and pod. Instance Pods are named
// <cluster>-<serial> (CloudNativePG pkg/specs/pods.go), so the pod
// matcher selects exactly the Cluster's instances.

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Bounds of a parsed answer: series kept per query, and samples per
// series. A Cluster has at most a few dozen instances; a range query of
// the console's own making asks for at most MaxRangePoints points.
const (
	MaxPromSeries  = 64
	MaxRangePoints = 1441
)

// dnsLabel is the shape of a Kubernetes namespace and of a Cluster name
// (RFC 1123 label, 63 bytes at most): it contains nothing PromQL would
// interpret inside a string literal or a regular expression.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// PromQL is the query for one catalogued series of one Cluster: the
// series' first candidate metric name or the others (exporters renamed
// some counters), restricted to the Cluster's instance Pods and to the
// Match labels, rated over window for counters, folded per pod by the
// series' aggregate. ok is false when the series names no metric or the
// names are not RFC 1123 labels.
func PromQL(def SeriesDef, namespace, cluster string, window time.Duration) (query string, ok bool) {
	if len(def.Names) == 0 || !dnsLabel.MatchString(namespace) || !dnsLabel.MatchString(cluster) {
		return "", false
	}
	labels := []string{`namespace="` + namespace + `"`, `pod=~"` + cluster + `-[0-9]+"`}
	keys := make([]string, 0, len(def.Match))
	for k := range def.Match {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		labels = append(labels, k+`="`+promString(def.Match[k])+`"`)
	}
	selector := "{" + strings.Join(labels, ",") + "}"
	agg := "sum"
	if def.Aggregate == Max {
		agg = "max"
	}
	terms := make([]string, 0, len(def.Names))
	for _, name := range def.Names {
		expr := name + selector
		if def.Kind == Counter {
			expr = "rate(" + expr + "[" + promDuration(window) + "])"
		}
		terms = append(terms, agg+" by (pod) ("+expr+")")
	}
	return strings.Join(terms, " or "), true
}

// promString escapes a PromQL double-quoted string.
func promString(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// promDuration renders d in whole seconds, at least one.
func promDuration(d time.Duration) string {
	s := max(int64(d/time.Second), 1)
	return strconv.FormatInt(s, 10) + "s"
}

// RangeStep is the step of a range query over rng that yields about
// points samples, never under the floor (Prometheus refuses a query of
// more than 11,000 points per series; the console asks for far fewer).
func RangeStep(rng time.Duration, points int, floor time.Duration) time.Duration {
	if points < 1 {
		points = 1
	}
	step := (rng / time.Duration(points)).Truncate(time.Second)
	return max(step, floor, time.Second)
}

// Errors of ParseRange.
var (
	ErrPromStatus    = errors.New("prometheus answered with an error status")
	ErrPromResult    = errors.New("prometheus answered with a result that is not a matrix")
	ErrPromMalformed = errors.New("prometheus answer is malformed")
)

// PromSeries is one pod's samples of a range answer.
type PromSeries struct {
	Pod     string
	Samples []PromSample
}

// PromSample is one (instant, value) pair.
type PromSample struct {
	At    time.Time
	Value float64
}

type promRangeAnswer struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// ParseRange reads the body of a Prometheus /api/v1/query_range answer.
// The body comes from a server the console does not control: it is
// size-bounded by the caller, every series and sample count is bounded
// here, samples without a pod label or with a non-finite or unparsable
// value are dropped (a gap, never a zero), and pods come back sorted.
func ParseRange(body []byte) ([]PromSeries, error) {
	var a promRangeAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, ErrPromMalformed
	}
	if a.Status != "success" {
		return nil, ErrPromStatus
	}
	if a.Data.ResultType != "matrix" {
		return nil, ErrPromResult
	}
	byPod := map[string][]PromSample{}
	for _, r := range a.Data.Result {
		pod := r.Metric["pod"]
		if !dnsSubdomainLike(pod) {
			continue
		}
		if _, seen := byPod[pod]; !seen && len(byPod) >= MaxPromSeries {
			continue
		}
		samples := byPod[pod]
		for _, v := range r.Values {
			if len(samples) >= MaxRangePoints {
				break
			}
			ts, ok := v[0].(float64)
			raw, ok2 := v[1].(string)
			if !ok || !ok2 || math.IsNaN(ts) || math.IsInf(ts, 0) || ts < 0 || ts > 1e11 {
				continue
			}
			f, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				continue
			}
			// Prometheus timestamps carry milliseconds.
			samples = append(samples, PromSample{At: time.UnixMilli(int64(math.Round(ts * 1000))).UTC(), Value: f})
		}
		byPod[pod] = samples
	}
	out := make([]PromSeries, 0, len(byPod))
	for pod, samples := range byPod {
		slices.SortFunc(samples, func(a, b PromSample) int { return a.At.Compare(b.At) })
		out = append(out, PromSeries{Pod: pod, Samples: samples})
	}
	slices.SortFunc(out, func(a, b PromSeries) int { return strings.Compare(a.Pod, b.Pod) })
	return out, nil
}

// dnsSubdomainLike accepts a plausible Pod name: non-empty, at most 253
// bytes, lowercase alphanumerics, '-' and '.'.
func dnsSubdomainLike(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

// AlignRange lays series on the grid start, start+step, ..., end: one
// value per grid instant and pod, nil where the pod had no sample at that
// instant. Prometheus evaluates a range query exactly on that grid, so a
// sample off the grid is dropped rather than moved.
func AlignRange(series []PromSeries, start, end time.Time, step time.Duration) (times []time.Time, lines map[string][]*float64) {
	if step <= 0 || end.Before(start) {
		return nil, map[string][]*float64{}
	}
	n := min(int(end.Sub(start)/step)+1, MaxRangePoints)
	times = make([]time.Time, n)
	for i := range times {
		times[i] = start.Add(time.Duration(i) * step).UTC()
	}
	lines = make(map[string][]*float64, len(series))
	for _, s := range series {
		values := make([]*float64, n)
		for _, sample := range s.Samples {
			off := sample.At.Sub(start)
			if off < 0 || off%step != 0 {
				continue
			}
			if i := int(off / step); i < n {
				v := sample.Value
				values[i] = &v
			}
		}
		lines[s.Pod] = values
	}
	return times, lines
}
