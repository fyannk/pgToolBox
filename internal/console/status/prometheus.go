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

package status

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"k8s.io/client-go/transport"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
)

// PromRanger runs one PromQL range query about one namespace. The console
// builds every query itself (metrics.PromQL): no caller text ever reaches
// it.
type PromRanger interface {
	QueryRange(ctx context.Context, namespace, query string, start, end time.Time, step time.Duration) ([]metrics.PromSeries, error)
}

// PrometheusOptions configure the PromQL client (PROMETHEUS_URL and its
// companions). The console reads Prometheus with its own identity, never
// the caller's: no person's token leaves for a third party.
type PrometheusOptions struct {
	// URL is the base of the Prometheus HTTP API (the path before
	// /api/v1), such as https://thanos-querier.openshift-monitoring.svc:9092.
	URL string
	// TokenFile, when set, is sent as a bearer token on every query and
	// re-read as the kubelet rotates it (a projected ServiceAccount token).
	TokenFile string
	// CAFile verifies the server; empty uses the system roots.
	CAFile string
	// Namespaced adds namespace=<ns> to every query, as the tenancy
	// port of OpenShift's thanos-querier (kube-rbac-proxy) requires.
	Namespaced bool
}

// Bounds of one query: the answer's size and its time.
const (
	maxPromBody     = 4 << 20
	promTimeout     = 10 * time.Second
	promConcurrency = 6
	promCacheTTL    = 30 * time.Second
)

// PrometheusClient is the PromRanger over the Prometheus HTTP API.
type PrometheusClient struct {
	base       *url.URL
	client     *http.Client
	namespaced bool
}

// NewPrometheus validates o and builds the client. A token is only ever
// sent over https.
func NewPrometheus(o PrometheusOptions) (*PrometheusClient, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("prometheus: the URL must be an absolute http(s) URL without credentials, query or fragment")
	}
	if o.TokenFile != "" && u.Scheme != "https" {
		return nil, errors.New("prometheus: a token is only sent over https")
	}
	rt, err := transport.New(&transport.Config{
		BearerTokenFile: o.TokenFile,
		TLS:             transport.TLSConfig{CAFile: o.CAFile},
	})
	if err != nil {
		return nil, fmt.Errorf("prometheus transport: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return &PrometheusClient{
		base: u,
		client: &http.Client{
			Transport: rt, Timeout: promTimeout,
			// A redirect could carry the token elsewhere: refuse them.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		namespaced: o.Namespaced,
	}, nil
}

// promError is a failed query, worded without the URL, the query or any
// header.
type promError struct {
	state  viewmodel.SourceStateKind
	reason string
}

func (e *promError) Error() string { return e.reason }

// QueryRange implements PromRanger.
func (p *PrometheusClient) QueryRange(ctx context.Context, namespace, query string, start, end time.Time, step time.Duration) ([]metrics.PromSeries, error) {
	form := url.Values{}
	form.Set("query", query)
	form.Set("start", strconv.FormatInt(start.Unix(), 10))
	form.Set("end", strconv.FormatInt(end.Unix(), 10))
	form.Set("step", strconv.FormatInt(int64(step/time.Second), 10))
	if p.namespaced && namespace != "" {
		form.Set("namespace", namespace)
	}
	u := *p.base
	u.Path += "/api/v1/query_range"
	u.RawQuery = form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, &promError{state: viewmodel.SourceUnavailable, reason: "the query could not be built"}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &promError{state: viewmodel.SourceUnavailable, reason: "Prometheus did not answer in time"}
		}
		return nil, &promError{state: viewmodel.SourceUnavailable, reason: "Prometheus is unreachable from the console"}
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, &promError{state: viewmodel.SourceForbidden, reason: "Prometheus refused the console's identity (HTTP " + strconv.Itoa(resp.StatusCode) + ")"}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, &promError{state: viewmodel.SourceUnavailable, reason: "Prometheus answered with a redirect, which the console does not follow"}
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusUnprocessableEntity:
		return nil, &promError{state: viewmodel.SourceUnavailable, reason: "Prometheus answered HTTP " + strconv.Itoa(resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPromBody+1))
	if err != nil {
		return nil, &promError{state: viewmodel.SourceUnavailable, reason: "Prometheus's answer could not be read"}
	}
	if len(body) > maxPromBody {
		return nil, &promError{state: viewmodel.SourceUnavailable, reason: "Prometheus's answer exceeds the console's bound"}
	}
	series, err := metrics.ParseRange(body)
	if err != nil {
		return nil, &promError{state: viewmodel.SourceUnavailable, reason: "Prometheus's answer was refused: " + err.Error()}
	}
	return series, nil
}

// promCharts is the Prometheus half of the metrics screen, cached per
// Cluster for promCacheTTL so a page refresh does not re-run the whole
// catalogue.
type promCharts struct {
	ranger PromRanger
	rng    time.Duration
	now    func() time.Time

	mu     sync.Mutex
	cache  map[string]promEntry
	flight singleflight.Group
}

type promEntry struct {
	at     time.Time
	charts []viewmodel.MetricChart
	state  viewmodel.SourceState
	step   time.Duration
}

func newPromCharts(r PromRanger, rng time.Duration, now func() time.Time) *promCharts {
	if rng <= 0 {
		rng = time.Hour
	}
	return &promCharts{ranger: r, rng: rng, now: now, cache: map[string]promEntry{}}
}

// charts runs one query per catalogued series with a metric name, bounded
// in concurrency and time. The first failure decides the source state;
// the charts that did answer are kept, so one renamed metric does not
// blank the screen.
func (p *promCharts) charts(ctx context.Context, namespace, cluster string, groupTitle map[string]string) promEntry {
	key := namespace + "/" + cluster
	now := p.now().UTC()
	p.mu.Lock()
	if e, ok := p.cache[key]; ok && now.Sub(e.at) < promCacheTTL {
		p.mu.Unlock()
		return e
	}
	p.mu.Unlock()
	// One run per Cluster at a time: concurrent viewers share it instead
	// of multiplying the queries and the decoded answers.
	v, _, _ := p.flight.Do(key, func() (any, error) {
		return p.run(context.WithoutCancel(ctx), namespace, cluster, groupTitle, now), nil
	})
	e, _ := v.(promEntry)
	return e
}

func (p *promCharts) run(ctx context.Context, namespace, cluster string, groupTitle map[string]string, now time.Time) promEntry {
	key := namespace + "/" + cluster
	step := metrics.RangeStep(p.rng, 120, 15*time.Second)
	end := now.Truncate(step)
	start := end.Add(-p.rng).Truncate(step)
	defs := metrics.Instance.Series
	results := make([]*viewmodel.MetricChart, len(defs))
	errs := make([]error, len(defs))
	ctx, cancel := context.WithTimeout(ctx, promTimeout+2*time.Second)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(promConcurrency)
	for i, def := range defs {
		query, ok := metrics.PromQL(def, namespace, cluster, max(4*step, 5*time.Minute))
		if !ok {
			continue
		}
		g.Go(func() error {
			series, err := p.ranger.QueryRange(gctx, namespace, query, start, end, step)
			if err != nil {
				errs[i] = err
				return nil //nolint:nilerr // the error is kept in errs[i]; returning it would cancel the other series through the errgroup
			}
			if len(series) == 0 {
				return nil
			}
			times, lines := metrics.AlignRange(series, start, end, step)
			chart := &viewmodel.MetricChart{Key: def.Key, Label: def.Title, Group: groupTitle[def.Group], Unit: def.Unit, Note: bound(def.Note.Watch), Times: times}
			for _, s := range series {
				chart.Lines = append(chart.Lines, viewmodel.MetricLine{Instance: s.Pod, Values: lines[s.Pod]})
			}
			results[i] = chart
			return nil
		})
	}
	_ = g.Wait()
	e := promEntry{at: now, step: step, charts: []viewmodel.MetricChart{}, state: viewmodel.SourceState{Source: "prometheus", State: viewmodel.SourceLive}}
	for _, c := range results {
		if c != nil {
			e.charts = append(e.charts, *c)
		}
	}
	for _, err := range errs {
		if err == nil {
			continue
		}
		var pe *promError
		if errors.As(err, &pe) {
			e.state.State, e.state.Reason = pe.state, pe.reason
		} else {
			e.state.State, e.state.Reason = viewmodel.SourceUnavailable, "Prometheus could not be queried"
		}
		break
	}
	if e.state.State == viewmodel.SourceLive && len(e.charts) == 0 {
		e.state.State = viewmodel.SourceUnavailable
		e.state.Reason = "Prometheus answered but holds no series for this Cluster's instance Pods " +
			"(the scrape must keep the namespace and pod labels)"
	}
	if e.state.State != viewmodel.SourceLive {
		since := now
		e.state.Since = &since
	}
	p.mu.Lock()
	for k, v := range p.cache {
		if now.Sub(v.at) >= promCacheTTL {
			delete(p.cache, k)
		}
	}
	p.cache[key] = e
	p.mu.Unlock()
	return e
}
