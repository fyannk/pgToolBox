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

package statesnap

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics of the snapshots, on the console's /metrics. The label is the
// part's fixed name (sweep, log-matches).
var (
	snapshotSaved = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_state_snapshot_last_success_timestamp_seconds",
		Help: "When each state snapshot was last written to the history volume (Unix seconds); its age is time() minus this.",
	}, []string{"part"})
	snapshotBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_state_snapshot_bytes",
		Help: "Size of each state snapshot's last successful write.",
	}, []string{"part"})
	snapshotErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_state_snapshot_errors_total",
		Help: "State snapshot writes that failed; the previous snapshot stays on the volume.",
	}, []string{"part"})
)

func init() {
	crmetrics.Registry.MustRegister(snapshotSaved, snapshotBytes, snapshotErrors)
}
