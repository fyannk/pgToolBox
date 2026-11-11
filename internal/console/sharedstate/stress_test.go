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

package sharedstate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// reordered is the API server with watches that deliver each event after
// a random delay of up to 4 ms, so out of order, and Gets whose answer
// takes up to 2 ms to arrive, so that the watch can overtake them: what a
// busy API server and three replicas' concurrent writes look like to one
// replica.
type reordered struct{ *apiServer }

func (r reordered) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error) {
	cm, err := r.apiServer.Get(ctx, name, opts)
	time.Sleep(time.Duration(rand.IntN(2000)) * time.Microsecond)
	return cm, err
}

type delayedWatch struct {
	in   watch.Interface
	out  chan watch.Event
	stop chan struct{}
	once sync.Once
}

func (d *delayedWatch) ResultChan() <-chan watch.Event { return d.out }
func (d *delayedWatch) Stop()                          { d.once.Do(func() { close(d.stop); d.in.Stop() }) }

func (r reordered) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	in, err := r.apiServer.Watch(ctx, opts)
	if err != nil {
		return nil, err
	}
	d := &delayedWatch{in: in, out: make(chan watch.Event, 1024), stop: make(chan struct{})}
	go func() {
		for ev := range in.ResultChan() {
			go func(ev watch.Event) {
				select {
				case <-time.After(time.Duration(rand.IntN(4000)) * time.Microsecond):
				case <-d.stop:
					return
				}
				select {
				case d.out <- ev:
				case <-d.stop:
				}
			}(ev)
		}
	}()
	return d, nil
}

// Three replicas with reordered watches, each spending the same
// single-use values and marking the same steps at once, while other
// writes land: every value and every mark is spent at most once (round 16,
// go P2-1: one confirmation, one operation).
func TestThreeReplicasSpendEachValueAtMostOnce(t *testing.T) {
	budget := 2 * time.Second
	if testing.Short() {
		budget = 500 * time.Millisecond
	}
	clock := &testClock{now: t0}
	api := newAPIServer()
	stores := make([]*Store, 3)
	for i := range stores {
		stores[i] = New(Options{
			Client: reordered{api}, Name: "console-state", Now: clock.Now, Logger: slog.New(slog.DiscardHandler),
			Debounce: time.Millisecond, UrgentDebounce: time.Millisecond,
			Backoff: func(int) time.Duration { return time.Millisecond },
		})
		run(t, stores[i])
	}
	for _, s := range stores {
		eventually(t, "every replica reading", s.GrantsUsable)
	}
	until := t0.Add(time.Hour)
	alice := uh("alice")
	// once runs step on every replica at once and returns how many
	// returned nil.
	once := func(step func(ctx context.Context, s *Store) error) int {
		var mu sync.Mutex
		var wg sync.WaitGroup
		n := 0
		for _, s := range stores {
			wg.Go(func() {
				err := step(t.Context(), s)
				switch {
				case err == nil:
					mu.Lock()
					n++
					mu.Unlock()
				case !errors.Is(err, ErrSpent) && !errors.Is(err, ErrNotRecorded) && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrNotSpent):
					t.Errorf("unexpected: %v", err)
				}
			})
		}
		wg.Wait()
		return n
	}
	ctx, stop := context.WithCancel(t.Context())
	var noise sync.WaitGroup
	noise.Go(func() {
		// Other writes: what makes the write-throughs conflict.
		for i := 0; ctx.Err() == nil; i++ {
			stores[i%3].InstanceSince("pod-"+strconv.Itoa(i%7), strconv.Itoa(i), t0)
			time.Sleep(time.Duration(rand.IntN(2000)) * time.Microsecond)
		}
	})
	deadline := time.Now().Add(budget)
	var workers sync.WaitGroup
	for w := range 4 {
		workers.Go(func() {
			for r := 0; time.Now().Before(deadline); r++ {
				code := fmt.Sprintf("confirm-%d-%d", w, r)
				switch n := once(func(ctx context.Context, s *Store) error { return s.SpendCode(ctx, code, alice, until) }); {
				case n > 1:
					t.Errorf("%s spent on %d replicas", code, n)
				case n == 0:
					continue // not recorded anywhere: nothing to mark
				}
				// The marks race on every replica that holds the spend.
				for _, mark := range []string{"approve", "run"} {
					if n := once(func(ctx context.Context, s *Store) error { return s.MarkCode(ctx, code, mark, alice) }); n > 1 {
						t.Errorf("%s marked %q on %d replicas", code, mark, n)
					}
				}
			}
		})
	}
	workers.Wait()
	stop()
	noise.Wait()
}
