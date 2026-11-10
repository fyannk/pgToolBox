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

package instancestatus

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const sampleReport = `{
  "currentLsn": "0/6000060", "receivedLsn": "", "replayLsn": "", "systemID": "7411",
  "isPrimary": true, "replayPaused": false, "pendingRestart": true, "pendingRestartForDecrease": false,
  "isWalReceiverActive": false, "isPgRewindRunning": false, "mightBeUnavailable": false,
  "isArchivingWAL": true, "node": "worker-1",
  "lastArchivedWAL": "000000010000000000000005", "lastArchivedWALTime": "2026-09-17T12:00:00.123456Z",
  "lastFailedWAL": "000000010000000000000006", "lastFailedWALTime": "2026-09-17 12:01:00.5+00",
  "currentWAL": "000000010000000000000006", "readyWalFiles": 3, "timeLineID": 1,
  "instanceManagerVersion": "1.30.0", "instanceArch": "amd64",
  "replicationInfo": [{"applicationName": "orders-2", "state": "streaming", "syncState": "async", "syncPriority": "0", "replayLag": "00:00:00.5"}],
  "replicationSlotsInfo": [{"slotName": "_cnpg_orders_2", "slotType": "physical", "active": true, "walStatus": "reserved"},
                           {"slotName": "debezium", "slotType": "logical", "plugin": "pgoutput", "database": "app", "active": false, "walStatus": "reserved"}]
}`

// TestParseReadsTheReportVerbatim proves the subset the console keeps,
// both timestamp forms, and the bounds.
func TestParseReadsTheReportVerbatim(t *testing.T) {
	t.Parallel()
	reading, err := Parse(strings.NewReader(sampleReport))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reading.IsPrimary || !reading.PendingRestart || reading.ReadyWALFiles != 3 || reading.TimelineID != 1 ||
		reading.InstanceManagerVersion != "1.30.0" || reading.CurrentLSN != "0/6000060" || reading.Node != "worker-1" {
		t.Errorf("scalars wrong: %+v", reading)
	}
	if reading.LastArchivedWALTime == nil || !reading.LastArchivedWALTime.Equal(time.Date(2026, 9, 17, 12, 0, 0, 123456000, time.UTC)) {
		t.Errorf("RFC3339 archive time = %v", reading.LastArchivedWALTime)
	}
	if reading.LastFailedWALTime == nil || !reading.LastFailedWALTime.Equal(time.Date(2026, 9, 17, 12, 1, 0, 500000000, time.UTC)) {
		t.Errorf("PostgreSQL-form failure time = %v", reading.LastFailedWALTime)
	}
	if len(reading.Standbys) != 1 || reading.Standbys[0].ApplicationName != "orders-2" || reading.Standbys[0].ReplayLag != "00:00:00.5" {
		t.Errorf("standbys = %+v", reading.Standbys)
	}
	if len(reading.Slots) != 2 || reading.Slots[1].Name != "debezium" || reading.Slots[1].Active || reading.Slots[1].Plugin != "pgoutput" {
		t.Errorf("slots = %+v", reading.Slots)
	}
	long := strings.Repeat("x", 1000)
	bounded, err := Parse(strings.NewReader(`{"node": "` + long + `", "lastArchivedWALTime": "yesterday"}`))
	if err != nil || len(bounded.Node) != maxText || bounded.LastArchivedWALTime != nil {
		t.Errorf("bounds: node %d, time %v, err %v", len(bounded.Node), bounded.LastArchivedWALTime, err)
	}
	if _, err := Parse(strings.NewReader("not json")); err == nil {
		t.Error("a malformed report parsed")
	}
}

// TestParseNamesTheSentLSNAndFencing pins the two CloudNativePG JSON
// quirks: replicationInfo[].receivedLsn is pg_stat_replication.sent_lsn
// and surfaces as SentLSN, while the top-level receivedLsn stays the
// instance's own received position; isFenced is decoded only when
// present.
func TestParseNamesTheSentLSNAndFencing(t *testing.T) {
	t.Parallel()
	reading, err := Parse(strings.NewReader(`{
	  "receivedLsn": "0/5000000", "isFenced": true,
	  "replicationInfo": [{"applicationName": "orders-2", "receivedLsn": "0/6000060",
	    "writeLsn": "0/6000050", "flushLsn": "0/6000040", "replayLsn": "0/6000030"}]
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if reading.ReceivedLSN != "0/5000000" {
		t.Errorf("top-level ReceivedLSN = %q", reading.ReceivedLSN)
	}
	if reading.IsFenced == nil || !*reading.IsFenced {
		t.Errorf("IsFenced = %v, want true", reading.IsFenced)
	}
	want := Standby{ApplicationName: "orders-2", SentLSN: "0/6000060", WriteLSN: "0/6000050", FlushLSN: "0/6000040", ReplayLSN: "0/6000030"}
	if len(reading.Standbys) != 1 || reading.Standbys[0] != want {
		t.Errorf("standbys = %+v, want %+v", reading.Standbys, want)
	}
	absent, err := Parse(strings.NewReader(sampleReport))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if absent.IsFenced != nil {
		t.Errorf("IsFenced = %v for a report without the key, want nil", *absent.IsFenced)
	}
}

// TestParseVolumeUsages pins the fork's volumeUsages[] (origin/dev/64):
// an absent key is "not reported", a malformed one costs only itself,
// and entries that are misnamed or contradict themselves are dropped.
func TestParseVolumeUsages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		body     string
		reported bool
		volumes  []VolumeUsage
	}{
		{"upstream report", `{"isPrimary": true}`, false, nil},
		{"null", `{"volumeUsages": null}`, false, nil},
		{"malformed keeps the report", `{"isPrimary": true, "volumeUsages": {"pgdata": 1}}`, true, nil},
		{
			"fork report",
			`{"volumeUsages": [
			  {"volume": "pgdata", "totalBytes": 1000, "availableBytes": 400, "usedBytes": 550, "inodes": 100, "inodesFree": 60},
			  {"volume": "wal", "totalBytes": 500, "availableBytes": 450, "usedBytes": 40},
			  {"volume": "tbs-hist", "totalBytes": 10, "availableBytes": 1, "usedBytes": 9, "inodes": 4, "inodesFree": 0}
			]}`,
			true,
			[]VolumeUsage{
				{Volume: "pgdata", TotalBytes: 1000, AvailableBytes: 400, UsedBytes: 550, Inodes: 100, InodesFree: 60, HasInodes: true},
				{Volume: "wal", TotalBytes: 500, AvailableBytes: 450, UsedBytes: 40},
				{Volume: "tbs-hist", TotalBytes: 10, AvailableBytes: 1, UsedBytes: 9, Inodes: 4, HasInodes: true},
			},
		},
		{
			"contradictions and strangers dropped",
			`{"volumeUsages": [
			  {"volume": "pgdata", "totalBytes": 10, "availableBytes": 11, "usedBytes": 1},
			  {"volume": "wal", "totalBytes": 10, "availableBytes": 1, "usedBytes": 1, "inodes": 1, "inodesFree": 2},
			  {"volume": "../etc", "totalBytes": 10, "availableBytes": 1, "usedBytes": 1},
			  {"volume": "tbs-", "totalBytes": 10, "availableBytes": 1, "usedBytes": 1},
			  {"volume": "tbs-a", "availableBytes": 1, "usedBytes": 1},
			  {"volume": "tbs-b", "totalBytes": 10, "availableBytes": 1, "usedBytes": 1},
			  {"volume": "tbs-b", "totalBytes": 20, "availableBytes": 1, "usedBytes": 1}
			]}`,
			true,
			[]VolumeUsage{{Volume: "tbs-b", TotalBytes: 10, AvailableBytes: 1, UsedBytes: 1}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reading, err := Parse(strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if reading.VolumeUsagesReported != tc.reported {
				t.Errorf("reported = %v, want %v", reading.VolumeUsagesReported, tc.reported)
			}
			if len(reading.VolumeUsages) != len(tc.volumes) {
				t.Fatalf("volumes = %+v, want %+v", reading.VolumeUsages, tc.volumes)
			}
			for i := range tc.volumes {
				if reading.VolumeUsages[i] != tc.volumes[i] {
					t.Errorf("volume %d = %+v, want %+v", i, reading.VolumeUsages[i], tc.volumes[i])
				}
			}
		})
	}
	var many strings.Builder
	many.WriteString(`{"volumeUsages": [`)
	for i := range 100 {
		if i > 0 {
			many.WriteString(",")
		}
		fmt.Fprintf(&many, `{"volume": "tbs-t%d", "totalBytes": 1, "availableBytes": 1, "usedBytes": 0}`, i)
	}
	many.WriteString(`]}`)
	reading, err := Parse(strings.NewReader(many.String()))
	if err != nil || len(reading.VolumeUsages) != maxVolumes {
		t.Errorf("bound: %d volumes, err %v", len(reading.VolumeUsages), err)
	}
}
