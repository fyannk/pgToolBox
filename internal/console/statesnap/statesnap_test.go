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

package statesnap

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

type clock struct {
	mu    sync.Mutex
	now   time.Time
	waits int
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Wait(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waits == 0 {
		return context.Canceled
	}
	c.waits--
	c.now = c.now.Add(d)
	return ctx.Err()
}

// store is a toy in-memory store: samples with their times.
type store struct {
	mu      sync.Mutex
	samples map[string]time.Time
}

type state struct{ Samples map[string]int64 }

// part snapshots the store, keeping on load only samples younger than an
// hour, as a real store keeps its retention.
func (s *store) part(version uint32, maxBytes int64) Part {
	return Gob("toy", version, maxBytes, func(time.Time) state {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := state{Samples: map[string]int64{}}
		for k, v := range s.samples {
			out.Samples[k] = v.UnixNano()
		}
		return out
	}, func(st state, _, now time.Time) (int, int) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for k, v := range st.Samples {
			if at := time.Unix(0, v).UTC(); now.Sub(at) <= time.Hour {
				s.samples[k] = at
			}
		}
		return len(s.samples), len(st.Samples)
	})
}

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func logger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

func TestRoundTripKeepsWhatIsInsideTheRetention(t *testing.T) {
	dir := t.TempDir()
	src := &store{samples: map[string]time.Time{"recent": t0.Add(-10 * time.Minute), "old": t0.Add(-50 * time.Minute)}}
	c := &clock{now: t0, waits: 2}
	s := New(Options{Dir: dir, Clock: c}, src.part(1, 1<<20))
	// Two cadences, then the shutdown's final snapshot.
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	savedAt, size, errs := s.Saved("toy")
	if !savedAt.Equal(t0.Add(2*Every)) || size <= int64(headerSize) || errs != 0 {
		t.Fatalf("saved at %v, %d bytes, %d errors", savedAt, size, errs)
	}
	if st := s.State(); st.State != viewmodel.SourceLive {
		t.Fatalf("state = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "toy"+Suffix+".tmp")); !os.IsNotExist(err) {
		t.Fatal("a temporary file was left behind")
	}

	// Read back 15 minutes later: "old" is now past the hour.
	dst := &store{samples: map[string]time.Time{}}
	r := New(Options{Dir: dir, Clock: &clock{now: t0.Add(15 * time.Minute)}}, dst.part(1, 1<<20))
	r.Load()
	if r.Loaded("toy") != "loaded" || len(dst.samples) != 1 || !dst.samples["recent"].Equal(t0.Add(-10*time.Minute)) {
		t.Fatalf("loaded %q: %v", r.Loaded("toy"), dst.samples)
	}
}

func TestUnusableFilesStartEmpty(t *testing.T) {
	write := func(t *testing.T, dir string, version uint32, payload []byte) {
		t.Helper()
		src := &store{samples: map[string]time.Time{"a": t0, "b": t0}}
		s := New(Options{Dir: dir, Clock: &clock{now: t0}}, src.part(version, 1<<20))
		s.Flush()
		if payload != nil {
			if err := os.WriteFile(filepath.Join(dir, "toy"+Suffix), payload, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name    string
		version uint32
		payload []byte
		max     int64
		want    string
	}{
		{"no file", 0, nil, 1 << 20, "none"},
		{"not a snapshot", 1, []byte("PK\x03\x04 some zip"), 1 << 20, "unreadable"},
		{"truncated header", 1, magic[:5], 1 << 20, "unreadable"},
		{"corrupt payload", 1, append(append([]byte{}, magic[:]...), 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xfe, 0xfd), 1 << 20, "unreadable"},
		{"other version", 2, nil, 1 << 20, "other version"},
		{"larger than the bound", 1, nil, 8, "too large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.version != 0 {
				write(t, dir, tc.version, tc.payload)
			}
			dst := &store{samples: map[string]time.Time{}}
			log, buf := logger()
			s := New(Options{Dir: dir, Clock: &clock{now: t0}, Logger: log}, dst.part(1, tc.max))
			s.Load()
			if got := s.Loaded("toy"); got != tc.want || len(dst.samples) != 0 {
				t.Fatalf("loaded %q with %v, want %q and nothing", got, dst.samples, tc.want)
			}
			if tc.want != "none" && strings.Count(buf.String(), "starts empty") != 1 {
				t.Fatalf("log = %s, want one warning", buf.String())
			}
		})
	}
}

func TestFailedWritesKeepThePreviousSnapshot(t *testing.T) {
	dir := t.TempDir()
	src := &store{samples: map[string]time.Time{"a": t0}}
	log, buf := logger()
	s := New(Options{Dir: dir, Clock: &clock{now: t0}, Logger: log}, src.part(1, 1<<20))
	s.Flush()
	before, err := os.ReadFile(filepath.Join(dir, "toy"+Suffix))
	if err != nil {
		t.Fatal(err)
	}
	// Grow the store past the bound: the write is refused, the file on
	// the volume is still the last good snapshot.
	for i := range 200 {
		src.samples[strings.Repeat("k", 50)+string(rune('a'+i%26))+string(rune('a'+i/26))] = t0
	}
	small := New(Options{Dir: dir, Clock: &clock{now: t0.Add(time.Minute)}, Logger: log}, src.part(1, int64(len(before))))
	small.Flush()
	small.Flush()
	after, _ := os.ReadFile(filepath.Join(dir, "toy"+Suffix))
	if !bytes.Equal(before, after) {
		t.Fatal("a refused snapshot replaced the previous one")
	}
	if _, _, errs := small.Saved("toy"); errs != 2 {
		t.Fatalf("errors = %d, want 2", errs)
	}
	st := small.State()
	if st.State != viewmodel.SourceUnavailable || !strings.Contains(st.Reason, "larger than its bound") {
		t.Fatalf("state = %+v", st)
	}
	if strings.Count(buf.String(), "state snapshot failed") != 1 {
		t.Fatalf("log = %s, want the failure logged once", buf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "toy"+Suffix+".tmp")); !os.IsNotExist(err) {
		t.Fatal("a temporary file was left behind")
	}
}

func TestUnwritableVolume(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s := New(Options{Dir: dir, Clock: &clock{now: t0}}, (&store{samples: map[string]time.Time{}}).part(1, 1<<20))
	s.Flush()
	if st := s.State(); st.State != viewmodel.SourceUnavailable || !strings.Contains(st.Reason, "not writable") {
		t.Fatalf("state = %+v", st)
	}
}

func TestDisabledState(t *testing.T) {
	var s *Snapshotter
	if st := s.State(); st.State != viewmodel.SourceDisabled || st.Source != SourceName {
		t.Fatalf("state = %+v", st)
	}
}
