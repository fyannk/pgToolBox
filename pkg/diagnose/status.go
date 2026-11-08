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
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// The conditions here read the parts of the operator's Cluster status
// that are not a phase or a condition: its own classification of
// instances and claims, the certificates it manages, what it could not
// reconcile, and what each instance reported back. Several of them
// name a state that is ordinary in passing — a claim with no pod for
// the seconds a pod takes to be recreated, a cluster creating a replica
// — and a finding only when it lasts. The operator writes no clock
// beside those states, so the console keeps one: the store records
// when it first saw the state it now sees, and a check with a minimum
// age reads that record. It is a floor, not a start time — the console
// may have begun watching after the state began — and the evidence says
// so.

// heldEvidence is the console's own account of how long a tracked
// state has held: matched when the state has held at least minAge,
// unjudged with a reason when the console cannot tell.
func heldEvidence(in Input, key string, minAge time.Duration, what string) (Evidence, bool, string) {
	if minAge == 0 {
		return Evidence{}, true, ""
	}
	since, tracked := in.Cluster.Cluster.Since(key)
	if !tracked {
		return Evidence{}, false, fmt.Sprintf(
			"the console has no record yet of how long %s has held, and the check applies only after %s", what, minAge)
	}
	if in.Now.Sub(since) < minAge {
		return Evidence{}, false, ""
	}
	return Evidence{
		Origin: "console-observed",
		Object: "Cluster status",
		Detail: fmt.Sprintf("%s unchanged since at least %s, when the console first saw it — %s ago",
			what, since.UTC().Format("15:04:05Z"), in.Now.Sub(since).Round(time.Minute)),
	}, true, ""
}

// ClusterPhaseHeld matches the operator-reported phase once it has held
// for at least MinAge — the phases the operator passes through on the
// way somewhere, which are only a finding when the way is blocked. It
// is ClusterPhase with the console's clock on it.
type ClusterPhaseHeld struct {
	// AnyOf are the phases that match, any of them.
	AnyOf []string
	// MinAge is how long the phase must have held.
	MinAge time.Duration
}

func (c ClusterPhaseHeld) describe() string {
	return fmt.Sprintf("the operator reporting phase %s for at least %s", strings.Join(c.AnyOf, " or "), c.MinAge)
}

func (c ClusterPhaseHeld) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	phase := in.Cluster.Cluster.Phase
	matched := false
	for _, candidate := range c.AnyOf {
		if phase == candidate {
			matched = true
			break
		}
	}
	if !matched {
		return nil, ""
	}
	held, ok, unjudged := heldEvidence(in, "phase="+phase, c.MinAge, "the phase")
	if unjudged != "" {
		return nil, unjudged
	}
	if !ok {
		return nil, ""
	}
	detail := "phase " + phase
	if reason := in.Cluster.Cluster.PhaseReason; reason != "" {
		detail += ": " + reason
	}
	return []conditionMatch{{subject: clusterSubject, evidence: []Evidence{
		{Origin: "operator-reported", Object: "Cluster status", Detail: detail}, held,
	}}}, ""
}

// StatusList names one of the operator's status lists.
type StatusList string

const (
	// ListFailedInstances is status.instancesStatus.failed: instances
	// the operator will not schedule again.
	ListFailedInstances StatusList = "failed instances"
	// ListUnusablePVC is status.unusablePVC: claims that are part of an
	// incomplete group, such as data with its WAL claim missing.
	ListUnusablePVC StatusList = "unusable claims"
	// ListDanglingPVC is status.danglingPVC: claims attached to no pod.
	ListDanglingPVC StatusList = "dangling claims"
	// ListResizingPVC is status.resizingPVC: claims carrying a resize
	// condition.
	ListResizingPVC StatusList = "resizing claims"
	// ListInitializingPVC is status.initializingPVC: claims created by
	// a job that has produced no pod yet.
	ListInitializingPVC StatusList = "initializing claims"
)

// names, key and subject kind of the list on the given facts.
func (l StatusList) read(facts observe.ClusterFacts) (names []string, key, kind string) {
	switch l {
	case ListFailedInstances:
		return facts.FailedInstances, "failedInstance", "Pod"
	case ListUnusablePVC:
		return facts.UnusablePVCs, "unusablePVC", "PersistentVolumeClaim"
	case ListDanglingPVC:
		return facts.DanglingPVCs, "danglingPVC", "PersistentVolumeClaim"
	case ListResizingPVC:
		return facts.ResizingPVCs, "resizingPVC", "PersistentVolumeClaim"
	case ListInitializingPVC:
		return facts.InitializingPVCs, "initializingPVC", "PersistentVolumeClaim"
	}
	return nil, "", ""
}

// StatusListed matches each name on one of the operator's status lists,
// once it has been there for at least MinAge. The operator's list is
// its own classification, quoted as such; the console adds only how
// long the name has stayed on it.
type StatusListed struct {
	// List is the status list read.
	List StatusList
	// MinAge is how long the name must have been listed; zero matches
	// on presence.
	MinAge time.Duration
}

func (c StatusListed) describe() string {
	described := "the operator listing " + string(c.List)
	if c.MinAge > 0 {
		described += fmt.Sprintf(" for at least %s", c.MinAge)
	}
	return described
}

func (c StatusListed) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	names, key, kind := c.List.read(in.Cluster.Cluster)
	var matches []conditionMatch
	for _, name := range names {
		held, ok, unjudged := heldEvidence(in, key+"="+name, c.MinAge, "its listing")
		if unjudged != "" {
			return nil, unjudged
		}
		if !ok {
			continue
		}
		evidence := []Evidence{{
			Origin: "operator-reported",
			Object: kind + "/" + name,
			Detail: "listed under " + string(c.List) + " in the Cluster status",
		}}
		if held.Origin != "" {
			evidence = append(evidence, held)
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + name,
			subject:  EntityRef{Kind: kind, Name: name},
			evidence: evidence,
		})
	}
	return matches, ""
}

// PrimaryFailing matches the operator's own record that the current
// primary has been unhealthy since an instant, once that instant is at
// least MinAge ago. The operator stamps the instant when its status
// check on the primary starts failing and clears it when the primary
// recovers or is replaced, so a stamp that stays is a primary neither
// recovering nor being failed over.
type PrimaryFailing struct {
	// MinAge is how long the primary must have been failing.
	MinAge time.Duration
}

func (c PrimaryFailing) describe() string {
	return fmt.Sprintf("the operator reporting the current primary as failing for at least %s", c.MinAge)
}

func (c PrimaryFailing) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	facts := in.Cluster.Cluster
	since := facts.PrimaryFailingSince
	if since == nil || in.Now.Sub(*since) < c.MinAge {
		return nil, ""
	}
	primary := facts.CurrentPrimary
	detail := fmt.Sprintf("currentPrimaryFailingSinceTimestamp %s — %s ago",
		since.UTC().Format(time.RFC3339), in.Now.Sub(*since).Round(time.Minute))
	return []conditionMatch{{
		idSuffix: "/" + primary,
		subject:  EntityRef{Kind: "Pod", Name: primary},
		at:       *since,
		evidence: []Evidence{{Origin: "operator-reported", Object: "Cluster status", Detail: detail}},
	}}, ""
}

// TimelineDivergence matches an instance reporting a PostgreSQL
// timeline other than the cluster's, once it has reported it for at
// least MinAge. Every promotion opens a new timeline; a replica still
// on the old one after the move settled is not following the new
// primary and cannot fail over cleanly.
type TimelineDivergence struct {
	// MinAge is how long the divergence must have held.
	MinAge time.Duration
}

func (c TimelineDivergence) describe() string {
	return fmt.Sprintf("an instance reporting a timeline other than the cluster's for at least %s", c.MinAge)
}

func (c TimelineDivergence) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	facts := in.Cluster.Cluster
	if facts.TimelineID == nil {
		return nil, "the operator reports no cluster timeline to compare instances against"
	}
	var matches []conditionMatch
	for _, instance := range facts.InstanceTimelines {
		if instance.TimelineID == 0 || instance.TimelineID == *facts.TimelineID {
			continue
		}
		key := fmt.Sprintf("instanceTimeline=%s:%d", instance.Instance, instance.TimelineID)
		held, ok, unjudged := heldEvidence(in, key, c.MinAge, "the instance's timeline")
		if unjudged != "" {
			return nil, unjudged
		}
		if !ok {
			continue
		}
		evidence := []Evidence{
			{
				Origin: "operator-reported", Object: "Cluster status",
				Detail: fmt.Sprintf("timelineID %d", *facts.TimelineID),
			},
			{
				Origin: "instance-reported through the operator", Object: "Pod/" + instance.Instance,
				Detail: fmt.Sprintf("instancesReportedState timeLineID %d", instance.TimelineID),
			},
		}
		if held.Origin != "" {
			evidence = append(evidence, held)
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + instance.Instance,
			subject:  EntityRef{Kind: "Pod", Name: instance.Instance},
			summary: fmt.Sprintf("Instance %s reports timeline %d while the cluster is on timeline %d.",
				instance.Instance, instance.TimelineID, *facts.TimelineID),
			evidence: evidence,
		})
	}
	return matches, ""
}

// CertificateExpiring matches a certificate the operator reports as
// expiring within the given span of now, or already expired when the
// span is zero. The expiry instants are the operator's; the comparison
// is the console's, against its own clock.
type CertificateExpiring struct {
	// Within is how far ahead of now an expiry matches; zero matches
	// only an expiry already in the past.
	Within time.Duration
}

func (c CertificateExpiring) describe() string {
	if c.Within == 0 {
		return "a certificate the operator reports as already expired"
	}
	return fmt.Sprintf("a certificate the operator reports as expiring within %s", c.Within)
}

func (c CertificateExpiring) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	var matches []conditionMatch
	for _, certificate := range in.Cluster.Cluster.CertificateExpirations {
		left := certificate.ExpiresAt.Sub(in.Now)
		if left > c.Within {
			continue
		}
		if c.Within > 0 && left <= 0 {
			// Already expired: the zero-span check's finding, not this
			// one's, so the two never report the same certificate.
			continue
		}
		detail := "expires " + certificate.ExpiresAt.UTC().Format(time.RFC3339)
		if left <= 0 {
			detail += fmt.Sprintf(" — expired %s ago", (-left).Round(time.Minute))
		} else {
			detail += fmt.Sprintf(" — in %s", left.Round(time.Minute))
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + certificate.Secret,
			subject:  EntityRef{Kind: "Secret", Name: certificate.Secret},
			evidence: []Evidence{{Origin: "operator-reported", Object: "Secret/" + certificate.Secret, Detail: detail}},
		})
	}
	return matches, ""
}

// RoleUnreconcilable matches a managed role the operator reports it
// cannot reconcile, quoting the operator's own errors.
type RoleUnreconcilable struct{}

func (RoleUnreconcilable) describe() string {
	return "a managed role the operator reports it cannot reconcile"
}

func (RoleUnreconcilable) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	var matches []conditionMatch
	for _, problem := range in.Cluster.Cluster.UnreconcilableRoles {
		matches = append(matches, conditionMatch{
			idSuffix: "/" + problem.Role,
			subject:  EntityRef{Kind: "ManagedRole", Name: problem.Role},
			summary:  fmt.Sprintf("The operator cannot reconcile the managed role %q.", problem.Role),
			evidence: []Evidence{{
				Origin: "operator-reported",
				Object: "managed role " + problem.Role,
				Detail: "cannotReconcile: " + strings.Join(problem.Errors, "; "),
			}},
		})
	}
	return matches, ""
}

// TablespaceError matches a declared tablespace whose reconciliation
// the operator reports as erroring.
type TablespaceError struct{}

func (TablespaceError) describe() string {
	return "a declared tablespace whose reconciliation the operator reports an error for"
}

func (TablespaceError) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	var matches []conditionMatch
	for _, tablespace := range in.Cluster.Cluster.Tablespaces {
		if tablespace.Error == "" {
			continue
		}
		matches = append(matches, conditionMatch{
			idSuffix: "/" + tablespace.Name,
			subject:  EntityRef{Kind: "Tablespace", Name: tablespace.Name},
			summary:  fmt.Sprintf("The operator cannot reconcile the tablespace %q.", tablespace.Name),
			evidence: []Evidence{{
				Origin: "operator-reported",
				Object: "tablespace " + tablespace.Name,
				Detail: fmt.Sprintf("state %s: %s", tablespace.State, tablespace.Error),
			}},
		})
	}
	return matches, ""
}

// ReplicaSwitchStuck matches the operator reporting a switch to replica
// cluster in progress for at least MinAge.
type ReplicaSwitchStuck struct {
	// MinAge is how long the switch must have been in progress.
	MinAge time.Duration
}

func (c ReplicaSwitchStuck) describe() string {
	return fmt.Sprintf("the operator reporting a switch to replica cluster in progress for at least %s", c.MinAge)
}

func (c ReplicaSwitchStuck) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	if !in.Cluster.Cluster.ReplicaSwitchInProgress {
		return nil, ""
	}
	held, ok, unjudged := heldEvidence(in, "replicaSwitch=inProgress", c.MinAge, "the switch")
	if unjudged != "" {
		return nil, unjudged
	}
	if !ok {
		return nil, ""
	}
	return []conditionMatch{{subject: clusterSubject, evidence: []Evidence{
		{Origin: "operator-reported", Object: "Cluster status", Detail: "switchReplicaClusterStatus.inProgress true"}, held,
	}}}, ""
}

// InstancesShort matches fewer ready instances than declared, as the
// operator itself counts them, once the shortfall has held for at
// least MinAge. A rollout passes through this state one instance at a
// time; a shortfall that stays is a member that is not coming back.
type InstancesShort struct {
	// MinAge is how long the shortfall must have held.
	MinAge time.Duration
}

func (c InstancesShort) describe() string {
	return fmt.Sprintf("the operator reporting fewer ready instances than declared for at least %s", c.MinAge)
}

func (c InstancesShort) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	facts := in.Cluster.Cluster
	if facts.DesiredInstances == nil || facts.ReadyInstances == nil {
		return nil, "the operator reports no ready-instance count to compare against the declared one"
	}
	if *facts.ReadyInstances >= *facts.DesiredInstances {
		return nil, ""
	}
	key := fmt.Sprintf("readyShort=%d/%d", *facts.ReadyInstances, *facts.DesiredInstances)
	held, ok, unjudged := heldEvidence(in, key, c.MinAge, "the shortfall")
	if unjudged != "" {
		return nil, unjudged
	}
	if !ok {
		return nil, ""
	}
	return []conditionMatch{{
		subject: clusterSubject,
		summary: fmt.Sprintf("%d of %d declared instances are ready.", *facts.ReadyInstances, *facts.DesiredInstances),
		evidence: []Evidence{{
			Origin: "operator-reported", Object: "Cluster status",
			Detail: fmt.Sprintf("%d instances declared, readyInstances %d", *facts.DesiredInstances, *facts.ReadyInstances),
		}, held},
	}}, ""
}

// PoolerPhase matches a Pooler whose operator-reported phase is one of
// the given, quoting the phase reason the operator wrote beside it.
type PoolerPhase struct {
	// AnyOf are the phases that match.
	AnyOf []string
}

func (c PoolerPhase) describe() string {
	return "a Pooler the operator reports in phase " + strings.Join(c.AnyOf, " or ")
}

func (c PoolerPhase) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := poolersUnavailable(in); reason != "" {
		return nil, reason
	}
	var matches []conditionMatch
	for _, pooler := range in.Poolers.Poolers {
		matched := false
		for _, phase := range c.AnyOf {
			if pooler.Phase == phase {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		detail := "phase " + pooler.Phase
		if pooler.PhaseReason != "" {
			detail += ": " + pooler.PhaseReason
		}
		matches = append(matches, conditionMatch{
			idSuffix:  "/" + pooler.Name,
			subject:   EntityRef{Kind: "Pooler", Name: pooler.Name},
			summary:   fmt.Sprintf("Pooler %s is in phase %s.", pooler.Name, pooler.Phase),
			evidence:  []Evidence{{Origin: "operator-reported", Object: "Pooler/" + pooler.Name, Detail: detail}},
			link:      "/poolers",
			linkLabel: "Poolers",
		})
	}
	return matches, ""
}

// PrimaryLeaseExpired matches the primary lease when its last renewal
// is older than its own duration by at least Grace, or when it is
// released while the operator names a current primary and no move is
// in flight. The instance holding the primary role renews the lease
// every few seconds for as long as it runs; a lease nobody renews is a
// primary whose instance manager has stopped, or a primary that cannot
// reach the API server — the lease is the one thing it must keep
// writing.
type PrimaryLeaseExpired struct {
	// Grace is how far past its duration the lease must be. The
	// operator's own renewal deadline is shorter than the duration, so
	// a lease past its duration is already one the holder failed to
	// renew in time; the grace keeps a slow API server out of it.
	Grace time.Duration
}

func (c PrimaryLeaseExpired) describe() string {
	return fmt.Sprintf("the primary lease unrenewed for %s past its duration, or released with a primary named", c.Grace)
}

func (c PrimaryLeaseExpired) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := leaseUnavailable(in); reason != "" {
		return nil, reason
	}
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	lease := in.PrimaryLease.Lease
	cluster := in.Cluster.Cluster
	if !lease.Present {
		return nil, ""
	}
	moving := cluster.TargetPrimary != "" && cluster.TargetPrimary != cluster.CurrentPrimary
	if lease.Holder == "" {
		if cluster.CurrentPrimary == "" || moving {
			return nil, ""
		}
		if lease.RenewedAt != nil && in.Now.Sub(*lease.RenewedAt) < c.Grace {
			return nil, ""
		}
		detail := "holderIdentity empty"
		if lease.RenewedAt != nil {
			detail += ", released " + lease.RenewedAt.UTC().Format(time.RFC3339)
		}
		return []conditionMatch{{
			idSuffix: "/" + cluster.CurrentPrimary,
			subject:  EntityRef{Kind: "Pod", Name: cluster.CurrentPrimary},
			summary:  fmt.Sprintf("The primary lease is released while the operator names %s as the current primary.", cluster.CurrentPrimary),
			evidence: []Evidence{
				{Origin: "Kubernetes-observed", Object: "Lease/" + leaseName(in), Detail: detail},
				{Origin: "operator-reported", Object: "Cluster status", Detail: "currentPrimary " + cluster.CurrentPrimary},
			},
		}}, ""
	}
	if lease.RenewedAt == nil || lease.DurationSeconds == nil {
		return nil, "the primary lease carries no renewal time or duration, so its validity cannot be judged"
	}
	expiry := lease.RenewedAt.Add(time.Duration(*lease.DurationSeconds) * time.Second)
	if in.Now.Sub(expiry) < c.Grace {
		return nil, ""
	}
	return []conditionMatch{{
		idSuffix: "/" + lease.Holder,
		subject:  EntityRef{Kind: "Pod", Name: lease.Holder},
		at:       *lease.RenewedAt,
		summary: fmt.Sprintf("The primary lease held by %s has not been renewed since %s, %s past its duration.",
			lease.Holder, lease.RenewedAt.UTC().Format("15:04:05Z"), in.Now.Sub(expiry).Round(time.Second)),
		evidence: []Evidence{{
			Origin: "Kubernetes-observed",
			Object: "Lease/" + leaseName(in),
			Detail: fmt.Sprintf("holderIdentity %s, renewTime %s, leaseDurationSeconds %d",
				lease.Holder, lease.RenewedAt.UTC().Format(time.RFC3339), *lease.DurationSeconds),
		}},
	}}, ""
}

// PrimaryLeaseHolderMismatch matches a lease held by an instance other
// than the one the operator names as current primary, with no primary
// move in flight to explain the difference. The two claims come from
// two writers — the instance holds the lease, the operator writes the
// status — and the finding is their disagreement.
type PrimaryLeaseHolderMismatch struct{}

func (PrimaryLeaseHolderMismatch) describe() string {
	return "the primary lease held by an instance other than the operator's current primary"
}

func (PrimaryLeaseHolderMismatch) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := leaseUnavailable(in); reason != "" {
		return nil, reason
	}
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	lease := in.PrimaryLease.Lease
	cluster := in.Cluster.Cluster
	if !lease.Present || lease.Holder == "" || cluster.CurrentPrimary == "" {
		return nil, ""
	}
	if cluster.TargetPrimary != "" && cluster.TargetPrimary != cluster.CurrentPrimary {
		return nil, ""
	}
	if lease.Holder == cluster.CurrentPrimary {
		return nil, ""
	}
	return []conditionMatch{{
		idSuffix: "/" + lease.Holder,
		subject:  EntityRef{Kind: "Pod", Name: lease.Holder},
		summary: fmt.Sprintf("The primary lease is held by %s while the operator names %s as the current primary.",
			lease.Holder, cluster.CurrentPrimary),
		evidence: []Evidence{
			{Origin: "Kubernetes-observed", Object: "Lease/" + leaseName(in), Detail: "holderIdentity " + lease.Holder},
			{
				Origin: "operator-reported", Object: "Cluster status",
				Detail: "currentPrimary " + cluster.CurrentPrimary + ", targetPrimary " + cluster.TargetPrimary,
			},
		},
	}}, ""
}

// leaseName is the observed Lease's own name for the evidence, with a
// plain fallback for a fixture that carries none.
func leaseName(in Input) string {
	if name := in.PrimaryLease.Lease.Name; name != "" {
		return name
	}
	return "primary"
}
