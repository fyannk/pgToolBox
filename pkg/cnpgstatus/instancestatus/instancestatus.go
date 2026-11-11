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

// Package instancestatus reads what each instance manager says about its
// own PostgreSQL: the /pg/status report on the instance pod's status
// port, the same report the operator reads to make its decisions and
// kubectl cnpg status prints. It is the richest instance-side source the
// console has that involves no SQL — archiver state, pending restarts,
// replication slots, the manager's own version. This package is the
// report's model and its trust-boundary parser (Parse); the console's
// sweep (internal/console/sweep) does the reading: a bounded HTTP GET to
// each instance pod's IP, nothing stored beyond the last report per
// instance.
//
// The report is the instance's claim about itself, and everything here
// is attributed as such. A pod the sweep cannot reach keeps its last
// report with the failure beside it; a pod that leaves the roster is
// forgotten. From CloudNativePG 1.30 the status port's mutating paths
// require the operator's client certificate, and /pg/status does not;
// the port is read over TLS only, with the certificate verified against
// the cluster CA, and a port the console cannot read is reported per
// instance rather than guessed at.
package instancestatus

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/pkg/redact"
)

const (
	// maxStandbys and maxSlots bound the lists one report may carry.
	maxStandbys = 64
	maxSlots    = 64
	// maxText bounds each string the report carries.
	maxText = 256
	// maxVolumes bounds the volume usages one report may carry: PGDATA,
	// the WAL volume and one per tablespace.
	maxVolumes = 64
)

// Standby is one row of the primary's pg_stat_replication as the
// instance manager reports it. The instance manager reports only its own
// cluster's streaming_replica walsenders: replica clusters, logical
// walsenders and other users' base backups are absent, so this list is
// not the whole of pg_stat_replication.
type Standby struct {
	ApplicationName string
	State           string
	// SentLSN is pg_stat_replication.sent_lsn: what the primary has sent,
	// not what the standby has received. CloudNativePG serialises it
	// under the JSON key "receivedLsn" (pkg/postgres/status.go,
	// PgStatReplication.SentLsn); the name here says what it is.
	SentLSN string
	// WriteLSN, FlushLSN and ReplayLSN are the standby's positions as
	// the primary last heard them.
	WriteLSN     string
	FlushLSN     string
	ReplayLSN    string
	SyncState    string
	SyncPriority string
	WriteLag     string
	FlushLag     string
	ReplayLag    string
}

// Slot is one row of the instance's pg_replication_slots.
type Slot struct {
	Name      string
	Type      string
	Database  string
	Plugin    string
	WALStatus string
	Active    bool
}

// VolumeUsage is the fork's statfs reading of one mounted volume
// (FORK origin/dev/64 dfb05c5d4 pkg/postgres/status.go VolumeUsage):
// Volume is "pgdata", "wal" or "tbs-<tablespace>". UsedBytes counts the
// reserved blocks (blocks - bfree), AvailableBytes excludes them
// (bavail), so the volume is full for PostgreSQL when AvailableBytes
// reaches zero, before UsedBytes reaches TotalBytes. Inodes and
// InodesFree are zero when the filesystem reports no inodes; HasInodes
// says which.
type VolumeUsage struct {
	Volume         string
	TotalBytes     uint64
	AvailableBytes uint64
	UsedBytes      uint64
	Inodes         uint64
	InodesFree     uint64
	HasInodes      bool
}

// Reading is one instance's report, converted to a source-neutral shape.
// A string the instance did not report is empty; a time it did not
// report is nil.
type Reading struct {
	// Instance is the pod the report came from, and PodUID that pod's
	// identity: a recreated pod of the same name is another pod, and a
	// retained report never speaks for it.
	Instance string
	PodUID   string
	// ObservedAt is when the sweep read it.
	ObservedAt time.Time
	// IsPrimary is the instance's own claim to the primary role.
	IsPrimary bool
	// ReplayPaused reports a replica whose WAL replay is paused.
	ReplayPaused bool
	// PendingRestart reports a configuration change waiting for a
	// restart; PendingRestartForDecrease that it is a decrease of a
	// hot-standby-sensitive parameter.
	PendingRestart            bool
	PendingRestartForDecrease bool
	// IsWalReceiverActive reports a replica with a live WAL receiver.
	IsWalReceiverActive bool
	// IsPgRewindRunning reports a rewind in progress.
	IsPgRewindRunning bool
	// MightBeUnavailable is the instance manager's own flag that its
	// PostgreSQL may not be reachable, with the error it masked.
	MightBeUnavailable            bool
	MightBeUnavailableMaskedError string
	// IsArchivingWAL reports that this instance archives WAL.
	IsArchivingWAL bool
	// IsInstanceManagerUpgrading reports an in-place manager upgrade.
	IsInstanceManagerUpgrading bool
	// CurrentLSN, ReceivedLSN and ReplayLSN are the instance's positions.
	CurrentLSN  string
	ReceivedLSN string
	ReplayLSN   string
	// CurrentWAL is the segment being written; ReadyWALFiles counts the
	// segments waiting to be archived.
	CurrentWAL    string
	ReadyWALFiles int
	// LastArchivedWAL and LastFailedWAL are pg_stat_archiver's, with the
	// instants they carry.
	LastArchivedWAL     string
	LastArchivedWALTime *time.Time
	// LastArchivedWALTimeUnparsed reports a lastArchivedWALTime the
	// instance sent that does not parse as a time: LastArchivedWALTime is
	// then nil, and the instance did not say it never archived (an absent
	// or empty value does, and so does "-infinity", the value CloudNativePG
	// sends: COALESCE(last_archived_time,'-infinity') in fillArchiverStatus,
	// through pgx's text form; neverArchived).
	LastArchivedWALTimeUnparsed bool
	LastFailedWAL               string
	LastFailedWALTime           *time.Time
	// SystemID and TimelineID identify what the instance is replaying.
	SystemID   string
	TimelineID int
	// InstanceManagerVersion and InstanceArch are the manager's own.
	InstanceManagerVersion string
	InstanceArch           string
	// Node is the node the instance reports running on.
	Node string
	// IsFenced is the report's isFenced, nil when the report does not
	// carry it. CloudNativePG documents that the instance manager never
	// populates it — the operator fills it when merging reports — so
	// the fencing source of truth is the Cluster's
	// cnpg.io/fencedInstances annotation, and this is corroboration only.
	IsFenced *bool
	// Standbys is the primary's replication view, bounded; Slots the
	// instance's replication slots, bounded.
	Standbys []Standby
	Slots    []Slot
	// VolumeUsagesReported is true when the report carries the fork's
	// volumeUsages key at all; VolumeUsages are its well-formed entries,
	// bounded. Upstream CloudNativePG (v1.30.0, main) reports no
	// volume usage, so false is "not reported by this CloudNativePG",
	// never "no volume".
	VolumeUsagesReported bool
	VolumeUsages         []VolumeUsage
}

// Snapshot is what the console reads: the last report per instance,
// and per instance the failure of the last sweep, when it failed.
type Snapshot struct {
	// Generation increases by one on every sweep that published.
	Generation uint64
	// SweptAt is when the last sweep ran.
	SweptAt time.Time
	// Interval is the sweep cadence, so a reader can judge a report's
	// age against it.
	Interval time.Duration
	// Readings are the last report per instance, by instance name.
	Readings map[string]Reading
	// Failing records, per instance the last sweep could not read, the
	// failure's category. An instance present here may still have a
	// reading from an earlier sweep.
	Failing map[string]string
	// Disabled is non-empty when the sweep was asked for but does not
	// run, and says why, for the reader verbatim. A disabled snapshot
	// has no readings because nothing was read, not because no instance
	// reported.
	Disabled string
}

// Instances lists the instances with a reading, sorted.
func (s Snapshot) Instances() []string {
	names := make([]string, 0, len(s.Readings))
	for name := range s.Readings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// report is the subset of the instance manager's JSON the console reads.
// The field names are the operator's own JSON tags, pinned by the
// catalog rules that read them.
type report struct {
	CurrentLsn                    string `json:"currentLsn"`
	ReceivedLsn                   string `json:"receivedLsn"`
	ReplayLsn                     string `json:"replayLsn"`
	SystemID                      string `json:"systemID"`
	IsPrimary                     bool   `json:"isPrimary"`
	ReplayPaused                  bool   `json:"replayPaused"`
	PendingRestart                bool   `json:"pendingRestart"`
	PendingRestartForDecrease     bool   `json:"pendingRestartForDecrease"`
	IsWalReceiverActive           bool   `json:"isWalReceiverActive"`
	IsPgRewindRunning             bool   `json:"isPgRewindRunning"`
	MightBeUnavailable            bool   `json:"mightBeUnavailable"`
	MightBeUnavailableMaskedError string `json:"mightBeUnavailableMaskedError"`
	IsArchivingWAL                bool   `json:"isArchivingWAL"`
	Node                          string `json:"node"`
	LastArchivedWAL               string `json:"lastArchivedWAL"`
	LastArchivedWALTime           string `json:"lastArchivedWALTime"`
	LastFailedWAL                 string `json:"lastFailedWAL"`
	LastFailedWALTime             string `json:"lastFailedWALTime"`
	CurrentWAL                    string `json:"currentWAL"`
	ReadyWALFiles                 int    `json:"readyWalFiles"`
	TimeLineID                    int    `json:"timeLineID"`
	InstanceManagerVersion        string `json:"instanceManagerVersion"`
	InstanceArch                  string `json:"instanceArch"`
	IsInstanceManagerUpgrading    bool   `json:"isInstanceManagerUpgrading"`
	IsFenced                      *bool  `json:"isFenced"`
	ReplicationInfo               []struct {
		ApplicationName string `json:"applicationName"`
		State           string `json:"state"`
		// SentLsn is sent_lsn, under CloudNativePG's "receivedLsn" key.
		SentLsn      string `json:"receivedLsn"`
		WriteLsn     string `json:"writeLsn"`
		FlushLsn     string `json:"flushLsn"`
		ReplayLsn    string `json:"replayLsn"`
		WriteLag     string `json:"writeLag"`
		FlushLag     string `json:"flushLag"`
		ReplayLag    string `json:"replayLag"`
		SyncState    string `json:"syncState"`
		SyncPriority string `json:"syncPriority"`
	} `json:"replicationInfo"`
	ReplicationSlotsInfo []struct {
		SlotName  string `json:"slotName"`
		Plugin    string `json:"plugin"`
		SlotType  string `json:"slotType"`
		Database  string `json:"database"`
		Active    bool   `json:"active"`
		WalStatus string `json:"walStatus"`
	} `json:"replicationSlotsInfo"`
	// VolumeUsages is decoded on its own, so a malformed fork field
	// never costs the rest of the report.
	VolumeUsages json.RawMessage `json:"volumeUsages"`
}

// volumeUsage is the wire form of one volumeUsages[] entry.
type volumeUsage struct {
	Volume         string  `json:"volume"`
	TotalBytes     *uint64 `json:"totalBytes"`
	AvailableBytes *uint64 `json:"availableBytes"`
	UsedBytes      *uint64 `json:"usedBytes"`
	Inodes         uint64  `json:"inodes"`
	InodesFree     uint64  `json:"inodesFree"`
}

// Parse decodes one report into a bounded Reading. A malformed body is
// an error; an absent field stays at its zero value.
func Parse(body io.Reader) (Reading, error) {
	var r report
	if err := json.NewDecoder(body).Decode(&r); err != nil {
		return Reading{}, redact.NewError("instance status decode", redact.CategoryUnavailable, err)
	}
	reading := Reading{
		IsPrimary:                     r.IsPrimary,
		ReplayPaused:                  r.ReplayPaused,
		PendingRestart:                r.PendingRestart,
		PendingRestartForDecrease:     r.PendingRestartForDecrease,
		IsWalReceiverActive:           r.IsWalReceiverActive,
		IsPgRewindRunning:             r.IsPgRewindRunning,
		MightBeUnavailable:            r.MightBeUnavailable,
		MightBeUnavailableMaskedError: bound(r.MightBeUnavailableMaskedError),
		IsArchivingWAL:                r.IsArchivingWAL,
		IsInstanceManagerUpgrading:    r.IsInstanceManagerUpgrading,
		CurrentLSN:                    bound(r.CurrentLsn),
		ReceivedLSN:                   bound(r.ReceivedLsn),
		ReplayLSN:                     bound(r.ReplayLsn),
		CurrentWAL:                    bound(r.CurrentWAL),
		ReadyWALFiles:                 r.ReadyWALFiles,
		LastArchivedWAL:               bound(r.LastArchivedWAL),
		LastArchivedWALTime:           instant(r.LastArchivedWALTime),
		LastArchivedWALTimeUnparsed:   !neverArchived(r.LastArchivedWALTime) && instant(r.LastArchivedWALTime) == nil,
		LastFailedWAL:                 bound(r.LastFailedWAL),
		LastFailedWALTime:             instant(r.LastFailedWALTime),
		SystemID:                      bound(r.SystemID),
		TimelineID:                    r.TimeLineID,
		InstanceManagerVersion:        bound(r.InstanceManagerVersion),
		InstanceArch:                  bound(r.InstanceArch),
		Node:                          bound(r.Node),
		IsFenced:                      r.IsFenced,
	}
	for i, standby := range r.ReplicationInfo {
		if i == maxStandbys {
			break
		}
		reading.Standbys = append(reading.Standbys, Standby{
			ApplicationName: bound(standby.ApplicationName), State: bound(standby.State),
			SyncState: bound(standby.SyncState), SyncPriority: bound(standby.SyncPriority),
			WriteLag: bound(standby.WriteLag), FlushLag: bound(standby.FlushLag), ReplayLag: bound(standby.ReplayLag),
			SentLSN: bound(standby.SentLsn), WriteLSN: bound(standby.WriteLsn),
			FlushLSN: bound(standby.FlushLsn), ReplayLSN: bound(standby.ReplayLsn),
		})
	}
	for i, slot := range r.ReplicationSlotsInfo {
		if i == maxSlots {
			break
		}
		reading.Slots = append(reading.Slots, Slot{
			Name: bound(slot.SlotName), Type: bound(slot.SlotType), Database: bound(slot.Database),
			Plugin: bound(slot.Plugin), WALStatus: bound(slot.WalStatus), Active: slot.Active,
		})
	}
	reading.VolumeUsagesReported, reading.VolumeUsages = parseVolumeUsages(r.VolumeUsages)
	return reading, nil
}

// parseVolumeUsages decodes the fork's volumeUsages[]. An absent or null
// key is "not reported"; a key that does not decode is reported with no
// usable entry; an entry whose name is not pgdata, wal or tbs-<name>, or
// whose numbers contradict each other (available or used above total, or
// free inodes above inodes) is dropped rather than shown.
func parseVolumeUsages(raw json.RawMessage) (bool, []VolumeUsage) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	var entries []volumeUsage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return true, nil
	}
	var out []VolumeUsage
	seen := map[string]bool{}
	for _, e := range entries {
		if len(out) == maxVolumes {
			break
		}
		if !ValidVolumeName(e.Volume) || seen[e.Volume] || e.TotalBytes == nil || e.AvailableBytes == nil || e.UsedBytes == nil {
			continue
		}
		if *e.AvailableBytes > *e.TotalBytes || *e.UsedBytes > *e.TotalBytes || e.InodesFree > e.Inodes {
			continue
		}
		seen[e.Volume] = true
		out = append(out, VolumeUsage{
			Volume: e.Volume, TotalBytes: *e.TotalBytes, AvailableBytes: *e.AvailableBytes, UsedBytes: *e.UsedBytes,
			Inodes: e.Inodes, InodesFree: e.InodesFree, HasInodes: e.Inodes > 0,
		})
	}
	return true, out
}

// ValidVolumeName accepts the fork's volume names: pgdata, wal, and
// tbs- followed by a tablespace name (at most 63 bytes of letters,
// digits, '_', '-' or '.'), the names of the volume label of the
// cnpg_disk_* series too.
func ValidVolumeName(name string) bool {
	switch name {
	case "pgdata", "wal":
		return true
	}
	tbs, ok := strings.CutPrefix(name, "tbs-")
	if !ok || tbs == "" || len(tbs) > 63 {
		return false
	}
	for _, r := range tbs {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// bound cuts one reported string at the text ceiling.
func bound(s string) string {
	if len(s) > maxText {
		return s[:maxText]
	}
	return s
}

// instantLayouts are the forms the instance manager writes a timestamp
// in: RFC3339 from Go, and PostgreSQL's own text for pg_stat_archiver.
var instantLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07",
}

// instant parses one reported timestamp; nil for an absent or
// unparseable value, never a wrong instant.
// neverArchived reports a pg_stat_archiver time that says the instance
// has archived nothing: absent or empty, or the "-infinity" CloudNativePG
// coalesces a NULL last_archived_time to (probes.go fillArchiverStatus,
// v1.30.0 and v1.30.1; round 11, CNPG P3).
func neverArchived(raw string) bool {
	return raw == "" || raw == "-infinity"
}

func instant(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	for _, layout := range instantLayouts {
		if at, err := time.Parse(layout, raw); err == nil {
			at = at.UTC()
			return &at
		}
	}
	return nil
}
