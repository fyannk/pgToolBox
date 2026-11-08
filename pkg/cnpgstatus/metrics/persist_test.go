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

package metrics

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type persistClock struct {
	mu    sync.Mutex
	now   time.Time
	waits int
}

func (c *persistClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *persistClock) Wait(ctx context.Context, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waits == 0 {
		return context.Canceled
	}
	c.waits--
	c.now = c.now.Add(flushEvery)
	return ctx.Err()
}

func quietLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

func TestSnapshotRoundTripRestoresTheWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.snapshot")
	logger, _ := quietLogger()

	// Fill a store: gauges, a counter (so a rate lands), rollups.
	a := testStore()
	for i := 0; i < 12; i++ {
		a.Observe("orders-1", tick(i), map[string]float64{
			"connections": float64(i), "xact-commit": float64(1000 + i*100),
		}, map[string]float64{"nodes-used": 3})
	}
	clock := &persistClock{now: tick(12)}
	pa, err := OpenPersister(path, a, clock, logger)
	if err != nil {
		t.Fatalf("OpenPersister: %v", err)
	}
	if err := pa.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// A fresh store on the same file reads the identical window back.
	b := testStore()
	if _, err := OpenPersister(path, b, clock, logger); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	for _, tier := range []Tier{TierRaw, TierRollup} {
		for _, key := range []string{"connections", "xact-commit"} {
			at, av := a.Range(key, tier)
			bt, bv := b.Range(key, tier)
			if !reflect.DeepEqual(at, bt) || !reflect.DeepEqual(av, bv) {
				t.Fatalf("tier %d key %s differs after restore", tier, key)
			}
		}
	}

	// The counter baseline is not persisted: the first post-restart
	// sweep claims no rate, so the downtime reads as a gap.
	before, _ := b.Range("xact-commit", TierRaw)
	b.Observe("orders-1", tick(20), map[string]float64{"xact-commit": 9999}, nil)
	after, _ := b.Range("xact-commit", TierRaw)
	if len(after) != len(before) {
		t.Fatal("a restored counter claimed a rate across the restart")
	}
}

func TestUnreadableSnapshotStartsEmptyAndSaysSo(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "metrics.snapshot")
	if err := os.WriteFile(path, []byte("not a snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, logs := quietLogger()
	store := testStore()
	if _, err := OpenPersister(path, store, &persistClock{now: t0}, logger); err != nil {
		t.Fatalf("OpenPersister: %v", err)
	}
	if got := store.Instances(); len(got) != 0 {
		t.Fatalf("instances = %v, want an empty start", got)
	}
	if !strings.Contains(logs.String(), "metrics snapshot unreadable") {
		t.Fatal("the discarded snapshot was not stated")
	}
}

func TestUnwritablePathFailsBeforeListen(t *testing.T) {
	t.Parallel()
	logger, _ := quietLogger()
	path := filepath.Join(t.TempDir(), "missing", "metrics.snapshot")
	if _, err := OpenPersister(path, testStore(), &persistClock{now: t0}, logger); err == nil {
		t.Fatal("an unwritable path did not refuse")
	}
}

func TestRunTakesAFinalSnapshotOnShutdown(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "metrics.snapshot")
	logger, _ := quietLogger()
	store := testStore()
	clock := &persistClock{now: t0, waits: 1}
	p, err := OpenPersister(path, store, clock, logger)
	if err != nil {
		t.Fatalf("OpenPersister: %v", err)
	}
	// Samples land after the last periodic flush; only the final
	// shutdown snapshot can carry them.
	store.Observe("orders-1", tick(1), map[string]float64{"connections": 5}, nil)
	if err := p.Run(context.Background()); !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("Run: %v", err)
	}
	restored := testStore()
	if _, err := OpenPersister(path, restored, clock, logger); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := restored.Instances(); len(got) != 1 {
		t.Fatalf("instances after shutdown snapshot = %v", got)
	}
}

func TestImportClampsToTheConfiguredBounds(t *testing.T) {
	t.Parallel()
	// A snapshot from a generous configuration lands in a tight one:
	// four instances into a cap of three, more raw samples than the
	// window holds. The most recently observed instances win and the
	// rings keep only what their capacity allows.
	big := NewStore(Instance, Limits{
		Interval: 10 * time.Second, RawWindow: time.Hour,
		Retention: 24 * time.Hour, RollupEvery: time.Minute, MaxInstances: 8,
	})
	for i := 0; i < 100; i++ {
		for n, name := range []string{"a-1", "a-2", "a-3", "a-4"} {
			big.Observe(name, tick(i+n), map[string]float64{"connections": float64(i)}, nil)
		}
	}
	small := NewStore(Instance, Limits{
		Interval: 10 * time.Second, RawWindow: time.Minute,
		Retention: time.Hour, RollupEvery: time.Minute, MaxInstances: 3,
	})
	small.Import(big.Export(tick(200)), tick(104))
	if got := small.Instances(); len(got) != 3 || got[0] != "a-2" {
		t.Fatalf("instances = %v, want the three most recently observed", got)
	}
	times, _ := small.Range("connections", TierRaw)
	if len(times) > 8 {
		t.Fatalf("raw window holds %d timestamps, want it clamped to capacity", len(times))
	}
}

func TestImportLaysSeriesOfDifferentSweepsOnOneAxis(t *testing.T) {
	t.Parallel()
	s := testStore()
	s.Import(PersistedState{Version: snapshotVersion, Instruments: []PersistedInstrument{
		{Instance: "orders-1", Series: "connections", LastAt: tick(2).Unix(), Raw: []PersistedSample{
			{At: tick(0).Unix(), Val: 1}, {At: tick(2).Unix(), Val: 3},
		}},
		{Instance: "orders-1", Series: "xact-commit", LastAt: tick(2).Unix(), Raw: []PersistedSample{
			{At: tick(1).Unix(), Val: 20},
		}},
	}}, tick(3))
	s.Observe("orders-1", tick(3), map[string]float64{"connections": 4}, nil)

	times, byInstance := s.Range("connections", TierRaw)
	col := byInstance["orders-1"]
	// Sweep 1 claimed no connections: the read side breaks the line there.
	if len(times) != 4 || *col[0] != 1 || col[1] != nil || *col[2] != 3 || *col[3] != 4 {
		t.Fatalf("connections = %v / %v, want 1, gap, 3, 4 at sweeps 0–3", times, col)
	}
	times, byInstance = s.Range("xact-commit", TierRaw)
	if len(times) != 1 || times[0] != tick(1).Unix() || *byInstance["orders-1"][0] != 20 {
		t.Fatalf("xact-commit = %v / %v, want 20 at sweep 1 only", times, byInstance["orders-1"])
	}
	if got := s.Export(tick(4)); len(got.Instruments) != 2 || len(got.Instruments[0].Raw) != 3 || len(got.Instruments[1].Raw) != 1 {
		t.Fatalf("re-export = %+v, want the gaps left out", got.Instruments)
	}
}

func TestImportDropsWhatTheWindowsNoLongerHold(t *testing.T) {
	t.Parallel()
	// Raw window 1 h, rollups 24 h: a snapshot read 2 h after its last
	// sample keeps the rollups and no raw sample; one read 25 h later
	// keeps nothing at all, so the instance is not even listed.
	src := testStore()
	for i := range 30 {
		src.Observe("orders-1", tick(i), map[string]float64{"connections": float64(i)}, nil)
	}
	saved := src.Export(tick(30))

	later := testStore()
	later.Import(saved, tick(30).Add(2*time.Hour))
	if times, _ := later.Range("connections", TierRaw); len(times) != 0 {
		t.Fatalf("raw tier = %v, want nothing older than the raw window", times)
	}
	if times, _ := later.Range("connections", TierRollup); len(times) == 0 {
		t.Fatal("rollup tier is empty, want the buckets still inside the retention")
	}

	muchLater := testStore()
	muchLater.Import(saved, tick(30).Add(25*time.Hour))
	if got := muchLater.Instances(); len(got) != 0 {
		t.Fatalf("instances = %v, want none: every sample is past the retention", got)
	}
}

func TestCounterRestartGapIsAReset(t *testing.T) {
	t.Parallel()
	// Before the restart the counter grows 10/s; the console is down for
	// ten minutes while it grows by 60,000. The first sweep after the
	// restart must claim no rate (no baseline survived the snapshot), and
	// the next one the true 10/s, never the 100/s average across the gap.
	src := testStore()
	for i := range 4 {
		src.Observe("orders-1", tick(i), map[string]float64{"xact-commit": float64(1000 + 100*i)}, nil)
	}
	dst := testStore()
	dst.Import(src.Export(tick(4)), tick(64))
	dst.Observe("orders-1", tick(64), map[string]float64{"xact-commit": 61300}, nil)
	dst.Observe("orders-1", tick(65), map[string]float64{"xact-commit": 61400}, nil)

	times, byInstance := dst.Range("xact-commit", TierRaw)
	col := byInstance["orders-1"]
	var claimed []float64
	for i, at := range times {
		if col[i] != nil && at >= tick(64).Unix() {
			claimed = append(claimed, *col[i])
		}
	}
	if len(claimed) != 1 || claimed[0] != 10 {
		t.Fatalf("post-restart rates = %v, want exactly one claim of 10/s", claimed)
	}
}
