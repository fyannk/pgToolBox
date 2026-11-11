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
	"fmt"
	"maps"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// The documents a prune returns are the state's, and comparing or writing
// them is what comparing or writing a fresh encoding is: the lock is held
// for one encoding a prune, with the same outcome.
func TestPrunedDocumentsAreTheState(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 5))
	clock := &testClock{now: t0}
	a := synced(t, Options{Name: "console-state", Now: clock.Now, Budget: 8 << 10})
	b := synced(t, Options{Name: "console-state", Now: clock.Now, Budget: 8 << 10})
	for k := range 600 {
		clock.Add(time.Second)
		s := a
		if r.IntN(2) == 0 {
			s = b
		}
		switch r.IntN(6) {
		case 0:
			s.RecordSignOut(fmt.Sprint("s", k), tag(fmt.Sprint("p", r.IntN(5))), clock.Now().Add(time.Hour))
		case 1:
			s.KeepDrill(passed(fmt.Sprint("drill-", k), clock.Now()))
		case 2:
			s.MarkDigestSent(clock.Now().UTC().Format("20060102T1504Z"), "email", clock.Now())
		case 3:
			exchange(a, b)
		default:
			s.InstanceSince(fmt.Sprint("pod-", r.IntN(300)), strconv.Itoa(r.IntN(3)), clock.Now())
		}
		for _, s := range []*Store{a, b} {
			s.mu.Lock()
			docs := s.pruneLocked()
			fresh := s.encodeLocked()
			if !maps.Equal(docs, fresh) {
				s.mu.Unlock()
				t.Fatalf("step %d: the documents a prune returned are not the state's", k)
			}
			if s.inSyncWithLocked(docs) != s.inSyncLocked() {
				s.mu.Unlock()
				t.Fatalf("step %d: in sync differs", k)
			}
			if !maps.Equal(s.desiredWithLocked(docs).Data, s.desiredLocked().Data) {
				s.mu.Unlock()
				t.Fatalf("step %d: desired differs", k)
			}
			s.mu.Unlock()
		}
	}
}

// What the lock is held for at the worst bounds: a prune and its
// comparison (writer, pruner, merge), and a merge of the ConfigMap.
func BenchmarkPruneAtTheBounds(b *testing.B) {
	s := New(Options{Name: "console-state", Now: (&testClock{now: t0}).Now})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeLocked(&corev1.ConfigMap{})
	fillToTheBounds(s, true, drillAtTheBounds)
	s.pruneLocked()
	b.Run("prune+inSync", func(b *testing.B) {
		for b.Loop() {
			_ = s.inSyncWithLocked(s.pruneLocked())
		}
	})
	cm := s.desiredLocked()
	b.Run("merge", func(b *testing.B) {
		for b.Loop() {
			s.mergeLocked(cm)
		}
	})
}
