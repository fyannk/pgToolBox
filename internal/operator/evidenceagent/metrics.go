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

package evidenceagent

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// Per-CR gauges computed on scrape from the cache,
// kube-state-metrics style; labels are the object's identity plus a closed
// vocabulary, never a free-text value.
var (
	infoDescription = prometheus.NewDesc("pgtoolbox_evidenceagent_info",
		"One series per EvidenceAgent: the Cluster it reads, the issuer and the agent image.",
		[]string{"namespace", "name", "cluster", "issuer", "image"}, nil)
	conditionDescription = prometheus.NewDesc("pgtoolbox_evidenceagent_condition",
		"EvidenceAgent conditions: 1 for the current status of each condition type.",
		[]string{"namespace", "name", "type", "status"}, nil)
	epochDescription = prometheus.NewDesc("pgtoolbox_evidenceagent_producer_epoch",
		"The producer epoch the running agent publishes (status.producerEpoch).",
		[]string{"namespace", "name"}, nil)
)

const scrapeTimeout = 5 * time.Second

// Collector reads EvidenceAgents from the manager's cache on every scrape.
type Collector struct {
	Reader client.Reader
}

// Describe implements prometheus.Collector.
func (c Collector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- infoDescription
	descriptions <- conditionDescription
	descriptions <- epochDescription
}

// Collect implements prometheus.Collector. A failed list yields no series
// (absent, not zero), which is how a scrape says "unknown".
func (c Collector) Collect(metrics chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	var agents pgtoolboxv1alpha1.EvidenceAgentList
	if err := c.Reader.List(ctx, &agents); err != nil {
		return
	}
	for i := range agents.Items {
		agent := &agents.Items[i]
		metrics <- prometheus.MustNewConstMetric(infoDescription, prometheus.GaugeValue, 1,
			agent.Namespace, agent.Name, agent.Spec.ClusterRef.Name, string(agent.CertificateIssuer()), agent.Status.Image)
		metrics <- prometheus.MustNewConstMetric(epochDescription, prometheus.GaugeValue, float64(agent.Status.ProducerEpoch), agent.Namespace, agent.Name)
		for _, condition := range agent.Status.Conditions {
			for _, status := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown} {
				value := 0.0
				if condition.Status == status {
					value = 1
				}
				metrics <- prometheus.MustNewConstMetric(conditionDescription, prometheus.GaugeValue, value,
					agent.Namespace, agent.Name, condition.Type, string(status))
			}
		}
	}
}
