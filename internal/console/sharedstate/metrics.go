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

package sharedstate

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics of the shared state, on the console's /metrics; the labels are
// fixed words.
var (
	sharedWrites = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_shared_state_writes_total",
		Help: "Writes of the state ConfigMap by outcome (written, conflict: re-read and merged, failed).",
	}, []string{"outcome"})
	sharedMode = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_shared_state_mode",
		Help: "1 for the state ConfigMap's current mode (pending, live, missing, forbidden, failing); anything but live means each replica keeps its own clocks and sign-outs.",
	}, []string{"mode"})
)

func init() {
	crmetrics.Registry.MustRegister(sharedWrites, sharedMode)
}
