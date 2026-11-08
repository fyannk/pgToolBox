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
