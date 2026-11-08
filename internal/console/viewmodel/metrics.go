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

package viewmodel

import "time"

// Metrics is the /metrics screen (metrics are delegated).
// Without a monitoring system the console keeps a short ring of the
// instants its rules read and shows them as "now" tiles and sparklines;
// it is not a monitoring system and says so.
type Metrics struct {
	Ref         ClusterRef `json:"ref"`
	GeneratedAt time.Time  `json:"generatedAt"`
	// Source is the exporter scrape riding the instance sweep.
	Source SourceState `json:"source"`
	// IntervalSeconds and RetentionSeconds describe the ring.
	IntervalSeconds  int `json:"intervalSeconds,omitempty"`
	RetentionSeconds int `json:"retentionSeconds,omitempty"`
	// Tiles are the latest instants, one per instance and key.
	Tiles []MetricTile `json:"tiles"`
	// Charts are the series, one line per instance: the console's ring,
	// or Prometheus when PROMETHEUS_URL is set and answers (ChartSource).
	Charts []MetricChart `json:"charts"`
	// ChartSource says where Charts come from.
	ChartSource ChartSource `json:"chartSource,omitempty" enum:"ring,prometheus"`
	// Prometheus is the PromQL source's state; absent when PROMETHEUS_URL
	// is not set. When it is not live the charts fall back to the ring
	// and the monitoring link-out.
	Prometheus *SourceState `json:"prometheus,omitempty"`
	// RangeSeconds and StepSeconds describe Prometheus charts.
	RangeSeconds int `json:"rangeSeconds,omitempty"`
	StepSeconds  int `json:"stepSeconds,omitempty"`
	// Monitoring is the external monitoring link-out.
	Monitoring Monitoring  `json:"monitoring"`
	BlindSpots []BlindSpot `json:"blindSpots"`
}

// ChartSource is where the metrics charts come from.
type ChartSource string

// The chart sources.
const (
	// ChartSourceRing is the console's own short in-memory ring.
	ChartSourceRing ChartSource = "ring"
	// ChartSourcePrometheus is a PromQL range query per series.
	ChartSourcePrometheus ChartSource = "prometheus"
)

// MetricTile is one instant.
type MetricTile struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Group    string `json:"group,omitempty"`
	Unit     string `json:"unit,omitempty" doc:"bytes, seconds, count, ratio, ..."`
	Instance string `json:"instance"`
	// Value is absent when the instance did not report the metric.
	Value      *float64   `json:"value,omitempty"`
	At         *time.Time `json:"at,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// MetricChart is one series across instances on shared timestamps.
type MetricChart struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Group string `json:"group,omitempty"`
	Unit  string `json:"unit,omitempty"`
	// Times are the sample instants; each line has one value per time
	// (null where the instance had no sample: a gap, not a zero).
	Times []time.Time  `json:"times"`
	Lines []MetricLine `json:"lines"`
	Note  string       `json:"note,omitempty" doc:"How to read the chart (never alert on cnpg_pg_replication_lag alone, ...)."`
}

// MetricLine is one instance's values.
type MetricLine struct {
	Instance string     `json:"instance"`
	Values   []*float64 `json:"values"`
}

// Monitoring is the console's link to the site's monitoring system.
type Monitoring struct {
	// State is notConfigured unless the chart sets a monitoring URL.
	State string         `json:"state" enum:"notConfigured,configured"`
	URL   string         `json:"url,omitempty"`
	Links []ExternalLink `json:"links,omitempty"`
}

// ExternalLink is a link out of the console.
type ExternalLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}
