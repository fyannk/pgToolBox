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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// lossy is the API server with an Update whose answer can be lost: the
// write lands, the client sees a timeout.
type lossy struct {
	*apiServer
	lose bool
}

func (l *lossy) Update(ctx context.Context, cm *corev1.ConfigMap, o metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	out, err := l.apiServer.Update(ctx, cm, o)
	if err == nil && l.lose {
		return nil, errors.New("context deadline exceeded")
	}
	return out, err
}

// skewedStore is a replica whose clock is off by off.
func skewedStore(c ConfigMapClient, clock *testClock, off time.Duration) *Store {
	return New(Options{
		Client: c, Name: "console-state", Now: func() time.Time { return clock.Now().Add(off) }, Logger: slog.New(slog.DiscardHandler),
		Debounce: 5 * time.Millisecond, UrgentDebounce: time.Millisecond,
		Backoff: func(int) time.Duration { return 5 * time.Millisecond },
	})
}

// syncOnce is a replica re-reading the ConfigMap (a Get) and writing.
func syncOnce(s *Store, api *apiServer) {
	cm, err := api.Get(context.Background(), "console-state", metav1.GetOptions{})
	if err != nil {
		s.mu.Lock()
		s.base = nil
		s.setModeLocked(modeMissing, "")
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.mergeReadLocked(cm)
	s.setModeLocked(modeLive, "")
	s.mu.Unlock()
	_ = s.flush(context.Background())
}

// A harder property than TestNoPartitionBringsBackARevokedGrant (round
// 16, security): lost write answers, late (older) watch events, the
// ConfigMap deleted and recreated, crash restarts while cut off, code
// replay placeholders, rotated re-adds within the 30 s grace, and replica
// clocks skewed within the assumed bound. Once every replica is
// reconnected, no revocation is undone, except one a crash took before
// any document held it: such a revocation was never acknowledged (the
// session page answered 503, WriteRevocations), or the ConfigMap was
// deleted since (an administrator's reset), and is counted apart.
func TestNoPathBringsBackARevokedGrant(t *testing.T) {
	seeds := uint64(600)
	if testing.Short() || raceEnabled {
		seeds = 60
	}
	for _, skew := range []time.Duration{0, 30 * time.Second} {
		lost, acknowledged := 0, 0
		for seed := range seeds {
			l, a := pathModel(t, seed, skew)
			lost += l
			acknowledged += a
		}
		t.Logf("skew %v: %d seeds; %d disconnections acknowledged, none undone; %d unacknowledged revocations lost to a crash",
			skew, seeds, acknowledged, lost)
	}
}

func pathModel(t *testing.T, seed uint64, skew time.Duration) (lost, acknowledged int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 16))
	clock := &testClock{now: t0}
	api := newAPIServer()
	const replicas, steps = 3, 120
	clients := make([]*lossy, replicas)
	offs := make([]time.Duration, replicas)
	stores := make([]*Store, replicas)
	for i := range stores {
		clients[i] = &lossy{apiServer: api}
		offs[i] = time.Duration(i-1) * skew
		stores[i] = skewedStore(clients[i], clock, offs[i])
		syncOnce(stores[i], api)
	}
	cut := make([]bool, replicas)
	revokedBy := map[string]int{} // grant -> the replica that revoked it first
	pending := map[string][]int{} // revocations no document holds yet, by replica
	crashed := map[string]bool{}  // revocations a crash took before a document held them
	acked := map[string]bool{}    // revocations acknowledged (WriteRevocations nil)
	issued := map[int]time.Time{} // last token issuance per grant
	var snaps []*corev1.ConfigMap // every ConfigMap the API server held: late events
	var trace []string
	durable := func() {
		for id, e := range docGrants(t, api) {
			if e.Revoked > 0 {
				delete(pending, id)
			}
		}
	}
	next := 0
	for range steps {
		api.mu.Lock()
		if api.cm != nil {
			snaps = append(snaps, api.cm.DeepCopy())
		}
		api.mu.Unlock()
		i := rng.IntN(replicas)
		s := stores[i]
		now := clock.Now()
		switch rng.IntN(13) {
		case 0:
			cut[i] = !cut[i]
			trace = append(trace, fmt.Sprintf("%d cut=%v", i, cut[i]))
		case 1:
			next++
			if s.GrantsUsable() && s.AddGrant(testGrant(next, "alice", now.Add(offs[i]))) == nil {
				issued[next] = now
				trace = append(trace, fmt.Sprintf("%d add %d", i, next))
			}
		case 2:
			// A disconnection from the session page.
			if next == 0 {
				continue
			}
			n := 1 + rng.IntN(next)
			id := grantID(n)
			if g, ok := s.Grant(id); ok && g.Revoked.IsZero() {
				s.RevokeGrant(id, uh("alice"), now.Add(offs[i]), g.Expiry)
				if _, seen := revokedBy[id]; !seen {
					revokedBy[id] = i
				}
				pending[id] = append(pending[id], i)
				// Cut off, the write fails: 503, not acknowledged.
				if !cut[i] && s.WriteRevocations(context.Background(), id) == nil {
					acked[id] = true
					acknowledged++
				}
				trace = append(trace, fmt.Sprintf("%d revoke %d (acknowledged %v)", i, n, acked[id]))
			}
		case 3:
			ds := []time.Duration{0, 5 * time.Second, 20 * time.Second, time.Minute, 5 * time.Minute, 9 * time.Minute, 11 * time.Minute}
			d := ds[rng.IntN(len(ds))]
			clock.Add(d)
			trace = append(trace, fmt.Sprintf("+%v", d))
		case 4:
			// A restart: the final flush lands only if not cut off.
			if !cut[i] {
				syncOnce(s, api)
			}
			durable()
			for id, rs := range pending {
				for _, r := range rs {
					if r == i {
						crashed[id] = true
					}
				}
			}
			stores[i] = skewedStore(clients[i], clock, offs[i])
			cut[i] = false
			syncOnce(stores[i], api)
			trace = append(trace, fmt.Sprintf("%d restart", i))
		case 5:
			if cut[i] {
				continue
			}
			clients[i].lose = true
			syncOnce(s, api)
			clients[i].lose = false
			trace = append(trace, fmt.Sprintf("%d lost-answer write", i))
		case 6:
			if len(snaps) == 0 {
				continue
			}
			old := snaps[rng.IntN(len(snaps))]
			s.mu.Lock()
			s.mergeLocked(old.DeepCopy())
			s.mu.Unlock()
			trace = append(trace, fmt.Sprintf("%d late event rv=%s", i, old.ResourceVersion))
		case 7:
			// Deleted and recreated empty; the replicas not cut off see
			// the delete.
			api.mu.Lock()
			api.cm = nil
			api.mu.Unlock()
			for j := range stores {
				if !cut[j] {
					stores[j].mu.Lock()
					stores[j].base = nil
					stores[j].setModeLocked(modeMissing, "")
					stores[j].mu.Unlock()
				}
			}
			// The delete erases what the ConfigMap held: a revocation is
			// then only in the memory of the replicas that know it (and
			// written again by them, recreatedLocked), and a restart of
			// one before that may lose it, acknowledged or not.
			for id := range revokedBy {
				for j := range stores {
					if stores[j].GrantRevoked(id) {
						pending[id] = append(pending[id], j)
					}
				}
				delete(acked, id)
			}
			api.mu.Lock()
			api.bumpLocked(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: "console-state", Namespace: "team-a", UID: types.UID(fmt.Sprintf("uid-%d", api.rv+1)),
			}})
			api.mu.Unlock()
			trace = append(trace, "delete+recreate")
		case 8:
			// A replay placeholder for a grant this replica does not know.
			if next == 0 {
				continue
			}
			n := 1 + rng.IntN(next)
			id := grantID(n)
			if _, ok := s.Grant(id); !ok && now.Sub(issued[n]) < time.Minute {
				if s.RevokeGrant(id, uh("alice"), now.Add(offs[i]), now.Add(offs[i]).Add(RevokedKeep)) || s.GrantRevoked(id) {
					if _, seen := revokedBy[id]; !seen {
						revokedBy[id] = i
					}
					pending[id] = append(pending[id], i)
					trace = append(trace, fmt.Sprintf("%d placeholder %d", i, n))
				}
			}
		case 9:
			// A refresh on a replica that does not know the grant, within
			// the grace.
			if next == 0 {
				continue
			}
			n := 1 + rng.IntN(next)
			id := grantID(n)
			if _, ok := s.Grant(id); !ok && !s.GrantRevoked(id) && now.Sub(issued[n]) < 30*time.Second && s.GrantsUsable() {
				g := testGrant(n, "alice", t0)
				g.Created, g.Expiry, g.LastUsed, g.Generation, g.Rotated = issued[n].Add(offs[i]), issued[n].Add(offs[i]).Add(12*time.Hour), now.Add(offs[i]), 1, true
				if s.AddGrant(g) == nil {
					issued[n] = now
					trace = append(trace, fmt.Sprintf("%d rotated %d", i, n))
				}
			}
		default:
			if !cut[i] {
				syncOnce(s, api)
				trace = append(trace, fmt.Sprintf("%d sync", i))
			}
		}
		durable()
	}
	for range 3 {
		for i := range stores {
			syncOnce(stores[i], api)
		}
	}
	for id := range revokedBy {
		if crashed[id] && !acked[id] {
			for _, s := range stores {
				if standing(s, id) {
					lost++
					break
				}
			}
			continue
		}
		for i, s := range stores {
			if standing(s, id) {
				t.Fatalf("skew %v seed %d: %s revoked (by %d, acknowledged %v), standing on %d after\n%s",
					skew, seed, id, revokedBy[id], acked[id], i, strings.Join(trace, "\n"))
			}
		}
	}
	return lost, acknowledged
}
