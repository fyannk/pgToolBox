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

package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
)

// MetricsFilter protects /metrics with the API server's authentication
// and authorization, like controller-runtime's
// filters.WithAuthenticationAndAuthorization, with one
// difference: the scraper's token is authenticated by a SelfSubjectReview
// sent with that token rather than by a TokenReview sent by the console.
// A TokenReview would need create on tokenreviews, a cluster-scoped
// permission the console is not given; the API server's
// answer is the same. The scraper then needs get on the non-resource URL
// /metrics (the chart's metrics-reader ClusterRole), checked with a
// SelfSubjectAccessReview sent with the scraper's own token.
//
// The probe listener it is served on is never behind a reverse proxy
// (Prometheus scrapes the Pod), so X-Forwarded-For is never read here: the
// per-address budget is the scraper's own address.
func MetricsFilter(identity auth.IdentityReviewer, az *authz.Authorizer) metricsserver.Filter {
	// A scrape every few seconds is plenty; beyond that each request
	// would cost the API server a review.
	limiter := auth.NewRateLimiter(60, 10, 1000).Named(auth.BucketMetricsScrapes)
	return func(log logr.Logger, h http.Handler) (http.Handler, error) {
		if identity == nil || az == nil {
			return nil, errors.New("metrics filter needs an identity reviewer and an authorizer")
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.Allow(auth.ClientKey(r, auth.ProxyTrust{})) {
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
			tok, ok := bearer(r.Header.Get("Authorization"))
			if !ok {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			u, err := identity.Review(r.Context(), tok)
			if errors.Is(err, auth.ErrTokenRejected) {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			if err != nil {
				log.Error(err, "authenticating a metrics scrape failed")
				http.Error(w, "Authentication unavailable", http.StatusServiceUnavailable)
				return
			}
			d := az.Allowed(auth.WithUserToken(r.Context(), tok), u, authz.Attributes{Verb: strings.ToLower(r.Method), Path: r.URL.Path})
			if d.Err != nil {
				log.Error(d.Err, "authorizing a metrics scrape failed")
				http.Error(w, "Authorization unavailable", http.StatusServiceUnavailable)
				return
			}
			if !d.Allowed {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			h.ServeHTTP(w, r)
		}), nil
	}
}

func bearer(h string) (string, bool) {
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

// MetricsHandler serves the controller-runtime registry, where every
// console metric is registered, behind MetricsFilter.
func MetricsHandler(identity auth.IdentityReviewer, az *authz.Authorizer, log logr.Logger) (http.Handler, error) {
	return MetricsFilter(identity, az)(log, promhttp.HandlerFor(crmetrics.Registry, promhttp.HandlerOpts{}))
}
