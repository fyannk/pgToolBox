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

package main

import (
	"crypto/tls"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// metricsOptions serves /metrics: HTTPS only, and every scrape authenticated by a TokenReview
// and authorized by a SubjectAccessReview for `get` on the non-resource
// URL /metrics (filters.WithAuthenticationAndAuthorization), which the
// chart's metrics-reader ClusterRole grants to the scraper. The operator
// already holds create on tokenreviews and subjectaccessreviews (the
// evidence agents' ClusterRole needs both, and RBAC escalation prevention
// makes the operator hold what it grants).
//
// certDir holds tls.crt and tls.key (a cert-manager or OpenShift
// service-CA Secret mounted by the chart); empty, controller-runtime
// generates a self-signed certificate at start, which a ServiceMonitor
// accepts with insecureSkipVerify — the bearer token still decides.
// HTTP/2 is off: a metrics endpoint gains nothing from it and it is the
// surface of the HTTP/2 rapid-reset and stream-exhaustion advisories.
func metricsOptions(bindAddress, certDir string) metricsserver.Options {
	return metricsserver.Options{
		BindAddress:    bindAddress,
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
		CertDir:        certDir,
		TLSOpts: []func(*tls.Config){func(c *tls.Config) {
			c.MinVersion = tls.VersionTLS12
			c.NextProtos = []string{"http/1.1"}
		}},
	}
}

// droppedGauge is pgtoolbox_watch_namespaces_dropped{reason}: set at start,
// then changed only by the background review (reReviewFailed), when a
// namespace of review-failed leaves it for a content cause. The zero value
// does nothing (a start with nothing registered).
type droppedGauge struct {
	vec *prometheus.GaugeVec
}

// move counts one namespace under to instead of from.
func (g droppedGauge) move(from, to dropCause) {
	if g.vec == nil || from == to {
		return
	}
	g.vec.WithLabelValues(string(from)).Dec()
	g.vec.WithLabelValues(string(to)).Inc()
}

// registerWatchMetrics registers the start-up's watch decisions on
// registry:
//
//   - pgtoolbox_watch_namespaces_dropped{reason}: watched namespaces left
//     out at start, per cause (a fixed set, every series exported, 0 when
//     none): clusters-unlistable, the operator may not list CloudNativePG
//     Clusters there (deleted since the install, or the operator not bound
//     in them); permissions, it lacks there another namespaced permission
//     of its manager role (a narrower Role bound in its place: an informer
//     refused there would never sync); review-failed, its permissions
//     there could not be reviewed (an API server error or a lost
//     connection, after the retries; reviewed again in the background);
//     review-too-large, its permission review answered more than
//     managerrole.ReviewResponseLimit; review-too-slow, it got no answer
//     in time at every attempt (neither reviewed again before the next
//     start). An optional API refused never leaves a namespace out. Fixed
//     for the life of the process, but for a review-failed namespace whose
//     background review turns too large or too slow: it moves to that
//     reason (the returned droppedGauge).
//   - pgtoolbox_optional_api_refused_namespaces{group,kind}: per optional
//     kind, the watched namespaces where it is refused while others allow
//     it; the feature is off in those namespaces only. A series is
//     exported for every optional kind (0 when none refuses it).
func registerWatchMetrics(registry prometheus.Registerer, dropped map[dropCause]int, kinds []optionalKind) (droppedGauge, error) {
	droppedVec := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_watch_namespaces_dropped",
		Help: "Watched namespaces left out at start, per reason: clusters-unlistable (CloudNativePG Clusters may not be listed there: deleted, or not bound), permissions (another permission of the manager role is missing there), review-failed (the permission review there failed with an API server error; reviewed again every minute), review-too-large (the permission review there answered too much), review-too-slow (the permission review there got no answer in time; it and review-too-large are reviewed again at the next start only).",
	}, []string{"reason"})
	for _, cause := range dropCauses {
		droppedVec.WithLabelValues(string(cause)).Set(float64(dropped[cause]))
	}
	refused := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_optional_api_refused_namespaces",
		Help: "Watched namespaces where an optional API is refused to the operator while others allow it: the feature is off there only.",
	}, []string{"group", "kind"})
	for _, kind := range kinds {
		gvk := kind.Result.GVK
		refused.WithLabelValues(gvk.Group, gvk.Kind).Set(float64(len(kind.Result.RefusedIn)))
	}
	if err := registry.Register(droppedVec); err != nil {
		return droppedGauge{}, fmt.Errorf("register pgtoolbox_watch_namespaces_dropped: %w", err)
	}
	if err := registry.Register(refused); err != nil {
		return droppedGauge{}, fmt.Errorf("register pgtoolbox_optional_api_refused_namespaces: %w", err)
	}
	return droppedGauge{droppedVec}, nil
}
