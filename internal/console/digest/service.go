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
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// DefaultGrace is how long after its slot a digest is made: the end of
// the window gets time to reach the console (a backup's status, the
// diagnostics run, the last watch events) before it is summed up.
const DefaultGrace = 10 * time.Minute

// readyPoll paces the wait for the informer cache's first sync.
const readyPoll = 5 * time.Second

// marksWait bounds how long a new leader waits for the shared delivery
// marks before it sends what its own marks call unsent.
const marksWait = 2 * time.Minute

// DefaultBackoff is the wait before each delivery attempt: four attempts
// over about ten minutes, then the channel waits for the next digest.
var DefaultBackoff = []time.Duration{0, 30 * time.Second, 2 * time.Minute, 8 * time.Minute}

// Options configure the Service.
type Options struct {
	Hour       int
	Retention  time.Duration
	Persistent bool
	// Console names the console in messages (its namespace).
	Console string
	// PublicURL is the console's external URL; links are PublicURL/digests/<id>.
	PublicURL string
	// Timeout bounds one delivery attempt.
	Timeout time.Duration
	// Grace overrides DefaultGrace; negative is none (tests).
	Grace time.Duration
	// Ready reports that the read model synced once; no digest is made
	// before (an unsynced cache would list no Cluster: an all-clear).
	// Nil is always ready.
	Ready func() bool
	// Backoff overrides DefaultBackoff (tests).
	Backoff  []time.Duration
	Clock    Clock
	Source   Source
	Journal  Journal
	Store    *Store
	Channels []Channel
	Logger   *slog.Logger
	// Marks are the delivery marks the replicas share (the state
	// ConfigMap); nil keeps them with the stored digests only.
	Marks Marks
	// Elected: only the replica holding the digest Lease delivers, from
	// Lead. Unset, this console delivers alone (one replica,
	// or the local mode).
	Elected bool
}

// Marks are the delivery marks every replica sees (sharedstate.Store).
type Marks interface {
	DigestSent(id, channel string) (time.Time, bool)
	MarkDigestSent(id, channel string, at time.Time)
	// MarksKnown is false until the shared marks were read once.
	MarksKnown() bool
}

// Service makes a digest at Hour:00 UTC every day and delivers it.
// Delivery runs beside the schedule and never blocks it, nor anything
// else in the console: a channel that is down costs its own retries.
type Service struct {
	o Options

	mu     sync.Mutex
	next   time.Time
	states map[string]*viewmodel.DeliveryState
	gen    map[string]int
	wg     sync.WaitGroup
	// lead is the leadership's context while this replica holds the
	// digest Lease (Options.Elected); nil otherwise.
	lead context.Context //nolint:containedctx // the leadership's lifetime, handed over by Lead
}

// AllChannels is every channel the admin page lists.
var AllChannels = []string{"chat", "webhook", "mail"}

// NewService returns the scheduler.
func NewService(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if len(o.Backoff) == 0 {
		o.Backoff = DefaultBackoff
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	switch {
	case o.Grace == 0:
		o.Grace = DefaultGrace
	case o.Grace < 0:
		o.Grace = 0
	}
	if o.Ready == nil {
		o.Ready = func() bool { return true }
	}
	s := &Service{o: o, states: map[string]*viewmodel.DeliveryState{}, gen: map[string]int{}}
	for _, name := range AllChannels {
		s.states[name] = &viewmodel.DeliveryState{Channel: name}
	}
	for _, c := range o.Channels {
		s.states[c.Name()].Configured = true
	}
	return s
}

// Slot is the newest digest end at or before now.
func Slot(now time.Time, hour int) time.Time {
	now = now.UTC()
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
	if t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

// Run makes the digests until ctx ends. It first waits for the read
// model's first sync. Then, and after every wait, it makes the latest
// slot whose grace has passed when the store lacks it: one catch-up
// digest after a start or a long suspend, never one per missed day. A
// digest made at start is delivered only when the store survives
// restarts (otherwise every restart would send it again); a stored
// digest some channel had not delivered is sent to that channel.
func (s *Service) Run(ctx context.Context) error {
	defer s.wg.Wait()
	for !s.o.Ready() {
		if s.o.Clock.Wait(ctx, readyPoll) != nil {
			return nil
		}
	}
	first := true
	for {
		due := Slot(s.o.Clock.Now().Add(-s.o.Grace), s.o.Hour)
		switch {
		case !s.o.Store.Has(due):
			s.make(ctx, due, !first || s.o.Persistent)
		case first && s.o.Persistent:
			s.resume(ctx, due)
		}
		first = false
		next := due.Add(Window)
		s.mu.Lock()
		s.next = next
		s.mu.Unlock()
		if err := s.o.Clock.Wait(ctx, next.Add(s.o.Grace).Sub(s.o.Clock.Now())); err != nil {
			return nil //nolint:nilerr // a cancelled context ends the scheduler cleanly
		}
	}
}

func (s *Service) make(ctx context.Context, to time.Time, deliver bool) {
	d, err := Generate(ctx, s.o.Source, s.o.Journal, to, s.o.Clock.Now())
	if err != nil {
		s.o.Logger.Warn("the daily digest could not be made", slog.String("digest", to.UTC().Format(IDLayout)), slog.Any("error", err))
		return
	}
	s.o.Store.Add(d, s.o.Clock.Now())
	s.o.Logger.Info("daily digest made", slog.String("digest", d.ID), slog.Int("clusters", d.Counts.Clusters),
		slog.Int("attention", d.Counts.Attention), slog.Bool("partial", d.Partial))
	if deliver {
		s.send(ctx, d, s.o.Channels)
	}
}

// resume sends the stored digest `due` to the channels whose delivery
// is not marked, and shows the others' marks on the admin page.
func (s *Service) resume(ctx context.Context, due time.Time) {
	d, ok := s.o.Store.Get(due.UTC().Format(IDLayout))
	if !ok {
		return
	}
	unsent := 0
	for _, c := range s.o.Channels {
		if _, done := s.sent(d.ID, c.Name()); !done {
			unsent++
		}
	}
	if unsent > 0 {
		s.o.Logger.Info("daily digest: resuming delivery", slog.String("digest", d.ID), slog.Int("channels", unsent))
	}
	// Also for the channels already marked: send records their marks for
	// the admin page.
	s.send(ctx, d, s.o.Channels)
}

// Lead delivers while this replica holds the digest Lease, until ctx
// (the leadership) ends. It first sends the latest digest to the
// channels no replica marked: the previous leader may have stopped
// before or during its delivery. The marks are shared, so what was sent
// is not sent again; a delivery whose mark was lost with its sender (it
// stopped between sending and the mark's write, about a second) is sent
// twice: at least once, never zero times.
func (s *Service) Lead(ctx context.Context) {
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	s.lead = ctx
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.lead == ctx {
			s.lead = nil
		}
		s.mu.Unlock()
	}()
	// The shared marks must have been read once, or "no mark" would read
	// as "not sent"; bounded, then the local marks are all there is.
	for deadline := s.o.Clock.Now().Add(marksWait); s.o.Marks != nil && !s.o.Marks.MarksKnown() && s.o.Clock.Now().Before(deadline); {
		if s.o.Clock.Wait(ctx, time.Second) != nil {
			return
		}
	}
	s.resume(ctx, Slot(s.o.Clock.Now().Add(-s.o.Grace), s.o.Hour))
	<-ctx.Done()
}

// sent reports whether channel delivered the digest id, on this replica
// or on any other (the shared marks).
func (s *Service) sent(id, channel string) (time.Time, bool) {
	if at, ok := s.o.Store.Sent(id, channel); ok {
		return at, true
	}
	if s.o.Marks != nil {
		return s.o.Marks.DigestSent(id, channel)
	}
	return time.Time{}, false
}

// send delivers d to the channels that have not had it: within ctx when
// this console delivers alone; when it is elected, within ctx and the
// leadership both (a lost Lease stops the delivery), and not at all while
// another replica leads.
func (s *Service) send(ctx context.Context, d viewmodel.Digest, channels []Channel) {
	var lead context.Context
	if s.o.Elected {
		s.mu.Lock()
		lead = s.lead
		s.mu.Unlock()
		if lead == nil {
			return // another replica delivers
		}
	}
	m := Message{Console: s.o.Console, Summary: d.DigestSummary, URL: Link(s.o.PublicURL, d.ID)}
	for _, c := range channels {
		if at, done := s.sent(d.ID, c.Name()); done {
			s.mu.Lock()
			st := s.states[c.Name()]
			st.DigestID, st.LastSuccess, st.Pending = d.ID, &at, false
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		if st := s.states[c.Name()]; st.Pending && st.DigestID == d.ID {
			// Already on its way (the scheduler and a new leadership
			// both reached it).
			s.mu.Unlock()
			continue
		}
		s.gen[c.Name()]++
		gen := s.gen[c.Name()]
		st := s.states[c.Name()]
		st.DigestID, st.Attempts, st.Pending = d.ID, 0, true
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			deliverCtx := ctx
			if lead != nil {
				var cancel context.CancelFunc
				deliverCtx, cancel = context.WithCancel(ctx)
				defer cancel()
				defer context.AfterFunc(lead, cancel)()
			}
			s.deliver(deliverCtx, c, m, gen)
		}()
	}
}

// Link is the digest's page, or empty without a public URL.
func Link(publicURL, id string) string {
	if publicURL == "" {
		return ""
	}
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.String(), "/") + "/digests/" + url.PathEscape(id)
}

func (s *Service) deliver(ctx context.Context, c Channel, m Message, gen int) {
	// stopped ends a delivery cut short by the leadership or the console
	// stopping: not pending any more, and no error of its own.
	stopped := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.gen[c.Name()] == gen {
			s.states[c.Name()].Pending = false
		}
	}
	for i, wait := range s.o.Backoff {
		if wait > 0 && s.o.Clock.Wait(ctx, wait) != nil {
			stopped()
			return
		}
		s.mu.Lock()
		superseded := s.gen[c.Name()] != gen
		s.mu.Unlock()
		if superseded {
			return // a newer digest took over this channel
		}
		attemptCtx, cancel := context.WithTimeout(ctx, s.o.Timeout)
		err := c.Send(attemptCtx, m)
		cancel()
		if err != nil && ctx.Err() != nil {
			stopped()
			return
		}
		now := s.o.Clock.Now().UTC()
		s.mu.Lock()
		if s.gen[c.Name()] != gen {
			s.mu.Unlock()
			return
		}
		st := s.states[c.Name()]
		st.Attempts = i + 1
		st.LastAttempt = &now
		if err == nil {
			st.LastSuccess, st.LastError, st.Pending = &now, "", false
			s.mu.Unlock()
			s.o.Store.MarkSent(m.Summary.ID, c.Name(), now)
			if s.o.Marks != nil {
				s.o.Marks.MarkDigestSent(m.Summary.ID, c.Name(), now)
			}
			s.o.Logger.Info("daily digest delivered", slog.String("channel", c.Name()), slog.String("digest", m.Summary.ID))
			return
		}
		msg := bounded(err.Error())
		st.LastError = msg
		st.Pending = i+1 < len(s.o.Backoff)
		s.mu.Unlock()
		s.o.Logger.Warn("daily digest delivery failed", slog.String("channel", c.Name()), slog.String("digest", m.Summary.ID),
			slog.Int("attempt", i+1), slog.String("error", msg))
	}
}

// Status is the admin page's account of the digest.
func (s *Service) Status() *viewmodel.DigestStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &viewmodel.DigestStatus{Enabled: true, Schedule: s.schedule(), Channels: make([]viewmodel.DeliveryState, 0, len(AllChannels))}
	if all := s.o.Store.All(); len(all) > 0 {
		st.Last = all[0].ID
	}
	for _, name := range AllChannels {
		channel := *s.states[name]
		if channel.Configured && channel.LastSuccess == nil && st.Last != "" {
			// Delivered by another replica (the leader): its shared mark.
			if at, ok := s.sent(st.Last, name); ok {
				channel.DigestID, channel.LastSuccess = st.Last, &at
			}
		}
		st.Channels = append(st.Channels, channel)
	}
	return st
}

// Schedule is the digest schedule for the Digest page.
func (s *Service) Schedule() viewmodel.DigestSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.schedule()
}

func (s *Service) schedule() viewmodel.DigestSchedule {
	sc := viewmodel.DigestSchedule{Hour: s.o.Hour, Retention: s.o.Retention.String(), Persistent: s.o.Persistent}
	if !s.next.IsZero() {
		next := s.next
		sc.Next = &next
	}
	return sc
}

// Store returns the digests.
func (s *Service) Store() *Store { return s.o.Store }

// Archive is every stored digest, newest first, with the schedule.
func (s *Service) Archive() ([]viewmodel.Digest, viewmodel.DigestSchedule) {
	return s.o.Store.All(), s.Schedule()
}
