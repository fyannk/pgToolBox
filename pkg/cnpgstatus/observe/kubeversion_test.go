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

package observe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
)

// scriptedVersionSource plays fetch outcomes in order, cancelling the
// run when the script is exhausted.
type scriptedVersionSource struct {
	script []func() (string, error)
	cancel context.CancelFunc
}

func (s *scriptedVersionSource) FetchServerVersion(context.Context) (string, error) {
	if len(s.script) == 0 {
		s.cancel()
		return "", errors.New("script exhausted")
	}
	next := s.script[0]
	s.script = s.script[1:]
	return next()
}

// TestKubeVersionPollerRetainsLastGoodAcrossFailure proves the polling
// contract: a success publishes, a failure marks the retained snapshot
// stale without erasing it, and the wait shortens after a failure.
func TestKubeVersionPollerRetainsLastGoodAcrossFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	source := &scriptedVersionSource{cancel: cancel, script: []func() (string, error){
		func() (string, error) { return "v1.33.1", nil },
		func() (string, error) { return "", errors.New("unreachable") },
	}}
	store := NewKubeVersionStore()
	clock := newFakeClock()
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))

	if err := NewKubeVersionPoller(source, store, clock, logger).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	snap, has := store.CurrentKubeVersion()
	if !has {
		t.Fatal("no snapshot after a successful poll")
	}
	if snap.GitVersion != "v1.33.1" {
		t.Errorf("GitVersion = %q, want the last-good observation retained", snap.GitVersion)
	}
	if !snap.Stale {
		t.Error("snapshot not marked stale after the failed poll")
	}
	if snap.ObservedAt.IsZero() {
		t.Error("ObservedAt not stamped")
	}

	waits := clock.recorded()
	if len(waits) < 2 || waits[0] != kubeVersionInterval || waits[1] != kubeVersionRetry {
		t.Errorf("waits = %v, want the interval then the shortened retry", waits)
	}
}

// TestKubeVersionStoreUnobservedReportsNothing pins the empty store's
// answer: no snapshot, not a zero-valued one.
func TestKubeVersionStoreUnobservedReportsNothing(t *testing.T) {
	t.Parallel()
	if _, has := NewKubeVersionStore().CurrentKubeVersion(); has {
		t.Error("an empty store claims an observation")
	}
}
