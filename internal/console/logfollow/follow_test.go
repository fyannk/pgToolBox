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

package logfollow

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
)

type roster struct {
	mu   sync.Mutex
	keys []Key
	uids map[Key]string
}

func (r *roster) Keys(context.Context) []Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Key(nil), r.keys...)
}

func (r *roster) Members(_ context.Context, k Key) []logstream.Member {
	return []logstream.Member{{Pod: k.Name + "-1", Container: "postgres"}}
}

func (r *roster) UID(_ context.Context, k Key) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if uid, ok := r.uids[k]; ok {
		return uid
	}
	return "uid-" + k.Name
}

type opener struct {
	mu   sync.Mutex
	seen []string
}

func (o *opener) FollowLogs(ctx context.Context, ns, pod, container string) (io.ReadCloser, error) {
	o.mu.Lock()
	o.seen = append(o.seen, ns+"/"+pod+"/"+container)
	o.mu.Unlock()
	r, w := io.Pipe()
	go func() {
		_, _ = w.Write([]byte("PANIC: could not locate a valid checkpoint record\n"))
		<-ctx.Done()
		_ = w.Close()
	}()
	return r, nil
}

func TestFollowerTracksTheClusters(t *testing.T) {
	ro := &roster{keys: []Key{{"team-a", "orders"}}}
	op := &opener{}
	rule := logstream.Rule{ID: "checkpoint", Contains: []string{"could not locate a valid checkpoint"}}
	f := New(Options{Roster: ro, Opener: op, Rules: []logstream.Rule{rule}, Every: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = f.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		obs := f.Observations("team-a", "orders")
		if obs != nil && len(obs.Observations()) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the matcher never saw the line")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.Observations("team-a", "billing") != nil {
		t.Error("an unwatched Cluster has observations")
	}
	ro.mu.Lock()
	ro.keys = nil
	ro.mu.Unlock()
	for f.Observations("team-a", "orders") != nil {
		if time.Now().After(deadline) {
			t.Fatal("a Cluster no longer watched is still followed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	op.mu.Lock()
	defer op.mu.Unlock()
	if len(op.seen) == 0 || !strings.HasPrefix(op.seen[0], "team-a/orders-1/postgres") {
		t.Errorf("opened %v", op.seen)
	}
}
