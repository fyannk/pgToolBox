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

// Package diag runs pkg/diagnose in the background for every watched
// Cluster and publishes immutable snapshots:
// a run follows every change of a Cluster's inputs (debounced) and a
// safety cadence, never a request. Each published run carries the
// "state that lasts" clock: when the console first saw each finding in
// an unbroken series of runs.
package diag

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog"
	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog/cnpg"
)

// InputSource assembles a Cluster's diagnostics input from the published
// snapshots (internal/console/status implements it). Both methods are
// in-memory reads.
type InputSource interface {
	// Keys lists every watched Cluster.
	Keys(ctx context.Context) []cache.ClusterKey
	// Input returns the Cluster's input; false when it no longer exists.
	Input(ctx context.Context, key cache.ClusterKey) (diagnose.Input, bool)
}

// Changes is the cache's change feed.
type Changes interface {
	Subscribe() *cache.Subscription
	Unsubscribe(*cache.Subscription)
}

// Run is one published diagnostics run of one Cluster.
type Run struct {
	At         time.Time
	Generation uint64
	Input      diagnose.Input
	Result     diagnose.Result
	// Since maps a finding ID to the first run of the unbroken series of
	// runs that reported it.
	Since map[string]time.Time
	// Failed is set when the run panicked; Result is then empty and the
	// screen says the run failed rather than "nothing found".
	Failed string
}

// Options configures a Runner.
type Options struct {
	Interval time.Duration
	Debounce time.Duration
	// Disabled, when non-empty, is why the runner does not run.
	Disabled string
	Now      func() time.Time
	Logger   *slog.Logger
	// Rules overrides the catalog (tests).
	Rules []diagnose.Rule
	// Lifecycle is the end-of-life table in force; nil is the built-in one.
	Lifecycle *LifecycleTable
	// OnRun, when set, receives every published run, outside the
	// runner's lock (the incident timeline compares successive runs).
	OnRun func(key cache.ClusterKey, run Run)
	// Shared, when set, merges the findings' "since" with what the other
	// replicas, or this console before a restart, saw (the console's
	// state ConfigMap).
	Shared SharedFindings
	// engine overrides diagnose.Run (tests).
	engine func(diagnose.Input) diagnose.Result
}

// SharedFindings is the slice of the shared state the runner needs.
type SharedFindings interface {
	// FindingsSince returns, for each finding present in a run of the
	// Cluster, the earliest first-seen time known, and records the ones
	// no longer present as ended at at.
	FindingsSince(clusterUID string, present map[string]time.Time, at time.Time) map[string]time.Time
}

// Runner is the background diagnostics runner.
type Runner struct {
	src     InputSource
	changes Changes
	opts    Options

	mu   sync.RWMutex
	runs map[cache.ClusterKey]Run
	// failed mirrors the runs whose evaluation panicked, so State (asked
	// once per Cluster of a list) does not scan every run.
	failed     map[cache.ClusterKey]string
	generation uint64
	started    bool
}

// New wires a runner.
func New(src InputSource, changes Changes, opts Options) *Runner {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Rules == nil {
		opts.Rules = catalog.Rules()
	}
	if opts.Lifecycle == nil {
		opts.Lifecycle = NewLifecycleTable("", opts.Logger)
	}
	if opts.engine == nil {
		rules := opts.Rules
		opts.engine = func(in diagnose.Input) diagnose.Result { return diagnose.Run(in, rules...) }
	}
	return &Runner{src: src, changes: changes, opts: opts, runs: map[cache.ClusterKey]Run{}, failed: map[cache.ClusterKey]string{}}
}

// CatalogPins are the CNPG releases the rule catalog was verified
// against (pkg/diagnose/catalog/cnpg.VerifiedReleases).
func CatalogPins() []string { return append([]string(nil), cnpg.VerifiedReleases...) }

// Run drives the runner until ctx is done.
func (r *Runner) Run(ctx context.Context) error {
	if r.opts.Disabled != "" {
		<-ctx.Done()
		return nil
	}
	go func() {
		if err := r.opts.Lifecycle.Watch(ctx); err != nil {
			r.opts.Logger.Warn("lifecycle ConfigMap changes are not watched; restart the console after editing it", slog.String("error", err.Error()))
		}
	}()
	sub := r.changes.Subscribe()
	defer r.changes.Unsubscribe(sub)
	r.RunAll(ctx)
	ticker := time.NewTicker(r.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.RunAll(ctx)
		case <-sub.Ready():
			if r.opts.Debounce > 0 {
				timer := time.NewTimer(r.opts.Debounce)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
			}
			r.RunKeys(ctx, r.expand(ctx, sub.Drain()))
		}
	}
}

// expand turns namespace-wide keys into the namespace's Clusters.
func (r *Runner) expand(ctx context.Context, keys []cache.ClusterKey) []cache.ClusterKey {
	var all []cache.ClusterKey
	out := map[cache.ClusterKey]bool{}
	for _, key := range keys {
		if key.Name != "" {
			out[key] = true
			continue
		}
		if all == nil {
			all = r.src.Keys(ctx)
		}
		for _, k := range all {
			if k.Namespace == key.Namespace {
				out[k] = true
			}
		}
	}
	list := make([]cache.ClusterKey, 0, len(out))
	for key := range out {
		list = append(list, key)
	}
	return list
}

// RunAll runs every watched Cluster and forgets the ones gone.
func (r *Runner) RunAll(ctx context.Context) {
	keys := r.src.Keys(ctx)
	present := map[cache.ClusterKey]bool{}
	for _, key := range keys {
		present[key] = true
	}
	r.mu.Lock()
	for key := range r.runs {
		if !present[key] {
			r.dropRun(key)
		}
	}
	r.mu.Unlock()
	r.RunKeys(ctx, keys)
}

// RunKeys runs the given Clusters once each.
func (r *Runner) RunKeys(ctx context.Context, keys []cache.ClusterKey) {
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		r.runOne(ctx, key)
	}
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
}

func (r *Runner) runOne(ctx context.Context, key cache.ClusterKey) {
	in, ok := r.src.Input(ctx, key)
	if !ok {
		r.mu.Lock()
		r.dropRun(key)
		r.mu.Unlock()
		return
	}
	at := r.opts.Now().UTC()
	in.Lifecycle = r.opts.Lifecycle.Current()
	result, failed := r.evaluate(in)
	r.mu.Lock()
	previous, had := r.runs[key]
	r.generation++
	run := Run{At: at, Generation: r.generation, Input: in, Result: result, Since: map[string]time.Time{}, Failed: failed}
	for _, finding := range result.Findings {
		if had {
			if since, ok := previous.Since[finding.ID]; ok {
				run.Since[finding.ID] = since
				continue
			}
		}
		run.Since[finding.ID] = at
	}
	// A failed run found nothing because it did not run: the shared
	// clocks are left as they are rather than told every finding ended.
	if r.opts.Shared != nil && failed == "" {
		run.Since = r.opts.Shared.FindingsSince(string(in.Cluster.Cluster.UID), run.Since, at)
	}
	r.setRun(key, run)
	r.mu.Unlock()
	if r.opts.OnRun != nil {
		r.opts.OnRun(key, run)
	}
}

// setRun and dropRun change runs and failed together; r.mu is held.
func (r *Runner) setRun(key cache.ClusterKey, run Run) {
	r.runs[key] = run
	if run.Failed != "" {
		r.failed[key] = run.Failed
	} else {
		delete(r.failed, key)
	}
}

func (r *Runner) dropRun(key cache.ClusterKey) {
	delete(r.runs, key)
	delete(r.failed, key)
}

// RunPanicked is what a run that panicked says, wherever it is shown.
const RunPanicked = "the diagnostics run failed; the console's log has the details"

// evaluate runs the engine, turning a rule panic into a failed run so
// one bad rule never takes the console down.
func (r *Runner) evaluate(in diagnose.Input) (result diagnose.Result, failed string) {
	defer func() {
		if p := recover(); p != nil {
			// The panic value can carry anything the rule touched: it goes
			// to the log, and the run, its source state and the timeline
			// get a fixed sentence.
			failed = RunPanicked
			r.opts.Logger.Error("diagnostics run panicked", slog.String("panic", fmt.Sprint(p)))
			result = diagnose.Result{}
		}
	}()
	return r.opts.engine(in), ""
}

// Latest returns the latest run of a Cluster.
func (r *Runner) Latest(key cache.ClusterKey) (Run, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, ok := r.runs[key]
	return run, ok
}

// State is the runner as a source.
func (r *Runner) State() viewmodel.SourceState {
	if r.opts.Disabled != "" {
		return viewmodel.SourceState{Source: "diagnostics", State: viewmodel.SourceDisabled, Reason: r.opts.Disabled}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.started {
		return viewmodel.SourceState{Source: "diagnostics", State: viewmodel.SourcePending}
	}
	for _, reason := range r.failed {
		return viewmodel.SourceState{Source: "diagnostics", State: viewmodel.SourceUnavailable, Reason: reason}
	}
	return viewmodel.SourceState{Source: "diagnostics", State: viewmodel.SourceLive}
}
