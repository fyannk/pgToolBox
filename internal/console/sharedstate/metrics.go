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
	sharedModeSince = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_shared_state_mode_since_timestamp_seconds",
		Help: "When (Unix seconds) the state ConfigMap's mode last changed (pgtoolbox_console_shared_state_mode). A replica failing is live again once its watch of the ConfigMap is open and a read or write lands (a write alone does not end it); alert on time() minus this while the mode is not live (and ignoring(mode) the mode series).",
	})
	sharedBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_shared_state_bytes",
		Help: "Bytes of each document of the state ConfigMap as this replica would write it (sessions.json, since.json, digests.json, grants.json, drills.json), and total: the ConfigMap's data, keys and values, every document counted (held within 1,032,192 bytes; the API server refuses an object past 1 MiB).",
	}, []string{"document"})
	signOutsRefused = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_shared_state_signouts_refused_total",
		Help: "Sign-outs the sessions document did not keep (each still refused by the replica that recorded it): replaced (a person at their own bound gave up their own earliest-ending one for the new one), unnamed (the sessions that name nobody at their bound), budget (the document full and the person holding as much as the most), evicted (another person's entry, of the person holding the most, taken for a newcomer holding less), merge (a merge of two replicas' documents past a bound dropped one each had accepted; every replica merging counts it).",
	}, []string{"reason"})
	sharedDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_shared_state_dropped_total",
		Help: "Entries dropped to keep the state ConfigMap within its size, by document: since.json (the oldest since times, past the budget), drills.json (the oldest restore drill results, past the ConfigMap's ceiling).",
	}, []string{"document"})
	sharedForeign = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_shared_state_foreign_documents",
		Help: "1 for each document of the state ConfigMap this replica leaves as it is (neither merged nor rewritten): of a format version newer than it reads (a newer console wrote it: a rolling upgrade, or a rollback), or, for grants.json, unreadable (MCP grants are refused until it is fixed). A newer document is taken over once no console of another version has written the ConfigMap for 30 minutes (after a rollback); deleting the key recovers at once, losing what it holds. Older versions are read and rewritten at this one.",
	}, []string{"document"})
	sharedExhausted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pgtoolbox_console_shared_state_write_exhausted_total",
		Help: "Writes of the state ConfigMap that still conflicted after 5 re-reads: the replica then counts as failing. Rising steadily: the ConfigMap is rewritten faster than this replica can merge, or its resourceVersion went back (an etcd restore).",
	})
	signOutsHeld = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_shared_state_signouts",
		Help: "Entries of the sessions document against each of its bounds: subject (the most one person holds, of 32), unnamed (the sessions that name nobody, of 128), total (of 1536).",
	}, []string{"bound"})
)

func init() {
	crmetrics.Registry.MustRegister(sharedWrites, sharedMode, sharedModeSince, sharedBytes, signOutsRefused, sharedDropped, sharedForeign, signOutsHeld, sharedExhausted)
}
