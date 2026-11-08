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

package sse

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

type recorder struct {
	mu      sync.Mutex
	items   []string
	reasons []string
	failAt  int
	notify  chan struct{}
}

func newRecorder() *recorder { return &recorder{notify: make(chan struct{}, 1024)} }

func (r *recorder) add(s string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failAt > 0 && len(r.items)+1 >= r.failAt {
		return errors.New("broken pipe")
	}
	r.items = append(r.items, s)
	r.notify <- struct{}{}
	return nil
}

func (r *recorder) Event(data any) error {
	if rs, ok := data.(Resync); ok {
		r.mu.Lock()
		r.reasons = append(r.reasons, rs.Reason)
		r.mu.Unlock()
	}
	return r.add(fmt.Sprintf("%T", data))
}
func (r *recorder) Comment(string) error { return r.add(":") }

// resyncReasons are the reasons of the resync events written so far.
func (r *recorder) resyncReasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reasons...)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.items...)
}

func (r *recorder) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		for _, it := range r.snapshot() {
			if it == want {
				return
			}
		}
		select {
		case <-r.notify:
		case <-deadline:
			t.Fatalf("no %s in %v", want, r.snapshot())
		}
	}
}

func run(t *testing.T, b *Broker, rec *recorder, o Options) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	sub := b.Subscribe()
	go func() { done <- Run(ctx, sub, rec, o) }()
	return cancel, done
}

func TestHelloHeartbeatAndInvalidate(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	rec := newRecorder()
	cancel, done := run(t, b, rec, Options{Heartbeat: 20 * time.Millisecond})
	rec.waitFor(t, "sse.Hello")
	rec.waitFor(t, ":")
	b.Publish(Invalidate{Topic: TopicCluster, Namespace: "a", Name: "x"})
	rec.waitFor(t, "sse.Invalidate")
	if got := rec.snapshot()[0]; got != "sse.Hello" {
		t.Fatalf("first item = %s, want the hello event", got)
	}
	if b.Clients() != 1 {
		t.Fatalf("clients = %d", b.Clients())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if b.Clients() != 0 {
		t.Fatal("the subscription outlived the stream")
	}
}

func TestReauthThenEnd(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	rec := newRecorder()
	now := time.Now()
	_, done := run(t, b, rec, Options{Heartbeat: time.Hour, ReauthAt: now.Add(20 * time.Millisecond), EndAt: now.Add(60 * time.Millisecond), Refreshable: true})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end with the session")
	}
	got := rec.snapshot()
	if len(got) != 3 || got[1] != "sse.Reauth" || got[2] != "sse.Resync" {
		t.Fatalf("items = %v, want hello, reauth, then the last resync", got)
	}
	if r := rec.resyncReasons(); len(r) != 1 || r[0] != ReasonSessionExpired {
		t.Fatalf("resync reasons = %q, want %q", r, ReasonSessionExpired)
	}
}

func TestVisibilityFilter(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	rec := newRecorder()
	cancel, done := run(t, b, rec, Options{Visible: func(_ context.Context, ev Invalidate) (bool, error) { return ev.Name != "secret", nil }})
	rec.waitFor(t, "sse.Hello")
	b.Publish(Invalidate{Topic: TopicCluster, Namespace: "a", Name: "secret"})
	b.Publish(Invalidate{Topic: TopicCluster, Namespace: "a", Name: "open"})
	rec.waitFor(t, "sse.Invalidate")
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
	n := 0
	for _, it := range rec.snapshot() {
		if it == "sse.Invalidate" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("delivered %d invalidations, want only the visible one", n)
	}
}

func TestSlowClientGetsResync(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	sub := b.Subscribe()
	for range subscriberBuffer + 5 {
		b.Publish(Invalidate{Topic: TopicClusters})
	}
	rec := newRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, sub, rec, Options{}) }()
	rec.waitFor(t, "sse.Resync")
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

func TestBrokerCloseEndsStreams(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	rec := newRecorder()
	_, done := run(t, b, rec, Options{})
	rec.waitFor(t, "sse.Hello")
	b.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not end the stream")
	}
	if s := b.Subscribe(); s == nil || b.Clients() != 0 {
		t.Fatal("a closed broker accepted a subscriber")
	}
}

func TestWriteErrorEndsStream(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	rec := newRecorder()
	rec.failAt = 2
	_, done := run(t, b, rec, Options{Heartbeat: 10 * time.Millisecond})
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a failed write must end the stream with its error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream kept running after a failed write")
	}
}

func TestRecheckEndsStream(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	rec := newRecorder()
	var mu sync.Mutex
	valid := true
	_, done := run(t, b, rec, Options{Heartbeat: 10 * time.Millisecond, Recheck: func(context.Context) bool {
		mu.Lock()
		defer mu.Unlock()
		return valid
	}})
	rec.waitFor(t, ":")
	mu.Lock()
	valid = false
	mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked session kept its stream")
	}
	got := rec.snapshot()
	if got[len(got)-1] != "sse.Resync" {
		t.Fatalf("items = %v, want the last resync before the close", got)
	}
	if r := rec.resyncReasons(); len(r) != 1 || r[0] != ReasonSessionEnded {
		t.Fatalf("resync reasons = %q, want %q", r, ReasonSessionEnded)
	}
}

// An event whose visibility cannot be decided (the reviewer failed or
// timed out) is replaced by a resync, never dropped silently, and never
// written itself. A burst of them costs one resync at once and one more
// at the next heartbeat, not one per event.
func TestUndecidedVisibilitySendsResync(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	rec := newRecorder()
	cancel, done := run(t, b, rec, Options{
		Heartbeat: 100 * time.Millisecond,
		Visible: func(_ context.Context, ev Invalidate) (bool, error) {
			if ev.Name == "undecided" {
				return false, errors.New("SubjectAccessReview: context deadline exceeded")
			}
			return true, nil
		},
	})
	rec.waitFor(t, "sse.Hello")
	b.Publish(Invalidate{Topic: TopicCluster, Namespace: "a", Name: "undecided"})
	rec.waitFor(t, "sse.Resync")
	b.Publish(Invalidate{Topic: TopicCluster, Namespace: "a", Name: "undecided"})
	b.Publish(Invalidate{Topic: TopicCluster, Namespace: "a", Name: "undecided"})
	b.Publish(Invalidate{Topic: TopicCluster, Namespace: "a", Name: "open"})
	rec.waitFor(t, "sse.Invalidate")
	// The folded resync goes out with the next heartbeat.
	deadline := time.After(5 * time.Second)
	for len(rec.resyncReasons()) < 2 {
		select {
		case <-rec.notify:
		case <-deadline:
			t.Fatalf("the folded resync never came: %v", rec.snapshot())
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Three undecided events: one resync at once, then at most one per
	// heartbeat interval (whether the second and third fall in one
	// interval depends on the scheduler; TestUndecidedPacing pins it).
	reasons := rec.resyncReasons()
	if len(reasons) < 2 || len(reasons) > 3 {
		t.Fatalf("resync reasons = %q, want two or three for three undecided events", reasons)
	}
	for _, r := range reasons {
		if r != ReasonUndecided {
			t.Errorf("resync reason = %q, want %q", r, ReasonUndecided)
		}
	}
	n := 0
	for _, it := range rec.snapshot() {
		if it == "sse.Invalidate" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("delivered %d invalidations, want only the decided one", n)
	}
}

// The pacing of undecided resyncs, step by step: "e" an undecided event,
// "t" a heartbeat; the result is the number of resyncs after each step.
func TestUndecidedPacing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		steps string
		want  []int
	}{
		"one event resyncs at once":           {"e", []int{1}},
		"a burst folds into the next tick":    {"eeet", []int{1, 1, 1, 2}},
		"a quiet tick sends nothing":          {"ett", []int{1, 1, 1}},
		"a folded resync covers the interval": {"eeteet", []int{1, 1, 2, 2, 2, 3}},
		"after a quiet interval, at once":     {"ette", []int{1, 1, 1, 2}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder()
			var u undecided
			got := make([]int, 0, len(tc.steps))
			for _, s := range tc.steps {
				var err error
				switch s {
				case 'e':
					err = u.event(rec)
				case 't':
					err = u.tick(rec)
				default:
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, len(rec.resyncReasons()))
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("resyncs after each step = %v, want %v", got, tc.want)
			}
		})
	}
}

// A check that finds the subscriber's token no longer accepted (an access
// token that expired and was not refreshed) ends the stream with the
// session-expired resync.
func TestTokenRejectedEndsStream(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	rec := newRecorder()
	_, done := run(t, b, rec, Options{
		Heartbeat: time.Hour,
		Visible: func(context.Context, Invalidate) (bool, error) {
			return false, fmt.Errorf("%w: the API server does not accept this token", ErrSessionEnded)
		},
	})
	rec.waitFor(t, "sse.Hello")
	b.Publish(Invalidate{Topic: TopicCluster, Namespace: "a", Name: "x"})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a rejected token kept its stream")
	}
	if got := rec.snapshot(); len(got) != 2 || got[1] != "sse.Resync" {
		t.Fatalf("items = %v, want hello then the last resync", got)
	}
	if r := rec.resyncReasons(); len(r) != 1 || r[0] != ReasonSessionExpired {
		t.Fatalf("resync reasons = %q, want %q", r, ReasonSessionExpired)
	}
}

// A check that failed because the client went away writes nothing more.
func TestCanceledCheckWritesNothing(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	rec := newRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	sub := b.Subscribe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, sub, rec, Options{Heartbeat: time.Hour, Visible: func(context.Context, Invalidate) (bool, error) {
			cancel()
			return false, context.Canceled
		}})
	}()
	rec.waitFor(t, "sse.Hello")
	b.Publish(Invalidate{Topic: TopicCluster, Namespace: "a", Name: "x"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("items = %v, want only the hello", got)
	}
}

// orderSender records invalidations by name and heartbeats as ":".
type orderSender struct {
	mu    sync.Mutex
	items []string
}

func (s *orderSender) Event(data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev, ok := data.(Invalidate); ok {
		s.items = append(s.items, ev.Name)
	}
	return nil
}

func (s *orderSender) Comment(string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, ":")
	return nil
}

func (s *orderSender) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.items...)
}

// TestHeartbeatsFlowWhileChecksRun is the regression test of the stalled
// stream: slow visibility checks (cold SAR caches behind a burst of
// events) must not keep the heartbeat from being written, or the client
// declares the stream dead and reconnects. Events still go out in order,
// each only after its own check allowed it.
func TestHeartbeatsFlowWhileChecksRun(t *testing.T) {
	t.Parallel()
	b := NewBroker()
	send := &orderSender{}
	ctx, cancel := context.WithCancel(t.Context())
	sub := b.Subscribe()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, sub, send, Options{Heartbeat: 20 * time.Millisecond, Visible: func(ctx context.Context, ev Invalidate) (bool, error) {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return false, ctx.Err()
			}
			return ev.Name != "hidden", nil
		}})
	}()
	names := []string{"e0", "e1", "hidden", "e2", "e3"}
	for _, n := range names {
		b.Publish(Invalidate{Topic: TopicCluster, Namespace: "a", Name: n})
	}
	deadline := time.After(5 * time.Second)
	for {
		got := send.snapshot()
		if slices.Contains(got, "e3") {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("not every event was delivered: %v", got)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := send.snapshot()
	var events []string
	beatsBefore := 0
	for _, it := range got {
		if it == ":" {
			if !slices.Contains(events, "e3") {
				beatsBefore++
			}
			continue
		}
		events = append(events, it)
	}
	if want := []string{"e0", "e1", "e2", "e3"}; !slices.Equal(events, want) {
		t.Errorf("events = %v, want %v in order and without the hidden one", events, want)
	}
	// Five checks of 100 ms take 500 ms: at a 20 ms interval the stream
	// must have kept beating during them, not only once at the end.
	if beatsBefore < 5 {
		t.Errorf("only %d heartbeats while the checks ran: %v", beatsBefore, got)
	}
}
