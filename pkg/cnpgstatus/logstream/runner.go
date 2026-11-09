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

package logstream

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/fyannk/pgToolBox/pkg/redact"
)

// Bounds of the follower itself. These are not configurable: they are
// properties of following a live stream safely, not deployment choices.
const (
	// maxLineBytes truncates a single line before it reaches any sink.
	// A log line has no length limit, and one long line must not become
	// one large allocation.
	maxLineBytes = 64 * 1024
	// reconnectMin and reconnectMax bound the backoff after a stream
	// ends. A container that restarts in a loop must not become a
	// reconnect loop against the API server.
	reconnectMin = 2 * time.Second
	reconnectMax = 2 * time.Minute
	// reconcileEvery is how often the follower set is compared against
	// the observed pods.
	reconcileEvery = 30 * time.Second
)

// Member is one container worth following.
type Member struct {
	Pod       string
	Container string
}

// Source opens streams and says what to follow. The Kubernetes client
// implements it; a test supplies its own.
type Source interface {
	// Members lists the containers of every pod proven to belong to the
	// cluster. Membership is the source's job, not the runner's.
	Members() []Member
	// Follow opens a live stream for one container. The stream ends when
	// the container restarts, the connection drops, or ctx is done.
	Follow(ctx context.Context, pod, container string) (io.ReadCloser, error)
}

// Clock is the injected time source: the instants the sinks are given
// and the reconnect backoff's waits.
type Clock interface {
	Now() time.Time
	// After is time.After on this clock.
	After(d time.Duration) <-chan time.Time
}

// Runner follows every member container continuously and offers each
// line to the sinks.
//
// It owns one goroutine per container plus one reconciler. Following is
// best effort by construction: a stream ends on every container restart
// and on any connection fault, and Kubernetes gives no way to learn what
// was emitted in between. Each reconnect therefore records an explicit
// gap rather than joining the two halves silently.
type Runner struct {
	source Source
	sink   Sink
	clock  Clock
	logger *slog.Logger

	mu      sync.Mutex
	running map[Member]context.CancelFunc
	// followers counts the goroutines still winding down. Cancelling a
	// follower's context only asks it to stop; it still has a last
	// coverage transition to report on its way out, and a Run that
	// returned before that landed would leave a sink being written to
	// after its owner believed the follower had stopped.
	followers sync.WaitGroup
}

// NewRunner wires a runner onto a source and its sinks.
func NewRunner(source Source, sink Sink, clock Clock, logger *slog.Logger) *Runner {
	return &Runner{
		source: source, sink: sink, clock: clock, logger: logger,
		running: map[Member]context.CancelFunc{},
	}
}

// Run reconciles followers against the observed members until ctx ends.
func (r *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()
	r.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			r.stopAll()
			return nil
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

// reconcile starts followers for new containers and stops those whose
// container is gone.
func (r *Runner) reconcile(ctx context.Context) {
	wanted := map[Member]bool{}
	for _, member := range r.source.Members() {
		wanted[member] = true
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for member, cancel := range r.running {
		if !wanted[member] {
			cancel()
			delete(r.running, member)
		}
	}
	for member := range wanted {
		if _, already := r.running[member]; already {
			continue
		}
		//nolint:gosec // cancel is retained in r.running and invoked by reconcile and stopAll.
		followCtx, cancel := context.WithCancel(ctx)
		r.running[member] = cancel
		r.followers.Add(1)
		go func() {
			defer r.followers.Done()
			r.follow(followCtx, member)
		}()
	}
}

// stopAll cancels every follower and waits for them to finish. The wait
// is what makes Run's return mean something: each follower drops its
// coverage as it exits, and a caller that took Run returning as "the
// followers are done" would otherwise be racing that last write.
func (r *Runner) stopAll() {
	r.mu.Lock()
	for member, cancel := range r.running {
		cancel()
		delete(r.running, member)
	}
	r.mu.Unlock()
	r.followers.Wait()
}

// follow keeps one container's stream open, reconnecting with backoff.
func (r *Runner) follow(ctx context.Context, member Member) {
	// The container counts as unread from the moment it is worth
	// following, not from the first failure. Between the reconciler
	// naming it and the first stream opening, nothing from it has been
	// seen -- and a check that cleared in that window would be reporting
	// on a container the console had never listened to.
	r.sink.Detached(member.Pod, member.Container, r.clock.Now(),
		"the follower has not attached to this container's stream yet")
	// Following stops when the container leaves the roster or the
	// process ends. Either way its coverage is dropped rather than left
	// standing open: a pod that no longer exists is not one the console
	// is failing to read.
	defer r.sink.Dropped(member.Pod, member.Container)

	backoff := reconnectMin
	for ctx.Err() == nil {
		reason, attachedAt, err := r.readOnce(ctx, member)
		if ctx.Err() != nil {
			return
		}
		// A stream that stayed attached longer than the shortest wait was
		// a working connection, not part of a failing streak: the next
		// reconnect starts from the shortest wait again. Without the reset
		// one bad hour would leave every later restart of the container
		// read two minutes late.
		if !attachedAt.IsZero() && r.clock.Now().Sub(attachedAt) > reconnectMin {
			backoff = reconnectMin
		}
		if err != nil {
			r.logger.Info("log stream ended",
				slog.String("pod", member.Pod),
				slog.String("container", member.Container),
				slog.String("category", redact.Safe(err)))
		}
		// The gap is recorded the moment the stream ends, not when the
		// next one opens: the unobserved window starts here, and dating
		// it from the reconnect would misplace it by the whole backoff.
		r.sink.Gap(member.Pod, member.Container, r.clock.Now(), reason)
		// The same instant closes the reading window. Gap marks the hole
		// in the record; this says the hole is still open.
		r.sink.Detached(member.Pod, member.Container, r.clock.Now(), reason)

		select {
		case <-ctx.Done():
			return
		case <-r.clock.After(backoff):
		}
		if backoff *= 2; backoff > reconnectMax {
			backoff = reconnectMax
		}
	}
}

// readOnce consumes one stream to its end, returning the reason the
// record is now broken, the instant the stream attached (zero when it
// never opened) and any error.
func (r *Runner) readOnce(ctx context.Context, member Member) (reason string, attachedAt time.Time, err error) {
	stream, err := r.source.Follow(ctx, member.Pod, member.Container)
	if err != nil {
		return "stream could not be opened; nothing from this container was observed", time.Time{}, err
	}
	defer func() { _ = stream.Close() }()
	attachedAt = r.clock.Now()
	r.sink.Attached(member.Pod, member.Container, attachedAt)

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 4096), maxLineBytes)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return "stream stopped", attachedAt, ctx.Err()
		}
		r.sink.Observe(Line{
			Pod: member.Pod, Container: member.Container,
			Text: scanner.Text(), At: r.clock.Now(),
		})
	}
	// A line longer than the buffer stops the scanner, which is its own
	// kind of break and worth naming separately from a plain end.
	if err := scanner.Err(); err != nil {
		return "stream interrupted; a line may have exceeded the read bound", attachedAt, err
	}
	return "stream ended; lines emitted before it was reopened were not observed", attachedAt, nil
}

// Opener opens one container's stream. The Kubernetes client implements
// it; separating it from the roster is what lets the follower be
// assembled where the roster lives rather than where the client does.
type Opener interface {
	FollowLogs(ctx context.Context, pod, container string) (io.ReadCloser, error)
}

// NewSource pairs a roster function with an opener to make a Source.
//
// The roster is a function rather than a snapshot so it is read at each
// reconcile: a pod that has gone must stop being followed, and a pod
// that has appeared must start, without the runner holding a stale list.
func NewSource(members func() []Member, opener Opener) Source {
	return funcSource{members: members, opener: opener}
}

// funcSource is a Source over a member function and an Opener.
type funcSource struct {
	members func() []Member
	opener  Opener
}

// Members implements Source.
func (s funcSource) Members() []Member { return s.members() }

// Follow implements Source by following the container through the Opener.
func (s funcSource) Follow(ctx context.Context, pod, container string) (io.ReadCloser, error) {
	return s.opener.FollowLogs(ctx, pod, container)
}
