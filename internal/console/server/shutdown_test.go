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
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunnersOutliveTheDrain: at SIGTERM the server keeps serving for the
// shutdown delay and then drains, and the runners that feed its streams
// (the cache, the invalidations) keep running all that time; they stop
// only once the server has returned, and the call waits for them.
func TestRunnersOutliveTheDrain(t *testing.T) {
	t.Parallel()
	ctx, signal := context.WithCancel(t.Context())
	var runnerDone atomic.Bool
	started := make(chan struct{})
	sup := NewSupervisor(slog.New(slog.DiscardHandler))
	sup.Add(Runner{Name: "invalidations", Core: true, Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond) // a runner that takes a moment to stop
		runnerDone.Store(true)
		return nil
	}})
	aliveDuringDrain := false
	serveErr := errors.New("served")
	serve := func(ctx context.Context) error {
		<-started
		signal() // SIGTERM while serving
		<-ctx.Done()
		// The shutdown delay and the drain: the streams are still open,
		// and so must their feed be.
		time.Sleep(20 * time.Millisecond)
		aliveDuringDrain = !runnerDone.Load() && sup.Check() == nil
		return serveErr
	}
	if err := serveThenStop(ctx, sup.Run, serve); !errors.Is(err, serveErr) {
		t.Fatalf("serveThenStop = %v, want the server's error", err)
	}
	if !aliveDuringDrain {
		t.Fatal("the runners stopped at the signal, before the drain ended: open streams would freeze")
	}
	if !runnerDone.Load() {
		t.Fatal("serveThenStop returned before the runners stopped")
	}
}

// TestRunnersStopWhenServingFails: a server that cannot listen returns at
// once, and the runners are stopped rather than left running.
func TestRunnersStopWhenServingFails(t *testing.T) {
	t.Parallel()
	stopped := make(chan struct{})
	runners := func(ctx context.Context) {
		<-ctx.Done()
		close(stopped)
	}
	listenErr := errors.New("listening on :8080: address already in use")
	if err := serveThenStop(t.Context(), runners, func(context.Context) error { return listenErr }); !errors.Is(err, listenErr) {
		t.Fatalf("serveThenStop = %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("the runners were not stopped")
	}
}
