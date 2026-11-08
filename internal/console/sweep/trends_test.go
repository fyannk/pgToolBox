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

package sweep

import (
	"math"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/volumetrend"
)

func TestFeedTrendsDetectsTheData(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	key := Key{Namespace: "team-a", Name: "orders"}
	volumes := func(s *volumetrend.Store) map[string]volumetrend.Reading {
		out := map[string]volumetrend.Reading{}
		for _, v := range s.Volumes("team-a", "orders") {
			out[v.Key.Volume] = v.Latest
		}
		return out
	}

	t.Run("volumeUsages win over the metrics, tablespaces included", func(t *testing.T) {
		s := volumetrend.NewStore(volumetrend.Limits{})
		reading := &instancestatus.Reading{VolumeUsagesReported: true, VolumeUsages: []instancestatus.VolumeUsage{
			{Volume: "pgdata", TotalBytes: 100, AvailableBytes: 40, UsedBytes: 60, Inodes: 10, InodesFree: 4, HasInodes: true},
			{Volume: "tbs-hot", TotalBytes: 50, AvailableBytes: 50},
		}}
		series := map[string]float64{"disk-pgdata-available": 1, "disk-pgdata-used": 1}
		feedTrends(s, key, "orders-1", at, reading, series, map[string]float64{"disk-pgdata-size": 2})
		got := volumes(s)
		if len(got) != 2 || got["pgdata"].AvailableBytes != 40 || got["pgdata"].Source != volumetrend.SourceStatus || !got["pgdata"].HasInodes {
			t.Fatalf("volumes = %+v", got)
		}
		if _, ok := got["tbs-hot"]; !ok {
			t.Error("tablespace volume missing")
		}
	})

	t.Run("the cnpg_disk_* metrics stand in without volumeUsages", func(t *testing.T) {
		s := volumetrend.NewStore(volumetrend.Limits{})
		series := map[string]float64{
			"disk-pgdata-available": 30, "disk-pgdata-used": 70, "disk-pgdata-inodes-free": 5,
			"disk-wal-available": 8, "disk-wal-used": 2,
		}
		instants := map[string]float64{"disk-pgdata-size": 100, "disk-pgdata-inodes": 10, "disk-wal-size": 10}
		feedTrends(s, key, "orders-1", at, &instancestatus.Reading{}, series, instants)
		got := volumes(s)
		if got["pgdata"].TotalBytes != 100 || got["pgdata"].Source != volumetrend.SourceMetrics || !got["pgdata"].HasInodes || got["pgdata"].InodesFree != 5 {
			t.Errorf("pgdata = %+v", got["pgdata"])
		}
		if got["wal"].TotalBytes != 10 || got["wal"].HasInodes {
			t.Errorf("wal = %+v", got["wal"])
		}
	})

	t.Run("stock CloudNativePG records nothing", func(t *testing.T) {
		s := volumetrend.NewStore(volumetrend.Limits{})
		feedTrends(s, key, "orders-1", at, &instancestatus.Reading{}, map[string]float64{"backends": 3}, nil)
		feedTrends(s, key, "orders-2", at, nil, nil, nil)
		if got := volumes(s); len(got) != 0 {
			t.Errorf("volumes = %+v", got)
		}
	})

	t.Run("incoherent metrics are no reading", func(t *testing.T) {
		s := volumetrend.NewStore(volumetrend.Limits{})
		for _, c := range []struct{ size, available, used float64 }{
			{0, 0, 0}, {100, 200, 10}, {100, 10, math.NaN()}, {100, -1, 10}, {math.Inf(1), 1, 1},
		} {
			feedTrends(s, key, "orders-1", at, nil,
				map[string]float64{"disk-pgdata-available": c.available, "disk-pgdata-used": c.used},
				map[string]float64{"disk-pgdata-size": c.size})
		}
		feedTrends(s, key, "orders-1", at, nil, map[string]float64{"disk-pgdata-available": 1}, map[string]float64{"disk-pgdata-size": 2})
		if got := volumes(s); len(got) != 0 {
			t.Errorf("volumes = %+v", got)
		}
	})
}
