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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gatedGets reads the ConfigMap when a Get is sent, then holds the answer
// until the gate opens: a read in flight while the ConfigMap changes. It
// counts the full reads.
type gatedGets struct {
	*apiServer
	gate chan struct{}
	full atomic.Int32
	// fail, when set, is every read's answer.
	fail atomic.Pointer[error]
}

func (g *gatedGets) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.ConfigMap, error) {
	g.full.Add(1)
	cm, err := g.apiServer.Get(ctx, name, o)
	if f := g.fail.Load(); f != nil {
		cm, err = nil, *f
	}
	if g.gate != nil {
		select {
		case <-g.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return cm, err
}

func handLoaded(t *testing.T, api *apiServer, client ConfigMapClient, clock *testClock, log *slog.Logger) *Store {
	t.Helper()
	s := newStore(client, clock, log)
	cm, err := api.Get(t.Context(), "console-state", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.mergeLocked(cm)
	s.watchOpenedLocked()
	s.mu.Unlock()
	return s
}

// Concurrent ReadGrants share their reads (round 22, k8s P2-1): many
// callers at once cost at most two reads, the one in flight and the one
// they all wait for.
func TestConcurrentReadGrantsAreCoalesced(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	g := &gatedGets{apiServer: api, gate: make(chan struct{})}
	s := handLoaded(t, api, g, clock, slog.New(slog.DiscardHandler))
	const callers = 64
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() { errs <- s.ReadGrants(t.Context()) })
	}
	eventually(t, "a read in flight", func() bool { return g.full.Load() == 1 })
	time.Sleep(50 * time.Millisecond) // every caller in do
	close(g.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ReadGrants: %v", err)
		}
	}
	if n := g.full.Load(); n > 2 {
		t.Errorf("%d concurrent ReadGrants made %d reads, want at most 2", callers, n)
	}
}

// A caller is never answered by a read sent before it arrived: the grant
// recorded elsewhere while that read was in flight is in the answer.
func TestReadGrantsIsAnsweredByAReadSentAfterIt(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	g := &gatedGets{apiServer: api, gate: make(chan struct{})}
	s := handLoaded(t, api, g, clock, slog.New(slog.DiscardHandler))
	first := make(chan error, 1)
	go func() { first <- s.ReadGrants(t.Context()) }()
	eventually(t, "the first read in flight", func() bool { return g.full.Load() == 1 })
	_, id := grantOnTwoReplicas(t, api, clock) // recorded while the read is in flight
	second := make(chan error, 1)
	go func() { second <- s.ReadGrants(t.Context()) }()
	time.Sleep(20 * time.Millisecond)
	close(g.gate)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Grant(id); !ok {
		t.Error("the second caller was answered by the read sent before it arrived: the grant is missing")
	}
}

// On a replica whose watch is current, ReadGrants reads the metadata only
// while it shows nothing the watch has not delivered; the whole ConfigMap
// once it does, or once it is another ConfigMap (round 22, k8s P2-1).
func TestReadGrantsOnACurrentReplicaReadsTheMetadataFirst(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	m := &metaAPI{ConfigMapClient: api, api: api}
	s := handLoaded(t, api, m, clock, slog.New(slog.DiscardHandler))
	s.opts.Meta = metaGets{m}
	for range 20 {
		if err := s.ReadGrants(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if f, n := m.full.Load(), m.metas.Load(); f != 0 || n != 20 {
		t.Fatalf("current replica: %d full reads, %d metadata reads; want 0 and 20", f, n)
	}
	// Another replica records a grant this one's watch has not delivered.
	_, id := grantOnTwoReplicas(t, api, clock)
	if err := s.ReadGrants(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Grant(id); !ok || m.full.Load() != 1 {
		t.Fatalf("watch behind: grant found %v after %d full reads, want found after 1", ok, m.full.Load())
	}
	// The watch delivers it: metadata only again.
	cm, _ := api.Get(t.Context(), "console-state", metav1.GetOptions{})
	s.locked(func() {
		s.mergeLocked(cm)
		s.delivered = watchProgress{seen: cm.ResourceVersion, object: cm.ResourceVersion}
	})
	if err := s.ReadGrants(t.Context()); err != nil || m.full.Load() != 1 {
		t.Fatalf("watch caught up: err %v, %d full reads, want nil and still 1", err, m.full.Load())
	}
	// Recreated at the same version: another UID is a full read.
	api.set(func(x *apiServer) { x.cm.UID = "uid-2" })
	if err := s.ReadGrants(t.Context()); err != nil || m.full.Load() != 2 {
		t.Fatalf("recreated: err %v, %d full reads, want nil and 2", err, m.full.Load())
	}
}

// Only a 403 or a 404 of ReadGrants changes the mode (round 22, k8s
// P2-1): a person repeating Disconnect all against a throttled or slow
// API server cannot put the replica in the failing mode.
func TestReadGrantsFailuresChangeTheModeOnlyWhenAnswered(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		mode     mode
		want     error
		forbidMe bool
	}{
		"throttled": {err: apierrors.NewTooManyRequests("slow down", 1), mode: modeLive, want: ErrNotRecorded},
		"timeout":   {err: apierrors.NewTimeoutError("the request took too long", 1), mode: modeLive, want: ErrNotRecorded},
		"deadline":  {err: context.DeadlineExceeded, mode: modeLive, want: ErrNotRecorded},
		"limiter":   {err: errors.New("client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline"), mode: modeLive, want: ErrNotRecorded},
		"internal":  {err: apierrors.NewInternalError(errors.New("etcdserver: leader changed")), mode: modeLive, want: ErrNotRecorded},
		"forbidden": {err: apierrors.NewForbidden(configMaps, "console-state", errors.New("no")), mode: modeForbidden, want: ErrNotRecorded},
		"missing":   {err: apierrors.NewNotFound(configMaps, "console-state"), mode: modeMissing},
	} {
		t.Run(name, func(t *testing.T) {
			clock := &testClock{now: t0}
			api := newAPIServer()
			g := &gatedGets{apiServer: api}
			s := handLoaded(t, api, g, clock, slog.New(slog.DiscardHandler))
			g.fail.Store(&tc.err)
			for range 3 {
				err := s.ReadGrants(t.Context())
				if !errors.Is(err, tc.want) || tc.want == nil && err != nil {
					t.Fatalf("ReadGrants = %v, want %v", err, tc.want)
				}
			}
			if got := modeOf(s); got != tc.mode {
				t.Errorf("mode %s, want %s", got, tc.mode)
			}
			if tc.mode == modeLive && !s.GrantsUsable() {
				t.Error("grants refused after a failed ReadGrants")
			}
		})
	}
}

// staleGets answers every Get with the ConfigMap as it was when the
// client was made: reads overtaken by the watch.
type staleGets struct {
	*apiServer
	old *corev1.ConfigMap
}

func (g *staleGets) Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error) {
	return g.old.DeepCopy(), nil
}

// Answers of ReadGrants older than the base (overtaken by the watch) are
// not merged, however many: they never make an older ConfigMap the base,
// dropping what the newer one holds (round 22, Go P3-2).
func TestReadGrantsOvertakenByTheWatchKeepTheBase(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	old, _ := api.Get(t.Context(), "console-state", metav1.GetOptions{})
	s := handLoaded(t, api, &staleGets{apiServer: api, old: old}, clock, slog.New(slog.DiscardHandler))
	_, id := grantOnTwoReplicas(t, api, clock)
	watchMerge(t, api, s)
	for range rewoundAfter + 1 {
		if err := s.ReadGrants(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := s.Grant(id); !ok {
		t.Error("older reads replaced the base: the grant the watch delivered is gone")
	}
	s.locked(func() {
		if s.base.ResourceVersion == old.ResourceVersion {
			t.Errorf("base set back to %s", old.ResourceVersion)
		}
	})
}

// A ReadGrants that finds the ConfigMap while it is known missing (it was
// recreated) leaves the missing mode: a revocation is then written to it,
// not answered done as held (round 22, Go P3-3).
func TestReadGrantsFindingARecreatedConfigMapIsPending(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	s := handLoaded(t, api, api, clock, slog.New(slog.DiscardHandler))
	api.set(func(x *apiServer) { x.cm = nil })
	if err := s.ReadGrants(t.Context()); err != nil || modeOf(s) != modeMissing {
		t.Fatalf("missing: ReadGrants = %v, mode %s", err, modeOf(s))
	}
	api.set(func(x *apiServer) {
		x.rv++
		x.cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", Namespace: "team-a", ResourceVersion: "50", UID: "uid-2"}}
	})
	if err := s.ReadGrants(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := modeOf(s); got != modePending {
		t.Fatalf("mode %s after reading the recreated ConfigMap, want pending", got)
	}
	id := grantID(3)
	s.RevokeGrant(id, uh("alice"), t0, t0.Add(time.Hour))
	if err := s.WriteRevocations(t.Context(), id); err != nil {
		t.Fatalf("WriteRevocations = %v, want the revocation written", err)
	}
	if !strings.Contains(api.data(KeyGrants), id) {
		t.Error("the recreated ConfigMap does not hold the revocation")
	}
}

// undecodable is a metadata client behind a proxy that ignores
// as=PartialObjectMetadata: every answer fails to decode (no API status).
type undecodable struct{ calls atomic.Int32 }

func (u *undecodable) Get(context.Context, string, metav1.GetOptions, ...string) (*metav1.PartialObjectMetadata, error) {
	u.calls.Add(1)
	return nil, errors.New("unable to decode returned object as PartialObjectMetadata")
}

// A metadata answer that does not decode falls back to the full read in
// the same read; once that full read answers, the metadata is given up
// for the process, logged once (round 22, k8s P3-1). Without an answer
// to the full read either (the connection, not the decoding), it is kept.
func TestUndecodableMetadataFallsBackToFullReads(t *testing.T) {
	t.Run("freshness and ReadGrants", func(t *testing.T) {
		clock := &testClock{now: t0}
		api := newAPIServer()
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(syncWriter{&sync.Mutex{}, &buf}, nil))
		s := handLoaded(t, api, api, clock, log)
		u := &undecodable{}
		s.opts.Meta = u
		p := watchProgress{seen: "1", object: "1"}
		r := s.readFresh(t.Context(), t0, p)
		if r.err != nil || r.meta || r.cm == nil {
			t.Fatalf("freshness read = %+v, want the full ConfigMap", r)
		}
		if !s.metaOff.Load() {
			t.Fatal("metadata kept after an undecodable answer")
		}
		_ = s.readFresh(t.Context(), t0, p)
		if err := s.ReadGrants(t.Context()); err != nil {
			t.Fatal(err)
		}
		if n := u.calls.Load(); n != 1 {
			t.Errorf("%d metadata reads, want 1 (given up after it)", n)
		}
		if n := strings.Count(buf.String(), "could not be decoded"); n != 1 {
			t.Errorf("logged %d times, want once:\n%s", n, buf.String())
		}
	})
	t.Run("ReadGrants first", func(t *testing.T) {
		clock := &testClock{now: t0}
		api := newAPIServer()
		s := handLoaded(t, api, api, clock, slog.New(slog.DiscardHandler))
		s.opts.Meta = &undecodable{}
		if err := s.ReadGrants(t.Context()); err != nil {
			t.Fatalf("ReadGrants = %v, want the full read's answer", err)
		}
		if !s.metaOff.Load() || modeOf(s) != modeLive {
			t.Errorf("metaOff %v, mode %s; want given up, live", s.metaOff.Load(), modeOf(s))
		}
	})
	t.Run("no answer at all", func(t *testing.T) {
		clock := &testClock{now: t0}
		api := newAPIServer()
		g := &gatedGets{apiServer: api}
		dial := error(errors.New("dial tcp 10.0.0.1:443: connect: connection refused"))
		g.fail.Store(&dial)
		s := handLoaded(t, api, g, clock, slog.New(slog.DiscardHandler))
		s.opts.Meta = &undecodable{}
		if r := s.readFresh(t.Context(), t0, watchProgress{seen: "1", object: "1"}); r.err == nil {
			t.Fatal("freshness read answered without an API server")
		}
		if s.metaOff.Load() {
			t.Error("metadata given up for a connection failure")
		}
	})
}

// One 403 while the watch is open (a ReadGrants, a write) is left at the
// next freshness read that is answered (round 22, Go P3-1): the watch is
// opened again and the replica is live, rather than refusing every grant
// until the watch ends.
func TestAGoodFreshnessReadLeavesTheForbiddenMode(t *testing.T) {
	t.Run("takeFresh", func(t *testing.T) {
		clock := &testClock{now: t0}
		api := newAPIServer()
		s := handLoaded(t, api, api, clock, slog.New(slog.DiscardHandler))
		_ = s.failed(apierrors.NewForbidden(configMaps, "console-state", errors.New("no")))
		cm, _ := api.Get(t.Context(), "console-state", metav1.GetOptions{})
		behind := ""
		reopen, err := s.takeFresh(freshRead{cm: cm, asked: t0}, watchProgress{seen: "1", object: "1"}, &behind)
		if err != nil || !reopen {
			t.Errorf("takeFresh while forbidden = %v, %v; want the watch reopened", reopen, err)
		}
	})
	t.Run("running", func(t *testing.T) {
		clock := &testClock{now: t0}
		api := newAPIServer()
		g := &gatedGets{apiServer: api}
		s := freshStore(g, clock, time.Hour, 20*time.Millisecond, 10*time.Millisecond)
		run(t, s)
		eventually(t, "live", func() bool { return modeOf(s) == modeLive && s.GrantsUsable() })
		forbidden := error(apierrors.NewForbidden(configMaps, "console-state", errors.New("no")))
		g.fail.Store(&forbidden)
		_ = s.ReadGrants(t.Context())
		g.fail.Store(nil)
		eventually(t, "live again", func() bool { return modeOf(s) == modeLive && s.GrantsUsable() })
	})
}

// WriteGrant puts a grant recorded here in the ConfigMap before its first
// token: nil once it is there, ErrNotRecorded when it cannot be written.
func TestWriteGrant(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	s := handLoaded(t, api, api, clock, slog.New(slog.DiscardHandler))
	g := testGrant(4, "alice", t0)
	if err := s.AddGrant(g); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteGrant(t.Context(), g.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(api.data(KeyGrants), g.ID) {
		t.Fatal("WriteGrant returned nil; the ConfigMap does not hold the grant")
	}
	if err := s.WriteGrant(t.Context(), g.ID); err != nil {
		t.Errorf("again: %v, want nil (held already)", err)
	}
	if err := s.WriteGrant(t.Context(), grantID(5)); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("a grant not recorded here: %v, want ErrNotRecorded", err)
	}
	g6 := testGrant(6, "alice", t0)
	if err := s.AddGrant(g6); err != nil {
		t.Fatal(err)
	}
	api.set(func(x *apiServer) { x.forbidden = true })
	if err := s.WriteGrant(t.Context(), g6.ID); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("forbidden: %v, want ErrNotRecorded", err)
	}
	if err := New(Options{}).WriteGrant(t.Context(), g6.ID); err != nil {
		t.Errorf("one replica: %v, want nil", err)
	}
}

// The security reviewer's probe of round 22
// (TestR22UnwrittenGrantOutlivesDisconnectAll), on the redemption as
// mcpoauth makes it now: b, cut off for a few seconds inside staleGrace,
// records alice's grant; its write-through fails, so the grant is revoked
// there and no token is issued. alice's Disconnect all on c, live,
// answers done (nothing of hers in the ConfigMap); b reconnects; neither
// b nor c serves the grant two hours later. Before the fix the grant was
// in b's memory only, c answered done, and b wrote it unrevoked.
func TestAGrantNotWrittenIsNeverServedAfterADoneDisconnectAll(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	bc := &cutAPI{apiServer: api, clock: clock}
	b := freshStore(bc, clock, time.Hour, time.Hour, 50*time.Millisecond)
	run(t, b)
	c := freshStore(api, clock, time.Hour, time.Hour, 50*time.Millisecond)
	run(t, c)
	eventually(t, "b,c live", func() bool {
		return modeOf(b) == modeLive && modeOf(c) == modeLive && b.GrantsUsable() && c.GrantsUsable()
	})
	bc.down.Store(true)
	bc.stopWatches()
	id := grantID(11)
	if !b.GrantsUsable() {
		t.Fatal("b refuses at once; the scenario needs it inside staleGrace")
	}
	g := Grant{ID: id, UserHash: uh("alice"), Client: "thief", Created: clock.Now(), Expiry: clock.Now().Add(8 * time.Hour)}
	if err := b.AddGrant(g); err != nil {
		t.Fatal(err)
	}
	// As mcpoauth's redemption: the grant is written through before its
	// first token, and revoked here when it cannot be.
	if err := b.WriteGrant(t.Context(), id); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("WriteGrant on a cut-off replica = %v, want ErrNotRecorded", err)
	}
	b.RevokeGrant(id, g.UserHash, clock.Now(), g.Expiry)
	// alice's Disconnect all on c, as mcpoauth.Server.DisconnectAll.
	read := c.ReadGrants(t.Context())
	ids := c.RevokeGrantsOf(uh("alice"), clock.Now())
	if read != nil || len(ids) != 0 {
		t.Fatalf("c: ReadGrants %v, revoked %v; the scenario is c answering done", read, ids)
	}
	clock.Add(5 * time.Second)
	bc.down.Store(false)
	eventually(t, "b live again", func() bool { return modeOf(b) == modeLive && b.GrantsUsable() })
	eventually(t, "the revocation written", func() bool { return strings.Contains(api.data(KeyGrants), id) })
	clock.Add(2 * time.Hour)
	eventually(t, "c sees it", func() bool { _, ok := c.Grant(id); return ok })
	if !b.GrantRevoked(id) {
		t.Errorf("Disconnect all answered done on c; b (mode %s) serves the grant 2 h later", modeOf(b))
	}
	if !c.GrantRevoked(id) {
		t.Error("Disconnect all answered done on c; c serves the grant 2 h later")
	}
}
