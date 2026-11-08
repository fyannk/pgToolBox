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
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

func TestVolumeForecastDetector(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	forecast := func(measure, state string, full *time.Time) VolumeForecast {
		return VolumeForecast{
			Instance: "orders-1", Volume: "pgdata", Primary: true, Measure: measure, State: state, FullAt: full,
			Headroom: 4 << 30, Capacity: 20 << 30, Target: "filesystem", RatePerDay: 1 << 30, WindowHours: 168, Confidence: "high", AsOf: now,
		}
	}

	if _, why := (volumeForecastDetector{}).Detect(Input{Now: now}); why != reasonVolumesNotReported || !sourceOff(why) {
		t.Errorf("without volume data: %q, want the permanent stock-CloudNativePG reason", why)
	}

	cases := []struct {
		name string
		f    VolumeForecast
		want Severity
		none bool
	}{
		{"full within three days", forecast("bytes", "forecast", at(50*time.Hour)), SeverityHigh, false},
		{"full within two weeks", forecast("bytes", "forecast", at(10*24*time.Hour)), SeverityWarning, false},
		{"inodes run out too", forecast("inodes", "forecast", at(5*24*time.Hour)), SeverityWarning, false},
		{"beyond two weeks", forecast("bytes", "forecast", at(40*24*time.Hour)), 0, true},
		{"not growing", forecast("bytes", "notGrowing", nil), 0, true},
		{"too few points", forecast("bytes", "tooFewPoints", nil), 0, true},
		{"full now", forecast("bytes", "full", nil), SeverityCritical, false},
	}
	for _, c := range cases {
		findings, why := (volumeForecastDetector{}).Detect(Input{Now: now, HasVolumes: true, Volumes: VolumesSnapshot{Forecasts: []VolumeForecast{c.f}}})
		if why != "" {
			t.Fatalf("%s: unavailable %q", c.name, why)
		}
		if c.none {
			if len(findings) != 0 {
				t.Errorf("%s: %+v", c.name, findings)
			}
			continue
		}
		if len(findings) != 1 || findings[0].Severity != c.want || len(findings[0].Evidence) == 0 {
			t.Errorf("%s: %+v", c.name, findings)
		}
	}

	t.Run("autoGrow and acceleration are stated", func(t *testing.T) {
		f := forecast("bytes", "forecast", at(6*24*time.Hour))
		f.Target, f.AutoGrowSteps, f.Accelerating = "autoGrowMaxSize", true, true
		findings, _ := (volumeForecastDetector{}).Detect(Input{Now: now, HasVolumes: true, Volumes: VolumesSnapshot{Forecasts: []VolumeForecast{f}}})
		if len(findings) != 1 || !strings.Contains(findings[0].Summary, "autoGrow.maxSize") ||
			!strings.Contains(findings[0].Detail, "faster than linear") || !strings.Contains(findings[0].Detail, "step the PVC") {
			t.Errorf("%+v", findings)
		}
	})

	t.Run("a full filesystem is critical whatever autoGrow may do", func(t *testing.T) {
		full := forecast("bytes", "full", nil)
		full.Headroom, full.AutoGrowSteps = 0, true
		grow := forecast("bytes", "forecast", at(60*24*time.Hour))
		grow.Target, grow.AutoGrowSteps, grow.Capacity = "autoGrowMaxSize", true, 50<<30
		findings, _ := (volumeForecastDetector{}).Detect(Input{Now: now, HasVolumes: true, Volumes: VolumesSnapshot{Forecasts: []VolumeForecast{full, grow}}})
		if len(findings) != 1 || findings[0].Severity != SeverityCritical || !strings.Contains(findings[0].Detail, "VolumeExpansionRefused") {
			t.Errorf("%+v", findings)
		}
	})

	t.Run("with autoGrow the maxSize date is the finding, not the next step", func(t *testing.T) {
		fs := forecast("bytes", "forecast", at(2*24*time.Hour))
		fs.AutoGrowSteps = true
		grow := forecast("bytes", "forecast", at(5*24*time.Hour))
		grow.Target, grow.AutoGrowSteps = "autoGrowMaxSize", true
		findings, _ := (volumeForecastDetector{}).Detect(Input{Now: now, HasVolumes: true, Volumes: VolumesSnapshot{Forecasts: []VolumeForecast{fs, grow}}})
		if len(findings) != 1 || findings[0].Severity != SeverityWarning || !strings.HasSuffix(findings[0].ID, "/autoGrowMaxSize") {
			t.Errorf("%+v", findings)
		}
		// maxSize unread: the filesystem date stands, saying autoGrow may step.
		findings, _ = (volumeForecastDetector{}).Detect(Input{Now: now, HasVolumes: true, Volumes: VolumesSnapshot{Forecasts: []VolumeForecast{fs}}})
		if len(findings) != 1 || findings[0].Severity != SeverityHigh {
			t.Errorf("%+v", findings)
		}
	})
}

func TestWALArchivingDetector(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	wal := VolumeForecast{Instance: "orders-1", Volume: "wal", Primary: true, Measure: "bytes", State: "forecast", RatePerDay: 1 << 30, AsOf: now}
	in := Input{Now: now, HasVolumes: true, Volumes: VolumesSnapshot{Forecasts: []VolumeForecast{wal}}}
	if findings, why := (walArchivingDetector{}).Detect(in); why != "" || len(findings) != 0 {
		t.Errorf("archiving healthy: %v %q", findings, why)
	}
	in.Volumes.ArchivingFailing, in.Volumes.ArchivingDetail = "wal", "ContinuousArchiving False"
	findings, _ := (walArchivingDetector{}).Detect(in)
	if len(findings) != 1 || findings[0].Severity != SeverityHigh {
		t.Fatalf("%+v", findings)
	}
	in.Volumes.Forecasts[0].State = "notGrowing"
	if findings, _ := (walArchivingDetector{}).Detect(in); len(findings) != 0 {
		t.Errorf("a flat WAL volume is not piling up: %+v", findings)
	}
}

func TestAutoGrowStoppedDetector(t *testing.T) {
	t.Parallel()
	in := Input{HasCluster: true, Cluster: observe.Snapshot{Cluster: observe.ClusterFacts{Conditions: []observe.Condition{
		{Type: "VolumeAutoGrowStopped", Status: "True", Reason: "WALArchivingFailing", Message: "WAL archiving is failing"},
		{Type: "VolumeExpansionRefused", Status: "True", Reason: "ExpansionNotAllowed"},
		{Type: "Ready", Status: "True"},
	}}}}
	findings, why := (autoGrowStoppedDetector{}).Detect(in)
	if why != "" || len(findings) != 2 || findings[0].Severity != SeverityHigh || findings[1].Severity != SeverityWarning {
		t.Fatalf("%q %+v", why, findings)
	}
	in.Cluster.Cluster.Conditions[0].Status = "False"
	in.Cluster.Cluster.Conditions[1].Status = "False"
	if findings, _ := (autoGrowStoppedDetector{}).Detect(in); len(findings) != 0 {
		t.Errorf("resumed: %+v", findings)
	}
}
