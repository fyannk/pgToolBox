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

package readmodel

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/volumetrend"
)

// metricTrends is the trend store the metrics read: the console builds
// one read model, a test may build several (the last one wins).
var metricTrends atomic.Pointer[volumetrend.Store]

func init() {
	crmetrics.Registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "pgtoolbox_console_volume_trend_series",
			Help: "Volume series the trend store holds (bounded by VOLUME_TREND_MAX_SERIES).",
		}, func() float64 {
			if s := metricTrends.Load(); s != nil {
				return float64(s.Len())
			}
			return 0
		}),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "pgtoolbox_console_volume_trend_refused_total",
			Help: "New volume series the trend store turned away at VOLUME_TREND_MAX_SERIES; those volumes have no forecast.",
		}, func() float64 {
			if s := metricTrends.Load(); s != nil {
				return float64(s.Refused())
			}
			return 0
		}),
	)
}
