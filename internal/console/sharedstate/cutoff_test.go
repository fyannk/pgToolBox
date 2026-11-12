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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// cutAPI is one replica's connection to the shared apiServer: cut() stops
// its open watch and fails every later call (a node that lost its route to
// the control plane while still taking user traffic).
type cutAPI struct {
	*apiServer
	clock *testClock
	down  atomic.Bool
	hang  atomic.Bool
	hung  atomic.Int32
	slow  time.Duration
	mu    sync.Mutex
	ws    []watch.Interface
}

func (c *cutAPI) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.ConfigMap, error) {
	if c.hang.Load() {
		c.hung.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if c.down.Load() {
		c.clock.Add(c.slow)
		return nil, apierrors.NewInternalError(errors.New("dial tcp: i/o timeout"))
	}
	return c.apiServer.Get(ctx, name, o)
}

func (c *cutAPI) Watch(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
	if c.down.Load() {
		return nil, apierrors.NewInternalError(errors.New("dial tcp: i/o timeout"))
	}
	w, err := c.apiServer.Watch(ctx, o)
	if err == nil {
		c.mu.Lock()
		c.ws = append(c.ws, w)
		c.mu.Unlock()
	}
	return w, err
}

func (c *cutAPI) Update(ctx context.Context, cm *corev1.ConfigMap, o metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	if c.down.Load() {
		return nil, apierrors.NewInternalError(errors.New("dial tcp: i/o timeout"))
	}
	return c.apiServer.Update(ctx, cm, o)
}

func (c *cutAPI) stopWatches() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.ws {
		w.Stop()
	}
}

// Probe 1: c is cut off from the API server (failing) but still serves
// users; b records alice's new grant; alice's Disconnect all lands on c,
// which knows no grant of hers and answers done; nothing is written, and b
// serves the grant on.
func TestR21DisconnectAllOnAFailingReplicaWithAStaleBase(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	b := controlled(api, clock, 50*time.Millisecond)
	run(t, b)
	eventually(t, "b live", func() bool { return modeOf(b) == modeLive })
	cc := &cutAPI{apiServer: api, clock: clock}
	c := freshStore(cc, clock, time.Hour, time.Hour, 50*time.Millisecond)
	run(t, c)
	eventually(t, "c live", func() bool { return modeOf(c) == modeLive && c.GrantsUsable() })
	cc.down.Store(true)
	cc.stopWatches()
	eventually(t, "c failing", func() bool { return modeOf(c) == modeFailing })
	id := grantID(9)
	if err := b.AddGrant(Grant{ID: id, UserHash: uh("alice"), Client: "thief", Created: clock.Now(), Expiry: clock.Now().Add(8 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "grant written", func() bool { return api.data(KeyGrants) != "" })
	if ids := c.RevokeGrantsOf(uh("alice"), clock.Now()); len(ids) != 0 {
		t.Fatalf("c knows %v", ids)
	}
	err := c.WriteRevocations(t.Context())
	t.Logf("c (mode %s) WriteRevocations() = %v", modeOf(c), err)
	if err != nil {
		return
	}
	clock.Add(2 * time.Hour)
	time.Sleep(200 * time.Millisecond)
	if b.GrantsUsable() && !b.GrantRevoked(id) {
		t.Errorf("c answered Disconnect all done; b (mode %s) still serves the grant 2 h later", modeOf(b))
	}
}

// Probe 2: c read the ConfigMap, then was cut off; b recorded a grant;
// the permission is withdrawn; c reconnects to a 403. The docs say the
// answer stays 503 on a forbidden replica, "Disconnect all with nothing to
// revoke included".
func TestR21ForbiddenWithABaseAnswersDone(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	cc := &cutAPI{apiServer: api, clock: clock}
	c := freshStore(cc, clock, time.Hour, time.Hour, 50*time.Millisecond)
	run(t, c)
	eventually(t, "c live", func() bool { return modeOf(c) == modeLive && c.GrantsUsable() })
	api.set(func(x *apiServer) { x.forbidden = true })
	cc.stopWatches()
	eventually(t, "c forbidden", func() bool { return modeOf(c) == modeForbidden })
	if err := c.WriteRevocations(t.Context()); !errors.Is(err, ErrNotRecorded) {
		t.Errorf("forbidden c with a base: WriteRevocations() = %v; docs (mcp.md) say 503", err)
	}
}

// Probe 3: a freshness read hangs (sent at T); the watch ends at T+9 s
// before the read times out; the reopen read fails. The docs say the 30 s
// run from when the read was sent.
func TestR21WatchEndsWhileAFreshnessReadHangs(t *testing.T) {
	clock := &testClock{now: t0}
	api := newAPIServer()
	cc := &cutAPI{apiServer: api, clock: clock, slow: 10 * time.Second}
	a := freshStore(cc, clock, time.Hour, 50*time.Millisecond, 50*time.Millisecond)
	run(t, a)
	eventually(t, "a live", func() bool { return modeOf(a) == modeLive && a.GrantsUsable() })
	sent := clock.Now()
	cc.hang.Store(true)
	eventually(t, "a freshness read hung", func() bool { return cc.hung.Load() >= 1 })
	clock.Add(9 * time.Second)
	cc.down.Store(true)
	cc.hang.Store(false)
	cc.stopWatches()
	eventually(t, "a failing", func() bool { return modeOf(a) == modeFailing })
	a.mu.Lock()
	since := a.since
	a.mu.Unlock()
	t.Logf("failing since sent+%s", since.Sub(sent))
	clock.Set(sent.Add(staleGrace + time.Second))
	if a.GrantsUsable() {
		t.Errorf("grants usable %s after the unanswered freshness read was sent (since = sent+%s)", staleGrace+time.Second, since.Sub(sent))
	}
}
