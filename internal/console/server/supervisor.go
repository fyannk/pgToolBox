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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Runner is one long-running part of the console: the informer cache,
// the status sweep, the diagnostics runner, the revocation-list watch.
type Runner struct {
	Name string
	// Core runners gate readiness: while one is down (exited and waiting
	// for its restart) the console reports not ready, because what it
	// would serve is no longer being kept current.
	Core bool
	Run  func(ctx context.Context) error
}

// Supervisor runs runners, logs every exit, and restarts them with
// exponential backoff. It never lets a runner's failure end
// the process: a console whose cache died should say so through readiness
// and recover, not crash-loop away its open sessions.
type Supervisor struct {
	log     *slog.Logger
	backoff func(failures int) time.Duration

	mu      sync.Mutex
	runners []Runner
	down    map[string]error
	started bool
	// restarts and last keep each runner's count and last exit error
	// for the admin page.
	restarts map[string]int
	last     map[string]string
}

// NewSupervisor returns an empty supervisor.
func NewSupervisor(log *slog.Logger) *Supervisor {
	return &Supervisor{
		log:  log,
		down: map[string]error{}, restarts: map[string]int{}, last: map[string]string{},
		backoff: func(failures int) time.Duration {
			return min(time.Second<<min(failures, 6), time.Minute)
		},
	}
}

// Add registers a runner; it must be called before Run.
func (s *Supervisor) Add(r Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		panic("server: Supervisor.Add after Run")
	}
	s.runners = append(s.runners, r)
}

// Run starts every runner and blocks until ctx ends and all have
// returned.
func (s *Supervisor) Run(ctx context.Context) {
	s.mu.Lock()
	s.started = true
	runners := append([]Runner(nil), s.runners...)
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range runners {
		wg.Go(func() { s.supervise(ctx, r) })
	}
	wg.Wait()
}

func (s *Supervisor) supervise(ctx context.Context, r Runner) {
	failures := 0
	for {
		start := time.Now()
		err := safeRun(ctx, r)
		if ctx.Err() != nil {
			s.log.Info("runner stopped", "runner", r.Name)
			return
		}
		if err == nil {
			err = errors.New("returned without error before shutdown")
		}
		// A runner that ran for a while before failing starts its backoff
		// over: the failure is new, not a crash loop.
		if time.Since(start) > 5*time.Minute {
			failures = 0
		}
		failures++
		runnerRestarts.WithLabelValues(r.Name).Inc()
		wait := s.backoff(failures)
		s.setDown(r.Name, err)
		s.log.Error("runner exited; restarting", "runner", r.Name, "core", r.Core, "error", err, "attempt", failures, "backoff", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		s.setDown(r.Name, nil)
	}
}

// safeRun turns a runner's panic into an error, so one bad runner
// restarts instead of taking the process down.
func safeRun(ctx context.Context, r Runner) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return r.Run(ctx)
}

func (s *Supervisor) setDown(name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.down, name)
		return
	}
	s.down[name] = err
	s.restarts[name]++
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	s.last[name] = msg
}

// States reports every runner: running or restarting, its restart count
// and its last exit error (bounded; runner errors carry no secret).
func (s *Supervisor) States() []viewmodel.RunnerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]viewmodel.RunnerState, 0, len(s.runners))
	for _, r := range s.runners {
		_, down := s.down[r.Name]
		out = append(out, viewmodel.RunnerState{Name: r.Name, Core: r.Core, Running: s.started && !down, Restarts: s.restarts[r.Name], LastError: s.last[r.Name]})
	}
	return out
}

// Check reports the core runners that are down, for readiness.
func (s *Supervisor) Check() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for _, r := range s.runners {
		if _, down := s.down[r.Name]; down && r.Core {
			names = append(names, r.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return fmt.Errorf("core runners down: %s", strings.Join(names, ", "))
}

var runnerRestarts = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "pgtoolbox_console_runner_restarts_total",
	Help: "Restarts of the console's background runners by runner name.",
}, []string{"runner"})

var (
	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_build_info",
		Help: "Always 1; the console's version and Go version as labels.",
	}, []string{"version", "goversion"})
	configInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_config_info",
		Help: "Always 1; one series per effective setting (secrets are named by file path, never by value).",
	}, []string{"name", "value"})
)

func init() { crmetrics.Registry.MustRegister(runnerRestarts, buildInfo, configInfo) }

// publishInfo sets pgtoolbox_console_build_info and pgtoolbox_console_config_info.
func publishInfo(version string, infos ...map[string]string) {
	if version == "" {
		version = "dev"
	}
	buildInfo.Reset()
	buildInfo.WithLabelValues(version, runtime.Version()).Set(1)
	configInfo.Reset()
	for _, info := range infos {
		for k, v := range info {
			configInfo.WithLabelValues(k, v).Set(1)
		}
	}
}
