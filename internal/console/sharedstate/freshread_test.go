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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A replica whose watch has not delivered a grant recorded elsewhere (its
// watch lags, or stopped) finds it with ReadGrants, so that a Disconnect
// all made there revokes it (round 21, security P2-1).
func TestReadGrantsCatchesUpAWatchThatLags(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	sw := &staleWatch{apiServer: api, clock: clock}
	c := freshStore(sw, clock, time.Hour, time.Hour, time.Hour)
	run(t, c)
	eventually(t, "c live", func() bool { return modeOf(c) == modeLive })
	b, id := grantOnTwoReplicas(t, api, clock)
	if ids := c.RevokeGrantsOf(uh("alice"), clock.Now()); len(ids) != 0 {
		t.Fatalf("c's watch delivered %v", ids)
	}
	if err := c.ReadGrants(t.Context()); err != nil {
		t.Fatalf("ReadGrants: %v", err)
	}
	ids := c.RevokeGrantsOf(uh("alice"), clock.Now())
	if !slices.Equal(ids, []string{id}) {
		t.Fatalf("after ReadGrants c revokes %v, want [%s]", ids, id)
	}
	if err := c.WriteRevocations(t.Context(), ids...); err != nil {
		t.Fatal(err)
	}
	watchMerge(t, api, b)
	if !b.GrantRevoked(id) {
		t.Error("b still serves the grant c revoked")
	}
}

// ReadGrants on a replica cut off from the API server, or forbidden, is
// no answer; on a missing ConfigMap it is (every grant is refused).
func TestReadGrantsAnswersOnlyWhatTheAPIServerShows(t *testing.T) {
	for name, tc := range map[string]struct {
		cut  func(cc *cutAPI)
		want error
	}{
		"cut off":   {cut: func(cc *cutAPI) { cc.down.Store(true) }, want: ErrNotRecorded},
		"forbidden": {cut: func(cc *cutAPI) { cc.set(func(x *apiServer) { x.forbidden = true }) }, want: ErrNotRecorded},
		"missing":   {cut: func(cc *cutAPI) { cc.set(func(x *apiServer) { x.cm = nil }) }},
		"answered":  {cut: func(*cutAPI) {}},
	} {
		t.Run(name, func(t *testing.T) {
			clock := &testClock{now: t0}
			cc := &cutAPI{apiServer: newAPIServer(), clock: clock}
			c := freshStore(cc, clock, time.Hour, time.Hour, time.Hour)
			run(t, c)
			eventually(t, "c live", func() bool { return modeOf(c) == modeLive })
			tc.cut(cc)
			err := c.ReadGrants(t.Context())
			if !errors.Is(err, tc.want) || tc.want == nil && err != nil {
				t.Errorf("ReadGrants = %v, want %v", err, tc.want)
			}
		})
	}
}

// slowGets answers every read after slow (real time), recording when each
// was sent.
type slowGets struct {
	*apiServer
	slow time.Duration
	mu   sync.Mutex
	sent []time.Time
}

func (g *slowGets) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.ConfigMap, error) {
	g.mu.Lock()
	g.sent = append(g.sent, time.Now())
	g.mu.Unlock()
	time.Sleep(g.slow)
	return g.apiServer.Get(ctx, name, o)
}

func (g *slowGets) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sent)
}

// The freshness reads are Freshness apart counted from their sending, not
// from their answer (round 21, Go P3-1): an API server that takes its time
// does not push the check of the permission back.
func TestFreshnessReadsAreCountedFromTheirSending(t *testing.T) {
	const freshness, slow = 200 * time.Millisecond, 150 * time.Millisecond
	g := &slowGets{apiServer: newAPIServer(), slow: slow}
	a := freshStore(g, &testClock{now: t0}, time.Hour, freshness, time.Hour)
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive })
	deadline := time.Now().Add(10 * time.Second)
	for g.count() < 6 {
		if time.Now().After(deadline) {
			t.Fatalf("%d reads", g.count())
		}
		time.Sleep(10 * time.Millisecond)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	shortest := time.Hour
	for i := 2; i < len(g.sent); i++ { // the first is the read before the watch
		shortest = min(shortest, g.sent[i].Sub(g.sent[i-1]))
	}
	// From the sending: [200, 250) ms apart; from the answer: 350 ms or more.
	if shortest >= freshness+slow {
		t.Errorf("freshness reads at least %v apart, want about %v (counted from the sending)", shortest, freshness)
	}
}

// opaque shows the ConfigMap's resourceVersions as strings that are not
// integers, which an API server may (Kubernetes asks clients to treat them
// as opaque); its watch delivers this replica's writes, and the test sends
// the bookmarks.
type opaque struct{ *staleWatch }

func (o *opaque) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error) {
	cm, err := o.staleWatch.Get(ctx, name, opts)
	if err == nil {
		cm.ResourceVersion = "o-" + cm.ResourceVersion
	}
	return cm, err
}

func (o *opaque) Update(ctx context.Context, cm *corev1.ConfigMap, opts metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	in := cm.DeepCopy()
	in.ResourceVersion = strings.TrimPrefix(in.ResourceVersion, "o-")
	out, err := o.staleWatch.Update(ctx, in, opts)
	if err == nil {
		out.ResourceVersion = "o-" + out.ResourceVersion
		o.mu.Lock()
		w := o.last
		o.mu.Unlock()
		if w != nil {
			w.Modify(out.DeepCopy()) // the watch delivers every write
		}
	}
	return out, err
}

// With opaque resourceVersions, bookmarks carrying the watch cache's own
// version do not make a watch that delivered the ConfigMap look behind
// it: it is not reopened (round 21, Go P3-2).
func TestOpaqueVersionsKeepAHealthyWatch(t *testing.T) {
	clock := &testClock{now: t0}
	o := &opaque{&staleWatch{apiServer: newAPIServer(), clock: clock}}
	a := freshStore(o, clock, time.Hour, 20*time.Millisecond, 10*time.Millisecond)
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive })
	for i := range 25 {
		o.bookmark("bm-" + string(rune('a'+i)))
		time.Sleep(20 * time.Millisecond)
	}
	if n := o.opens.Load(); n != 1 {
		t.Errorf("%d watches opened, want the healthy one kept", n)
	}
}

// A watch cache that stopped progressing sends bookmarks at its old
// resourceVersion; with WatchIdle long (its default is ten minutes now),
// the freshness read is what catches the watch up and reopens it (round
// 21, k8s P3-2).
func TestAStaleBookmarkWatchIsCaughtByTheFreshnessRead(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	b, id := grantOnTwoReplicas(t, api, clock)
	sw := &staleWatch{apiServer: api, clock: clock}
	a := freshStore(sw, clock, time.Hour, 50*time.Millisecond, 10*time.Millisecond)
	run(t, a)
	eventually(t, "a knows the grant", func() bool { _, ok := a.Grant(id); return ok })
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(10 * time.Millisecond)
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
	eventually(t, "a sees the revocation", func() bool { return a.GrantRevoked(id) })
	eventually(t, "a's watch reopened", func() bool { return sw.opens.Load() >= 2 })
	if DefaultWatchIdle < 10*time.Minute {
		t.Errorf("DefaultWatchIdle %v: a quiet ConfigMap's watch is reopened that often", DefaultWatchIdle)
	}
}

// metaAPI answers metadata reads of the ConfigMap (Options.Meta),
// counting them and the full reads.
type metaAPI struct {
	ConfigMapClient
	api         *apiServer
	metas, full atomic.Int32
}

func (m *metaAPI) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.ConfigMap, error) {
	m.full.Add(1)
	return m.ConfigMapClient.Get(ctx, name, o)
}

type metaGets struct{ *metaAPI }

func (m metaGets) Get(ctx context.Context, name string, o metav1.GetOptions, _ ...string) (*metav1.PartialObjectMetadata, error) {
	m.metas.Add(1)
	cm, err := m.api.Get(ctx, name, o)
	if err != nil {
		return nil, err
	}
	return &metav1.PartialObjectMetadata{ObjectMeta: cm.ObjectMeta}, nil
}

func metaStore(m *metaAPI, clock *testClock) *Store {
	s := freshStore(m, clock, time.Hour, 20*time.Millisecond, 10*time.Millisecond)
	s.opts.Meta = metaGets{m}
	return s
}

// With Options.Meta, a watch that is current costs metadata reads only;
// one that fell behind gets the full read, which merges what it missed;
// a metadata read refused is the forbidden mode (round 21, k8s).
func TestMetadataFreshnessReads(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		clock := &testClock{now: t0}
		api := newAPIServer()
		m := &metaAPI{ConfigMapClient: api, api: api}
		a := metaStore(m, clock)
		run(t, a)
		eventually(t, "a live", func() bool { return modeOf(a) == modeLive })
		eventually(t, "metadata reads", func() bool { return m.metas.Load() >= 5 })
		if n := m.full.Load(); n != 1 {
			t.Errorf("%d full reads, want only the one before the watch", n)
		}
	})
	t.Run("behind", func(t *testing.T) {
		clock := &testClock{now: t0}
		api := newAPIServer()
		b, id := grantOnTwoReplicas(t, api, clock)
		m := &metaAPI{ConfigMapClient: &staleWatch{apiServer: api, clock: clock}, api: api}
		a := metaStore(m, clock)
		run(t, a)
		eventually(t, "a knows the grant", func() bool { _, ok := a.Grant(id); return ok })
		full := m.full.Load()
		revoke(t, b, clock, id)
		eventually(t, "a sees the revocation", func() bool { return a.GrantRevoked(id) })
		if m.full.Load() == full {
			t.Error("the revocation was merged without a full read")
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		clock := &testClock{now: t0}
		api := newAPIServer()
		m := &metaAPI{ConfigMapClient: api, api: api}
		a := metaStore(m, clock)
		run(t, a)
		eventually(t, "a live", func() bool { return modeOf(a) == modeLive && a.GrantsUsable() })
		api.set(func(x *apiServer) { x.forbidden = true })
		eventually(t, "a forbidden", func() bool { return modeOf(a) == modeForbidden })
		if a.GrantsUsable() {
			t.Error("grants usable after the metadata read was refused")
		}
	})
}

// Metadata alone is never merged, even when the watch has not delivered
// its version: it would read as every document deleted.
func TestMetadataIsNeverMerged(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	s := loaded(t, api, clock)
	if err := s.AddGrant(testGrant(1, "alice", t0)); err != nil {
		t.Fatal(err)
	}
	behind := ""
	stub := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", ResourceVersion: "999", UID: "uid-1"}}
	if _, err := s.takeFresh(freshRead{cm: stub, meta: true, asked: t0}, watchProgress{seen: "1", object: "1"}, &behind); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Grant(grantID(1)); !ok || behind != "999" {
		t.Errorf("after metadata ahead of the watch: grant kept %v, behind %q (want the version kept for the next read)", ok, behind)
	}
}
