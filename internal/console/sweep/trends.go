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
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/volumetrend"
)

// trendVolumes are the volumes the metrics fallback can describe: the
// catalog charts the fixed ones only (tablespaces come from /pg/status).
var trendVolumes = []string{"pgdata", "wal"}

// feedTrends records one fresh read of one instance in the long volume
// trend. /pg/status volumeUsages wins: it is one consistent statfs per
// volume, tablespaces included. Without it, the cnpg_disk_* metrics of
// the same sweep stand in for the data and WAL volumes. Neither present
// (stock CloudNativePG) records nothing: the trend is detected from the
// data, never from the build.
func feedTrends(store *volumetrend.Store, key Key, instance string, at time.Time, reading *instancestatus.Reading, series, instants map[string]float64) {
	if store == nil {
		return
	}
	if reading != nil && reading.VolumeUsagesReported && len(reading.VolumeUsages) > 0 {
		for _, v := range reading.VolumeUsages {
			store.Observe(volumetrend.Key{Namespace: key.Namespace, Cluster: key.Name, Instance: instance, Volume: v.Volume}, volumetrend.Reading{
				At: at, TotalBytes: v.TotalBytes, AvailableBytes: v.AvailableBytes, UsedBytes: v.UsedBytes,
				Inodes: v.Inodes, InodesFree: v.InodesFree, HasInodes: v.HasInodes, Source: volumetrend.SourceStatus,
			})
		}
		return
	}
	for _, volume := range trendVolumes {
		total, okTotal := count(instants, "disk-"+volume+"-size")
		available, okAvailable := count(series, "disk-"+volume+"-available")
		used, okUsed := count(series, "disk-"+volume+"-used")
		if !okTotal || !okAvailable || !okUsed || total == 0 || available > total || used > total {
			continue
		}
		r := volumetrend.Reading{At: at, TotalBytes: total, AvailableBytes: available, UsedBytes: used, Source: volumetrend.SourceMetrics}
		inodes, okInodes := count(instants, "disk-"+volume+"-inodes")
		free, okFree := count(series, "disk-"+volume+"-inodes-free")
		if okInodes && okFree && inodes > 0 && free <= inodes {
			r.Inodes, r.InodesFree, r.HasInodes = inodes, free, true
		}
		store.Observe(volumetrend.Key{Namespace: key.Namespace, Cluster: key.Name, Instance: instance, Volume: volume}, r)
	}
}

// count reads a metric value as a non-negative integer count; a missing,
// NaN, negative or out-of-range value is no reading.
func count(values map[string]float64, key string) (uint64, bool) {
	v, ok := values[key]
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= math.MaxUint64 {
		return 0, false
	}
	return uint64(v), true
}
