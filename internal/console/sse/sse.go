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

// Package sse is the console's live-update stream: a broker that fans invalidations out to subscribers, and the loop
// that writes one subscriber's stream with the edge settings it
// needs (15 s heartbeat comments so idle proxies keep the connection,
// a reauth event before the session ends, a last resync and the stream
// closed when it ends).
//
// The stream carries invalidations, never data: an event names what
// changed ("clusters", or one cluster), and the UI refetches it through
// the JSON API, where every read is authorized. Events about a cluster
// the subscriber may not get are filtered out before they are written; an
// event whose check cannot be decided is replaced by a resync, so the UI
// refetches through the authorized API instead of missing a change.
//
// # Contract for the UI (polling backstop)
//
//   - hello: sent first on every connection. Refetch every active query:
//     anything may have changed while disconnected.
//   - invalidate {topic: "cluster", namespace, name}: refetch that
//     Cluster's views.
//   - invalidate {topic: "clusters"}: refetch the list and every open
//     Cluster view (a change the console could not attribute to one
//     Cluster is announced this way, without naming its namespace).
//   - resync {reason}: the server could not deliver every event (the
//     client was too slow, or whether it may see one could not be
//     checked). Refetch everything. It is also the last event of a stream
//     that ends with its session (reason "session expired" or "session
//     ended"): the refetch then meets the 401 that leads to sign-in, or,
//     for a session renewed meanwhile, simply succeeds.
//   - reauth {expiresAt, refreshable}: the session ends at expiresAt. Call
//     GET /api/v1/session: a refreshable session is renewed by that call
//     and its expiresAt moves; otherwise warn and offer to sign in again.
//     The stream itself closes at expiresAt, after a last resync.
//   - A comment line arrives at least every 15 s. No byte for 35 s means
//     the stream is dead even if the socket looks open.
//   - While the stream is down, poll the active queries every 2 s,
//     backing off to 10 s, and show "live updates paused". EventSource
//     reconnects by itself (the server sends retry: 2000). A 401 from any
//     call means the session is gone: go to its loginURL.
package sse

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Topics of invalidation events.
const (
	// TopicClusters: the list changed, or something not attributable to
	// one Cluster did; refetch every cluster view.
	TopicClusters = "clusters"
	// TopicCluster: one cluster's overview, instances, events or
	// diagnostics changed.
	TopicCluster = "cluster"
)

// Hello is the first event of every stream.
type Hello struct {
	ServerTime       time.Time `json:"serverTime" doc:"Server clock when the stream opened (UTC)."`
	HeartbeatSeconds int       `json:"heartbeatSeconds" doc:"Interval of the keep-alive comment lines."`
	PollMinSeconds   int       `json:"pollMinSeconds" doc:"Polling backstop: first interval while the stream is down."`
	PollMaxSeconds   int       `json:"pollMaxSeconds" doc:"Polling backstop: interval the backoff stops at."`
}

// Invalidate says that something the UI may show has changed.
type Invalidate struct {
	Topic     string `json:"topic" enum:"clusters,cluster" doc:"What changed: clusters (refetch the list and every open cluster view) or cluster (refetch that one)."`
	Namespace string `json:"namespace,omitempty" doc:"Namespace of the cluster, for topic cluster."`
	Name      string `json:"name,omitempty" doc:"Name of the cluster, for topic cluster."`
}

// Reauth announces the end of the session.
type Reauth struct {
	ExpiresAt   time.Time `json:"expiresAt" doc:"When the session ends unless renewed (UTC)."`
	Refreshable bool      `json:"refreshable" doc:"Whether GET /api/v1/session can renew it without a new sign-in."`
}

// Resync tells the client it missed events.
type Resync struct {
	Reason string `json:"reason" doc:"Why the server could not deliver every event."`
}

// Defaults of the stream.
const (
	DefaultHeartbeat = 15 * time.Second
	PollMin          = 2 * time.Second
	PollMax          = 10 * time.Second
	// RetryMillis is the reconnect delay EventSource is told to use.
	RetryMillis = 2000
	// Reasons of the resync events (the UI refetches whatever the reason).
	ReasonDropped        = "events were dropped for this client"
	ReasonUndecided      = "whether you may see an update could not be checked"
	ReasonSessionExpired = "session expired"
	ReasonSessionEnded   = "session ended"
	// subscriberBuffer bounds the events queued for one slow client
	// before it is told to resync.
	subscriberBuffer = 64
)

// Broker fans invalidations out to subscribers. Publish never blocks: a
// subscriber whose buffer is full loses the event and gets a resync.
type Broker struct {
	mu     sync.Mutex
	subs   map[*Subscription]struct{}
	closed bool
}

// NewBroker returns an empty broker.
func NewBroker() *Broker { return &Broker{subs: map[*Subscription]struct{}{}} }

// Subscription is one client's queue.
type Subscription struct {
	b       *Broker
	c       chan Invalidate
	overrun atomic.Bool
	done    chan struct{}
	once    sync.Once
}

// Subscribe registers a client. On a closed broker the subscription is
// already done.
func (b *Broker) Subscribe() *Subscription {
	s := &Subscription{b: b, c: make(chan Invalidate, subscriberBuffer), done: make(chan struct{})}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		s.once.Do(func() { close(s.done) })
		return s
	}
	b.subs[s] = struct{}{}
	clients.Inc()
	return s
}

// Close unregisters the subscription.
func (s *Subscription) Close() {
	s.b.mu.Lock()
	if _, ok := s.b.subs[s]; ok {
		delete(s.b.subs, s)
		clients.Dec()
	}
	s.b.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

// Publish queues ev for every subscriber.
func (b *Broker) Publish(ev Invalidate) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		select {
		case s.c <- ev:
		default:
			s.overrun.Store(true)
			dropped.Inc()
		}
	}
}

// Close ends every subscription: used on shutdown so open streams finish
// and the HTTP server can drain.
func (b *Broker) Close() {
	b.mu.Lock()
	b.closed = true
	subs := make([]*Subscription, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.Unlock()
	for _, s := range subs {
		s.Close()
	}
}

// Clients is the number of open subscriptions.
func (b *Broker) Clients() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Sender writes one stream. Event writes a named event whose name derives
// from the type of data; Comment writes a comment line.
type Sender interface {
	Event(data any) error
	Comment(text string) error
}

// Options of one stream.
type Options struct {
	// Heartbeat interval; DefaultHeartbeat when zero.
	Heartbeat time.Duration
	// ReauthAt is when to send Reauth; EndAt when to close the stream.
	// Zero disables each.
	ReauthAt    time.Time
	EndAt       time.Time
	Refreshable bool
	// Visible filters events the subscriber may not see. Nil allows all.
	// An error means the check could not decide: the event is replaced by
	// a resync (at most one per heartbeat interval, later ones folded into
	// one sent at the next heartbeat), never dropped silently. An error
	// wrapping ErrSessionEnded (the subscriber's token is no longer
	// accepted) ends the stream as the session's end does.
	Visible func(ctx context.Context, ev Invalidate) (bool, error)
	// Recheck runs on every heartbeat; false ends the stream (the session
	// was revoked or its token is no longer accepted) after a last
	// resync, and the client's refetch and reconnect then meet the
	// sign-in check. Nil never ends it.
	Recheck func(ctx context.Context) bool
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// ErrSessionEnded means the subscriber's credentials are no longer
// accepted: a Visible error wrapping it ends the stream with its session.
var ErrSessionEnded = errors.New("the session ended")

// Run writes the stream until ctx ends, the subscription closes, the
// session ends or a write fails. It returns the write error, if any.
func Run(ctx context.Context, sub *Subscription, send Sender, o Options) error {
	defer sub.Close()
	now := o.Now
	if now == nil {
		now = time.Now
	}
	hb := o.Heartbeat
	if hb <= 0 {
		hb = DefaultHeartbeat
	}
	if err := send.Event(Hello{
		ServerTime: now().UTC(), HeartbeatSeconds: int(hb / time.Second),
		PollMinSeconds: int(PollMin / time.Second), PollMaxSeconds: int(PollMax / time.Second),
	}); err != nil {
		return err
	}
	ticker := time.NewTicker(hb)
	defer ticker.Stop()
	reauth := timerAt(now, o.ReauthAt)
	defer reauth.Stop()
	end := timerAt(now, o.EndAt)
	defer end.Stop()

	// Visibility checks run off this loop, one event at a time in arrival
	// order: a burst of events behind cold SAR caches can take longer
	// than the client's dead-stream threshold, and the heartbeat must keep
	// flowing meanwhile. Every write stays on this goroutine, and an event
	// reaches it only with its decision.
	checkCtx, stopChecks := context.WithCancel(ctx)
	defer stopChecks()
	checked := make(chan checkedEvent)
	go check(checkCtx, sub, o.Visible, checked)

	var u undecided
	for {
		if sub.overrun.Swap(false) {
			if err := send.Event(Resync{Reason: ReasonDropped}); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-sub.done:
			return nil
		case <-end.C:
			return send.Event(Resync{Reason: ReasonSessionExpired})
		case <-reauth.C:
			if err := send.Event(Reauth{ExpiresAt: o.EndAt.UTC(), Refreshable: o.Refreshable}); err != nil {
				return err
			}
		case <-ticker.C:
			if o.Recheck != nil && !o.Recheck(ctx) {
				if ctx.Err() != nil {
					return nil
				}
				return send.Event(Resync{Reason: ReasonSessionEnded})
			}
			if err := u.tick(send); err != nil {
				return err
			}
			if err := send.Comment("heartbeat"); err != nil {
				return err
			}
		case c := <-checked:
			stop, err := deliver(ctx, send, &u, c)
			if stop || err != nil {
				return err
			}
		}
	}
}

// checkedEvent is an event with the outcome of its visibility check.
type checkedEvent struct {
	ev      Invalidate
	visible bool
	err     error
}

// check decides the visibility of each queued event, in order, and hands
// it to the stream loop; nil visible allows all. It returns when ctx ends
// or the subscription closes.
func check(ctx context.Context, sub *Subscription, visible func(context.Context, Invalidate) (bool, error), out chan<- checkedEvent) {
	for {
		var ev Invalidate
		select {
		case <-ctx.Done():
			return
		case <-sub.done:
			return
		case ev = <-sub.c:
		}
		c := checkedEvent{ev: ev, visible: true}
		if visible != nil {
			c.visible, c.err = visible(ctx, ev)
		}
		select {
		case out <- c:
		case <-ctx.Done():
			return
		}
	}
}

// deliver writes a checked event when the subscriber may see it. An
// undecided check sends a resync in its place; a check that found the
// session over sends the last resync and stops the stream.
func deliver(ctx context.Context, send Sender, u *undecided, c checkedEvent) (stop bool, err error) {
	ok, verr := c.visible, c.err
	switch {
	case ctx.Err() != nil:
		// The client is gone and the check failed because of it: the
		// stream ends as it does on ctx.Done, without an error.
		return true, nil //nolint:nilerr // a canceled check is the end of the stream, not a failure

	case errors.Is(verr, ErrSessionEnded):
		return true, send.Event(Resync{Reason: ReasonSessionExpired})
	case verr != nil:
		return false, u.event(send)
	case ok:
		return false, send.Event(c.ev)
	}
	return false, nil
}

// undecided paces the resyncs that replace undecided events: the first in
// a heartbeat interval goes at once, later ones in the same interval are
// folded into one sent at the next heartbeat. Every undecided event is
// thus followed by a resync, and a reviewer outage costs the client at
// most one full refetch per interval.
type undecided struct{ sent, pending bool }

func (u *undecided) event(send Sender) error {
	if u.sent {
		u.pending = true
		return nil
	}
	u.sent = true
	return send.Event(Resync{Reason: ReasonUndecided})
}

func (u *undecided) tick(send Sender) error {
	u.sent = u.pending
	if !u.pending {
		return nil
	}
	u.pending = false
	return send.Event(Resync{Reason: ReasonUndecided})
}

// timerAt fires at t, immediately when t has passed, never when t is
// zero.
func timerAt(now func() time.Time, t time.Time) *time.Timer {
	if t.IsZero() {
		tm := time.NewTimer(time.Hour)
		tm.Stop()
		return tm
	}
	return time.NewTimer(max(t.Sub(now()), 0))
}

var (
	clients = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pgtoolbox_console_sse_clients",
		Help: "Open live-update streams.",
	})
	dropped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pgtoolbox_console_sse_dropped_events_total",
		Help: "Invalidations dropped for a slow client (each answered by a resync event).",
	})
)

func init() {
	crmetrics.Registry.MustRegister(clients, dropped)
}
