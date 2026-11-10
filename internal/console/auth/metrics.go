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
	throttledRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pgtoolbox_console_throttled_requests_total",
		Help: "Requests refused by a budget, by bucket (a fixed set: the Bucket* names of internal/console/auth).",
	}, []string{"bucket"})
	revocationValid = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_revocation_list_valid",
		Help: "1 when the last revocation list read parsed, 0 when it was refused (the previous one stays in force).",
	})
)

func init() {
	crmetrics.Registry.MustRegister(logins, sessionRejections, tokenRefreshes, throttledRequests, revocationValid)
}

// The buckets of pgtoolbox_console_throttled_requests_total. The set is
// closed: a limiter is Named with one of these, never with anything a
// request carries, so the metric's cardinality is fixed.
const (
	// BucketSignInAddress: OAuth callbacks (and unrecognised MCP bearers)
	// per client address.
	BucketSignInAddress = "sign_in_address"
	// BucketTokenFormToken and BucketTokenFormAddress: the token form per
	// pasted token and per client address.
	BucketTokenFormToken   = "token_form_token"
	BucketTokenFormAddress = "token_form_address" // #nosec G101 -- a metric label, not a credential
	// BucketTokenReviews: SelfSubjectReviews of tokens the console has not
	// accepted before (pasted tokens, unknown raw bearers), per replica.
	BucketTokenReviews = "token_reviews"
	// BucketKnownReviews: re-reviews of raw bearers this replica accepted
	// before, per replica.
	BucketKnownReviews = "known_reviews"
	// BucketCodeExchanges: OAuth code exchanges with the IdP, per replica.
	BucketCodeExchanges = "code_exchanges"
	// BucketExchangeReviews: SelfSubjectReviews of tokens the IdP has just
	// issued, per replica.
	BucketExchangeReviews = "exchange_reviews"
	// MCP endpoint: calls per user, writes per user, confirmation pages.
	BucketMCPCalls       = "mcp_calls"
	BucketMCPAttempts    = "mcp_attempts"
	BucketMCPWrites      = "mcp_writes"
	BucketMCPPageViews   = "mcp_page_views"
	BucketMCPPageAnswers = "mcp_page_answers"
	// MCP authorization server.
	BucketOAuthRequests      = "oauth_requests"
	BucketOAuthRegistry      = "oauth_registry"
	BucketOAuthTokens        = "oauth_tokens"         // #nosec G101 -- a metric label, not a credential
	BucketOAuthUnknownTokens = "oauth_unknown_tokens" // #nosec G101 -- a metric label, not a credential
	BucketOAuthRegistrations = "oauth_registrations"
	BucketOAuthDecisions     = "oauth_decisions"
	// BucketMetricsScrapes: /metrics per client address.
	BucketMetricsScrapes = "metrics_scrapes"
)
