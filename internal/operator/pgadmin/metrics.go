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

package pgadmin

import (
	"context"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// Per-CR gauges computed on scrape from the cache, kube-state-metrics
// style, with a fixed, closed label set. clusters
// is the number of Clusters the PgAdmin registers (distinct in
// status.servers), not their names: a name list would mint a new series on
// every change of the selection.
var (
	infoDescription = prometheus.NewDesc("pgtoolbox_pgadmin_info",
		"One series per PgAdmin: how many Clusters it registers and the pgAdmin image it runs.",
		[]string{"namespace", "name", "clusters", "image"}, nil)
	conditionDescription = prometheus.NewDesc("pgtoolbox_pgadmin_condition",
		"PgAdmin conditions: 1 for the current status of each condition type.",
		[]string{"namespace", "name", "type", "status"}, nil)
)

const scrapeTimeout = 5 * time.Second

// Collector reads PgAdmins from the manager's cache on every scrape.
type Collector struct {
	Reader client.Reader
}

// Describe implements prometheus.Collector.
func (c Collector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- infoDescription
	descriptions <- conditionDescription
}

// Collect implements prometheus.Collector. A failed list yields no series
// (absent, not zero), which is how a scrape says "unknown".
func (c Collector) Collect(metrics chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
	defer cancel()
	var list pgtoolboxv1alpha1.PgAdminList
	if err := c.Reader.List(ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		p := &list.Items[i]
		clusters := map[string]bool{}
		for _, server := range p.Status.Servers {
			clusters[server.Cluster] = true
		}
		metrics <- prometheus.MustNewConstMetric(infoDescription, prometheus.GaugeValue, 1,
			p.Namespace, p.Name, strconv.Itoa(len(clusters)), p.Status.Image)
		for _, condition := range p.Status.Conditions {
			for _, status := range []metav1.ConditionStatus{metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown} {
				value := 0.0
				if condition.Status == status {
					value = 1
				}
				metrics <- prometheus.MustNewConstMetric(conditionDescription, prometheus.GaugeValue, value,
					p.Namespace, p.Name, condition.Type, string(status))
			}
		}
	}
}
