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

package digest

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// sharedMarks is an in-memory Marks: what the state ConfigMap gives every
// replica.
type sharedMarks struct {
	mu      sync.Mutex
	sent    map[string]time.Time
	unknown bool
}

// newSharedMarks returns empty, known marks.
func newSharedMarks() *sharedMarks { return &sharedMarks{sent: map[string]time.Time{}} }

func (m *sharedMarks) DigestSent(id, channel string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.sent[id+"/"+channel]
	return at, ok
}

func (m *sharedMarks) MarkDigestSent(id, channel string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sent[id+"/"+channel]; !ok {
		m.sent[id+"/"+channel] = at
	}
}

func (m *sharedMarks) MarksKnown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.unknown
}

// instantClock never sleeps: a wait only checks the context.
type instantClock struct{ now time.Time }

func (c instantClock) Now() time.Time { return c.now }

func (c instantClock) Wait(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// replica is one console replica's digest service with a stored digest.
func replica(t *testing.T, marks Marks, channels ...Channel) (*Service, string) {
	t.Helper()
	store, _ := NewStore(30*24*time.Hour, &memPersister{data: map[string][]byte{}}, nil)
	due := Slot(t0.Add(-DefaultGrace), 7)
	d, err := Generate(context.Background(), source(), nil, due, due.Add(DefaultGrace))
	if err != nil {
		t.Fatal(err)
	}
	store.Add(d, due)
	return NewService(Options{
		Hour: 7, Retention: 30 * 24 * time.Hour, Persistent: true, Console: "team-a",
		Backoff: []time.Duration{0}, Clock: instantClock{now: t0}, Source: source(), Store: store,
		Channels: channels, Marks: marks, Elected: true,
	}), d.ID
}

// lead runs s.Lead until the returned stop is called, which waits for it
// and its deliveries to end.
func lead(s *Service) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Lead(ctx); close(done) }()
	return func() { cancel(); <-done; s.wg.Wait() }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fakeChannel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// Two replicas hold the same digest. Only the one leading sends; a new
// leader sends nothing already marked, and finishes what the old one had
// not delivered.
func TestElectedDeliveryOnlyByTheLeaderAndNoResendOnFailover(t *testing.T) {
	marks := newSharedMarks()
	chatA, mailA := &fakeChannel{name: "chat"}, &fakeChannel{name: "mail", fails: 1}
	chatB, mailB := &fakeChannel{name: "chat"}, &fakeChannel{name: "mail"}
	a, id := replica(t, marks, chatA, mailA)
	b, _ := replica(t, marks, chatB, mailB)

	// Neither leads: the scheduler's path sends nothing.
	a.send(context.Background(), mustGet(t, a, id), a.o.Channels)
	b.send(context.Background(), mustGet(t, b, id), b.o.Channels)
	if chatA.count()+mailA.count()+chatB.count()+mailB.count() != 0 {
		t.Fatal("a replica that does not lead delivered")
	}

	// A leads: chat is delivered, mail fails its one attempt.
	stopA := lead(a)
	waitFor(t, "A's chat delivery", func() bool { return chatA.count() == 1 })
	waitFor(t, "A's mail attempt", func() bool {
		for _, c := range a.Status().Channels {
			if c.Channel == "mail" && c.Attempts == 1 && !c.Pending {
				return true
			}
		}
		return false
	})
	// B, following, shows the leader's delivery from the shared mark.
	for _, c := range b.Status().Channels {
		if c.Channel == "chat" && (c.LastSuccess == nil || c.DigestID != id) {
			t.Fatalf("B's chat state %+v: want A's delivery", c)
		}
	}
	stopA()

	// Failover: B leads. Chat was marked: not sent again. Mail was not:
	// B delivers it.
	stopB := lead(b)
	waitFor(t, "B's mail delivery", func() bool { return mailB.count() == 1 })
	stopB()
	if chatB.count() != 0 {
		t.Fatalf("the new leader sent chat again (%d)", chatB.count())
	}
	if _, ok := marks.DigestSent(id, "mail"); !ok {
		t.Fatal("B's delivery is not marked for the next leader")
	}

	// A leads again (B rolled): everything is marked, nothing is sent.
	// The new leadership runs in a bubble so "nothing" is a settled
	// state (every goroutine of it blocked), not a pause that happened
	// to be too short.
	synctest.Test(t, func(t *testing.T) {
		stopA = lead(a)
		synctest.Wait()
		stopA()
	})
	if chatA.count() != 1 || mailA.count() != 0 {
		t.Fatalf("A resent after taking the lead back: chat %d, mail %d", chatA.count(), mailA.count())
	}
}

// A new leader waits for the shared marks before it decides what is
// unsent: without them every channel would look unsent.
func TestLeaderWaitsForTheSharedMarks(t *testing.T) {
	// In a bubble the clock is fake: a minute of the leader's polling
	// passes at once, and synctest.Wait makes "nothing was sent" a
	// settled state rather than a sleep that happened to be long enough.
	synctest.Test(t, func(t *testing.T) {
		marks := newSharedMarks()
		marks.unknown = true
		chat := &fakeChannel{name: "chat"}
		s, id := replica(t, marks, chat)
		marks.MarkDigestSent(id, "chat", t0)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		// The instant clock makes the bounded wait pass at once, so hold
		// the marks unknown through a clock that never advances past the
		// bound.
		s.o.Clock = &frozenClock{now: t0}
		go func() { s.Lead(ctx); close(done) }()
		time.Sleep(time.Minute)
		synctest.Wait()
		if chat.count() != 0 {
			t.Fatal("sent before the shared marks were read")
		}
		marks.mu.Lock()
		marks.unknown = false
		marks.mu.Unlock()
		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		<-done
		s.wg.Wait()
		if chat.count() != 0 {
			t.Fatal("a digest marked sent by another replica was sent again")
		}
	})
}

// A delivery cut short by the end of the leadership is not left pending:
// the next leadership of this replica sends it.
func TestLostLeadershipLeavesNothingPending(t *testing.T) {
	marks := newSharedMarks()
	blocked := &blockingChannel{name: "chat", release: make(chan struct{})}
	s, id := replica(t, marks, blocked)
	stop := lead(s)
	waitFor(t, "the attempt", func() bool { return blocked.started() })
	stop()
	for _, c := range s.Status().Channels {
		if c.Channel == "chat" && (c.Pending || c.LastError != "") {
			t.Fatalf("chat state after losing the lead: %+v", c)
		}
	}
	close(blocked.release)
	stop = lead(s)
	waitFor(t, "the delivery on the next leadership", func() bool {
		_, ok := marks.DigestSent(id, "chat")
		return ok
	})
	stop()
}

func mustGet(t *testing.T, s *Service, id string) viewmodel.Digest {
	t.Helper()
	d, ok := s.o.Store.Get(id)
	if !ok {
		t.Fatalf("no digest %s", id)
	}
	return d
}

// frozenClock waits briefly (on the bubble's fake clock inside
// synctest.Test) and never advances.
type frozenClock struct{ now time.Time }

func (c *frozenClock) Now() time.Time { return c.now }

func (c *frozenClock) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Millisecond):
		return nil
	}
}

// blockingChannel holds each send until released or cancelled.
type blockingChannel struct {
	name    string
	mu      sync.Mutex
	n       int
	release chan struct{}
}

func (b *blockingChannel) Name() string { return b.name }

func (b *blockingChannel) Send(ctx context.Context, _ Message) error {
	b.mu.Lock()
	b.n++
	b.mu.Unlock()
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingChannel) started() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n > 0
}
