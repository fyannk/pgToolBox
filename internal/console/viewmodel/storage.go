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

package viewmodel

import "time"

// Storage is the /storage screen: what the Cluster requests, the PVCs
// that exist and the group the operator files each under, and volume
// usage only where an instance reports it (upstream CloudNativePG does
// not; BlindSpots says so).
type Storage struct {
	Ref         ClusterRef       `json:"ref"`
	GeneratedAt time.Time        `json:"generatedAt"`
	Requests    []VolumeSpec     `json:"requests" doc:"Data, WAL and tablespace volumes the Cluster declares."`
	Volumes     []VolumeRow      `json:"volumes" doc:"One row per PVC the operator lists or the console sees."`
	Forecasts   StorageForecasts `json:"forecasts"`
	Sources     []SourceState    `json:"sources"`
	BlindSpots  []BlindSpot      `json:"blindSpots"`
}

// VolumeSpec is one declared volume.
type VolumeSpec struct {
	Role               string `json:"role" doc:"PG_DATA, PG_WAL or PG_TABLESPACE."`
	Tablespace         string `json:"tablespace,omitempty"`
	Size               string `json:"size,omitempty" doc:"Requested size, verbatim."`
	StorageClass       string `json:"storageClass,omitempty"`
	ResizeInUseVolumes *bool  `json:"resizeInUseVolumes,omitempty"`
	Temporary          bool   `json:"temporary,omitempty"`
	// AutoGrow is the fork's autoGrow stanza; absent when unset or when
	// the console cannot read it.
	AutoGrow   *AutoGrow  `json:"autoGrow,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// VolumeRow is one PVC.
type VolumeRow struct {
	Name       string `json:"name"`
	Instance   string `json:"instance,omitempty" doc:"cnpg.io/instanceName."`
	Role       string `json:"role,omitempty" doc:"PG_DATA, PG_WAL or PG_TABLESPACE; absent when the label is missing."`
	Tablespace string `json:"tablespace,omitempty"`
	// Group is the operator's PVC group: healthy, initializing,
	// resizing, dangling, unusable, or unlisted when status names it in
	// none (or the PVC exists but the operator never listed it).
	Group     string `json:"group" enum:"healthy,initializing,resizing,dangling,unusable,unlisted"`
	GroupText string `json:"groupText,omitempty"`
	Health    Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Phase, Capacity, Requested, StorageClass come from the PVC object;
	// absent when the PVC itself was not read.
	Phase        string `json:"phase,omitempty"`
	Capacity     string `json:"capacity,omitempty"`
	Requested    string `json:"requested,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
	// Resizing reports a Resizing or FileSystemResizePending condition.
	Resizing bool `json:"resizing,omitempty"`
	// Usage is the instance's own report; absent when not reported.
	Usage      *VolumeUsage `json:"usage,omitempty"`
	Provenance Provenance   `json:"provenance"`
}

// VolumeUsage is what an instance reports about one mounted volume.
type VolumeUsage struct {
	TotalBytes     *int64     `json:"totalBytes,omitempty"`
	UsedBytes      *int64     `json:"usedBytes,omitempty"`
	AvailableBytes *int64     `json:"availableBytes,omitempty"`
	Inodes         *int64     `json:"inodes,omitempty"`
	InodesFree     *int64     `json:"inodesFree,omitempty"`
	Provenance     Provenance `json:"provenance"`
}

// StorageForecasts is the volume forecast panel: how fast each volume
// fills, from the console's own long trend of the fork's volume
// readings. Upstream CloudNativePG reports no volume usage; the panel
// then says why it is unavailable instead of guessing.
type StorageForecasts struct {
	// State: available (at least one volume trended), unavailable (no
	// instance reports volume usage, Reason says why) or disabled.
	State  string `json:"state" enum:"available,unavailable,disabled"`
	Reason string `json:"reason,omitempty"`
	// IntervalSeconds is the trend's slot (VOLUME_TREND_INTERVAL);
	// RetentionSeconds its window (VOLUME_TREND_RETENTION).
	IntervalSeconds  int64 `json:"intervalSeconds"`
	RetentionSeconds int64 `json:"retentionSeconds"`
	// Persistent: the trend survives a console restart (HISTORY_PATH).
	Persistent bool             `json:"persistent"`
	Volumes    []VolumeForecast `json:"volumes" doc:"One entry per instance and volume, sorted by instance then volume."`
}

// VolumeForecast is one instance's volume.
type VolumeForecast struct {
	Instance string `json:"instance"`
	// Volume is pgdata, wal or tbs-<name>.
	Volume  string `json:"volume"`
	Primary bool   `json:"primary,omitempty"`
	// Latest is the newest reading; AsOf its time.
	TotalBytes     int64     `json:"totalBytes"`
	AvailableBytes int64     `json:"availableBytes"`
	UsedBytes      int64     `json:"usedBytes"`
	Inodes         *int64    `json:"inodes,omitempty"`
	InodesFree     *int64    `json:"inodesFree,omitempty"`
	AsOf           time.Time `json:"asOf"`
	// Since is the oldest retained sample; Samples counts them.
	Since   time.Time `json:"since"`
	Samples int       `json:"samples"`
	// BytesForecast and InodesForecast are the two forecasts, each
	// toward its own line. BytesForecast always runs toward the current
	// filesystem: a filesystem with nothing left is full whatever
	// autoGrow may do. AutoGrowForecast runs toward autoGrow.maxSize,
	// when autoGrow is set with a readable maxSize above the size.
	BytesForecast    VolumeFillForecast  `json:"bytesForecast"`
	AutoGrowForecast *VolumeFillForecast `json:"autoGrowForecast,omitempty"`
	InodesForecast   VolumeFillForecast  `json:"inodesForecast"`
	// AutoGrow is the fork's autoGrow stanza of this volume's storage
	// section; absent when unset or not readable.
	AutoGrow *AutoGrow `json:"autoGrow,omitempty"`
	// ArchivingFailing: WAL is accumulating on this volume because WAL
	// archiving fails (the primary's WAL volume, or its data volume when
	// WAL shares it). Growth then is a symptom, not a workload.
	ArchivingFailing bool   `json:"archivingFailing,omitempty"`
	ArchivingText    string `json:"archivingText,omitempty"`
	// Trend is the downsampled series (consumed bytes and inodes).
	Trend      []TrendPoint `json:"trend"`
	Provenance Provenance   `json:"provenance"`
}

// AutoGrow is the fork's spec.*.autoGrow of one volume.
type AutoGrow struct {
	MaxSize   string `json:"maxSize,omitempty"`
	Threshold int    `json:"threshold,omitempty" doc:"Percent used that triggers a step."`
	Step      string `json:"step,omitempty" doc:"Growth per step, a quantity or a percentage."`
}

// VolumeFillForecast is the time until one measure of one volume is
// exhausted.
type VolumeFillForecast struct {
	Measure string `json:"measure" enum:"bytes,inodes"`
	// State: forecast (FullAt is set), notGrowing, tooFewPoints (fewer
	// than MinimumHours hourly points, or under a day), full, noData
	// (the filesystem reports no inode count) or totalChanged (the inode
	// total changed inside the window, as XFS allocates inodes on
	// demand: no fixed line to run toward).
	State string `json:"state" enum:"forecast,notGrowing,tooFewPoints,full,noData,totalChanged"`
	// Consumed, Capacity and Headroom are bytes or inodes; Target the
	// line: the filesystem's size, or autoGrow.maxSize.
	Consumed float64 `json:"consumed"`
	Capacity float64 `json:"capacity"`
	Headroom float64 `json:"headroom"`
	Target   string  `json:"target" enum:"filesystem,autoGrowMaxSize"`
	// AutoGrowSteps: the operator may still grow the PVC, in steps,
	// before the date.
	AutoGrowSteps bool       `json:"autoGrowSteps,omitempty"`
	RatePerDay    float64    `json:"ratePerDay"`
	WindowStart   *time.Time `json:"windowStart,omitempty"`
	WindowEnd     *time.Time `json:"windowEnd,omitempty"`
	WindowHours   int        `json:"windowHours"`
	FullAt        *time.Time `json:"fullAt,omitempty"`
	EarliestAt    *time.Time `json:"earliestAt,omitempty"`
	LatestAt      *time.Time `json:"latestAt,omitempty"`
	Confidence    string     `json:"confidence,omitempty" enum:"high,medium,low"`
	// Accelerating: the recent half of the window grows faster than
	// linear (≥ 1.5× the first half); the date uses the recent rate.
	Accelerating        bool    `json:"accelerating,omitempty"`
	FirstHalfRatePerDay float64 `json:"firstHalfRatePerDay"`
	RecentRatePerDay    float64 `json:"recentRatePerDay"`
	MinimumGrowth       float64 `json:"minimumGrowth" doc:"Growth over the window below which the volume counts as not growing."`
	MinimumHours        int     `json:"minimumHours"`
	HoursFound          int     `json:"hoursFound"`
	// ResizedAt is the last size change inside the window. A resize
	// shifts consumed by the reserved blocks it adds: with
	// FitSinceResize the fit starts at the change, else the readings
	// before it were shifted by the jump across it.
	ResizedAt      *time.Time `json:"resizedAt,omitempty"`
	FitSinceResize bool       `json:"fitSinceResize,omitempty"`
}

// TrendPoint is one downsampled trend point; a nil value is a gap.
type TrendPoint struct {
	At     time.Time `json:"at"`
	Bytes  *float64  `json:"bytes,omitempty"`
	Inodes *float64  `json:"inodes,omitempty"`
}
