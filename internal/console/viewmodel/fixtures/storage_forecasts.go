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

package fixtures

import (
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

const gib = 1 << 30

// billingRef is the billing Cluster every fixture of this package names
// (storage forecasts, timeline, digest).
var billingRef = viewmodel.ClusterRef{Namespace: "team-a", Name: "billing", UID: "4d2f1e0a-1b2c-4d3e-8f90-a1b2c3d4e5f6"}

// StorageUnavailable is the forecast panel of a stock CloudNativePG
// Cluster: no instance reports its volumes.
func StorageUnavailable() viewmodel.StorageForecasts {
	return viewmodel.StorageForecasts{
		State: "unavailable",
		Reason: "No instance of this cluster reports its volumes: neither /pg/status volumeUsages nor the cnpg_disk_* metrics are present. " +
			"Upstream CloudNativePG reports neither; builds that do (such as the autoGrow fork) fill this panel on their own. " +
			"PVC capacity and the metrics are the evidence meanwhile.",
		IntervalSeconds: 900, RetentionSeconds: 30 * 86400, Volumes: []viewmodel.VolumeForecast{},
	}
}

// trend is a deterministic consumed-bytes series: start, plus perDay a
// day, over days, one point every four hours, with an optional gap.
func trend(start, perDay float64, days int, inodes bool, gapAt int) []viewmodel.TrendPoint {
	n := days * 6
	out := make([]viewmodel.TrendPoint, 0, n)
	for i := range n {
		at := Now.Add(-time.Duration(n-i) * 4 * time.Hour)
		p := viewmodel.TrendPoint{At: at}
		if i != gapAt {
			b := start + perDay*float64(i)/6
			p.Bytes = &b
			if inodes {
				v := 40_000 + float64(i)*3
				p.Inodes = &v
			}
		}
		out = append(out, p)
	}
	return out
}

func window(hours int) (*time.Time, *time.Time) {
	return ago(time.Duration(hours) * time.Hour), ago(15 * time.Minute)
}

// StorageForecasts is billing on a build that reports its volumes: the
// primary's data volume fills its filesystem within a week and reaches
// autoGrow.maxSize in about 46 days, its WAL volume
// grows faster than linear while archiving fails, a replica's data
// volume is flat since autoGrow stepped it 60 hours ago and a fresh
// replica has too few points.
func StorageForecasts() viewmodel.Storage {
	prov := viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status volumeUsages", ObservedAt: ago(20 * time.Second)}
	usage := func(total, available, used int64) *viewmodel.VolumeUsage {
		return &viewmodel.VolumeUsage{TotalBytes: new(total), AvailableBytes: new(available), UsedBytes: new(used), Inodes: new(int64(2_621_440)), InodesFree: new(int64(2_580_000)), Provenance: prov}
	}
	pvc := func(name, instance, role string, u *viewmodel.VolumeUsage) viewmodel.VolumeRow {
		return viewmodel.VolumeRow{
			Name: name, Instance: instance, Role: role, Group: "healthy", Health: viewmodel.HealthOK, Phase: "Bound", Capacity: "40Gi", Requested: "40Gi",
			StorageClass: "fast-ssd", Usage: u, Provenance: kubeProv("PersistentVolumeClaim; group: Cluster.status.healthyPVC"),
		}
	}
	grow := &viewmodel.AutoGrow{MaxSize: "100Gi", Threshold: 80, Step: "20%"}
	start, end := window(168)
	walStart, walEnd := window(96)
	inodes := func(state string) viewmodel.VolumeFillForecast {
		f := viewmodel.VolumeFillForecast{
			Measure: "inodes", State: state, Consumed: 41_440, Capacity: 2_621_440, Headroom: 2_580_000, Target: "filesystem",
			RatePerDay: 18, WindowStart: start, WindowEnd: end, WindowHours: 168, MinimumGrowth: 1310.72, MinimumHours: 24, HoursFound: 168,
			FirstHalfRatePerDay: 17, RecentRatePerDay: 19,
		}
		return f
	}
	primaryData := viewmodel.VolumeForecast{
		Instance: "billing-1", Volume: "pgdata", Primary: true,
		TotalBytes: 40 * gib, AvailableBytes: 9 * gib, UsedBytes: 31 * gib, Inodes: new(int64(2_621_440)), InodesFree: new(int64(2_580_000)),
		AsOf: *ago(20 * time.Second), Since: *ago(30 * 24 * time.Hour), Samples: 2875,
		BytesForecast: viewmodel.VolumeFillForecast{
			Measure: "bytes", State: "forecast", Consumed: 31 * gib, Capacity: 40 * gib, Headroom: 9 * gib,
			Target: "filesystem", AutoGrowSteps: true, RatePerDay: 1.5 * gib,
			WindowStart: start, WindowEnd: end, WindowHours: 168,
			FullAt: new(Now.Add(6 * 24 * time.Hour)), EarliestAt: new(Now.Add(5 * 24 * time.Hour)), LatestAt: new(Now.Add(7 * 24 * time.Hour)),
			Confidence: "high", FirstHalfRatePerDay: 1.4 * gib, RecentRatePerDay: 1.6 * gib, MinimumGrowth: 21_474_836, MinimumHours: 24, HoursFound: 168,
		},
		AutoGrowForecast: &viewmodel.VolumeFillForecast{
			Measure: "bytes", State: "forecast", Consumed: 31 * gib, Capacity: 91 * gib, Headroom: 69 * gib,
			Target: "autoGrowMaxSize", AutoGrowSteps: true, RatePerDay: 1.5 * gib,
			WindowStart: start, WindowEnd: end, WindowHours: 168,
			FullAt: new(Now.Add(46 * 24 * time.Hour)), EarliestAt: new(Now.Add(41 * 24 * time.Hour)), LatestAt: new(Now.Add(53 * 24 * time.Hour)),
			Confidence: "high", FirstHalfRatePerDay: 1.4 * gib, RecentRatePerDay: 1.6 * gib, MinimumGrowth: 48_855_187, MinimumHours: 24, HoursFound: 168,
		},
		InodesForecast: inodes("notGrowing"),
		AutoGrow:       grow,
		Trend:          trend(21*gib, 0.33*gib, 30, true, 40),
		Provenance:     prov,
	}
	primaryWAL := viewmodel.VolumeForecast{
		Instance: "billing-1", Volume: "wal", Primary: true,
		TotalBytes: 10 * gib, AvailableBytes: 2 * gib, UsedBytes: 8 * gib,
		AsOf: *ago(20 * time.Second), Since: *ago(4 * 24 * time.Hour), Samples: 384,
		BytesForecast: viewmodel.VolumeFillForecast{
			Measure: "bytes", State: "forecast", Consumed: 8 * gib, Capacity: 10 * gib, Headroom: 2 * gib, Target: "filesystem",
			RatePerDay: 1.6 * gib, WindowStart: walStart, WindowEnd: walEnd, WindowHours: 96,
			FullAt: new(Now.Add(30 * time.Hour)), EarliestAt: new(Now.Add(22 * time.Hour)), LatestAt: new(Now.Add(44 * time.Hour)),
			Confidence: "medium", Accelerating: true, FirstHalfRatePerDay: 0.4 * gib, RecentRatePerDay: 1.6 * gib,
			MinimumGrowth: 5_368_709, MinimumHours: 24, HoursFound: 96,
		},
		InodesForecast:   viewmodel.VolumeFillForecast{Measure: "inodes", State: "noData", Target: "filesystem", MinimumHours: 24},
		ArchivingFailing: true,
		ArchivingText: "WAL archiving is failing (ContinuousArchiving False (ContinuousArchivingFailing)). PostgreSQL keeps every WAL segment " +
			"until it is archived, so this volume grows until archiving recovers: the growth is a symptom, fix archiving first.",
		Trend:      trend(2*gib, 1.5*gib, 4, false, -1),
		Provenance: prov,
	}
	replicaData := viewmodel.VolumeForecast{
		Instance: "billing-2", Volume: "pgdata",
		TotalBytes: 40 * gib, AvailableBytes: 22 * gib, UsedBytes: 18 * gib, Inodes: new(int64(2_621_440)), InodesFree: new(int64(2_590_000)),
		AsOf: *ago(20 * time.Second), Since: *ago(30 * 24 * time.Hour), Samples: 2880,
		BytesForecast: viewmodel.VolumeFillForecast{
			Measure: "bytes", State: "notGrowing", Consumed: 18 * gib, Capacity: 40 * gib, Headroom: 22 * gib, Target: "filesystem", AutoGrowSteps: true,
			RatePerDay: 120_000, WindowStart: start, WindowEnd: end, WindowHours: 168, MinimumGrowth: 21_474_836, MinimumHours: 24, HoursFound: 168,
			ResizedAt: ago(60 * time.Hour), FitSinceResize: true,
		},
		AutoGrowForecast: &viewmodel.VolumeFillForecast{
			Measure: "bytes", State: "notGrowing", Consumed: 18 * gib, Capacity: 91 * gib, Headroom: 73 * gib, Target: "autoGrowMaxSize", AutoGrowSteps: true,
			RatePerDay: 120_000, WindowStart: start, WindowEnd: end, WindowHours: 168, MinimumGrowth: 48_855_187, MinimumHours: 24, HoursFound: 168,
		},
		InodesForecast: inodes("notGrowing"),
		AutoGrow:       grow,
		Trend:          trend(18*gib, 0, 30, true, -1),
		Provenance:     prov,
	}
	freshReplica := viewmodel.VolumeForecast{
		Instance: "billing-3", Volume: "pgdata",
		TotalBytes: 40 * gib, AvailableBytes: 21 * gib, UsedBytes: 19 * gib,
		AsOf: *ago(20 * time.Second), Since: *ago(9 * time.Hour), Samples: 36,
		BytesForecast: viewmodel.VolumeFillForecast{
			Measure: "bytes", State: "tooFewPoints", Consumed: 19 * gib, Capacity: 40 * gib, Headroom: 21 * gib, Target: "filesystem", AutoGrowSteps: true,
			WindowHours: 9, MinimumGrowth: 21_474_836, MinimumHours: 24, HoursFound: 9,
		},
		AutoGrowForecast: &viewmodel.VolumeFillForecast{
			Measure: "bytes", State: "tooFewPoints", Consumed: 19 * gib, Capacity: 91 * gib, Headroom: 72 * gib, Target: "autoGrowMaxSize", AutoGrowSteps: true,
			WindowHours: 9, MinimumGrowth: 48_855_187, MinimumHours: 24, HoursFound: 9,
		},
		InodesForecast: viewmodel.VolumeFillForecast{Measure: "inodes", State: "noData", Target: "filesystem", MinimumHours: 24},
		AutoGrow:       grow,
		Trend:          trend(19*gib, 0.1*gib, 1, false, -1)[:2],
		Provenance:     prov,
	}
	return viewmodel.Storage{
		Ref: billingRef, GeneratedAt: Now,
		Requests: []viewmodel.VolumeSpec{
			{Role: "PG_DATA", Size: "40Gi", StorageClass: "fast-ssd", ResizeInUseVolumes: new(true), AutoGrow: grow, Provenance: operatorProv("Cluster.spec.storage")},
			{Role: "PG_WAL", Size: "10Gi", StorageClass: "fast-ssd", Provenance: operatorProv("Cluster.spec.storage")},
		},
		Volumes: []viewmodel.VolumeRow{
			pvc("billing-1", "billing-1", "PG_DATA", usage(40*gib, 9*gib, 31*gib)),
			pvc("billing-1-wal", "billing-1", "PG_WAL", &viewmodel.VolumeUsage{TotalBytes: new(int64(10 * gib)), AvailableBytes: new(int64(2 * gib)), UsedBytes: new(int64(8 * gib)), Provenance: prov}),
			pvc("billing-2", "billing-2", "PG_DATA", usage(40*gib, 22*gib, 18*gib)),
			pvc("billing-3", "billing-3", "PG_DATA", &viewmodel.VolumeUsage{TotalBytes: new(int64(40 * gib)), AvailableBytes: new(int64(21 * gib)), UsedBytes: new(int64(19 * gib)), Provenance: prov}),
		},
		Forecasts: viewmodel.StorageForecasts{
			State: "available", IntervalSeconds: 900, RetentionSeconds: 30 * 86400, Persistent: true,
			Volumes: []viewmodel.VolumeForecast{primaryData, primaryWAL, replicaData, freshReplica},
		},
		Sources:    []viewmodel.SourceState{src("clusters", since(3*time.Hour)), src("pvcs", since(3*time.Hour))},
		BlindSpots: []viewmodel.BlindSpot{},
	}
}
