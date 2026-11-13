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

package cnpgstatus

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
)

// The failover rehearsal: "if the primary died now", which instance
// CloudNativePG would promote and what it would cost, from the facts
// the console already holds. It follows the operator's own election as
// of v1.30.1 (unchanged on main):
//
//   - pkg/postgres/status.go PostgresqlStatusList.Less sorts instances:
//     those whose status could not be read last, then fenced ones after
//     the rest (new in 1.30.1: IsFenced), then primaries first, then
//     replicas by received LSN (descending), replay LSN (descending) and
//     pod name;
//   - internal/controller/replicas.go reconcileTargetPrimaryForNonReplicaCluster
//     promotes the first of that list unless it is fenced (every
//     instance fenced: no election, new in 1.30.1) or has no status,
//     after spec.failoverDelay;
//   - with the failover quorum active (spec.postgresql.synchronous.
//     failoverQuorum), internal/controller/replicas_quorum.go
//     evaluateQuorumCheckWithStatus allows the failover only when
//     R + W > N: N the FailoverQuorum's standbyNames, W its
//     standbyNumber, R the names among them whose status was read and
//     whose pod is ready. A missing object, a non-positive number or an
//     empty list refuses the failover. The check runs when the failover
//     starts and, from 1.30.1, again just before the promotion
//     (ErrQuorumCheckFailed).
//
// v1.30.0 has neither fenced rule nor the second quorum check: the
// operator may rank a fenced instance first. On a Cluster whose operator
// is detected at 1.30.0, or whose patch the console cannot tell, an
// answer that rests on fencing is not certain, and the notes say which
// rule is 1.30.1's (electionRulesOf).
//
// The console's view of an instance is its own sweep, not the
// operator's: an instance the console could not read is "not known",
// and when one is, the answer says the operator might rank it
// differently.

// ParseLSN reads PostgreSQL's "X/Y" LSN text (two hex halves of a
// 64-bit position).
func ParseLSN(s string) (uint64, bool) {
	hi, lo, ok := strings.Cut(s, "/")
	if !ok || hi == "" || lo == "" || len(hi) > 8 || len(lo) > 8 {
		return 0, false
	}
	h, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, false
	}
	l, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, false
	}
	return h<<32 | l, true
}

// RehearsalInstance is one non-primary-or-primary instance as the
// rehearsal reads it.
type RehearsalInstance struct {
	Name string
	// Reading is the instance's last good /pg/status report; nil when
	// the console never read one for this pod.
	Reading *instancestatus.Reading
	// Fresh is true when the last read succeeded, so Reading is the
	// current sweep's.
	Fresh bool
	// PodReady is the pod's Ready condition; nil when the pod is not
	// observed.
	PodReady *bool
	// Node is pod.spec.nodeName; empty when the pod is not observed or
	// not scheduled.
	Node string
}

// read reports a current reading of PostgreSQL itself: fresh, and not
// masked (instancestatus.Reading.Masked: the instance manager answered
// without reaching PostgreSQL, so its positions and walsenders are not
// read, never empty).
func (i RehearsalInstance) read() bool {
	return i.Fresh && i.Reading != nil && !i.Reading.Masked()
}

// RehearsalInput is everything the rehearsal reads.
type RehearsalInput struct {
	// Primary is Cluster.status.currentPrimary.
	Primary   string
	Instances []RehearsalInstance
	Fencing   FencingReading
	Sync      SyncReading
	// Quorum is the FailoverQuorum object; QuorumReadable is false when
	// the console cannot read the kind (not served, or forbidden), so
	// its absence means nothing.
	Quorum         *cnpgv1.FailoverQuorum
	QuorumReadable bool
	ReplicaCluster bool
	// FailoverDelay is spec.failoverDelay, in seconds.
	FailoverDelay int32
	Hibernated    bool
	// SynchronousCommit is synchronous_commit as declared in
	// spec.postgresql.parameters, verbatim; empty when not declared
	// (PostgreSQL's default, on). CloudNativePG does not set it.
	SynchronousCommit string
	// OperatorVersion is the version of the operator that runs the
	// election (ElectionVersion: "1.30.1", or "" when the operator's own
	// version is not known): the fencing rules and the second quorum
	// check are 1.30.1's.
	OperatorVersion string
	// SweepStale says why the instance readings are too old to rest a
	// "no loss" answer on (the caller's staleness rule); empty when they
	// are fresh. Whether commits waited for the candidate is a fact about
	// the sweep, so a stale sweep turns "none" into "unknown".
	SweepStale string
}

// Rehearsal outcomes.
const (
	RehearsalPromote     = "promote"
	RehearsalBlocked     = "blocked"
	RehearsalNoCandidate = "noCandidate"
	RehearsalUnknown     = "unknown"
	RehearsalNotApplied  = "notApplicable"
)

// Data loss estimates.
const (
	// LossNone: synchronous replication was satisfied for the candidate
	// at the last sweep, so no acknowledged commit would be lost.
	LossNone = "none"
	// LossUpTo: asynchronous for the candidate; the WAL it had not
	// received (BehindBytes) would be lost, committed or not.
	LossUpTo = "upTo"
	// LossUnknown: the positions were not read in good standing.
	LossUnknown = "unknown"
)

// RehearsalCandidate is one replica in the operator's order.
type RehearsalCandidate struct {
	Name string `json:"name"`
	// Eligible is false for an instance the operator would not promote
	// (fenced), or that the console cannot judge (status not read).
	Eligible    bool   `json:"eligible"`
	Reason      string `json:"reason,omitempty"`
	ReceivedLSN string `json:"receivedLsn,omitempty"`
	ReplayLSN   string `json:"replayLsn,omitempty"`
	// BehindBytes is the primary's current LSN minus this replica's
	// received LSN (its replay LSN when it reports none: an upper
	// bound), both as last read; nil when unknown.
	BehindBytes *int64 `json:"behindBytes,omitempty"`
	// SyncState is the primary's pg_stat_replication.sync_state for it.
	SyncState string `json:"syncState,omitempty"`
	PodReady  *bool  `json:"podReady,omitempty"`
	Node      string `json:"node,omitempty"`
	// SameNodeAsPrimary: one node failure takes both.
	SameNodeAsPrimary bool `json:"sameNodeAsPrimary,omitempty"`
}

// RehearsalQuorum is the failover quorum check as the operator would
// make it now.
type RehearsalQuorum struct {
	// Active is spec.postgresql.synchronous.failoverQuorum.
	Active bool `json:"active"`
	// Required is W, the FailoverQuorum's standbyNumber.
	Required int `json:"required"`
	// Set is N's members, the FailoverQuorum's standbyNames.
	Set []string `json:"set,omitempty"`
	// Promotable is R's members: the set's instances whose status was
	// read and whose pod is ready.
	Promotable []string `json:"promotable,omitempty"`
	// Met is R + W > N; nil when the console cannot tell (the object is
	// not readable).
	Met  *bool  `json:"met,omitempty"`
	Text string `json:"text"`
}

// Rehearsal is the answer.
type Rehearsal struct {
	Outcome string `json:"outcome"`
	Class   Class  `json:"class"`
	// Text is the one sentence: "orders-2 would be promoted, ..." .
	Text      string `json:"text"`
	Candidate string `json:"candidate,omitempty"`
	// BehindBytes is the candidate's distance behind the primary.
	BehindBytes *int64 `json:"behindBytes,omitempty"`
	DataLoss    string `json:"dataLoss"`
	// DataLossText explains DataLoss in words.
	DataLossText string `json:"dataLossText"`
	// Certain is false when an instance the operator might rank higher
	// could not be read.
	Certain bool                 `json:"certain"`
	Ranking []RehearsalCandidate `json:"ranking"`
	Quorum  *RehearsalQuorum     `json:"quorum,omitempty"`
	// PrimaryNode is the primary pod's node, when observed.
	PrimaryNode string `json:"primaryNode,omitempty"`
	// SameNodeAsPrimary are the replicas scheduled on the primary's
	// node.
	SameNodeAsPrimary []string `json:"sameNodeAsPrimary,omitempty"`
	// Notes are the qualifications, in reading order.
	Notes []string `json:"notes,omitempty"`
}

// electionRules says which election of the 1.30 line the operator runs.
type electionRules int

const (
	// electionPatchUnknown: the version is a floor, unknown, or a
	// pre-release of 1.30.1.
	electionPatchUnknown electionRules = iota
	// election1300 is v1.30.0: fenced instances are not ranked last, an
	// all-fenced Cluster still elects, the quorum is checked once.
	election1300
	// election1301 is v1.30.1 and later.
	election1301
)

// electionRulesOf reads the detected version, exact to the patch only.
func electionRulesOf(version string) electionRules {
	v, ok := parseSemver(version)
	core, _, _ := strings.Cut(strings.TrimPrefix(version, "v"), "-")
	if !ok || strings.Count(core, ".") != 2 {
		return electionPatchUnknown
	}
	switch first := (semver{major: 1, minor: 30, patch: 1}); {
	case !v.less(first):
		return election1301
	case v.major == 1 && v.minor == 30 && v.patch == 0:
		return election1300
	}
	return electionPatchUnknown
}

// Rehearse answers "if the primary died now".
func Rehearse(in RehearsalInput) Rehearsal {
	out := Rehearsal{Ranking: []RehearsalCandidate{}, DataLoss: LossUnknown, Certain: true}
	if in.Hibernated {
		out.Outcome, out.Class = RehearsalNotApplied, ClassInfo
		out.Text = "The Cluster is hibernated: there is no primary to lose."
		out.DataLossText = "not applicable"
		return out
	}
	if in.Primary == "" {
		out.Outcome, out.Class, out.Certain = RehearsalUnknown, ClassUnknown, false
		out.Text = "The operator names no current primary, so there is nothing to rehearse."
		out.DataLossText = "not known"
		return out
	}
	primary, primaryLSN, standbys := rehearsalPrimary(in)
	if primary != nil {
		out.PrimaryNode = primary.Node
	}
	list, sameNode := rankReplicas(in, out.PrimaryNode, primaryLSN, standbys)
	out.SameNodeAsPrimary = sameNode
	var unread []string
	for _, r := range list {
		out.Ranking = append(out.Ranking, r.c)
		if !r.read {
			unread = append(unread, r.c.Name)
		}
	}
	out.Quorum = rehearsalQuorum(in)
	rehearsalNotes(&out, in, unread)

	switch {
	case len(list) == 0:
		out.Outcome, out.Class = RehearsalNoCandidate, ClassWarning
		out.Text = "There is no replica: the Cluster would have no primary until " + in.Primary + " is back."
		out.DataLossText = "not applicable: no replica"
		return out
	case in.Fencing.State == FencingAll:
		out.Outcome, out.Class = RehearsalBlocked, ClassCritical
		out.Text = "Every instance is fenced: the operator would promote none."
		out.DataLossText = "not applicable"
		return out
	}
	first := list[0]
	if !first.c.Eligible {
		rehearsalUndecided(&out, in, list)
		return out
	}
	out.Candidate = first.c.Name
	out.BehindBytes = first.c.BehindBytes
	out.Outcome = RehearsalPromote
	lossClass := rehearsalLoss(&out, in, first.c, primary, primaryLSN)

	if out.Quorum != nil && out.Quorum.Active && (out.Quorum.Met == nil || !*out.Quorum.Met) {
		if out.Quorum.Met == nil {
			out.Certain = false
		} else {
			out.Outcome, out.Class = RehearsalBlocked, ClassCritical
			out.Text = "The operator would refuse to fail over: the failover quorum check fails (" + out.Quorum.Text + ")."
			return out
		}
	}

	out.Class = lossClass
	if !out.Certain && out.Class == ClassOK {
		out.Class = ClassWarning
	}
	if first.c.SameNodeAsPrimary && out.Class == ClassOK {
		out.Class = ClassWarning
	}
	out.Text = promotionText(out, in, first.c)
	return out
}

// rehearsalPrimary finds the primary's instance, and, from a fresh
// reading of it, its current LSN and its walsenders by application name.
// The primary may be absent from the instances, or unread.
func rehearsalPrimary(in RehearsalInput) (primary *RehearsalInstance, primaryLSN string, standbys map[string]instancestatus.Standby) {
	for i := range in.Instances {
		if in.Instances[i].Name == in.Primary {
			primary = &in.Instances[i]
		}
	}
	standbys = map[string]instancestatus.Standby{}
	if primary != nil && primary.read() {
		primaryLSN = primary.Reading.CurrentLSN
		for _, sb := range primary.Reading.Standbys {
			standbys[sb.ApplicationName] = sb
		}
	}
	return primary, primaryLSN, standbys
}

// rankedReplica is one replica as the election sorts it.
type rankedReplica struct {
	c      RehearsalCandidate
	read   bool
	fenced bool
	// selfPrimary: the instance reports itself primary.
	selfPrimary bool
	received    uint64
	replay      uint64
}

// rankReplicas qualifies every instance but the primary and sorts them
// the way the operator's PostgresqlStatusList.Less does: read before
// unread, unfenced before fenced, further received, then further
// replayed, then by name. sameNode lists the replicas on the primary's
// node in the order the instances came.
func rankReplicas(in RehearsalInput, primaryNode, primaryLSN string, standbys map[string]instancestatus.Standby) (list []rankedReplica, sameNode []string) {
	fenced := map[string]bool{}
	for _, n := range in.Fencing.Instances {
		fenced[n] = true
	}
	all := in.Fencing.State == FencingAll
	for _, inst := range in.Instances {
		if inst.Name == in.Primary {
			continue
		}
		r := qualifyReplica(inst, all || fenced[inst.Name], primaryNode, primaryLSN, standbys)
		if r.c.SameNodeAsPrimary {
			sameNode = append(sameNode, inst.Name)
		}
		list = append(list, r)
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.read != b.read {
			return a.read
		}
		if a.fenced != b.fenced {
			return !a.fenced
		}
		if a.received != b.received {
			return a.received > b.received
		}
		if a.replay != b.replay {
			return a.replay > b.replay
		}
		return a.c.Name < b.c.Name
	})
	return list, sameNode
}

// qualifyReplica says whether one replica could be promoted, why not when
// it cannot, and how far behind the primary it is.
func qualifyReplica(inst RehearsalInstance, fenced bool, primaryNode, primaryLSN string, standbys map[string]instancestatus.Standby) rankedReplica {
	r := rankedReplica{c: RehearsalCandidate{Name: inst.Name, PodReady: inst.PodReady, Node: inst.Node}}
	r.c.SameNodeAsPrimary = inst.Node != "" && primaryNode != "" && inst.Node == primaryNode
	if sb, ok := standbys[inst.Name]; ok {
		r.c.SyncState = sb.SyncState
	}
	r.fenced = fenced
	r.read = inst.read()
	switch {
	case inst.Fresh && inst.Reading != nil && inst.Reading.IsPrimary:
		// A second instance claiming the primary role: the old primary
		// not yet demoted, or a split brain. The operator would sort it
		// first as a primary; the rehearsal does not promote it. A
		// masked answer still carries it: GetStatus re-reads isPrimary
		// from the data directory after the mask (TestPinMaskedStatusIsA200;
		// round 18, CNPG P3-3), so it is checked before the mask.
		r.read, r.selfPrimary = false, true
		r.c.Reason = "it reports itself primary"
	case !r.read && inst.Fresh && inst.Reading != nil:
		r.c.Reason = "its instance manager answered without reading PostgreSQL (fenced, restarting or rewinding)"
	case !r.read:
		r.c.Reason = "its status could not be read by the console"
	case r.fenced:
		r.c.Reason = "it is fenced"
	default:
		r.c.Eligible = true
	}
	if inst.Reading != nil {
		r.c.ReceivedLSN, r.c.ReplayLSN = inst.Reading.ReceivedLSN, inst.Reading.ReplayLSN
		r.received, _ = ParseLSN(inst.Reading.ReceivedLSN)
		r.replay, _ = ParseLSN(inst.Reading.ReplayLSN)
		if r.read {
			// Received is what a promotion keeps (the replica replays it
			// before it opens); without it, replay is still an upper
			// bound of the loss, since received >= replay.
			pos := inst.Reading.ReceivedLSN
			if _, ok := ParseLSN(pos); !ok {
				pos = inst.Reading.ReplayLSN
			}
			r.c.BehindBytes = behind(primaryLSN, pos)
		}
	}
	return r
}

// rehearsalNotes adds the qualifications, in reading order, and marks the
// answer uncertain where the console could not read what the operator
// will.
func rehearsalNotes(out *Rehearsal, in RehearsalInput, unread []string) {
	if in.ReplicaCluster && in.Sync.FailoverQuorum {
		// reconcileTargetPrimaryForReplicaCluster never evaluates the
		// failover quorum: the protection does not gate this election.
		out.Notes = append(out.Notes, "The failover quorum check does not apply to a replica cluster: the operator does not evaluate it there.")
	}
	if in.FailoverDelay > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("spec.failoverDelay: the operator waits %d s after the primary fails before promoting.", in.FailoverDelay))
	}
	if in.ReplicaCluster {
		out.Notes = append(out.Notes, "This is a replica cluster: the promoted instance becomes the new designated primary and keeps replicating from the source.")
	}
	if in.Fencing.State == FencingUnreadable {
		out.Certain = false
		out.Notes = append(out.Notes, "The cnpg.io/fencedInstances annotation is not readable: CloudNativePG reads it as no fencing, so every instance runs and may be promoted, whatever fencing was meant.")
	}
	rules := electionRulesOf(in.OperatorVersion)
	if fenced := in.Fencing.State == FencingSome || in.Fencing.State == FencingAll; fenced && rules != election1301 {
		out.Certain = false
		if rules == election1300 {
			out.Notes = append(out.Notes, "CloudNativePG 1.30.0 neither ranks fenced instances last nor skips the election when every instance is fenced (both from 1.30.1): the operator may pick a fenced instance, which cannot be promoted while fenced.")
		} else {
			out.Notes = append(out.Notes, "Ranking fenced instances last and skipping the election when every instance is fenced are CloudNativePG 1.30.1 behaviour, and the console does not know the operator's patch version: 1.30.0 may pick a fenced instance, which cannot be promoted while fenced.")
		}
	}
	if in.Sync.FailoverQuorum && !in.ReplicaCluster && rules != election1301 {
		if rules == election1300 {
			out.Notes = append(out.Notes, "CloudNativePG 1.30.0 checks the failover quorum only when the failover starts; from 1.30.1 it checks it again just before promoting.")
		} else {
			out.Notes = append(out.Notes, "The failover quorum is checked when the failover starts; from CloudNativePG 1.30.1 it is checked again just before promoting (the operator's patch version is not known).")
		}
	}
	if len(unread) > 0 {
		out.Certain = false
		out.Notes = append(out.Notes, fmt.Sprintf("Not read by the console: %s. The operator reads the instances itself and would rank one of them first if it is further ahead.",
			strings.Join(unread, ", ")))
	}
}

// rehearsalUndecided answers when the first-ranked replica cannot be
// promoted: blocked when every replica is known not promotable, unknown
// when one the console could not read, or one claiming the primary role,
// might be what the operator picks.
func rehearsalUndecided(out *Rehearsal, in RehearsalInput, list []rankedReplica) {
	out.Certain = false
	var selfPrimary, unreadOpen []string
	for _, r := range list {
		switch {
		case r.selfPrimary:
			selfPrimary = append(selfPrimary, r.c.Name)
		case !r.read && !r.fenced:
			unreadOpen = append(unreadOpen, r.c.Name)
		}
	}
	out.DataLossText = "not known"
	out.Outcome, out.Class = RehearsalUnknown, ClassUnknown
	switch {
	case len(selfPrimary) > 0:
		// The operator sorts an instance reporting itself primary first;
		// what it does then is not the rehearsal's to guess.
		out.Text = strings.Join(selfPrimary, ", ") + " reports itself primary while the Cluster names " + in.Primary + ": the console cannot tell what the operator would promote."
	case len(unreadOpen) > 0 && list[0].read:
		out.Text = "Every replica the console read is fenced, but " + strings.Join(unreadOpen, ", ") + " could not be read: the console cannot tell whether the operator would promote it."
	case len(unreadOpen) > 0:
		out.Text = "No replica's status could be read, so the console cannot tell which one the operator would promote."
	default:
		out.Outcome, out.Class = RehearsalBlocked, ClassCritical
		out.Text = "Every replica is fenced or cannot be promoted: the operator would promote none."
		out.DataLossText = "not applicable"
	}
}

// promotionText is the one sentence of a promotion: who, how much WAL it
// could lose and why, and whether it shares the primary's node.
func promotionText(out Rehearsal, in RehearsalInput, first RehearsalCandidate) string {
	parts := []string{out.Candidate + " would be promoted"}
	switch out.DataLoss {
	case LossNone:
		parts = append(parts, "with no acknowledged commit lost (synchronous, for transactions using the cluster default synchronous_commit)")
	case LossUpTo:
		why := "asynchronous"
		if in.Sync.Mode != SyncModeNone {
			why = "not proven synchronous"
		}
		switch {
		case out.BehindBytes != nil && *out.BehindBytes == 0:
			parts = append(parts, "with no WAL missing at the last sweep ("+why+", so not guaranteed)")
		case out.BehindBytes != nil:
			parts = append(parts, fmt.Sprintf("losing up to %s of WAL it had not received (%s)", byteWords(*out.BehindBytes), why))
		default:
			parts = append(parts, "losing the WAL it had not received, an amount not known ("+why+")")
		}
	default:
		parts = append(parts, "with an unknown data loss")
	}
	if first.SameNodeAsPrimary {
		parts = append(parts, "but it runs on the primary's node")
	}
	return strings.Join(parts, ", ") + "."
}

// behind is primary − received, clamped at zero (the two were read at
// different instants of one sweep); nil when either is unknown.
func behind(primaryLSN, received string) *int64 {
	p, ok := ParseLSN(primaryLSN)
	if !ok {
		return nil
	}
	r, ok := ParseLSN(received)
	if !ok {
		return nil
	}
	var d int64
	if p > r {
		if diff := p - r; diff <= math.MaxInt64 {
			d = int64(diff)
		}
	}
	return &d
}

// rehearsalLoss sets DataLoss for the candidate and returns the class
// it deserves. Zero loss needs two things: the candidate was a
// synchronous standby the commits waited for (sync, or a quorum that
// holds), and synchronous_commit makes commits wait for standbys at all.
func rehearsalLoss(out *Rehearsal, in RehearsalInput, c RehearsalCandidate, primary *RehearsalInstance, primaryLSN string) Class {
	if primary == nil || primaryLSN == "" {
		out.DataLoss = LossUnknown
		out.DataLossText = "The primary's position was not read in good standing at the last sweep, so the distance is not known."
		return ClassUnknown
	}
	sync := ClassifySyncState(c.SyncState)
	covered := false
	out.DataLoss = LossUpTo
	switch {
	case in.Sync.Mode != SyncModeNone && c.SyncState == SyncStateSync:
		covered = true
		out.DataLossText = "The primary reported " + c.Name + " as a synchronous standby (sync_state sync): commits waited for it."
	case in.Sync.Mode != SyncModeNone && c.SyncState == SyncStateQuorum:
		q, why := liveQuorum(in, primary)
		switch {
		case q == nil:
			out.DataLossText = "Quorum synchronous replication, but the console cannot tell whether " + c.Name + " holds every acknowledged commit: " + why + "."
		case *q:
			covered = true
			out.DataLossText = "Quorum synchronous replication (sync_state quorum) and enough promotable members of the quorum (" + why + "): the most advanced of them holds every acknowledged commit."
		default:
			out.DataLossText = "Quorum synchronous replication, but too few members of the quorum are promotable to be sure " + c.Name + " holds every acknowledged commit (" + why + ")."
		}
	case in.Sync.Mode != SyncModeNone:
		state := c.SyncState
		if state == "" {
			state = "not among the primary's walsenders"
		}
		out.DataLossText = "Synchronous replication is configured, but " + c.Name + " is not a synchronous standby (" + state + "): " + sync.Text + "."
	default:
		out.DataLossText = "Asynchronous replication: WAL the replica had not received is lost, committed transactions included."
	}
	if covered {
		level, ok := SynchronousCommitLevel(in.SynchronousCommit)
		switch {
		case !ok:
			out.DataLoss = LossUnknown
			out.DataLossText += " But synchronous_commit is declared as " + strconv.Quote(in.SynchronousCommit) + ", a value PostgreSQL does not accept, so whether commits wait is not known."
			return ClassUnknown
		case level == "off" || level == "local":
			out.DataLossText += " But synchronous_commit is " + level + " in the Cluster's parameters: commits do not wait for any standby."
		case in.SweepStale != "":
			out.DataLoss = LossUnknown
			out.DataLossText += " But " + in.SweepStale + ": whether that still holds is not known."
			return ClassUnknown
		default:
			out.DataLoss = LossNone
			if level == "remote_write" {
				out.DataLossText += " synchronous_commit is remote_write: the standby had written the WAL but not flushed it, so a crash of its host at the same moment would still lose it."
			}
			out.DataLossText += " This holds for transactions using the cluster default: a role, a database, a session or a transaction can lower synchronous_commit, which the console cannot see."
			return ClassOK
		}
	}
	if c.BehindBytes != nil && *c.BehindBytes == 0 {
		if in.SweepStale != "" {
			// Caught up at a sweep too old to rest on: the distance now
			// is not known, whatever it was then.
			out.DataLossText += " The replica had received everything at the last sweep, but " + in.SweepStale + ": whether it still has is not known."
			return ClassUnknown
		}
		return ClassOK
	}
	return ClassWarning
}

// SynchronousCommitLevel reads synchronous_commit as PostgreSQL's enum
// lookup does (guc.c config_enum_lookup_by_name: case-insensitive, no
// trimming, the hidden boolean spellings included). An empty value is
// the default, on.
func SynchronousCommitLevel(v string) (string, bool) {
	if v == "" {
		return "on", true
	}
	switch strings.ToLower(v) {
	case "local":
		return "local", true
	case "remote_write":
		return "remote_write", true
	case "remote_apply":
		return "remote_apply", true
	case "on", "true", "yes", "1":
		return "on", true
	case "off", "false", "no", "0":
		return "off", true
	}
	return "", false
}

// liveQuorum is R + W > N for durability: W the number each commit
// waited for (the FailoverQuorum's when present, else the explicit
// synchronous.number), N the set W was drawn from, R those of N read in
// good standing. N is the FailoverQuorum's standbyNames when present;
// otherwise an upper bound: every replica of the Cluster, the declared
// standbyNamesPre/Post and every standby the primary reports in quorum
// (a disconnected member is missing from pg_stat_replication, so the
// primary's view alone undercounts N). nil when W is not known (legacy
// mode computes it in the operator). The string says why, in words.
func liveQuorum(in RehearsalInput, primary *RehearsalInstance) (*bool, string) {
	w := 0
	if in.Quorum != nil && in.Quorum.Status.StandbyNumber > 0 {
		w = in.Quorum.Status.StandbyNumber
	} else if in.Sync.Mode == SyncModeExplicit {
		w = in.Sync.Number
	}
	if w <= 0 {
		return nil, "the number of standbys each commit waits for is computed by the operator and not published"
	}
	fresh := map[string]bool{}
	for _, inst := range in.Instances {
		fresh[inst.Name] = inst.read() && !inst.Reading.IsPrimary
	}
	set := map[string]bool{}
	if in.Quorum != nil && len(in.Quorum.Status.StandbyNames) > 0 {
		for _, n := range in.Quorum.Status.StandbyNames {
			set[n] = true
		}
	} else {
		for _, inst := range in.Instances {
			if inst.Name != in.Primary {
				set[inst.Name] = true
			}
		}
		for _, n := range append(append([]string{}, in.Sync.StandbyNamesPre...), in.Sync.StandbyNamesPost...) {
			set[n] = true
		}
		for _, sb := range primary.Reading.Standbys {
			if sb.SyncState == SyncStateQuorum {
				set[sb.ApplicationName] = true
			}
		}
	}
	n, r := len(set), 0
	for name := range set {
		if fresh[name] {
			r++
		}
	}
	met := n > 0 && r+w > n
	return &met, fmt.Sprintf("R + W > N: %d + %d > %d", r, w, n)
}

// rehearsalQuorum is the operator's failover quorum check, when the
// protection is requested.
func rehearsalQuorum(in RehearsalInput) *RehearsalQuorum {
	if !in.Sync.FailoverQuorum || in.ReplicaCluster {
		return nil
	}
	q := &RehearsalQuorum{Active: true}
	if !in.QuorumReadable {
		q.Text = "the FailoverQuorum cannot be read by the console"
		return q
	}
	met := false
	q.Met = &met
	if in.Quorum == nil {
		q.Text = "no FailoverQuorum object exists, so the operator denies the failover"
		return q
	}
	s := in.Quorum.Status
	q.Required, q.Set = s.StandbyNumber, boundAll(s.StandbyNames)
	if s.StandbyNumber <= 0 || len(s.StandbyNames) == 0 {
		q.Text = "the FailoverQuorum is empty, so the operator denies the failover"
		return q
	}
	// R counts the members the operator would read with a ready pod. The
	// console sees its own sweep: a member it could not read may still be
	// read by the operator, so it is "maybe", and the answer is only
	// certain when it holds either way.
	known, maybe := map[string]bool{}, map[string]bool{}
	for _, inst := range in.Instances {
		if inst.Name == in.Primary || inst.PodReady != nil && !*inst.PodReady {
			continue
		}
		switch {
		case inst.Fresh && inst.Reading != nil && inst.PodReady != nil:
			known[inst.Name] = true
		default:
			maybe[inst.Name] = true
		}
	}
	seen := map[string]bool{}
	var unsure []string
	for _, n := range s.StandbyNames {
		if !seen[n] {
			switch {
			case known[n]:
				q.Promotable = append(q.Promotable, n)
			case maybe[n]:
				unsure = append(unsure, n)
			}
		}
		seen[n] = true
	}
	r, w, n := len(q.Promotable), s.StandbyNumber, len(seen)
	switch {
	case r+w > n:
		met = true
		q.Text = fmt.Sprintf("quorum met: %d promotable of %d, %d required (R + W > N: %d + %d > %d)", r, n, w, r, w, n)
	case r+len(unsure)+w > n:
		q.Met = nil
		q.Text = fmt.Sprintf("cannot tell: %d promotable of %d, %d required, and %s not read by the console (R + W > N needs %d)",
			r, n, w, strings.Join(unsure, ", "), n-w+1)
		return q
	default:
		met = false
		q.Text = fmt.Sprintf("quorum not met: %d promotable of %d, %d required (R + W > N: %d + %d > %d is false)", r, n, w, r, w, n)
	}
	q.Met = &met
	return q
}

// byteWords renders a byte count for the sentence, IEC.
func byteWords(b int64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%d bytes", b)
	case b < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	case b < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	}
}
