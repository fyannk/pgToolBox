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

package evidence

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Fetch outcomes beyond the failure kinds: the answer was accepted, or
// accepted but from another configuration epoch than the status names.
const (
	outcomeSuccess    = "success"
	outcomeStaleEpoch = "stale_epoch"
)

// outcomes is the closed label set of pgtoolbox_console_evidence_fetches_total.
var outcomes = []string{
	outcomeSuccess, outcomeStaleEpoch,
	string(failureTimeout), string(failureCanceled), string(failureTLS), string(failureUnauthorized),
	string(failureForbidden), string(failureAuthUnavailable), string(failureBusy), string(failureContract),
	string(failureTooLarge), string(failureUnavailable), string(failureIdentity), string(failurePublicationChanged),
	string(failureEndpoint), string(failureCABundle), string(failureTokenMissing), string(failureTokenInvalid),
	string(failureAudienceMismatch), string(failureStatusWriter), string(failureCAFile),
}

var fetches = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pgtoolbox_console_evidence_fetches_total",
	Help: "Evidence agent fetches by outcome (success, stale_epoch, or the closed failure kind).",
}, []string{"outcome"})

// agentStates is the closed state label set of
// pgtoolbox_console_evidence_agent_status: the states an agent's Result can hold.
var agentStates = []viewmodel.EvidenceState{
	viewmodel.EvidencePending, viewmodel.EvidenceLive, viewmodel.EvidenceStale, viewmodel.EvidenceUnavailable,
}

// agentStatus is 1 for the state each listed EvidenceAgent is in and 0 for
// its other states, so an alert can select on state without absent()
// juggling; an agent's series are removed when it is no longer listed.
var agentStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "pgtoolbox_console_evidence_agent_status",
	Help: "Evidence state of each EvidenceAgent the console reads (1 for its current state, 0 for the others): pending, live, stale, unavailable.",
}, []string{"namespace", "name", "state"})

func init() {
	for _, outcome := range outcomes {
		fetches.WithLabelValues(outcome)
	}
	crmetrics.Registry.MustRegister(fetches, agentStatus)
}

// publishAgentStates moves the per-agent gauge from the previous states to
// the next ones (keyed "namespace/name"): series of agents no longer
// present are deleted. Called under the runner's lock.
func publishAgentStates(previous, next map[string]*agentState) {
	for key := range previous {
		if _, ok := next[key]; ok {
			continue
		}
		namespace, name, _ := strings.Cut(key, "/")
		agentStatus.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
	}
	for key, state := range next {
		namespace, name, _ := strings.Cut(key, "/")
		for _, candidate := range agentStates {
			value := 0.0
			if state.result.State == candidate {
				value = 1
			}
			agentStatus.WithLabelValues(namespace, name, string(candidate)).Set(value)
		}
	}
}

// countFetch increments the counter for a closed outcome.
func countFetch(outcome string) {
	fetches.WithLabelValues(outcome).Inc()
}
