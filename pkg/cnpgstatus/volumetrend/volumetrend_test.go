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

package volumetrend

import (
	"bytes"
	"context"
	"encoding/gob"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const gib = 1 << 30

var (
	t0  = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	key = Key{Namespace: "team-a", Cluster: "orders", Instance: "orders-1", Volume: "pgdata"}
)

// feed observes one reading per cadence for the given number of
// samples, consumed bytes given by f(i) on a 100 GiB volume.
func feed(s *Store, k Key, from time.Time, samples int, f func(i int) float64) time.Time {
	every := s.Limits().Every
	at := from
	for i := range samples {
		consumed := uint64(f(i))
		s.Observe(k, Reading{
			At: at, TotalBytes: 100 * gib, AvailableBytes: 100*gib - consumed, UsedBytes: consumed,
			Inodes: 1_000_000, InodesFree: 1_000_000 - uint64(1000+i), HasInodes: true, Source: SourceStatus,
		})
		at = at.Add(every)
	}
	return at.Add(-every)
}

func TestTheFirstReadingOfASlotIsTheSampleAndGapsStayGaps(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	r := func(at time.Time, avail uint64) Reading {
		return Reading{At: at, TotalBytes: 10 * gib, AvailableBytes: avail}
	}
	s.Observe(key, r(t0, 9*gib))
	s.Observe(key, r(t0.Add(5*time.Minute), 8*gib)) // same slot: refreshes Latest only
	s.Observe(key, r(t0.Add(45*time.Minute), 7*gib))
	vols := s.Volumes("team-a", "orders")
	if len(vols) != 1 || vols[0].Samples != 2 || vols[0].Latest.AvailableBytes != 7*gib || !vols[0].Since.Equal(t0) {
		t.Fatalf("volumes = %+v", vols)
	}
	points := s.Downsample(key, 100)
	if len(points) != 4 || points[0].Bytes == nil || *points[0].Bytes != gib || points[1].Bytes != nil || points[2].Bytes != nil || *points[3].Bytes != 3*gib {
		t.Fatalf("points = %+v", points)
	}
	if points[0].Inodes != nil {
		t.Error("a filesystem without inodes produced an inode sample")
	}
	// A reading from the past refreshes nothing.
	s.Observe(key, r(t0.Add(10*time.Minute), 1))
	if s.Volumes("team-a", "orders")[0].Latest.AvailableBytes != 7*gib {
		t.Error("an older reading replaced the latest")
	}
	// A contradictory reading is refused.
	s.Observe(key, Reading{At: t0.Add(time.Hour), TotalBytes: 1, AvailableBytes: 2})
	if s.Volumes("team-a", "orders")[0].Latest.TotalBytes != 10*gib {
		t.Error("available above total was recorded")
	}
}

func TestTheRingIsBoundedByRetentionOverCadence(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	if got := s.Limits().Capacity(); got != 2880 {
		t.Fatalf("capacity = %d, want 2,880 samples (30 days every 15 minutes)", got)
	}
	feed(s, key, t0, 3000, func(i int) float64 { return float64(i) })
	ser := s.series[key]
	if len(ser.buf) != 2880 || cap(ser.buf) > 2*2880 {
		t.Fatalf("ring len %d cap %d", len(ser.buf), cap(ser.buf))
	}
	points := s.Downsample(key, 2880)
	if *points[0].Bytes != 120 || *points[len(points)-1].Bytes != 2999 {
		t.Errorf("ring kept %v .. %v, want the newest 2,880", *points[0].Bytes, *points[len(points)-1].Bytes)
	}
	// A silence longer than the retention starts the series over.
	s.Observe(key, Reading{At: t0.Add(70 * 24 * time.Hour), TotalBytes: 10, AvailableBytes: 5})
	if got := len(s.series[key].buf); got != 1 {
		t.Errorf("after a long silence the ring holds %d samples, want 1", got)
	}
}

func TestSeriesAreCappedAndForgotten(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{MaxSeries: 2})
	reading := func(at time.Time) Reading { return Reading{At: at, TotalBytes: 10, AvailableBytes: 5} }
	a, b, c := key, key, key
	b.Volume, c.Volume = "wal", "tbs-hist"
	other := Key{Namespace: "team-a", Cluster: "billing", Instance: "billing-1", Volume: "pgdata"}
	// Past the cap a new series is refused, every sweep, and the series
	// already held keep growing: no thrash, no lost history.
	for i := range 8 {
		at := t0.Add(time.Duration(i) * time.Hour)
		s.Observe(a, reading(at))
		s.Observe(b, reading(at))
		s.Observe(c, reading(at))
		s.Observe(other, reading(at))
	}
	if len(s.series) != 2 || len(s.series[a].buf) != 29 || len(s.series[b].buf) != 29 {
		t.Fatalf("the held series lost history at the cap: %d series", len(s.series))
	}
	if _, ok := s.series[c]; ok {
		t.Error("a series past the cap was admitted")
	}
	if got := s.Refused(); got != 16 {
		t.Errorf("refused = %d, want 16", got)
	}
	if !s.RefusedFor("team-a", "billing", t0.Add(7*time.Hour)) || !s.RefusedFor("team-a", "orders", t0) || s.RefusedFor("team-a", "orders", t0.Add(8*time.Hour)) {
		t.Error("RefusedFor does not follow the refusals")
	}
	// Silent for a whole retention: forgotten, which makes room.
	s.Observe(c, reading(t0.Add(40*24*time.Hour)))
	if len(s.series) != 1 {
		t.Errorf("silent series were kept: %d", len(s.series))
	}
	s.Observe(other, reading(t0.Add(40*24*time.Hour)))
	if got := s.Volumes("team-a", "orders"); len(got) != 1 || got[0].Key != c {
		t.Errorf("the per-Cluster index kept a forgotten series: %+v", got)
	}
	if s.RefusedFor("team-a", "billing", t0) {
		t.Error("an old refusal outlived the prune")
	}
	if !s.Reported("team-a", "orders", t0.Add(39*24*time.Hour)) || s.Reported("team-a", "orders", t0.Add(41*24*time.Hour)) || s.Reported("team-a", "nobody", t0) {
		t.Error("Reported does not follow the latest readings")
	}
}

func BenchmarkObserveAtTheCap(b *testing.B) {
	s := NewStore(Limits{MaxSeries: 10_000})
	for i := range 10_000 {
		s.Observe(Key{Namespace: "n", Cluster: "c", Instance: strconv.Itoa(i), Volume: "pgdata"}, Reading{At: t0, TotalBytes: 10, AvailableBytes: 5})
	}
	refused := Key{Namespace: "n", Cluster: "late", Instance: "x", Volume: "pgdata"}
	b.ResetTimer()
	for i := range b.N {
		s.Observe(refused, Reading{At: t0.Add(time.Duration(i) * time.Second), TotalBytes: 10, AvailableBytes: 5})
	}
}

func TestSizeChangesAreStepsNotSamples(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	for i, total := range []uint64{10, 10, 20, 20, 30} {
		s.Observe(key, Reading{At: t0.Add(time.Duration(i) * 15 * time.Minute), TotalBytes: total * gib, AvailableBytes: gib})
	}
	if steps := s.series[key].steps; len(steps) != 3 || steps[2].total != 30*gib {
		t.Errorf("steps = %+v", steps)
	}
}

func day(n float64) time.Duration { return time.Duration(n * 24 * float64(time.Hour)) }

func TestForecastExtrapolatesALinearGrowth(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	// 1 GiB a day from 40 GiB, for 10 days: 50 GiB of 100 used, 50 left.
	perSample := float64(gib) / 96
	end := feed(s, key, t0, 960, func(i int) float64 { return 40*gib + float64(i)*perSample })
	bytes, _, inodes, ok := s.Forecasts(key, Options{})
	if !ok || bytes.State != StateForecast {
		t.Fatalf("bytes = %+v", bytes)
	}
	if math.Abs(bytes.RatePerDay-gib)/gib > 0.01 {
		t.Errorf("rate = %.0f bytes/day, want 1 GiB/day", bytes.RatePerDay)
	}
	want := end.Add(day(50))
	if bytes.FullAt == nil || bytes.FullAt.Sub(want).Abs() > 6*time.Hour {
		t.Errorf("full at %v, want about %v", bytes.FullAt, want)
	}
	if bytes.WindowHours != 168 || bytes.WindowEnd.Sub(bytes.WindowStart) != 167*time.Hour {
		t.Errorf("window %d hours %v..%v, want the last 7 days", bytes.WindowHours, bytes.WindowStart, bytes.WindowEnd)
	}
	if bytes.Confidence != ConfidenceHigh || bytes.Accelerating || bytes.Target != TargetFilesystem || bytes.AutoGrowSteps {
		t.Errorf("forecast = %+v", bytes)
	}
	// Inodes: one more in use per sample, 96 a day; ~989,000 free.
	if inodes.State != StateForecast || math.Abs(inodes.RatePerDay-96) > 1 {
		t.Errorf("inodes = %+v", inodes)
	}
}

func TestForecastRefusesWhatItCannotSupport(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		samples int
		f       func(i int) float64
		want    State
	}{
		{"half a day of samples", 48, func(i int) float64 { return float64(i) * gib / 10 }, StateTooFewPoints},
		{"flat", 960, func(int) float64 { return 40 * gib }, StateNotGrowing},
		{"shrinking", 960, func(i int) float64 { return 60*gib - float64(i)*1e6 }, StateNotGrowing},
		{"noise around a level", 960, func(i int) float64 { return 40*gib + float64(i%2)*1e8 }, StateNotGrowing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := NewStore(Limits{})
			feed(s, key, t0, tc.samples, tc.f)
			bytes, _, _, _ := s.Forecasts(key, Options{})
			if bytes.State != tc.want || bytes.FullAt != nil {
				t.Errorf("state %s full %v, want %s and no date", bytes.State, bytes.FullAt, tc.want)
			}
		})
	}
	s := NewStore(Limits{})
	s.Observe(key, Reading{At: t0, TotalBytes: 10, AvailableBytes: 0})
	if bytes, _, inodes, _ := s.Forecasts(key, Options{}); bytes.State != StateFull || inodes.State != StateNoData {
		t.Errorf("full volume: bytes %s inodes %s", bytes.State, inodes.State)
	}
}

func TestForecastIsRobustToADropAndASpike(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	perSample := float64(gib) / 96
	feed(s, key, t0, 960, func(i int) float64 {
		v := 40*gib + float64(i)*perSample
		switch {
		case i == 900: // a spike: a temporary file
			v += 30 * gib
		case i > 700: // a VACUUM FULL gave 2 GiB back
			v -= 2 * gib
		}
		return v
	})
	bytes, _, _, _ := s.Forecasts(key, Options{})
	if bytes.State != StateForecast || math.Abs(bytes.RatePerDay-gib)/gib > 0.1 {
		t.Errorf("rate = %.3f GiB/day (state %s), want about 1", bytes.RatePerDay/gib, bytes.State)
	}
}

func TestForecastSaysFasterThanLinear(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	// The rate doubles halfway through the last week.
	feed(s, key, t0, 960, func(i int) float64 {
		perSample := float64(gib) / 96
		if i > 960-336 {
			return 40*gib + float64(960-336)*perSample + float64(i-(960-336))*2*perSample
		}
		return 40*gib + float64(i)*perSample
	})
	bytes, _, _, _ := s.Forecasts(key, Options{})
	if !bytes.Accelerating || bytes.State != StateForecast {
		t.Fatalf("forecast = %+v", bytes)
	}
	if math.Abs(bytes.RatePerDay-2*gib)/gib > 0.1 || bytes.RatePerDay != bytes.RecentRatePerDay {
		t.Errorf("rate = %.2f GiB/day, want the recent half's 2", bytes.RatePerDay/gib)
	}
	if bytes.Confidence == ConfidenceHigh {
		t.Error("an accelerating curve was graded high confidence")
	}
}

func TestForecastRunsTowardTheAutoGrowMaximum(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	perSample := float64(gib) / 96
	end := feed(s, key, t0, 960, func(i int) float64 { return 40*gib + float64(i)*perSample })
	maxSize := uint64(200 * gib)
	bytes, grow, inodes, _ := s.Forecasts(key, Options{AutoGrow: true, AutoGrowMaxBytes: &maxSize})
	// The filesystem forecast stays: it is when this filesystem fills if
	// no step comes.
	if bytes.Target != TargetFilesystem || !bytes.AutoGrowSteps || bytes.Capacity != 100*gib || bytes.State != StateForecast {
		t.Fatalf("filesystem forecast = %+v", bytes)
	}
	if grow == nil || grow.Target != TargetAutoGrow || !grow.AutoGrowSteps || grow.Capacity != 200*gib {
		t.Fatalf("autoGrow forecast = %+v", grow)
	}
	if want := end.Add(day(150)); grow.FullAt.Sub(want).Abs() > 12*time.Hour {
		t.Errorf("maxSize at %v, want about %v", grow.FullAt, want)
	}
	if !bytes.FullAt.Before(*grow.FullAt) {
		t.Error("the filesystem fills after maxSize")
	}
	if !inodes.AutoGrowSteps || inodes.Capacity != 1_000_000 {
		t.Errorf("inodes = %+v", inodes)
	}
	reached := uint64(100 * gib)
	bytes, grow, _, _ = s.Forecasts(key, Options{AutoGrow: true, AutoGrowMaxBytes: &reached})
	if bytes.Target != TargetFilesystem || bytes.AutoGrowSteps || grow != nil {
		t.Errorf("maxSize reached: %+v %+v", bytes, grow)
	}
	bytes, grow, _, _ = s.Forecasts(key, Options{AutoGrow: true})
	if bytes.Target != TargetFilesystem || !bytes.AutoGrowSteps || grow != nil {
		t.Errorf("maxSize unread: %+v %+v", bytes, grow)
	}
}

func TestAFullFilesystemIsFullWhateverAutoGrowMayDo(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	maxSize := uint64(200 * gib)
	opts := Options{AutoGrow: true, AutoGrowMaxBytes: &maxSize}
	for _, available := range []uint64{0, 10 << 20} {
		s.Observe(key, Reading{At: t0.Add(time.Duration(available)), TotalBytes: 100 * gib, AvailableBytes: available})
		bytes, grow, _, _ := s.Forecasts(key, opts)
		if bytes.State != StateFull || bytes.Target != TargetFilesystem {
			t.Errorf("%d bytes free: the filesystem is %s toward %s", available, bytes.State, bytes.Target)
		}
		if grow == nil || grow.State == StateFull {
			t.Errorf("%d bytes free: the autoGrow headroom is not full: %+v", available, grow)
		}
	}
	// Above the floor, not full.
	s.Observe(key, Reading{At: t0.Add(time.Hour), TotalBytes: 100 * gib, AvailableBytes: gib})
	if bytes, _, _, _ := s.Forecasts(key, opts); bytes.State == StateFull {
		t.Error("1 GiB free of 100 GiB is called full")
	}
}

func TestForecastsAreCachedUntilANewSample(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	end := feed(s, key, t0, 200, func(i int) float64 { return float64(i) * 1e7 })
	s.Forecasts(key, Options{})
	version := s.fits[key].version
	s.Observe(key, Reading{At: end.Add(time.Minute), TotalBytes: 100 * gib, AvailableBytes: 50 * gib})
	s.Forecasts(key, Options{})
	if s.fits[key].version != version {
		t.Error("a reading inside the slot recomputed the fit")
	}
	if b, _, _, _ := s.Forecasts(key, Options{}); b.Headroom != 50*gib {
		t.Errorf("headroom %v, want the latest reading's", b.Headroom)
	}
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Wait(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestSnapshotRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "volume-trends.gob")
	s := NewStore(Limits{})
	end := feed(s, key, t0, 960, func(i int) float64 { return 40*gib + float64(i)*gib/96 })
	clock := &fakeClock{now: end}
	before, _, _, _ := s.Forecasts(key, Options{})
	p, err := OpenPersister(path, s, clock, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = p.Run(ctx)

	restored := NewStore(Limits{})
	if _, err := OpenPersister(path, restored, clock, nil); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	after, _, _, ok := restored.Forecasts(key, Options{})
	if !ok || after.FullAt == nil || !after.FullAt.Equal(*before.FullAt) || after.WindowHours != before.WindowHours {
		t.Errorf("after restart %+v, before %+v", after, before)
	}
	if len(restored.series[key].steps) != len(s.series[key].steps) {
		t.Error("size changes were not restored")
	}

	other := NewStore(Limits{Every: 5 * time.Minute})
	if _, err := OpenPersister(path, other, clock, nil); err != nil {
		t.Fatalf("reopen at another cadence: %v", err)
	}
	if len(other.Volumes("team-a", "orders")) != 0 {
		t.Error("a snapshot of another cadence was imported")
	}

	if err := os.WriteFile(path, []byte("not gob"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := NewStore(Limits{})
	if _, err := OpenPersister(path, empty, clock, nil); err != nil || len(empty.series) != 0 {
		t.Errorf("an unreadable snapshot: err %v, %d series", err, len(empty.series))
	}
	if _, err := OpenPersister(filepath.Join(t.TempDir(), "missing", "x.gob"), NewStore(Limits{}), &fakeClock{now: t0}, nil); err == nil {
		t.Error("an unwritable path opened")
	}
}

func TestImportRefusesWhatObserveWould(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	end := feed(s, key, t0, 200, func(i int) float64 { return 40*gib + float64(i)*1e6 })
	good := s.Export(end)
	if len(good.Series) != 1 {
		t.Fatalf("export: %d series", len(good.Series))
	}
	mutate := func(f func(p *PersistedSeries)) PersistedState {
		state := good
		p := good.Series[0]
		p.Bytes = append([]float32(nil), p.Bytes...)
		p.Inodes = append([]float32(nil), p.Inodes...)
		p.Steps = append([]PersistedStep(nil), p.Steps...)
		f(&p)
		state.Series = []PersistedSeries{p}
		return state
	}
	long := strings.Repeat("x", 254)
	cases := map[string]PersistedState{
		"free inodes above the total":    mutate(func(p *PersistedSeries) { p.Latest.InodesFree = p.Latest.Inodes + 1 }),
		"available above the total":      mutate(func(p *PersistedSeries) { p.Latest.AvailableBytes = p.Latest.TotalBytes + 1 }),
		"a latest reading in the future": mutate(func(p *PersistedSeries) { p.Latest.At = end.Add(24 * time.Hour) }),
		"a newest slot in the future":    mutate(func(p *PersistedSeries) { p.Newest += 1000 }),
		"a newest slot before latest":    mutate(func(p *PersistedSeries) { p.Newest -= 10 }),
		"an unbounded key":               mutate(func(p *PersistedSeries) { p.Key.Instance = long }),
		"an empty key part":              mutate(func(p *PersistedSeries) { p.Key.Volume = "" }),
		"rings of unequal length":        mutate(func(p *PersistedSeries) { p.Inodes = p.Inodes[1:] }),
		"a negative sample":              mutate(func(p *PersistedSeries) { p.Bytes[3] = -1 }),
		"an infinite sample":             mutate(func(p *PersistedSeries) { p.Bytes[3] = float32(math.Inf(1)) }),
		"no steps":                       mutate(func(p *PersistedSeries) { p.Steps = nil }),
		"unsorted steps": mutate(func(p *PersistedSeries) {
			p.Steps = append(p.Steps, PersistedStep{Slot: p.Steps[0].Slot, Total: 1})
		}),
		"a step after the newest sample": mutate(func(p *PersistedSeries) { p.Steps[0].Slot = p.Newest + 1 }),
		"too many steps": mutate(func(p *PersistedSeries) {
			for i := range maxSteps {
				p.Steps = append(p.Steps, PersistedStep{Slot: p.Steps[0].Slot - int64(maxSteps-i), Total: 1})
			}
			sort.Slice(p.Steps, func(i, j int) bool { return p.Steps[i].Slot < p.Steps[j].Slot })
		}),
	}
	for name, state := range cases {
		fresh := NewStore(Limits{})
		if n, ok := fresh.Import(state, end); !ok || n != 0 || len(fresh.series) != 0 {
			t.Errorf("%s: imported %d (ok %v)", name, n, ok)
		}
	}
	fresh := NewStore(Limits{})
	if n, ok := fresh.Import(good, end); !ok || n != 1 {
		t.Errorf("the good snapshot: %d %v", n, ok)
	}
	old := good
	old.Version = 0
	if n, ok := NewStore(Limits{}).Import(old, end); ok || n != 0 {
		t.Error("an older layout was imported")
	}
}

func TestATruncatedOrOversizedSnapshotIsRefused(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	end := feed(s, key, t0, 200, func(i int) float64 { return float64(i) * 1e6 })
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(s.Export(end)); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	if _, err := decodeSnapshot(bytes.NewReader(raw[:len(raw)/2]), int64(len(raw))); err == nil {
		t.Error("a truncated snapshot decoded")
	}
	if _, err := decodeSnapshot(bytes.NewReader(raw), int64(len(raw)/2)); err == nil {
		t.Error("a snapshot over its bound decoded")
	}
	if _, err := decodeSnapshot(bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Errorf("a snapshot at its bound: %v", err)
	}
}

func FuzzSnapshot(f *testing.F) {
	s := NewStore(Limits{})
	end := feed(s, key, t0, 120, func(i int) float64 { return float64(i) * 1e6 })
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(s.Export(end)); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add([]byte("not gob"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		store := NewStore(Limits{MaxSeries: 64})
		state, err := decodeSnapshot(bytes.NewReader(raw), store.maxSnapshotBytes())
		if err != nil {
			return
		}
		store.Import(state, end)
		capacity := store.Limits().Capacity()
		if len(store.series) > 64 {
			t.Fatalf("%d series past the cap", len(store.series))
		}
		for k, ser := range store.series {
			if len(ser.buf) > capacity || len(ser.steps) > maxSteps {
				t.Fatalf("%v: ring %d steps %d", k, len(ser.buf), len(ser.steps))
			}
			store.Forecasts(k, Options{})
			store.Downsample(k, 50)
		}
	})
}

// feedResized observes 10 days of readings on a 100 GiB filesystem that
// grows to 200 GiB resizeBefore the end; the resize adds 5 GiB of
// reserved blocks to consumed (ext4's 5 %), and growth is perDay.
func feedResized(s *Store, resizeBefore time.Duration, perDay float64, inodesTotalChanges bool) {
	const samples = 960
	end := t0.Add(time.Duration(samples-1) * 15 * time.Minute)
	at := t0
	for i := range samples {
		total, consumed, inodes := uint64(100*gib), 40*gib+float64(i)*perDay/96, uint64(1_000_000)
		if !at.Before(end.Add(-resizeBefore)) {
			total, consumed = 200*gib, consumed+5*gib
			if inodesTotalChanges {
				inodes = 1_500_000
			}
		}
		s.Observe(key, Reading{
			At: at, TotalBytes: total, AvailableBytes: total - uint64(consumed), UsedBytes: uint64(consumed),
			Inodes: inodes, InodesFree: inodes - uint64(1000+i), HasInodes: true, Source: SourceStatus,
		})
		at = at.Add(15 * time.Minute)
	}
}

func TestAResizeIsNotGrowth(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		resizeBefore time.Duration
		perDay       float64
		state        State
		sinceResize  bool
	}{
		// Negative: a step in the middle of the window splits the pairs
		// about evenly, so the median slope is one that crosses the step:
		// before resizes were handled, a flat volume read as growing
		// about 0.7 GiB a day.
		{"flat, resized mid-window: fit since the resize", 84 * time.Hour, 0, StateNotGrowing, true},
		{"flat, resized three days ago: fit since the resize", 3 * 24 * time.Hour, 0, StateNotGrowing, true},
		{"flat, resized six hours ago: the step is levelled", 6 * time.Hour, 0, StateNotGrowing, false},
		{"growing, resized six hours ago: the rate is the growth's", 6 * time.Hour, gib, StateForecast, false},
		{"growing, resized three days ago", 3 * 24 * time.Hour, gib, StateForecast, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := NewStore(Limits{})
			feedResized(s, c.resizeBefore, c.perDay, false)
			bytes, _, inodes, _ := s.Forecasts(key, Options{})
			if bytes.State != c.state || bytes.ResizedAt == nil || bytes.FitSinceResize != c.sinceResize {
				t.Fatalf("state %s resized %v since %v (rate %.2f GiB/day)", bytes.State, bytes.ResizedAt, bytes.FitSinceResize, bytes.RatePerDay/gib)
			}
			if c.state == StateForecast && math.Abs(bytes.RatePerDay-c.perDay)/c.perDay > 0.05 {
				t.Errorf("rate %.2f GiB/day, want %.2f", bytes.RatePerDay/gib, c.perDay/gib)
			}
			if inodes.State == StateTotalChanged {
				t.Error("a fixed inode total was called changed")
			}
		})
	}
}

func TestAChangingInodeTotalHasNoInodeForecast(t *testing.T) {
	t.Parallel()
	s := NewStore(Limits{})
	feedResized(s, 2*24*time.Hour, gib, true)
	_, _, inodes, _ := s.Forecasts(key, Options{})
	if inodes.State != StateTotalChanged || inodes.FullAt != nil {
		t.Errorf("inodes = %+v", inodes)
	}
	// Once the change leaves the window, the forecast comes back.
	s2 := NewStore(Limits{})
	feedResized(s2, 9*24*time.Hour, gib, true)
	if _, _, inodes, _ := s2.Forecasts(key, Options{}); inodes.State == StateTotalChanged {
		t.Errorf("a change before the window still blocks the forecast: %+v", inodes)
	}
}
