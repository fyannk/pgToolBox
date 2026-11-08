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

package auth

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics of the sign-in layer, served on the console's /metrics. Labels
// are bounded: provider names and fixed outcomes, never a user.
var (
	logins = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_logins_total",
		Help: "Sign-in attempts by provider and outcome (success, failure).",
	}, []string{"provider", "outcome"})
	sessionRejections = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_session_rejections_total",
		Help: "Requests whose session was refused, by reason (expired, revoked, token_rejected, malformed).",
	}, []string{"reason"})
	tokenRefreshes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_token_refreshes_total",
		Help: "OIDC token refreshes by outcome (success, failure).",
	}, []string{"outcome"})
	revocationValid = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_revocation_list_valid",
		Help: "1 when the last revocation list read parsed, 0 when it was refused (the previous one stays in force).",
	})
)

func init() {
	crmetrics.Registry.MustRegister(logins, sessionRejections, tokenRefreshes, revocationValid)
}
