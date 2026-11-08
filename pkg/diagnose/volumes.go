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

package diagnose

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// VolumesSnapshot is the console's account of the volumes of one
// Cluster: what each instance reports of its filesystems, trended and
// extrapolated by the console. Only builds that report their volumes
// (the fork's /pg/status volumeUsages or its cnpg_disk_* metrics) fill
// it; HasVolumes is false elsewhere.
type VolumesSnapshot struct {
	// Forecasts holds one entry per instance, volume and measure.
	Forecasts []VolumeForecast
	// ArchivingFailing is the primary's volume WAL accumulates on while
	// archiving fails, empty when archiving is not failing.
	ArchivingFailing string
	// ArchivingDetail is the archiving verdict quoted as evidence.
	ArchivingDetail string
}

// VolumeForecast is one measure of one volume.
type VolumeForecast struct {
	Instance string
	// Volume is pgdata, wal or tbs-<name>.
	Volume  string
	Primary bool
	// Measure is bytes or inodes; State forecast, notGrowing,
	// tooFewPoints, full or noData.
	Measure, State string
	// Headroom and Capacity are bytes or inodes; Target is filesystem or
	// autoGrowMaxSize.
	Headroom, Capacity float64
	Target             string
	// AutoGrowSteps: the operator may still grow the PVC before FullAt.
	AutoGrowSteps bool
	RatePerDay    float64
	WindowHours   int
	Accelerating  bool
	Confidence    string
	AsOf          time.Time
	FullAt        *time.Time
	EarliestAt    *time.Time
	// Source is where the readings come from.
	Source string
}

// Volume forecast horizons.
const (
	// VolumeFullHigh: full within three days leaves one working day to
	// act after a weekend.
	VolumeFullHigh = 3 * 24 * time.Hour
	// VolumeFullWarning: full within two weeks is a ticket, not a page.
	VolumeFullWarning = 14 * 24 * time.Hour
)

// reasonVolumesNotReported is permanent on stock CloudNativePG, so it is
// a source that is off, not one that stopped answering.
const reasonVolumesNotReported = "no instance reports its volumes (/pg/status volumeUsages or the cnpg_disk_* metrics); " +
	"upstream CloudNativePG reports neither"

// volumeForecastDetector reports a volume the trend says will be full
// soon, bytes and inodes alike.
//
// It implements Detector.
type volumeForecastDetector struct{}

func (volumeForecastDetector) Name() string { return "volume-full-forecast" }

func (volumeForecastDetector) Layer() Layer { return LayerKubernetes }

func (volumeForecastDetector) Describes() string {
	return "a volume whose trend reaches full within two weeks, in bytes or inodes"
}

func (volumeForecastDetector) Detect(in Input) ([]Finding, string) {
	if !in.HasVolumes {
		return nil, reasonVolumesNotReported
	}
	// A volume with a forecast toward autoGrow.maxSize: its filesystem
	// forecast is when the next step is due, not a finding; the maxSize
	// one is. Its filesystem being full now still is.
	growing := map[string]bool{}
	for _, f := range in.Volumes.Forecasts {
		if f.Target == "autoGrowMaxSize" {
			growing[f.Instance+"/"+f.Volume] = true
		}
	}
	var findings []Finding
	for _, f := range in.Volumes.Forecasts {
		what := "space"
		if f.Measure == "inodes" {
			what = "inodes"
		}
		subject := f.Instance + "/" + f.Volume
		id := "volume-full-forecast/" + subject + "/" + f.Measure
		if f.Target == "autoGrowMaxSize" {
			id += "/autoGrowMaxSize"
		}
		base := Finding{
			ID: id, Check: "volume-full-forecast",
			Subject: EntityRef{Kind: "Pod", Name: f.Instance}, At: f.AsOf, Link: "/storage", LinkLabel: "Storage",
		}
		switch f.State {
		case "full":
			if f.Target != "filesystem" {
				// At maxSize with nothing free: the filesystem forecast
				// says it.
				continue
			}
			base.Severity = SeverityCritical
			base.Summary = fmt.Sprintf("The %s volume of %s has no %s left for PostgreSQL.", f.Volume, f.Instance, what)
			base.Detail = "PostgreSQL cannot extend a file on it: writes fail, and on the WAL path the instance stops."
			if f.AutoGrowSteps {
				base.Detail += " autoGrow is set but has not grown this filesystem in time: check the VolumeExpansionRefused and VolumeAutoGrowStopped conditions and the storage class."
			}
			base.Evidence = []Evidence{{Origin: "instance-reported", Object: "Pod/" + f.Instance, Detail: fmt.Sprintf("%s: %s %s, %s available", f.Source, f.Volume, what, amount(f.Headroom, f.Measure))}}
			base.NextSteps = volumeNextSteps(f)
		case "forecast":
			if f.FullAt == nil || (f.Target == "filesystem" && f.Measure == "bytes" && growing[subject]) {
				continue
			}
			left := f.FullAt.Sub(in.Now)
			switch {
			case left <= VolumeFullHigh:
				base.Severity = SeverityHigh
			case left <= VolumeFullWarning:
				base.Severity = SeverityWarning
			default:
				continue
			}
			toward := "full"
			if f.Target == "autoGrowMaxSize" {
				toward = "at autoGrow.maxSize, where the operator stops growing it"
			}
			base.Summary = fmt.Sprintf("The %s volume of %s will be %s in about %s (%s).", f.Volume, f.Instance, toward, humanDays(left), f.FullAt.UTC().Format("2006-01-02 15:04 UTC"))
			detail := []string{fmt.Sprintf("A robust fit of the last %d hours grows %s a day, %s confidence.", f.WindowHours, amount(f.RatePerDay, f.Measure), f.Confidence)}
			if f.Accelerating {
				detail = append(detail, "Growth is faster than linear: the recent half of the window grows at least 1.5 times the first, and the date uses the recent rate.")
			}
			if f.AutoGrowSteps {
				detail = append(detail, "autoGrow will step the PVC before then; the date is when the steps run out, not when the next step happens.")
			}
			if f.EarliestAt != nil {
				detail = append(detail, "The earliest plausible date is "+f.EarliestAt.UTC().Format("2006-01-02 15:04 UTC")+".")
			}
			base.Detail = strings.Join(detail, " ")
			base.Evidence = []Evidence{{
				Origin: "console-observed", Object: "Pod/" + f.Instance,
				Detail: fmt.Sprintf("%s: %s left of %s (%s)", f.Source, amount(f.Headroom, f.Measure), amount(f.Capacity, f.Measure), f.Target),
			}}
			base.NextSteps = volumeNextSteps(f)
		default:
			continue
		}
		findings = append(findings, base)
	}
	return findings, ""
}

func volumeNextSteps(f VolumeForecast) string {
	switch {
	case f.Measure == "inodes":
		return "Find what creates the files — many small partitions, temporary files, a WAL directory that is not recycled — before resizing: a larger volume adds inodes only on some filesystems."
	case f.Volume == "wal":
		return "Check WAL archiving and replication slots first: retained WAL is the usual cause. Then raise walStorage.size (or autoGrow.maxSize)."
	case f.Target == "autoGrowMaxSize":
		return "Raise autoGrow.maxSize if the storage class allows it, or find and stop the growth."
	}
	return "Raise the storage size (the storage class must allow expansion), or find and stop the growth."
}

// walArchivingDetector reports WAL accumulating on the primary's volume
// while archiving fails: the growth is the symptom of the failure.
//
// It implements Detector.
type walArchivingDetector struct{}

func (walArchivingDetector) Name() string { return "wal-growing-archiving-failing" }

func (walArchivingDetector) Layer() Layer { return LayerBackups }

func (walArchivingDetector) Describes() string {
	return "WAL piling up on the primary's volume while archiving fails"
}

func (walArchivingDetector) Detect(in Input) ([]Finding, string) {
	if !in.HasVolumes {
		return nil, reasonVolumesNotReported
	}
	if in.Volumes.ArchivingFailing == "" {
		return nil, ""
	}
	for _, f := range in.Volumes.Forecasts {
		if !f.Primary || f.Volume != in.Volumes.ArchivingFailing || f.Measure != "bytes" || (f.State != "forecast" && f.State != "full") {
			continue
		}
		finding := Finding{
			ID: "wal-growing-archiving-failing/" + f.Instance, Check: "wal-growing-archiving-failing",
			Subject: EntityRef{Kind: "Pod", Name: f.Instance}, At: f.AsOf, Severity: SeverityHigh,
			Summary: fmt.Sprintf("WAL is piling up on the %s volume of %s while archiving fails.", f.Volume, f.Instance),
			Detail: "PostgreSQL keeps every WAL segment until it is archived. The volume grows " + amount(f.RatePerDay, "bytes") +
				" a day and fills when archiving does not recover; the recovery window stops at the last archived segment meanwhile.",
			Evidence: []Evidence{
				{Origin: "operator-reported", Object: "Cluster", Detail: in.Volumes.ArchivingDetail},
				{Origin: "console-observed", Object: "Pod/" + f.Instance, Detail: fmt.Sprintf("%s: %s left", f.Source, amount(f.Headroom, "bytes"))},
			},
			NextSteps: "Fix archiving first (credentials, bucket, network, the plugin's sidecar); growing the volume only buys time.",
			Link:      "/backups", LinkLabel: "Backups",
		}
		if f.State == "full" {
			finding.Severity = SeverityCritical
		}
		return []Finding{finding}, ""
	}
	return nil, ""
}

// autoGrowStoppedDetector quotes the fork operator's own conditions when
// it stops growing a volume, or the storage refuses an expansion.
//
// It implements Detector.
type autoGrowStoppedDetector struct{}

func (autoGrowStoppedDetector) Name() string { return "volume-autogrow-stopped" }

func (autoGrowStoppedDetector) Layer() Layer { return LayerOperator }

func (autoGrowStoppedDetector) Describes() string {
	return "the operator stopped growing a volume, or the storage refused an expansion"
}

func (autoGrowStoppedDetector) Detect(in Input) ([]Finding, string) {
	if !in.HasCluster {
		return nil, "the cluster has not been observed yet"
	}
	var findings []Finding
	for _, c := range in.Cluster.Cluster.Conditions {
		if c.Status != "True" {
			continue
		}
		var f Finding
		switch c.Type {
		case "VolumeAutoGrowStopped":
			f = Finding{Severity: SeverityWarning, Summary: "The operator stopped growing a volume automatically."}
			switch c.Reason {
			case "MaxSizeReached":
				f.Detail = "A volume reached its autoGrow.maxSize: from here it fills like any fixed volume."
				f.NextSteps = "Raise autoGrow.maxSize, or find and stop the growth."
			case "WALArchivingFailing":
				f.Severity = SeverityHigh
				f.Detail = "Growth is held while WAL archiving fails, so retained WAL cannot buy itself more room."
				f.NextSteps = "Fix WAL archiving; growth resumes on its own afterwards."
			default:
				f.NextSteps = "Read the condition's message for the volume and the cause."
			}
		case "VolumeExpansionRefused":
			f = Finding{
				Severity: SeverityWarning, Summary: "The storage refused to expand a volume.",
				Detail:    "The PVC was asked to grow and the storage class or the provisioner said no; the volume keeps its size.",
				NextSteps: "Check that the storage class sets allowVolumeExpansion and the provisioner's events on the PVC.",
			}
		default:
			continue
		}
		f.ID, f.Check = "volume-autogrow-stopped/"+c.Type, "volume-autogrow-stopped"
		f.Subject = clusterSubject
		if c.LastTransition != nil {
			f.At = *c.LastTransition
		}
		f.Evidence = []Evidence{{Origin: "operator-reported", Object: "Cluster", Detail: conditionText(c.Type, c.Reason, c.Message)}}
		f.Link, f.LinkLabel = "/storage", "Storage"
		findings = append(findings, f)
	}
	return findings, ""
}

func conditionText(kind, reason, message string) string {
	out := kind + "=True"
	if reason != "" {
		out += " (" + reason + ")"
	}
	if message != "" {
		out += ": " + message
	}
	return out
}

// humanDays renders a duration in hours below two days, else days.
func humanDays(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("%d hours", int(math.Max(1, math.Round(d.Hours()))))
	}
	return fmt.Sprintf("%d days", int(math.Round(d.Hours()/24)))
}

// amount renders bytes in binary units, inodes as a count.
func amount(v float64, measure string) string {
	if measure == "inodes" {
		return fmt.Sprintf("%.0f inodes", v)
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for math.Abs(v) >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", v)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
