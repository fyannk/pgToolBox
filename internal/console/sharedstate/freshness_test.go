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
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// staleWatch opens watches the API server never feeds: the test sends
// what it wants on the last one (bookmarks at an old resourceVersion, as a
// watch cache that stopped progressing does). Get fails once failGet is
// set, advancing the clock by slow first (an answer that takes its time).
type staleWatch struct {
	*apiServer
	clock   *testClock
	slow    time.Duration
	failGet atomic.Bool
	opens   atomic.Int32
	mu      sync.Mutex
	last    *watch.RaceFreeFakeWatcher
}

func (s *staleWatch) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.ConfigMap, error) {
	if s.failGet.Load() {
		s.clock.Add(s.slow)
		return nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
	}
	return s.apiServer.Get(ctx, name, o)
}

func (s *staleWatch) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	s.opens.Add(1)
	w := watch.NewRaceFreeFake()
	s.mu.Lock()
	s.last = w
	s.mu.Unlock()
	return w, nil
}

func (s *staleWatch) bookmark(rv string) {
	s.mu.Lock()
	w := s.last
	s.mu.Unlock()
	if w != nil {
		w.Action(watch.Bookmark, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{ResourceVersion: rv}})
	}
}

func freshStore(api ConfigMapClient, clock *testClock, idle, freshness, backoff time.Duration) *Store {
	return New(Options{
		Client: api, Name: "console-state", Now: clock.Now, Logger: slog.New(slog.DiscardHandler),
		Debounce: 5 * time.Millisecond, UrgentDebounce: time.Millisecond,
		Backoff: func(int) time.Duration { return backoff }, WatchIdle: idle, Freshness: freshness,
	})
}

// RBAC on the ConfigMap is withdrawn: b's watch, opened before, stays open
// (the API server authorizes a watch once, when it opens); c, started
// since, is forbidden and knows no grant. Its "disconnect all" is no
// answer (round 20, security P2-1; the reviewer's probe
// TestR20ForbiddenCountedAsReadWhileAnOldWatchServes, which saw c answer
// done while b served the grant 20 minutes on).
func TestDisconnectAllOnAForbiddenReplicaIsNoAnswer(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	b := controlled(api, clock, 50*time.Millisecond)
	run(t, b)
	eventually(t, "b live", func() bool { return modeOf(b) == modeLive })
	if err := b.AddGrant(Grant{ID: grantID(9), UserHash: uh("alice"), Client: "c", Created: t0, Expiry: t0.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "grant written", func() bool { return api.data(KeyGrants) != "" })
	api.set(func(x *apiServer) { x.forbidden = true })
	c := controlled(api, clock, 50*time.Millisecond)
	run(t, c)
	eventually(t, "c forbidden", func() bool { return modeOf(c) == modeForbidden })
	if ids := c.RevokeGrantsOf(uh("alice"), clock.Now()); len(ids) != 0 {
		t.Fatalf("c knows %v", ids)
	}
	if err := c.WriteRevocations(t.Context()); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("c: WriteRevocations() = %v, want ErrNotRecorded while b may still serve the grant", err)
	}
}

// The permission withdrawn while b's watch is open: b's freshness read
// sees the 403 and b refuses every grant, rather than serving them until
// the API server closes the watch (round 20, security P2-1).
func TestAPermissionWithdrawnIsSeenWhileWatching(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	b := freshStore(api, clock, time.Hour, 50*time.Millisecond, time.Hour)
	run(t, b)
	eventually(t, "b live", func() bool { return modeOf(b) == modeLive && b.GrantsUsable() })
	api.set(func(x *apiServer) { x.forbidden = true })
	eventually(t, "b forbidden", func() bool { return modeOf(b) == modeForbidden })
	if b.GrantsUsable() {
		t.Error("b serves grants after its freshness read was refused")
	}
}

// A watch that delivers nothing while the ConfigMap changes (WatchIdle an
// hour): the freshness read merges what the watch missed at once, and the
// next one, finding the watch still behind, opens it again.
func TestAWatchBehindIsCaughtUpByTheFreshnessRead(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	b, id := grantOnTwoReplicas(t, api, clock)
	sw := &staleWatch{apiServer: api, clock: clock}
	a := freshStore(sw, clock, time.Hour, 50*time.Millisecond, 10*time.Millisecond)
	run(t, a)
	eventually(t, "a knows the grant", func() bool { _, ok := a.Grant(id); return ok })
	revoke(t, b, clock, id)
	eventually(t, "a sees the revocation", func() bool { return a.GrantRevoked(id) })
	eventually(t, "a's watch reopened", func() bool { return sw.opens.Load() >= 2 })
}

// A freshness read the API server does not answer ends the watch: the
// failing mode, its staleGrace from when the read was sent, not from when
// the failure came back.
func TestAFailedFreshnessReadStartsTheGraceWhenSent(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	sw := &staleWatch{apiServer: api, clock: clock, slow: 10 * time.Second}
	a := freshStore(sw, clock, time.Hour, 50*time.Millisecond, time.Hour)
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive })
	sent := clock.Now()
	sw.failGet.Store(true)
	eventually(t, "a failing", func() bool { return modeOf(a) == modeFailing })
	a.mu.Lock()
	since, watching := a.since, a.watching
	a.mu.Unlock()
	if watching || !since.Equal(sent) {
		t.Errorf("failing since %v (watching %v), want since the read was sent (%v), the watch ended", since, watching, sent)
	}
	clock.Set(sent.Add(staleGrace + time.Second))
	if a.GrantsUsable() {
		t.Errorf("grants usable %s after the unanswered read was sent", staleGrace+time.Second)
	}
}

// A watch that delivers only bookmarks at an old resourceVersion (a watch
// cache whose etcd watch is stuck still sends them) is taken for deaf
// after WatchIdle and opened again, with a read that sees the revocation
// (round 20, security P3-1; the reviewer's probe
// TestR20BookmarkOnlyWatchStaysLive). The freshness read is left at its
// default, too far off to be what sees it.
func TestABookmarkOnlyWatchIsReopened(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	b, id := grantOnTwoReplicas(t, api, clock)
	sw := &staleWatch{apiServer: api, clock: clock}
	a := freshStore(sw, clock, 300*time.Millisecond, 0, 50*time.Millisecond)
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive })
	eventually(t, "a knows the grant", func() bool { _, ok := a.Grant(id); return ok })
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				sw.bookmark("1")
			}
		}
	}()
	defer func() { close(stop); <-done }()
	revoke(t, b, clock, id)
	eventually(t, "a sees the revocation through a bookmark-only watch", func() bool { return a.GrantRevoked(id) })
	if sw.opens.Load() < 2 {
		t.Errorf("%d watches opened, want the bookmark-only one reopened", sw.opens.Load())
	}
}

// A bookmark at a newer resourceVersion is a sign of life: the watch is
// not reopened while they keep coming.
func TestAdvancingBookmarksKeepTheWatch(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	sw := &staleWatch{apiServer: api, clock: clock}
	a := freshStore(sw, clock, 300*time.Millisecond, 0, 50*time.Millisecond)
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive })
	for rv := 100; rv < 112; rv++ {
		time.Sleep(100 * time.Millisecond)
		sw.bookmark(strconv.Itoa(rv))
	}
	if n := sw.opens.Load(); n != 1 {
		t.Errorf("%d watches opened, want the one whose bookmarks advance kept", n)
	}
}

// Deaf without bookmarks, WatchIdle short: reopened, sees the revocation
// (the reviewer's probe TestR20DeafWatchIdleReopens).
func TestADeafWatchIdleReopens(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	b, id := grantOnTwoReplicas(t, api, clock)
	sw := &staleWatch{apiServer: api, clock: clock}
	a := freshStore(sw, clock, 300*time.Millisecond, 0, 50*time.Millisecond)
	run(t, a)
	eventually(t, "a knows the grant", func() bool { _, ok := a.Grant(id); return ok })
	revoke(t, b, clock, id)
	eventually(t, "a sees the revocation", func() bool { return a.GrantRevoked(id) })
}

func TestNewerVersion(t *testing.T) {
	for _, tc := range []struct {
		rv, seen string
		want     bool
	}{
		{"11", "10", true},
		{"10", "10", false},
		{"9", "10", false},
		{"100", "99", true}, // as numbers, not as strings
		{"", "10", false},
		{"10", "", true},
		{"b", "a", true}, // not numbers: any change
		{"a", "a", false},
	} {
		if got := newerVersion(tc.rv, tc.seen); got != tc.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", tc.rv, tc.seen, got, tc.want)
		}
	}
}

// The grace windows are measured on the clock's monotonic reading (round
// 20, security P3-2): since and unwatchedAt keep it (UTC() strips it, and a
// backward step of the wall clock would then extend them); State shows
// since in UTC.
func TestGraceTimesKeepTheMonotonicReading(t *testing.T) {
	s := New(Options{Client: newAPIServer(), Name: "console-state"})
	s.mu.Lock()
	s.watchOpenedLocked()
	s.mu.Unlock()
	behind := ""
	_, _ = s.takeFresh(freshRead{err: apierrors.NewInternalError(errors.New("no")), asked: time.Now()}, watchProgress{seen: "1", object: "1"}, &behind)
	s.mu.Lock()
	since, unwatched, m := s.since, s.unwatchedAt, s.mode
	s.mu.Unlock()
	if m != modeFailing {
		t.Fatalf("mode %s, want failing", m)
	}
	for name, at := range map[string]time.Time{"since": since, "unwatchedAt": unwatched} {
		if !strings.Contains(at.String(), " m=") {
			t.Errorf("%s %v has no monotonic reading", name, at)
		}
	}
	if st := s.State(); st.Since == nil || st.Since.Location() != time.UTC {
		t.Errorf("State().Since = %v, want UTC", st.Since)
	}
}
