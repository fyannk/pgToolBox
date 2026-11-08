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

package status

import (
	"context"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// sharedPhases answers an hour earlier for the orders Cluster's phase.
type sharedPhases struct{ asked map[string]string }

func (s *sharedPhases) PhaseSince(uid, phase string, local time.Time) time.Time {
	s.asked[uid] = phase
	if earlier := now.Add(-time.Hour); uid == string(ordersUID) && earlier.Before(local) {
		return earlier
	}
	return local
}

func TestPhaseClockTakesTheSharedEarliest(t *testing.T) {
	ctx := context.Background()
	svc := newService(ctx, t, nil)
	shared := &sharedPhases{asked: map[string]string{}}
	svc.opts.Shared = shared
	g, err := svc.gather(ctx, cache.ClusterKey{Namespace: "team-a", Name: "orders"}, false)
	if err != nil {
		t.Fatal(err)
	}
	since := svc.phaseSince(g)
	if since == nil || !since.Equal(now.Add(-time.Hour)) || shared.asked[string(ordersUID)] != "Cluster in healthy state" {
		t.Fatalf("since = %v, asked %v", since, shared.asked)
	}
	// The clock keeps the shared time across reads.
	if again := svc.phaseSince(g); !again.Equal(*since) {
		t.Fatalf("second read = %v", again)
	}
}

func TestLiveUIDs(t *testing.T) {
	ctx := context.Background()
	svc := newService(ctx, t, nil)
	live, ok := svc.LiveUIDs(ctx)
	if !ok || !live[string(ordersUID)] || !live[string(billingUID)] || !live["billing-1-uid"] {
		t.Fatalf("live = %v, %v", live, ok)
	}
	// A namespace the cache cannot read gives no view at all: pruning on
	// it would forget every Cluster there.
	svc = newService(ctx, t, map[string]viewmodel.SourceState{cache.SourcePods: {Source: cache.SourcePods, State: viewmodel.SourceForbidden}})
	if _, ok := svc.LiveUIDs(ctx); ok {
		t.Fatal("a partial view was offered for pruning")
	}
}
