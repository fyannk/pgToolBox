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

package accessrequest

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// Per-CR gauges computed on scrape from the cache. The labels
// are the request's identity and a closed vocabulary: never the subject's
// name (a person) nor the justification (free text).
var (
	infoDescription = prometheus.NewDesc("pgtoolbox_pgaccessrequest_info",
		"One series per PgAccessRequest: its capability, phase and subject kind.",
		[]string{"namespace", "name", "capability", "phase", "subject_kind"}, nil)
	conditionDescription = prometheus.NewDesc("pgtoolbox_pgaccessrequest_condition",
		"PgAccessRequest conditions: 1 for the current status of each condition type.",
		[]string{"namespace", "name", "type", "status"}, nil)
	expiryDescription = prometheus.NewDesc("pgtoolbox_pgaccessrequest_expires_timestamp_seconds",
		"When an approved request's grant ends (spec.expiresAt), for requests that have one.",
		[]string{"namespace", "name"}, nil)
)

const scrapeTimeout = 5 * time.Second

// Collector reads PgAccessRequests from the manager's cache on every scrape.
type Collector struct {
	Reader client.Reader
}

// Describe implements prometheus.Collector.
func (c Collector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- infoDescription
	descriptions <- conditionDescription
	descriptions <- expiryDescription
}

// Collect implements prometheus.Collector. A failed list yields no series.
func (c Collector) Collect(metrics chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	var list pgtoolboxv1alpha1.PgAccessRequestList
	if err := c.Reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		r := &list.Items[i]
		metrics <- prometheus.MustNewConstMetric(infoDescription, prometheus.GaugeValue, 1,
			r.Namespace, r.Name, string(r.Spec.Capability), string(r.Phase()), string(r.Spec.Subject.Kind))
		if r.Spec.ExpiresAt != nil && r.Phase() == pgtoolboxv1alpha1.AccessPhaseApproved {
			metrics <- prometheus.MustNewConstMetric(expiryDescription, prometheus.GaugeValue, float64(r.Spec.ExpiresAt.Unix()), r.Namespace, r.Name)
		}
		for _, condition := range r.Status.Conditions {
			for _, status := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown} {
				value := 0.0
				if condition.Status == status {
					value = 1
				}
				metrics <- prometheus.MustNewConstMetric(conditionDescription, prometheus.GaugeValue, value, r.Namespace, r.Name, condition.Type, string(status))
			}
		}
	}
}
