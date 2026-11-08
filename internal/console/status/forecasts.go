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
	"math"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/volumetrend"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// trendBuckets bounds the points of one volume's trend on the wire:
// 30 days of 15-minute samples become 180 four-hour medians.
const trendBuckets = 180

// Forecast availability.
const (
	forecastsAvailable   = "available"
	forecastsUnavailable = "unavailable"
	forecastsDisabled    = "disabled"
)

// reasonNoVolumeData is why the forecast is unavailable on stock
// CloudNativePG: the console detects the data, never the build.
const reasonNoVolumeData = "No instance of this cluster reports its volumes: neither /pg/status volumeUsages nor the cnpg_disk_* metrics " +
	"are present. Upstream CloudNativePG reports neither; builds that do (such as the autoGrow fork) fill this panel on their own. " +
	"PVC capacity and the metrics are the evidence meanwhile."

// volumeFacts is what the storage screen and the diagnostics share about
// the volumes of one Cluster.
type volumeFacts struct {
	// trended are the current instances' volumes in the trend, sorted.
	trended []volumetrend.Volume
	// autoGrow is keyed by volume name (pgdata, wal, tbs-<name>); nil
	// when the raw Cluster view is not readable.
	autoGrow map[string]cnpgstatus.AutoGrow
	// autoGrowUnread: the raw Cluster view is readable but its read
	// failed, so a missing stanza says nothing about autoGrow.
	autoGrowUnread bool
	// archivingFailing is the primary's volume WAL accumulates on while
	// archiving fails ("" when archiving is not failing).
	archivingFailing string
	archiving        cnpgstatus.ArchivingReading
	primary          string
}

// volumeFactsOf reads the trend, the fork's autoGrow stanzas and the
// archiver facts of one Cluster.
func (s *Service) volumeFactsOf(ctx context.Context, g *gathered) volumeFacts {
	f := volumeFacts{primary: g.cluster.Status.CurrentPrimary}
	if s.cache.Readable(g.key.Namespace, cache.SourceClusterStorage) {
		raw := cache.NewClusterStorage()
		switch err := s.cache.Reader().Get(ctx, types.NamespacedName{Namespace: g.key.Namespace, Name: g.key.Name}, raw); {
		case err == nil:
			f.autoGrow = cnpgstatus.ForkAutoGrow(raw.Object)
		case !apierrors.IsNotFound(err):
			f.autoGrowUnread = true
		}
	}
	if s.opts.Trends == nil {
		return f
	}
	current := map[string]bool{}
	for _, pod := range g.pods {
		current[pod.Name] = true
	}
	for name := range g.swept.Instances {
		current[name] = true
	}
	hasWAL := false
	for _, v := range s.opts.Trends.Volumes(g.key.Namespace, g.key.Name) {
		if !current[v.Key.Instance] {
			continue
		}
		f.trended = append(f.trended, v)
		if v.Key.Instance == f.primary && v.Key.Volume == "wal" {
			hasWAL = true
		}
	}
	f.archiving = cnpgstatus.Archiving(g.cluster, primaryArchiver(g))
	if f.archiving.Class == cnpgstatus.ClassCritical || f.archiving.Reason == "FailureWithBacklog" {
		f.archivingFailing = "pgdata"
		if hasWAL {
			f.archivingFailing = "wal"
		}
	}
	return f
}

// forecastOptions are the autoGrow facts a forecast runs toward.
func (f volumeFacts) forecastOptions(volume string) (volumetrend.Options, *cnpgstatus.AutoGrow) {
	grow, ok := f.autoGrow[volume]
	if !ok {
		return volumetrend.Options{}, nil
	}
	return volumetrend.Options{AutoGrow: true, AutoGrowMaxBytes: grow.MaxBytes}, &grow
}

// storageForecasts is the forecast panel of the storage screen.
func (s *Service) storageForecasts(g *gathered, f volumeFacts, now time.Time) viewmodel.StorageForecasts {
	out := viewmodel.StorageForecasts{Volumes: []viewmodel.VolumeForecast{}}
	if s.opts.Trends == nil {
		out.State, out.Reason = forecastsDisabled, "the volume trend is not kept by this console"
		return out
	}
	limits := s.opts.Trends.Limits()
	out.IntervalSeconds, out.RetentionSeconds = int64(limits.Every/time.Second), int64(limits.Retention/time.Second)
	out.Persistent = s.opts.TrendsPersistent
	if len(f.trended) == 0 {
		out.State, out.Reason = forecastsUnavailable, reasonNoVolumeData
		if !g.hasSweep || len(g.swept.Instances) == 0 {
			out.Reason = "No instance status has been read for this cluster yet, so nothing says whether its instances report their volumes."
		}
		return out
	}
	out.State = forecastsAvailable
	for _, v := range f.trended {
		opts, grow := f.forecastOptions(v.Key.Volume)
		bytesF, growF, inodesF, ok := s.opts.Trends.Forecasts(v.Key, opts)
		if !ok {
			continue
		}
		row := viewmodel.VolumeForecast{
			Instance: v.Key.Instance, Volume: v.Key.Volume, Primary: v.Key.Instance == f.primary,
			TotalBytes: clampInt64(v.Latest.TotalBytes), AvailableBytes: clampInt64(v.Latest.AvailableBytes), UsedBytes: clampInt64(v.Latest.UsedBytes),
			AsOf: v.Latest.At, Since: v.Since, Samples: v.Samples,
			BytesForecast: fillForecast(bytesF), InodesForecast: fillForecast(inodesF),
			Trend:      []viewmodel.TrendPoint{},
			Provenance: s.trendProvenance(g, v.Latest, now),
		}
		if v.Latest.HasInodes {
			inodes, free := clampInt64(v.Latest.Inodes), clampInt64(v.Latest.InodesFree)
			row.Inodes, row.InodesFree = &inodes, &free
		}
		if grow != nil {
			row.AutoGrow = &viewmodel.AutoGrow{MaxSize: grow.MaxSize, Threshold: grow.Threshold, Step: grow.Step}
		}
		if growF != nil {
			fc := fillForecast(*growF)
			row.AutoGrowForecast = &fc
		}
		if row.Primary && v.Key.Volume == f.archivingFailing {
			row.ArchivingFailing = true
			row.ArchivingText = "WAL archiving is failing (" + archivingDetail(f.archiving) + "). PostgreSQL keeps every WAL segment " +
				"until it is archived, so this volume grows until archiving recovers: the growth is a symptom, fix archiving first."
		}
		for _, p := range s.opts.Trends.Downsample(v.Key, trendBuckets) {
			row.Trend = append(row.Trend, viewmodel.TrendPoint{At: p.At, Bytes: p.Bytes, Inodes: p.Inodes})
		}
		out.Volumes = append(out.Volumes, row)
	}
	return out
}

// volumeUsage is the latest reading of one PVC's volume, for its row.
func (s *Service) volumeUsage(g *gathered, f volumeFacts, instance, volume string, now time.Time) *viewmodel.VolumeUsage {
	if instance == "" || volume == "" {
		return nil
	}
	for _, v := range f.trended {
		if v.Key.Instance != instance || v.Key.Volume != volume {
			continue
		}
		u := &viewmodel.VolumeUsage{Provenance: s.trendProvenance(g, v.Latest, now)}
		total, available, used := clampInt64(v.Latest.TotalBytes), clampInt64(v.Latest.AvailableBytes), clampInt64(v.Latest.UsedBytes)
		u.TotalBytes, u.AvailableBytes, u.UsedBytes = &total, &available, &used
		if v.Latest.HasInodes {
			inodes, free := clampInt64(v.Latest.Inodes), clampInt64(v.Latest.InodesFree)
			u.Inodes, u.InodesFree = &inodes, &free
		}
		return u
	}
	return nil
}

// trendProvenance is an instance reading's provenance: stale once three
// sweeps have passed without a fresh one.
func (s *Service) trendProvenance(g *gathered, r volumetrend.Reading, now time.Time) viewmodel.Provenance {
	at := r.At
	p := viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: r.Source, ObservedAt: &at}
	interval := g.swept.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	if now.Sub(r.At) > 3*interval {
		p.Stale, p.StaleReason = true, "no fresh volume reading for "+now.Sub(r.At).Round(time.Second).String()
	}
	return p
}

func fillForecast(f volumetrend.Forecast) viewmodel.VolumeFillForecast {
	out := viewmodel.VolumeFillForecast{
		Measure: f.Measure, State: string(f.State), Consumed: f.Consumed, Capacity: f.Capacity, Headroom: f.Headroom,
		Target: string(f.Target), AutoGrowSteps: f.AutoGrowSteps, RatePerDay: f.RatePerDay, WindowHours: f.WindowHours,
		FullAt: f.FullAt, EarliestAt: f.EarliestAt, LatestAt: f.LatestAt, Confidence: f.Confidence, Accelerating: f.Accelerating,
		FirstHalfRatePerDay: f.FirstHalfRatePerDay, RecentRatePerDay: f.RecentRatePerDay,
		MinimumGrowth: f.MinimumGrowthOverWindow, MinimumHours: f.MinimumHours, HoursFound: f.HoursFound,
		ResizedAt: f.ResizedAt, FitSinceResize: f.FitSinceResize,
	}
	if !f.WindowStart.IsZero() {
		start, end := f.WindowStart, f.WindowEnd
		out.WindowStart, out.WindowEnd = &start, &end
	}
	return out
}

func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// diagnoseVolumes is the volume forecast in the shape pkg/diagnose
// reads; false when no current instance reports its volumes.
func (s *Service) diagnoseVolumes(ctx context.Context, g *gathered) (diagnose.VolumesSnapshot, bool) {
	if s.opts.Trends == nil || !g.volumesReported {
		return diagnose.VolumesSnapshot{}, false
	}
	f := s.volumeFactsOf(ctx, g)
	if len(f.trended) == 0 {
		return diagnose.VolumesSnapshot{}, false
	}
	out := diagnose.VolumesSnapshot{ArchivingFailing: f.archivingFailing}
	if f.archivingFailing != "" {
		out.ArchivingDetail = archivingDetail(f.archiving)
	}
	for _, v := range f.trended {
		opts, _ := f.forecastOptions(v.Key.Volume)
		bytesF, growF, inodesF, ok := s.opts.Trends.Forecasts(v.Key, opts)
		if !ok {
			continue
		}
		forecasts := []volumetrend.Forecast{bytesF, inodesF}
		if growF != nil {
			forecasts = append(forecasts, *growF)
		}
		for _, fc := range forecasts {
			out.Forecasts = append(out.Forecasts, diagnose.VolumeForecast{
				Instance: v.Key.Instance, Volume: v.Key.Volume, Primary: v.Key.Instance == f.primary,
				Measure: fc.Measure, State: string(fc.State), Headroom: fc.Headroom, Capacity: fc.Capacity, Target: string(fc.Target),
				AutoGrowSteps: fc.AutoGrowSteps, RatePerDay: fc.RatePerDay, WindowHours: fc.WindowHours, Accelerating: fc.Accelerating,
				Confidence: fc.Confidence, AsOf: fc.AsOf, FullAt: fc.FullAt, EarliestAt: fc.EarliestAt, Source: v.Latest.Source,
			})
		}
	}
	return out, true
}
