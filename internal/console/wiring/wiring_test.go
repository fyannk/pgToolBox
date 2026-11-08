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

package wiring

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/sse"
	"github.com/fyannk/pgToolBox/internal/console/status"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

type fakeRead struct{ err error }

func (f fakeRead) Timeline(context.Context, string, string, status.TimelineQuery) (viewmodel.Timeline, error) {
	return viewmodel.Timeline{}, f.err
}

func (f fakeRead) ClusterList(context.Context) (viewmodel.ClusterList, error) {
	return viewmodel.ClusterList{}, f.err
}

func (f fakeRead) Overview(context.Context, string, string) (viewmodel.ClusterOverview, error) {
	return viewmodel.ClusterOverview{}, f.err
}

func (f fakeRead) Instances(context.Context, string, string) (viewmodel.InstanceList, error) {
	return viewmodel.InstanceList{}, f.err
}

func (f fakeRead) Events(context.Context, string, string) (viewmodel.EventList, error) {
	return viewmodel.EventList{}, f.err
}

func (f fakeRead) Diagnostics(context.Context, string, string) (viewmodel.Diagnostics, error) {
	return viewmodel.Diagnostics{}, f.err
}

func (f fakeRead) Triage(context.Context, string, string) (viewmodel.Triage, error) {
	return viewmodel.Triage{}, f.err
}

func (f fakeRead) Replication(context.Context, string, string) (viewmodel.Replication, error) {
	return viewmodel.Replication{}, f.err
}

func (f fakeRead) Backups(context.Context, string, string) (viewmodel.Backups, error) {
	return viewmodel.Backups{}, f.err
}

func (f fakeRead) RestoreCheck(context.Context, string, string, time.Time) (viewmodel.RestoreCheck, error) {
	return viewmodel.RestoreCheck{}, f.err
}

func (f fakeRead) Storage(context.Context, string, string) (viewmodel.Storage, error) {
	return viewmodel.Storage{}, f.err
}

func (f fakeRead) Objects(context.Context, string, string) (viewmodel.Objects, error) {
	return viewmodel.Objects{}, f.err
}

func (f fakeRead) History(context.Context, string, string) (viewmodel.History, error) {
	return viewmodel.History{}, f.err
}

func (f fakeRead) Revision(context.Context, string, string, int64) (viewmodel.Revision, error) {
	return viewmodel.Revision{}, f.err
}

func (f fakeRead) Metrics(context.Context, string, string) (viewmodel.Metrics, error) {
	return viewmodel.Metrics{}, f.err
}

func (f fakeRead) Topology(context.Context, string, string, ...string) (viewmodel.Topology, error) {
	return viewmodel.Topology{}, f.err
}

func (f fakeRead) PgAdminLinks(context.Context, string, string) ([]viewmodel.PgAdminLink, error) {
	return nil, f.err
}

func (f fakeRead) Admin(context.Context) (viewmodel.Admin, error) { return viewmodel.Admin{}, f.err }

func (f fakeRead) Insight(context.Context, string, string) (viewmodel.Insight, error) {
	return viewmodel.Insight{}, f.err
}

func (f fakeRead) InsightBlocking(context.Context, string, string, bool) (viewmodel.InsightBlocking, error) {
	return viewmodel.InsightBlocking{}, f.err
}

func (f fakeRead) FleetStatements(context.Context, func([]viewmodel.ClusterRef) []viewmodel.ClusterRef) (viewmodel.FleetStatements, error) {
	return viewmodel.FleetStatements{}, f.err
}

func (f fakeRead) Compare(context.Context, string, string, string, string) (viewmodel.Comparison, error) {
	return viewmodel.Comparison{}, f.err
}

func TestSourceMapsNotFound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := NewSource(fakeRead{err: status.ErrNotFound})
	checks := make([]error, 0, 16)
	_, err := s.ClusterOverview(ctx, "a", "b")
	checks = append(checks, err)
	_, err = s.Instances(ctx, "a", "b")
	checks = append(checks, err)
	_, err = s.Events(ctx, "a", "b")
	checks = append(checks, err)
	_, err = s.Diagnostics(ctx, "a", "b")
	checks = append(checks, err)
	_, err = s.Triage(ctx, "a", "b")
	checks = append(checks, err)
	for _, read := range []func() error{
		func() error { _, err := s.Replication(ctx, "a", "b"); return err },
		func() error { _, err := s.Backups(ctx, "a", "b"); return err },
		func() error { _, err := s.Storage(ctx, "a", "b"); return err },
		func() error { _, err := s.Objects(ctx, "a", "b"); return err },
		func() error { _, err := s.History(ctx, "a", "b"); return err },
		func() error { _, err := s.Revision(ctx, "a", "b", 1); return err },
		func() error { _, err := s.Metrics(ctx, "a", "b"); return err },
		func() error { _, err := s.Topology(ctx, "a", "b"); return err },
		func() error { _, err := s.PgAdminLinks(ctx, "a", "b"); return err },
		func() error { _, err := s.Timeline(ctx, "a", "b", api.TimelineQuery{}); return err },
		func() error { _, err := s.Compare(ctx, "a", "b", "c", "d"); return err },
	} {
		checks = append(checks, read())
	}
	for i, err := range checks {
		if !errors.Is(err, api.ErrNotFound) {
			t.Errorf("call %d: %v is not api.ErrNotFound", i, err)
		}
	}
	other := errors.New("cache not synced")
	if _, err := NewSource(fakeRead{err: other}).ClusterOverview(ctx, "a", "b"); !errors.Is(err, other) || errors.Is(err, api.ErrNotFound) {
		t.Errorf("another error was rewritten: %v", err)
	}
}

type fakeFeed struct {
	mu    sync.Mutex
	keys  []cache.ClusterKey
	ready chan struct{}
}

func (f *fakeFeed) Ready() <-chan struct{} { return f.ready }

func (f *fakeFeed) Drain() []cache.ClusterKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.keys
	f.keys = nil
	return k
}

type capture struct {
	mu     sync.Mutex
	events []sse.Invalidate
	got    chan struct{}
}

func (c *capture) Event(data any) error {
	if ev, ok := data.(sse.Invalidate); ok {
		c.mu.Lock()
		c.events = append(c.events, ev)
		c.mu.Unlock()
		c.got <- struct{}{}
	}
	return nil
}
func (c *capture) Comment(string) error { return nil }

func TestInvalidationsBridge(t *testing.T) {
	t.Parallel()
	broker := sse.NewBroker()
	feed := &fakeFeed{ready: make(chan struct{}, 1)}
	unsubscribed := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Invalidations(func() (KeyFeed, func()) { return feed, func() { close(unsubscribed) } }, broker)(ctx)
	}()
	rec := &capture{got: make(chan struct{}, 16)}
	sub := broker.Subscribe()
	go func() { _ = sse.Run(ctx, sub, rec, sse.Options{Heartbeat: time.Hour}) }()
	for broker.Clients() == 0 {
		time.Sleep(time.Millisecond)
	}
	feed.mu.Lock()
	feed.keys = []cache.ClusterKey{{Namespace: "team-a"}, {Namespace: "team-a", Name: "orders"}}
	feed.mu.Unlock()
	feed.ready <- struct{}{}
	for range 2 {
		select {
		case <-rec.got:
		case <-time.After(5 * time.Second):
			t.Fatal("no invalidation")
		}
	}
	rec.mu.Lock()
	got := rec.events
	rec.mu.Unlock()
	if len(got) != 2 || got[0].Topic != sse.TopicClusters || got[0].Namespace != "" ||
		got[1] != (sse.Invalidate{Topic: sse.TopicCluster, Namespace: "team-a", Name: "orders"}) {
		t.Fatalf("events = %+v (a namespace-wide change must not name its namespace)", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-unsubscribed
}

func TestSourceDigestsDisabled(t *testing.T) {
	t.Parallel()
	if _, err := NewSource(fakeRead{}).Digests(t.Context()); !errors.Is(err, api.ErrDigestDisabled) {
		t.Fatalf("err = %v, want api.ErrDigestDisabled", err)
	}
}
