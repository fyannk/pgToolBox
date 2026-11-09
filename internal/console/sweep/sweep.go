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

// Package sweep reads every watched instance's /pg/status — and its
// metrics exporter — on a bounded sweep:
// HTTPS only, TLS 1.3, the certificate verified against the admin-filled
// CA bundle and the Cluster's Service name. A Pod without
// --status-port-tls, a Cluster whose CA is not in the bundle, or a
// console with no bundle at all is an error row, never an unverified or
// plaintext read. Reads are bounded in time, size and
// concurrency; the last good report per pod and short in-memory rings
// (default one hour) give "since when" and the sparklines.
package sweep

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/scrape"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/volumetrend"
)

const (
	// statusPort and metricsPort are CloudNativePG's.
	statusPort  = "8000"
	metricsPort = "9187"
	// maxBody bounds one /pg/status report.
	maxBody = 1 << 20
	// maxMetricsBody bounds one exposition. A longer one is cut at its
	// last whole line and the cut is reported, never read as whole.
	maxMetricsBody = 2 << 20
	// maxPackMetricsBody bounds the exposition of a pod whose last read
	// reported the insight pack, or was cut: the pack adds up to about
	// 50 KiB per database, and a cut can hide its series. At the default
	// concurrency of 8 the bound is 64 MiB of transient buffers at worst,
	// and only while such pods are read.
	maxPackMetricsBody = 8 << 20
	// insightRefreshEvery bounds the on-demand reads of one Cluster's
	// primary exporter (the blocking screen), whoever asks.
	insightRefreshEvery = time.Second
	// maxPodsPerCluster bounds the roster of one Cluster; a larger roster
	// is swept up to the bound and reported incomplete.
	maxPodsPerCluster = 32
)

// Key names one Cluster.
type Key struct{ Namespace, Name string }

// PodTarget is one instance pod as the API server describes it.
type PodTarget struct {
	Name, UID, IP string
	// StatusPortTLS is cnpgstatus.StatusPortTLSForMinor: false means the
	// pod predates CNPG 1.24 and is never read.
	StatusPortTLS bool
	Deleting      bool
}

// Target is one Cluster to sweep.
type Target struct {
	Key Key
	// ClusterUID is the Cluster's UID: what a restored snapshot is matched
	// against, so a Cluster recreated under the same name inherits
	// nothing.
	ClusterUID string
	// ServerName is the name the status certificate must carry
	// (cnpgstatus.StatusServerName); empty means none can be verified.
	ServerName string
	// MetricsTLS is spec.monitoring.tls.enabled: the exporter then
	// serves HTTPS and is verified like the status port.
	MetricsTLS bool
	Pods       []PodTarget
}

// Roster lists the Clusters to sweep, from the informer cache.
type Roster interface {
	Targets(ctx context.Context) []Target
}

// Options configures a Sweeper.
type Options struct {
	Interval    time.Duration
	Timeout     time.Duration
	Concurrency int
	Retention   time.Duration
	Metrics     bool
	// Trends, when set, receives every fresh volume reading (the fork's
	// volumeUsages, else its cnpg_disk_* metrics) for the long trend
	// behind the storage forecasts.
	Trends *volumetrend.Store
	Bundle *CABundle
	Logger *slog.Logger
	Now    func() time.Time
	// StatusPort and MetricsPort override CNPG's ports (tests only).
	StatusPort, MetricsPort string
	// Disabled, when non-empty, is why the sweep does not run; every
	// instance is then a "disabled" row with this reason.
	Disabled string
	// Shared, when set, merges each instance's "since" with what the
	// other replicas observed (the console's state ConfigMap).
	Shared SharedClocks
}

// SharedClocks is the slice of the shared state the sweep needs.
type SharedClocks interface {
	// InstanceSince returns when the pod was first seen in state: the
	// earliest of local and what is shared for the same state.
	InstanceSince(podUID, state string, local time.Time) time.Time
}

// Instance is the sweep's account of one pod.
type Instance struct {
	State  viewmodel.StatusReadState
	Reason string
	// At is the last attempt; Since when State began.
	At, Since time.Time
	// Reading is the last good report of this pod (same UID), possibly
	// older than At when the latest read failed.
	Reading *instancestatus.Reading
	uid     string
}

// Snapshot is the published state of one Cluster.
type Snapshot struct {
	SweptAt   time.Time
	Interval  time.Duration
	Instances map[string]Instance
	// Incomplete is set when the roster was cut at its bound.
	Incomplete bool
}

// clusterState is the per-Cluster mutable state behind a Snapshot.
type clusterState struct {
	snap    Snapshot
	rings   map[string]map[string]*ring // instance → series → ring
	metrics *metrics.Store
	// insight holds the insight pack's readings and histories; target is
	// the last roster entry, which the on-demand read dials.
	insight *insight.Tracker
	target  Target
	// refreshedAt and refreshing rate-limit the on-demand read.
	refreshedAt time.Time
	refreshing  bool
	// largeBody names the pods read with maxPackMetricsBody: their last
	// exposition reported the pack, or was cut.
	largeBody map[string]bool
}

// Sweeper runs the sweep.
type Sweeper struct {
	opts   Options
	roster Roster
	logger *slog.Logger
	now    func() time.Time

	// reads bounds the exporter and status reads in flight, the sweep's
	// and the on-demand ones together (Options.Concurrency).
	reads chan struct{}

	mu       sync.RWMutex
	clusters map[Key]*clusterState
	started  bool
	// pending holds what a snapshot restored, until the Cluster it names
	// is swept (persist.go).
	pending   map[Key]PersistedCluster
	pendingAt time.Time
}

// New wires a sweeper; nothing runs until Run.
func New(roster Roster, opts Options) *Sweeper {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.StatusPort == "" {
		opts.StatusPort = statusPort
	}
	if opts.MetricsPort == "" {
		opts.MetricsPort = metricsPort
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if opts.Bundle == nil {
		opts.Bundle = NewCABundle("", opts.Logger)
	}
	return &Sweeper{
		opts: opts, roster: roster, logger: opts.Logger, now: opts.Now, clusters: map[Key]*clusterState{},
		reads: make(chan struct{}, opts.Concurrency),
	}
}

// Run sweeps every Interval until ctx is done, and watches the CA
// bundle for changes. A disabled sweeper publishes its reason once.
func (s *Sweeper) Run(ctx context.Context) error {
	if s.opts.Disabled != "" {
		s.SweepOnce(ctx)
		<-ctx.Done()
		return nil
	}
	go func() {
		if err := s.opts.Bundle.Watch(ctx); err != nil {
			s.logger.Warn("CA bundle changes are not watched; restart the console after editing it", slog.String("error", err.Error()))
		}
	}()
	ticker := time.NewTicker(s.opts.Interval)
	defer ticker.Stop()
	for {
		s.SweepOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// SweepOnce reads every target once, within one Interval. Exported for
// tests and for a sweep on demand.
func (s *Sweeper) SweepOnce(ctx context.Context) {
	targets := s.roster.Targets(ctx)
	at := s.now().UTC()
	bundle := s.opts.Bundle.Current()
	ctx, cancel := context.WithTimeout(ctx, s.opts.Interval)
	defer cancel()

	type result struct {
		key      Key
		pod      PodTarget
		state    viewmodel.StatusReadState
		reason   string
		reading  *instancestatus.Reading
		series   map[string]float64
		instants map[string]float64
		insight  *insight.Reading
	}
	results := make(chan result)
	var wg sync.WaitGroup
	incomplete := map[Key]bool{}
	for _, target := range targets {
		pods := target.Pods
		if len(pods) > maxPodsPerCluster {
			pods = pods[:maxPodsPerCluster]
			incomplete[target.Key] = true
		}
		for _, pod := range pods {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := result{key: target.Key, pod: pod}
				if state, reason, read := s.precheck(target, pod, bundle); !read {
					r.state, r.reason = state, reason
					results <- r
					return
				}
				select {
				case s.reads <- struct{}{}:
				case <-ctx.Done():
					r.state, r.reason = viewmodel.StatusReadUnreachable, ReasonNotReachable+": the sweep ran out of time"
					results <- r
					return
				}
				defer func() { <-s.reads }()
				tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: bundle.Pool, ServerName: target.ServerName}
				reading, err := s.readStatus(ctx, tlsConfig, pod.IP)
				r.state, r.reason = classify(err)
				if err == nil {
					r.reading = &reading
				}
				if s.opts.Metrics {
					r.series, r.instants, r.insight = s.readMetrics(ctx, target, tlsConfig, pod.IP, s.bodyLimit(target.Key, pod.Name))
				}
				results <- r
			}()
		}
	}
	go func() { wg.Wait(); close(results) }()

	fresh := map[Key][]result{}
	for r := range results {
		fresh[r.key] = append(fresh[r.key], r)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	s.expirePendingLocked(at)
	seen := map[Key]bool{}
	for _, target := range targets {
		seen[target.Key] = true
		cs := s.clusters[target.Key]
		if cs == nil {
			cs = s.newClusterState()
			s.restoreLocked(cs, target, at)
			s.clusters[target.Key] = cs
		}
		cs.target = target
		previous := cs.snap.Instances
		next := Snapshot{SweptAt: at, Interval: s.opts.Interval, Instances: map[string]Instance{}, Incomplete: incomplete[target.Key]}
		for _, r := range fresh[target.Key] {
			inst := Instance{State: r.state, Reason: r.reason, At: at, Since: at, uid: r.pod.UID}
			if prev, ok := previous[r.pod.Name]; ok && prev.uid == r.pod.UID {
				if prev.State == r.state && prev.Reason == r.reason {
					inst.Since = prev.Since
				}
				inst.Reading = prev.Reading
			}
			if s.opts.Shared != nil && r.pod.UID != "" {
				// The other replicas' clock, and the one from before a
				// restart: the earliest observation of this state wins.
				inst.Since = s.opts.Shared.InstanceSince(r.pod.UID, string(r.state)+"\x00"+r.reason, inst.Since)
			}
			if r.reading != nil {
				reading := *r.reading
				reading.Instance, reading.PodUID, reading.ObservedAt = r.pod.Name, r.pod.UID, at
				inst.Reading = &reading
			}
			next.Instances[r.pod.Name] = inst
			if cs.metrics != nil && (len(r.series) > 0 || len(r.instants) > 0) {
				cs.metrics.Observe(r.pod.Name, at, r.series, r.instants)
			}
			if cs.largeBody == nil {
				cs.largeBody = map[string]bool{}
			}
			if r.insight != nil {
				cs.largeBody[r.pod.Name] = r.insight.Present() || r.insight.ExpositionCut()
			}
			if cs.insight != nil && r.insight != nil {
				cs.insight.Observe(r.pod.Name, at, *r.insight, inst.Reading != nil && inst.Reading.IsPrimary)
			}
			feedTrends(s.opts.Trends, target.Key, r.pod.Name, at, r.reading, r.series, r.instants)
		}
		if cs.insight != nil {
			names := make(map[string]bool, len(next.Instances))
			for name := range next.Instances {
				names[name] = true
			}
			cs.insight.Retain(names)
		}
		s.pushRings(cs, next, at)
		cs.snap = next
	}
	for key := range s.clusters {
		if !seen[key] {
			delete(s.clusters, key)
		}
	}
}

func (s *Sweeper) newClusterState() *clusterState {
	cs := &clusterState{rings: map[string]map[string]*ring{}}
	if s.opts.Metrics {
		cs.metrics = metrics.NewStore(metrics.Instance, metrics.Limits{
			Interval: s.opts.Interval, RawWindow: s.opts.Retention, Retention: s.opts.Retention,
			RollupEvery: s.opts.Retention / 12, MaxInstances: maxPodsPerCluster,
		})
		cs.insight = insight.NewTracker()
	}
	return cs
}

// precheck decides, from the API server's facts alone, whether a pod may
// be read at all. It never opens a connection.
func (s *Sweeper) precheck(target Target, pod PodTarget, bundle Bundle) (viewmodel.StatusReadState, string, bool) {
	switch {
	case s.opts.Disabled != "":
		return viewmodel.StatusReadDisabled, s.opts.Disabled, false
	case pod.Deleting:
		return viewmodel.StatusReadNotRunning, ReasonDeleting, false
	case pod.IP == "":
		return viewmodel.StatusReadNotRunning, ReasonNoIP, false
	case !pod.StatusPortTLS:
		return viewmodel.StatusReadNoTLS, ReasonNoTLS, false
	case bundle.Pool == nil:
		return viewmodel.StatusReadNotVerifiable, "status port not verifiable: " + bundle.Problem, false
	case target.ServerName == "":
		return viewmodel.StatusReadNotVerifiable, "status port not verifiable: " + ReasonNoServerName, false
	}
	return "", "", true
}

// client opens a fresh connection per read. Keep-alives were measured
// and left off: at 4,800 instances the handshakes cost under 1% of the
// console's CPU (the whole sweep 1.2%), while pooling would hold one idle
// TLS connection, with its buffers, per instance between sweeps, and a
// pooled connection could outlive the pod behind its IP.
func (s *Sweeper) client(tlsConfig *tls.Config) *http.Client {
	return &http.Client{
		Timeout: s.opts.Timeout,
		Transport: &http.Transport{
			TLSClientConfig:   tlsConfig,
			DisableKeepAlives: true,
			Proxy:             nil, // pod IPs are never reached through a proxy
			DialContext:       (&net.Dialer{Timeout: s.opts.Timeout}).DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// readStatus performs one verified GET of /pg/status.
func (s *Sweeper) readStatus(ctx context.Context, tlsConfig *tls.Config, ip string) (instancestatus.Reading, error) {
	body, err := s.get(ctx, s.client(tlsConfig), "https://"+net.JoinHostPort(ip, s.opts.StatusPort)+"/pg/status")
	if err != nil {
		return instancestatus.Reading{}, err
	}
	return decodeStatus(body)
}

// decodeStatus is the trust-boundary parser of a /pg/status body
// (fuzzed): bounded input, bounded output, a typed error.
func decodeStatus(body []byte) (instancestatus.Reading, error) {
	if len(body) > maxBody {
		body = body[:maxBody]
	}
	reading, err := instancestatus.Parse(bytes.NewReader(body))
	if err != nil {
		return instancestatus.Reading{}, errDecode{err}
	}
	return reading, nil
}

// readMetrics scrapes the exporter once for both the catalog and the
// insight pack; failures leave a gap, they are not an instance error
// (the status read is the row's verdict).
func (s *Sweeper) readMetrics(ctx context.Context, target Target, tlsConfig *tls.Config, ip string, limit int) (series, instants map[string]float64, pack *insight.Reading) {
	body, truncated, err := s.exposition(ctx, target, tlsConfig, ip, limit)
	if err != nil {
		return nil, nil, nil
	}
	series, instants, err = scrape.ParseCatalog(metrics.Instance, bytes.NewReader(body))
	if err != nil {
		return nil, nil, nil
	}
	if truncated {
		dropCutAggregates(metrics.Instance, series, instants)
	}
	return series, instants, parseInsight(body, truncated)
}

// dropCutAggregates removes the entries a cut exposition can only state
// wrong. A sum over the label sets that came before the cut is
// understated by whatever followed it, and a counter read low once
// looks like a reset to everything that charts its rate, whatever its
// fold: such entries are left out (a gap) rather than recorded wrong. A
// gauge folded by Max keeps the largest value read, a lower bound that
// the next whole read corrects without inventing a reset.
func dropCutAggregates(catalog metrics.Catalog, series, instants map[string]float64) {
	for _, def := range catalog.Series {
		if def.Aggregate == metrics.Sum || def.Kind == metrics.Counter {
			delete(series, def.Key)
		}
	}
	for _, def := range catalog.Instants {
		if def.Aggregate == metrics.Sum {
			delete(instants, def.Key)
		}
	}
}

// bodyLimit is the exposition bound of one pod: maxPackMetricsBody when
// its last read reported the pack or was cut, maxMetricsBody otherwise.
func (s *Sweeper) bodyLimit(key Key, pod string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cs := s.clusters[key]; cs != nil && cs.largeBody[pod] {
		return maxPackMetricsBody
	}
	return maxMetricsBody
}

// exposition GETs one exporter's /metrics, bounded by limit; a longer
// body is cut at its last whole line and truncated reports the cut.
func (s *Sweeper) exposition(ctx context.Context, target Target, tlsConfig *tls.Config, ip string, limit int) (body []byte, truncated bool, err error) {
	scheme := "http"
	if target.MetricsTLS {
		scheme = "https"
	}
	body, err = s.getLimited(ctx, s.client(tlsConfig), scheme+"://"+net.JoinHostPort(ip, s.opts.MetricsPort)+"/metrics", int64(limit)+1)
	if err != nil {
		return nil, false, err
	}
	if len(body) > limit {
		return cutAtLine(body[:limit]), true, nil
	}
	return body, false, nil
}

// cutAtLine drops what follows the last newline: a sample cut in the
// middle could read as another value (1234 cut to 12).
func cutAtLine(body []byte) []byte {
	if i := bytes.LastIndexByte(body, '\n'); i >= 0 {
		return body[:i+1]
	}
	return body[:0]
}

// parseInsight reads the insight pack out of an exposition; nil when the
// body carries none of it.
func parseInsight(body []byte, truncated bool) *insight.Reading {
	reading, err := insight.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	if truncated {
		reading.Truncated = append(reading.Truncated, insight.TruncatedExposition)
	}
	return &reading
}

// get reads a /pg/status body of at most maxBody bytes. One byte more is
// read so that a larger report is named as such: cut at the bound it
// would read as a parse failure, or worse parse as a different report.
func (s *Sweeper) get(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	body, err := s.getLimited(ctx, c, url, maxBody+1)
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, errTooLarge{}
	}
	return body, nil
}

func (s *Sweeper) getLimited(ctx context.Context, c *http.Client, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errBadStatus{resp.StatusCode}
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// pushRings appends one sample per series and instance; a failed read is
// a gap, so the sparkline shows the hole instead of interpolating.
func (s *Sweeper) pushRings(cs *clusterState, snap Snapshot, at time.Time) {
	var primaryLSN string
	for _, inst := range snap.Instances {
		if inst.State == viewmodel.StatusReadOK && inst.Reading != nil && inst.Reading.IsPrimary {
			primaryLSN = inst.Reading.CurrentLSN
		}
	}
	for name, inst := range snap.Instances {
		rings := cs.rings[name]
		if rings == nil {
			rings = map[string]*ring{SeriesLagBytes: {}, SeriesReadyWALFiles: {}}
			cs.rings[name] = rings
		}
		var lag, ready *float64
		if inst.State == viewmodel.StatusReadOK && inst.Reading != nil {
			files := float64(inst.Reading.ReadyWALFiles)
			ready = &files
			if !inst.Reading.IsPrimary {
				if bytes, ok := LagBytes(primaryLSN, inst.Reading.ReplayLSN); ok {
					v := float64(bytes)
					lag = &v
				}
			}
		}
		rings[SeriesLagBytes].push(at, lag, s.opts.Retention)
		rings[SeriesReadyWALFiles].push(at, ready, s.opts.Retention)
	}
	for name := range cs.rings {
		if _, ok := snap.Instances[name]; !ok {
			delete(cs.rings, name)
		}
	}
}

// Snapshot returns the published state of one Cluster.
func (s *Sweeper) Snapshot(key Key) (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, ok := s.clusters[key]
	if !ok {
		return Snapshot{}, false
	}
	return cs.snap, true
}

// Series returns the rings of one instance, sorted by key; empty rings
// are omitted.
func (s *Sweeper) Series(key Key, instance string) []viewmodel.Series {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, ok := s.clusters[key]
	if !ok {
		return nil
	}
	var out []viewmodel.Series
	for name, r := range cs.rings[instance] {
		if len(r.samples) == 0 {
			continue
		}
		out = append(out, viewmodel.Series{Key: name, Unit: seriesUnits[name], Points: r.points()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Metrics is the Cluster's metrics window (diagnose.MetricsWindow); nil
// when metrics are off or the Cluster was never swept.
func (s *Sweeper) Metrics(key Key) *metrics.Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cs, ok := s.clusters[key]; ok {
		return cs.metrics
	}
	return nil
}

// InstanceStatus is the Cluster's sweep in the shape pkg/diagnose reads
// (diagnose.InstanceStatusSource).
func (s *Sweeper) InstanceStatus(key Key) StatusSource { return StatusSource{s: s, key: key} }

// StatusSource adapts one Cluster's snapshot to
// diagnose.InstanceStatusSource.
type StatusSource struct {
	s   *Sweeper
	key Key
}

// CurrentInstanceStatus returns the snapshot and whether one was swept.
func (src StatusSource) CurrentInstanceStatus() (instancestatus.Snapshot, bool) {
	if src.s == nil {
		// A source not attached to a sweep has swept nothing.
		return instancestatus.Snapshot{}, false
	}
	if src.s.opts.Disabled != "" {
		return instancestatus.Snapshot{Interval: src.s.opts.Interval, Disabled: src.s.opts.Disabled, Readings: map[string]instancestatus.Reading{}, Failing: map[string]string{}}, true
	}
	snap, ok := src.s.Snapshot(src.key)
	if !ok {
		return instancestatus.Snapshot{}, false
	}
	out := instancestatus.Snapshot{SweptAt: snap.SweptAt, Interval: snap.Interval, Readings: map[string]instancestatus.Reading{}, Failing: map[string]string{}}
	for name, inst := range snap.Instances {
		if inst.Reading != nil {
			out.Readings[name] = *inst.Reading
		}
		if inst.State != viewmodel.StatusReadOK {
			out.Failing[name] = inst.Reason
		}
	}
	if snap.Incomplete {
		out.Failing["(roster)"] = "more pods than the roster bound, so the sweep is incomplete"
	}
	return out, true
}

// State is the sweep as a source.
func (s *Sweeper) State() viewmodel.SourceState {
	if s.opts.Disabled != "" {
		return viewmodel.SourceState{Source: "instanceStatus", State: viewmodel.SourceDisabled, Reason: s.opts.Disabled}
	}
	s.mu.RLock()
	started := s.started
	s.mu.RUnlock()
	if !started {
		return viewmodel.SourceState{Source: "instanceStatus", State: viewmodel.SourcePending}
	}
	if problem := s.opts.Bundle.Current().Problem; problem != "" {
		return viewmodel.SourceState{Source: "instanceStatus", State: viewmodel.SourceUnavailable, Reason: problem}
	}
	return viewmodel.SourceState{Source: "instanceStatus", State: viewmodel.SourceLive}
}

// Insight returns the Cluster's insight snapshot at the sweep's clock;
// ok is false when metrics are off or the Cluster was never swept.
func (s *Sweeper) Insight(key Key) (insight.Snapshot, bool) {
	s.mu.RLock()
	cs, ok := s.clusters[key]
	s.mu.RUnlock()
	if !ok || cs.insight == nil {
		return insight.Snapshot{}, false
	}
	return cs.insight.Snapshot(s.now().UTC()), true
}

// ErrNoPrimary is returned by RefreshInsight when the sweep knows no
// readable primary for the Cluster.
var ErrNoPrimary = errors.New("no primary instance with a readable exporter")

// ErrReadsBusy is returned by RefreshInsight when every exporter read
// slot stayed taken for refreshWait.
var ErrReadsBusy = errors.New("the console's exporter reads are all in use")

// refreshWait is how long an on-demand read waits for a read slot.
const refreshWait = 2 * time.Second

// RefreshInsight re-reads the insight pack from the Cluster's primary
// exporter, for a screen that needs fresher lock data than the sweep's
// cadence. It reads at most once per insightRefreshEvery per Cluster,
// whoever asks, through the same verified TLS client and trust rules as
// the sweep, and takes one of the sweep's read slots (Concurrency), so
// on-demand reads across all Clusters never add to the sweep's; a call
// inside that second, or while a read is running, returns at once with
// the time of the last read. Nothing is read when metrics or the sweep
// are off.
//
// What a read costs the exporter: it answers from its cache, and runs
// the custom queries again only when the cache is older than the
// Cluster's metricsQueriesTTL. With metricsQueriesTTL 0 every read runs
// them all, so each refresh is one run of the pack on the primary.
func (s *Sweeper) RefreshInsight(ctx context.Context, key Key) (time.Time, error) {
	if s.opts.Disabled != "" || !s.opts.Metrics {
		return time.Time{}, ErrNoPrimary
	}
	now := s.now().UTC()
	s.mu.Lock()
	cs, ok := s.clusters[key]
	if !ok || cs.insight == nil {
		s.mu.Unlock()
		return time.Time{}, ErrNoPrimary
	}
	if cs.refreshing || now.Sub(cs.refreshedAt) < insightRefreshEvery {
		at := cs.refreshedAt
		s.mu.Unlock()
		return at, nil
	}
	var pod PodTarget
	var found bool
	for name, inst := range cs.snap.Instances {
		if inst.State != viewmodel.StatusReadOK || inst.Reading == nil || !inst.Reading.IsPrimary {
			continue
		}
		for _, p := range cs.target.Pods {
			if p.Name == name && p.UID == inst.uid {
				pod, found = p, true
			}
		}
	}
	target := cs.target
	if !found {
		s.mu.Unlock()
		return time.Time{}, ErrNoPrimary
	}
	cs.refreshing, cs.refreshedAt = true, now
	tracker := cs.insight
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		cs.refreshing = false
		s.mu.Unlock()
	}()

	bundle := s.opts.Bundle.Current()
	if _, reason, read := s.precheck(target, pod, bundle); !read {
		return now, errors.New(reason)
	}
	wait, cancel := context.WithTimeout(ctx, refreshWait)
	defer cancel()
	select {
	case s.reads <- struct{}{}:
	case <-wait.Done():
		return now, ErrReadsBusy
	}
	defer func() { <-s.reads }()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: bundle.Pool, ServerName: target.ServerName}
	body, truncated, err := s.exposition(ctx, target, tlsConfig, pod.IP, s.bodyLimit(key, pod.Name))
	if err != nil {
		_, reason := classify(err)
		return now, errors.New(reason)
	}
	if reading := parseInsight(body, truncated); reading != nil {
		tracker.Observe(pod.Name, now, *reading, true)
	}
	return now, nil
}
