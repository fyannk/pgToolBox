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

//go:build scale

package inventory

import (
	"bytes"
	"context"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

const scaleObjectCount = 1_000_000

func TestScannerMillionObjectSnapshotIsBounded(t *testing.T) {
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	var peak atomic.Uint64
	peak.Store(baseline.Alloc)
	var peakRSS atomic.Uint64
	peakRSS.Store(currentRSSBytes())
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var current runtime.MemStats
				runtime.ReadMemStats(&current)
				for current.Alloc > peak.Load() && !peak.CompareAndSwap(peak.Load(), current.Alloc) {
				}
				rss := currentRSSBytes()
				for rss > peakRSS.Load() && !peakRSS.CompareAndSwap(peakRSS.Load(), rss) {
				}
			}
		}
	}()

	reader := millionObjectReader()
	scanner, cache, _ := newTestScanner(t, reader, scaleObjectCount, store.MaxPageObjects, MaxRecentObjects, time.Now)
	scanner.analyzeBarmanCatalog = true
	started := time.Now()
	err := scanner.Refresh(context.Background())
	duration := time.Since(started)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if snapshot.ObjectCount != scaleObjectCount || snapshot.PagesExamined != scaleObjectCount/store.MaxPageObjects || len(snapshot.RecentObjects) != MaxRecentObjects || len(snapshot.Scopes) != 1 {
		t.Fatalf("million-object snapshot was not compact: objects=%d pages=%d recent=%d scopes=%d", snapshot.ObjectCount, snapshot.PagesExamined, len(snapshot.RecentObjects), len(snapshot.Scopes))
	}
	if len(snapshot.BarmanWAL.Servers) != 1 || snapshot.BarmanWAL.Servers[0].Counts.Segments != scaleObjectCount-2 || len(snapshot.BarmanWAL.Servers[0].Ranges) != 1 || snapshot.BarmanWAL.Servers[0].State != evidence.Healthy {
		t.Fatalf("million-WAL continuity was not compact: %#v", snapshot.BarmanWAL)
	}
	peakDelta := peak.Load() - baseline.Alloc
	const scannerMemoryBudget = 128 * 1024 * 1024
	if peakDelta > scannerMemoryBudget {
		t.Fatalf("scanner heap delta %d bytes exceeds %d-byte test budget", peakDelta, scannerMemoryBudget)
	}
	const podMemoryLimit = 256 * 1024 * 1024
	if rss := peakRSS.Load(); rss > podMemoryLimit {
		t.Fatalf("process peak RSS %d bytes exceeds %d-byte pod memory limit", rss, podMemoryLimit)
	}
	t.Logf("scale objects=%d pages=%d duration=%s baseline_heap=%d peak_heap=%d heap_delta=%d peak_rss=%d retained_scopes=%d retained_recent=%d wal_segments=%d wal_ranges=%d", scaleObjectCount, snapshot.PagesExamined, duration, baseline.Alloc, peak.Load(), peakDelta, peakRSS.Load(), len(snapshot.Scopes), len(snapshot.RecentObjects), snapshot.BarmanWAL.Servers[0].Counts.Segments, len(snapshot.BarmanWAL.Servers[0].Ranges))
}

func currentRSSBytes() uint64 {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range bytes.Split(status, []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) >= 2 && string(fields[0]) == "VmRSS:" {
			kibibytes, parseErr := strconv.ParseUint(string(fields[1]), 10, 64)
			if parseErr == nil {
				return kibibytes * 1024
			}
		}
	}
	return 0
}

func millionObjectReader() store.Reader {
	return &storetest.Fake{
		ListFunc: func(_ context.Context, request store.ListRequest) (store.Page, error) {
			offset := 0
			if request.Cursor != "" {
				parsed, err := strconv.Atoi(request.Cursor)
				if err != nil {
					return store.Page{}, err
				}
				offset = parsed
			}
			remaining := scaleObjectCount - offset
			count := min(request.Limit, remaining)
			objects := make([]store.Object, count)
			for index := range objects {
				objectNumber := offset + index
				switch objectNumber {
				case 0:
					objects[index] = store.Object{Key: "alpha/base/scale/backup.info", Size: 256}
				case 1:
					objects[index] = store.Object{Key: "alpha/base/scale/data.tar", Size: 1}
				default:
					position := uint64(objectNumber - 2)
					name, err := barmancloud.WALName(1, position, 16<<20)
					if err != nil {
						return store.Page{}, err
					}
					objects[index] = store.Object{
						Key: "alpha/wals/" + name[:16] + "/" + name, Size: 16 * 1024 * 1024,
						LastModified: time.Unix(int64(position), 0).UTC(),
					}
				}
			}
			next := ""
			if offset+count < scaleObjectCount {
				next = strconv.Itoa(offset + count)
			}
			return store.Page{Objects: objects, NextCursor: next}, nil
		},
		OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("status=DONE\nversion=180000\nxlog_segment_size=16777216\nsize=1\n")), nil
		},
		StatFunc: func(_ context.Context, key string) (store.Object, error) {
			return store.Object{Key: key, Size: 1}, nil
		},
	}
}
