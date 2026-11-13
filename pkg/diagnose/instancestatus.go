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
	"sort"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
)

// The conditions here read each instance manager's own report of its
// PostgreSQL, swept from the status port. It is the instance speaking
// for itself — the same report the operator reads before deciding and
// kubectl cnpg status prints — and every finding says so. A report is
// judged one instance at a time against the age of the sweep that
// produced it, exactly as a scraped metric is: a report older than
// three sweeps is refused rather than read as the present.

// statusOrigin is the attribution every finding here carries.
const statusOrigin = "instance-reported, swept from the status port"

// statusReadings is the fresh reports, sorted by instance, or the
// reason none can be read. A stale report is refused and counted; an
// instance the last sweep could not reach is reported with its
// category; and a condition that matched nothing but refused something
// reports that it could not judge those, never that they are clear.
type statusReadings struct {
	fresh   []instancestatus.Reading
	refused []string
	failing []string
	// masked are the instances whose manager answered without reading
	// PostgreSQL (fenced, restarting: Reading.Masked), which a rule on
	// PostgreSQL's own statistics skips (skipMasked), never reads as zero.
	masked []string
}

// skipMasked records reading as unjudged when its manager answered
// without reading PostgreSQL, and says so.
func (r *statusReadings) skipMasked(reading instancestatus.Reading) bool {
	if !reading.Masked() {
		return false
	}
	r.masked = append(r.masked, reading.Instance)
	return true
}

func statusOf(in Input) (statusReadings, string) {
	if in.InstanceStatus == nil {
		return statusReadings{}, reasonStatusOff
	}
	snap, ok := in.InstanceStatus.CurrentInstanceStatus()
	if !ok {
		return statusReadings{}, "the instance managers' status reports have not been swept yet"
	}
	horizon := 3 * snap.Interval
	if horizon < time.Minute {
		horizon = time.Minute
	}
	var out statusReadings
	for _, name := range snap.Instances() {
		reading := snap.Readings[name]
		if in.Now.Sub(reading.ObservedAt) > horizon {
			out.refused = append(out.refused, fmt.Sprintf("%s (%s old)", name, in.Now.Sub(reading.ObservedAt).Round(time.Second)))
			continue
		}
		out.fresh = append(out.fresh, reading)
	}
	for name, category := range snap.Failing {
		if _, hasReading := snap.Readings[name]; !hasReading {
			out.failing = append(out.failing, name+": "+category)
		}
	}
	sort.Strings(out.failing)
	return out, ""
}

// unjudged words what a clear cannot cover.
func (r statusReadings) unjudged() string {
	var parts []string
	if len(r.refused) > 0 {
		parts = append(parts, "reports the sweep stopped refreshing were refused: "+strings.Join(r.refused, ", "))
	}
	if len(r.failing) > 0 {
		parts = append(parts, "instances the sweep could not read: "+strings.Join(r.failing, ", "))
	}
	if len(r.masked) > 0 {
		parts = append(parts, "instances whose manager answered without reading PostgreSQL (fenced, restarting or rewinding): "+strings.Join(r.masked, ", "))
	}
	return strings.Join(parts, "; ")
}

// finish turns matches and the unjudged remainder into the condition's
// answer: matches stand whatever else went unread; with none, anything
// unjudged withholds the clear.
func (r statusReadings) finish(matches []conditionMatch) ([]conditionMatch, string) {
	if len(matches) > 0 {
		return matches, ""
	}
	if reason := r.unjudged(); reason != "" {
		return nil, reason
	}
	return nil, ""
}

// InstanceFlag names one boolean the instance manager reports.
type InstanceFlag string

const (
	// FlagPendingRestart is a configuration change waiting for a restart.
	FlagPendingRestart InstanceFlag = "pendingRestart"
	// FlagReplayPaused is a replica whose WAL replay is paused.
	FlagReplayPaused InstanceFlag = "replayPaused"
	// FlagMightBeUnavailable is the instance manager's own doubt that
	// its PostgreSQL is reachable.
	FlagMightBeUnavailable InstanceFlag = "mightBeUnavailable"
	// FlagRewindRunning is a pg_rewind in progress.
	FlagRewindRunning InstanceFlag = "isPgRewindRunning"
)

// fromPostgres reports a flag the instance manager reads from
// PostgreSQL itself, unread on a masked report (Reading.Masked); the
// manager's own flags (mightBeUnavailable, isPgRewindRunning) are read
// whatever PostgreSQL's state.
func (f InstanceFlag) fromPostgres() bool {
	return f == FlagPendingRestart || f == FlagReplayPaused
}

func (f InstanceFlag) read(reading instancestatus.Reading) (bool, string) {
	switch f {
	case FlagPendingRestart:
		detail := "pendingRestart true"
		if reading.PendingRestartForDecrease {
			detail += ", pendingRestartForDecrease true"
		}
		return reading.PendingRestart, detail
	case FlagReplayPaused:
		return reading.ReplayPaused, "replayPaused true"
	case FlagMightBeUnavailable:
		detail := "mightBeUnavailable true"
		if reading.MightBeUnavailableMaskedError != "" {
			detail += ": " + reading.MightBeUnavailableMaskedError
		}
		return reading.MightBeUnavailable, detail
	case FlagRewindRunning:
		return reading.IsPgRewindRunning, "isPgRewindRunning true"
	}
	return false, ""
}

// InstanceFlagSet matches each instance whose report sets the flag.
type InstanceFlagSet struct {
	Flag InstanceFlag
	// PrimaryOnly and ReplicaOnly narrow to one role, by the
	// instance's own claim.
	PrimaryOnly, ReplicaOnly bool
}

func (c InstanceFlagSet) describe() string {
	subject := "an instance"
	if c.PrimaryOnly {
		subject = "the primary"
	} else if c.ReplicaOnly {
		subject = "a replica"
	}
	return fmt.Sprintf("%s whose own status report sets %s", subject, c.Flag)
}

func (c InstanceFlagSet) evaluate(_ string, in Input) ([]conditionMatch, string) {
	readings, reason := statusOf(in)
	if reason != "" {
		return nil, reason
	}
	var matches []conditionMatch
	for _, reading := range readings.fresh {
		if c.PrimaryOnly && !reading.IsPrimary || c.ReplicaOnly && reading.IsPrimary {
			continue
		}
		if c.Flag.fromPostgres() && readings.skipMasked(reading) {
			// pending_restart and pg_is_wal_replay_paused() are
			// PostgreSQL's: a masked report's false is not a read.
			continue
		}
		set, detail := c.Flag.read(reading)
		if !set {
			continue
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + reading.Instance,
			subject:  EntityRef{Kind: "Pod", Name: reading.Instance},
			at:       reading.ObservedAt,
			evidence: []Evidence{{Origin: statusOrigin, Object: "Pod/" + reading.Instance, Detail: detail}},
			link:     "/cluster/pods", linkLabel: "Pods",
		})
	}
	return readings.finish(matches)
}

// InstanceArchiveFailing matches an archiving instance whose last
// archive failure is more recent than its last success — PostgreSQL's
// own pg_stat_archiver, as the instance manager reports it.
type InstanceArchiveFailing struct{}

func (InstanceArchiveFailing) describe() string {
	return "an archiving instance whose last archive failure is more recent than its last success"
}

func (InstanceArchiveFailing) evaluate(_ string, in Input) ([]conditionMatch, string) {
	readings, reason := statusOf(in)
	if reason != "" {
		return nil, reason
	}
	var matches []conditionMatch
	for _, reading := range readings.fresh {
		if readings.skipMasked(reading) || reading.LastFailedWALTime == nil {
			continue
		}
		if reading.LastArchivedWALTime != nil && !reading.LastFailedWALTime.After(*reading.LastArchivedWALTime) {
			continue
		}
		detail := fmt.Sprintf("lastFailedWAL %s at %s", reading.LastFailedWAL, reading.LastFailedWALTime.UTC().Format(time.RFC3339))
		if reading.LastArchivedWALTime != nil {
			detail += fmt.Sprintf(", lastArchivedWAL %s at %s", reading.LastArchivedWAL, reading.LastArchivedWALTime.UTC().Format(time.RFC3339))
		} else {
			detail += ", no successful archive reported"
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + reading.Instance,
			subject:  EntityRef{Kind: "Pod", Name: reading.Instance},
			at:       *reading.LastFailedWALTime,
			evidence: []Evidence{{Origin: statusOrigin, Object: "Pod/" + reading.Instance, Detail: detail}},
			link:     "/backups", linkLabel: "Backups",
		})
	}
	return readings.finish(matches)
}

// InstanceReadyWAL matches an instance reporting at least Threshold WAL
// segments waiting to be archived. CloudNativePG counts them only on a
// primary (pkg/management/postgres/probes.go v1.30.0 and v1.30.1:461-463
// return before GetWALArchiveCounters): a standby's 0, a replica
// cluster's designated primary's included, is no reading. Only fresh
// primary readings are judged; with none, the rule says so instead of
// clearing (CNPG P3-1).
type InstanceReadyWAL struct {
	Threshold int
}

func (c InstanceReadyWAL) describe() string {
	return fmt.Sprintf("an instance reporting at least %d WAL segments waiting to be archived", c.Threshold)
}

func (c InstanceReadyWAL) evaluate(_ string, in Input) ([]conditionMatch, string) {
	readings, reason := statusOf(in)
	if reason != "" {
		return nil, reason
	}
	var matches []conditionMatch
	primaries := 0
	for _, reading := range readings.fresh {
		if !reading.IsPrimary || readings.skipMasked(reading) {
			continue
		}
		primaries++
		if reading.ReadyWALFiles < c.Threshold {
			continue
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + reading.Instance,
			subject:  EntityRef{Kind: "Pod", Name: reading.Instance},
			at:       reading.ObservedAt,
			summary:  fmt.Sprintf("Instance %s reports %d WAL segments waiting to be archived.", reading.Instance, reading.ReadyWALFiles),
			evidence: []Evidence{{
				Origin: statusOrigin, Object: "Pod/" + reading.Instance,
				Detail: fmt.Sprintf("readyWalFiles %d, currentWAL %s", reading.ReadyWALFiles, reading.CurrentWAL),
			}},
			link: "/backups", linkLabel: "Backups",
		})
	}
	if primaries == 0 {
		reason := "no fresh report of a primary: CloudNativePG counts the WAL segments waiting to be archived only on a primary, " +
			"so a standby, and a replica cluster's designated primary, report none"
		if more := readings.unjudged(); more != "" {
			reason += "; " + more
		}
		return nil, reason
	}
	return readings.finish(matches)
}

// InstanceSlotInactive matches an instance holding an inactive
// replication slot it did not create for its own replicas — the
// operator's slots carry a fixed prefix — which is a consumer that
// stopped while its slot keeps WAL.
type InstanceSlotInactive struct{}

// operatorSlotPrefix is the prefix CloudNativePG gives the slots it
// manages for its own replicas.
const operatorSlotPrefix = "_cnpg_"

func (InstanceSlotInactive) describe() string {
	return "an inactive replication slot the operator does not manage"
}

func (InstanceSlotInactive) evaluate(_ string, in Input) ([]conditionMatch, string) {
	readings, reason := statusOf(in)
	if reason != "" {
		return nil, reason
	}
	var matches []conditionMatch
	for _, reading := range readings.fresh {
		if readings.skipMasked(reading) {
			continue
		}
		for _, slot := range reading.Slots {
			if slot.Active || strings.HasPrefix(slot.Name, operatorSlotPrefix) {
				continue
			}
			detail := fmt.Sprintf("slot %s (%s) active false", slot.Name, slot.Type)
			if slot.Plugin != "" {
				detail += ", plugin " + slot.Plugin
			}
			if slot.Database != "" {
				detail += ", database " + slot.Database
			}
			if slot.WALStatus != "" {
				detail += ", wal_status " + slot.WALStatus
			}
			matches = append(matches, conditionMatch{
				idSuffix: "/" + reading.Instance + "/" + slot.Name,
				subject:  EntityRef{Kind: "Pod", Name: reading.Instance},
				at:       reading.ObservedAt,
				summary:  fmt.Sprintf("Replication slot %q on %s is inactive: its consumer is not connected.", slot.Name, reading.Instance),
				evidence: []Evidence{{Origin: statusOrigin, Object: "Pod/" + reading.Instance, Detail: detail}},
				link:     "/cluster/pods", linkLabel: "Pods",
			})
		}
	}
	return readings.finish(matches)
}

// InstanceManagerDrift matches the instances reporting more than one
// instance-manager version among them: a rollout or in-place upgrade
// that did not reach every instance.
type InstanceManagerDrift struct{}

func (InstanceManagerDrift) describe() string {
	return "instances reporting more than one instance-manager version"
}

func (InstanceManagerDrift) evaluate(_ string, in Input) ([]conditionMatch, string) {
	readings, reason := statusOf(in)
	if reason != "" {
		return nil, reason
	}
	versions := map[string][]string{}
	for _, reading := range readings.fresh {
		if reading.InstanceManagerVersion == "" {
			continue
		}
		versions[reading.InstanceManagerVersion] = append(versions[reading.InstanceManagerVersion], reading.Instance)
	}
	if len(versions) < 2 {
		return readings.finish(nil)
	}
	keys := make([]string, 0, len(versions))
	for version := range versions {
		keys = append(keys, version)
	}
	sort.Strings(keys)
	var evidence []Evidence
	for _, version := range keys {
		evidence = append(evidence, Evidence{
			Origin: statusOrigin,
			Object: "instances " + strings.Join(versions[version], ", "),
			Detail: "instanceManagerVersion " + version,
		})
	}
	return []conditionMatch{{
		subject:  clusterSubject,
		summary:  fmt.Sprintf("The instances run %d different instance-manager versions.", len(versions)),
		evidence: evidence,
		link:     "/cluster/pods", linkLabel: "Pods",
	}}, ""
}
