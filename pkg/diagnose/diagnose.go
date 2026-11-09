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

// Package diagnose correlates facts the other screens already carry into
// findings: statements about what is wrong and where, each anchored to
// the claim it rests on.
//
// It observes nothing of its own. Every detector is a pure function from
// already-published snapshots to findings, which is what keeps the whole
// package hermetically testable and keeps the screen inside the
// snapshot-first rule — no detector may reach the API server.
//
// The honesty rules the rest of the console follows apply here with more
// force, because a finding is the one thing on the site that is
// application-derived rather than reported:
//
//   - A finding restates and correlates. It never concludes something no
//     source reports.
//   - Every finding carries its evidence verbatim, with the origin of
//     that evidence named.
//   - A detector that cannot read its inputs says so. It never reports
//     "nothing wrong" from missing data, which is why Run returns what it
//     checked alongside what it found: an empty finding list means no
//     detector matched, never that the cluster is healthy.
package diagnose

import (
	"sort"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/evidence"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// Severity orders findings on the screen. It is a property of the
// finding, not of the detector: the same detector may report a note or a
// critical depending on what it found.
type Severity int

const (
	// SeverityNote is worth knowing and costs nothing.
	SeverityNote Severity = iota
	// SeverityWarning is something an operator should look at.
	SeverityWarning
	// SeverityHigh has removed a protection or a redundancy the cluster
	// counts on — archiving, backups, a replica able to take over — while
	// PostgreSQL still serves. Nothing is lost yet; left alone it becomes
	// critical.
	SeverityHigh
	// SeverityCritical is doing damage now: PostgreSQL not serving or
	// refusing writes, data unusable or being lost, recovery impossible.
	SeverityCritical
)

// String names the severity for display and for the state token the
// stylesheet keys off.
func (s Severity) String() string {
	switch s {
	case SeverityCritical:
		return "critical"
	case SeverityHigh:
		return "high"
	case SeverityWarning:
		return "warning"
	default:
		return "note"
	}
}

// Evidence is one verbatim claim a finding rests on, with the origin
// that made it. The text is quoted, never paraphrased: the finding above
// may summarise, but the reader must be able to see the underlying words
// and judge for themselves.
type Evidence struct {
	// Origin names who made the claim: the operator, Kubernetes, or the
	// console's own observation of them.
	Origin string
	// Object is the resource the claim is about, such as
	// "ScheduledBackup/orders-nightly".
	Object string
	// Detail is the claim, quoted as the source stated it.
	Detail string
}

// EntityRef names the object a finding is about, in the console's
// shared vocabulary: a Kubernetes kind and a name, such as Pod/orders-1
// or Backup/orders-20260902. A finding about a container is about its
// pod, and a finding about the cluster as a whole carries the Cluster
// kind with no name — the console watches one cluster, so the kind
// alone identifies it. The zero value means the finding names no single
// object, which is honest for a detector that aggregates several.
type EntityRef struct {
	Kind string
	Name string
}

// String renders the reference as kind/name, or the kind alone.
func (e EntityRef) String() string {
	if e.Name == "" {
		return e.Kind
	}
	return e.Kind + "/" + e.Name
}

// Finding is one correlated statement about what is wrong.
type Finding struct {
	// ID is the stable detector-scoped identifier, used for ordering
	// ties and for referring to a finding without quoting its prose.
	ID string
	// Check names the detector or rule that produced this finding, so
	// findings can be related to each other by their producers.
	Check string
	// Subject is the object the finding is about, when it is about one.
	// It is what lets a relation between two findings ask "the same
	// pod?" instead of assuming that two matches in one cluster must be
	// one incident.
	Subject EntityRef
	// At is when the observation the finding rests on was made, as the
	// source reported it: an event's last occurrence, a log line's most
	// recent match, a backup's creation. Zero when the source carries no
	// time — a condition or a phase is a current state, not an instant —
	// and a relation with a time window then cannot be enforced and says
	// so rather than pretending.
	At time.Time
	// Severity orders the finding on the screen.
	Severity Severity
	// Summary is one sentence stating what is wrong, in plain language.
	Summary string
	// Detail expands on the consequence when the summary alone would
	// understate it. Empty when the summary says everything.
	Detail string
	// Evidence is what the finding rests on, in the order a reader
	// should encounter it. Never empty: a finding without evidence is an
	// opinion.
	Evidence []Evidence
	// NextSteps is what an operator does about it: console-pinned
	// guidance, stated as guidance and rendered apart from the quoted
	// evidence — advice is the one thing on this screen no source
	// reported. Empty when the summary and evidence say everything.
	NextSteps string
	// ConsequenceOf names the checks whose findings this one follows
	// from, in preference order, each with the scope and window inside
	// which the relation is claimed. When one of them also matched in
	// the same run on a finding that satisfies the relation, the screen
	// presents this finding beneath it as part of one incident rather
	// than as a second alarm. The relation is catalog-pinned knowledge,
	// stated as such; each nested finding keeps its own evidence.
	ConsequenceOf []Relation
	// Link is where the reader goes next — always a screen, never an
	// action. Diagnostics proposes; it does not remediate.
	Link string
	// LinkLabel names the link.
	LinkLabel string
}

// Scope is where a causal relation is claimed to hold: between any two
// findings in the cluster, or only between findings about the same pod.
// A cluster-level cause — an operator condition, a phase, a namespace
// quota — relates to effects anywhere; a pod-level cause, such as a
// full WAL volume read from one instance's log, explains a crash on that
// instance and says nothing about a crash on another.
type Scope int

const (
	// ScopeCluster relates findings anywhere in the cluster. The zero
	// value, so a relation written without a scope claims the widest
	// one and reads as such.
	ScopeCluster Scope = iota
	// ScopePod relates only findings about the same pod. Both findings
	// must name a pod; a relation pinned to a pod that either side does
	// not name does not hold.
	ScopePod
)

// String is the scope in the words the screen uses.
func (s Scope) String() string {
	if s == ScopePod {
		return "same pod"
	}
	return "same cluster"
}

// Strength is how firmly the catalog claims a relation, in two
// qualitative grades rather than a number nobody could calibrate.
type Strength int

const (
	// StrengthEstablished marks a mechanism the upstream component
	// documents or implements: archiving that fails retains WAL, a
	// container the kernel killed is restarted by the kubelet. The
	// default, because a relation that does not deserve it should be
	// written as plausible on purpose.
	StrengthEstablished Strength = iota
	// StrengthPlausible marks a common pattern the catalog names without
	// a mechanism it can point to: a refused volume claim usually means a
	// quota, but the scheduler's message is the only thing that says so.
	StrengthPlausible
)

// String is the strength in the words the screen uses.
func (s Strength) String() string {
	if s == StrengthPlausible {
		return "plausible"
	}
	return "established mechanism"
}

// Relation is one declared causal edge: the finding carrying it is a
// consequence of the named check's findings, inside the scope and the
// time window given. Scope and window are what turn "both matched" into
// "these two matches are one incident"; without them, any two findings
// the catalog ever related would nest whatever objects they were about.
type Relation struct {
	// Cause is the ID of the check whose findings this one follows from.
	Cause string
	// Scope is where the relation holds. See Scope.
	Scope Scope
	// Within bounds how far apart the two findings' observations may be
	// for the relation to hold; zero means no bound. It is enforced only
	// when both findings carry a time — a state with no instant cannot
	// be placed in a window, and the relation then rests on scope alone.
	Within time.Duration
	// Strength is how firmly the relation is claimed. See Strength.
	Strength Strength
}

// Holds reports whether the relation joins the effect to a candidate
// cause finding, and when it does not, why — in words the screen can
// show beside the finding that stayed separate.
func (r Relation) Holds(effect, cause Finding) (bool, string) {
	if cause.Check != r.Cause {
		return false, "not the named cause"
	}
	if r.Scope == ScopePod {
		if effect.Subject.Kind != "Pod" || cause.Subject.Kind != "Pod" ||
			effect.Subject.Name == "" || cause.Subject.Name == "" {
			return false, "the catalog relates the two only on the same pod, and one of them names none"
		}
		if effect.Subject.Name != cause.Subject.Name {
			return false, "the catalog relates the two only on the same pod; this one is on " +
				cause.Subject.String()
		}
	}
	if r.Within > 0 && !effect.At.IsZero() && !cause.At.IsZero() {
		gap := effect.At.Sub(cause.At)
		if gap < 0 {
			gap = -gap
		}
		if gap > r.Within {
			return false, "the catalog relates the two only within " + r.Within.String() +
				"; these observations are " + gap.Round(time.Minute).String() + " apart"
		}
	}
	return true, ""
}

// String is the relation's declared terms: strength, scope, and the
// window when there is one. It describes the claim as written; what
// was actually enforced on one pair of findings is Terms.
func (r Relation) String() string {
	terms := r.Strength.String() + " · " + r.Scope.String()
	if r.Within > 0 {
		terms += " · within " + r.Within.String()
	}
	return terms
}

// Terms is what the relation actually rested on when it admitted this
// pair, as the screen states it beneath the nested finding. A window
// counts only when both findings carried a time and it was enforced;
// when one side has no time the label says so, because a card that
// claimed "within 1h" for a pair nothing placed in time would be
// asserting more than the evidence did.
func (r Relation) Terms(effect, cause Finding) string {
	terms := r.Strength.String() + " · " + r.Scope.String()
	switch {
	case r.Within == 0:
	case effect.At.IsZero() && cause.At.IsZero():
		terms += " · window not enforced: neither finding carries a time"
	case effect.At.IsZero() || cause.At.IsZero():
		terms += " · window not enforced: one finding carries no time"
	default:
		terms += " · within " + r.Within.String()
	}
	return terms
}

// CheckOutcome reports what became of one detector in a run.
type CheckOutcome int

const (
	// CheckMatched means the detector reported at least one finding.
	CheckMatched CheckOutcome = iota
	// CheckClear means the detector ran against readable inputs and
	// found nothing.
	CheckClear
	// CheckUnavailable means the detector could not run: its input was
	// absent, forbidden, or not yet observed. This is the outcome that
	// stops an empty screen from reading as a healthy one.
	CheckUnavailable
	// CheckNotApplicable means a catalog rule's version pins exclude the
	// observed versions. Distinct from clear on purpose: the rule looked
	// at the versions and ruled itself out, not the failure.
	CheckNotApplicable
)

// String names the outcome for display.
func (o CheckOutcome) String() string {
	switch o {
	case CheckMatched:
		return "matched"
	case CheckUnavailable:
		return "could not run"
	case CheckNotApplicable:
		return "does not apply"
	default:
		return "clear"
	}
}

// Check is one detector's account of itself in a run.
type Check struct {
	// Name is the detector's stable name.
	Name string
	// Layer is where in the stack the check looks.
	Layer Layer
	// Describes states what the detector looks for, so a reader can tell
	// what a clear result actually rules out.
	Describes string
	// Outcome is what became of it.
	Outcome CheckOutcome
	// Because states why an unavailable check could not run. Empty
	// otherwise.
	Because string
	// SourceOff marks an unavailable check whose input is switched off
	// rather than failing: log following, the object timeline, a
	// scraper, the repository-evidence consumer. The distinction is not
	// cosmetic. A source nobody turned on is permanent, identical on
	// every refresh, and asks for a decision once; a source that is on
	// and not answering is a fault, is new, and asks to be looked at
	// now. Listed together, the second is read as more of the first.
	SourceOff bool
}

// Input is everything the detectors may read: the published snapshots,
// and the clock, so that anything time-relative is testable.
//
// Each snapshot is paired with a flag saying whether it exists at all. A
// detector must consult the flag rather than the zero value, because an
// unobserved snapshot and an empty one are different claims and only the
// second one licenses "clear".
type Input struct {
	// Now is the run instant.
	Now time.Time
	// Lifecycle is the end-of-life table the EndOfLife rules read; the
	// caller supplies it (the catalog's built-in one, or a deployment's).
	Lifecycle Lifecycle
	// Backups is the backup catalog, and HasBackups whether it has been
	// observed at all.
	Backups    observe.BackupsSnapshot
	HasBackups bool
	// Events is the namespace's event window, filtered to this cluster's
	// objects. It is where Kubernetes has already written down why it
	// refused something, which is why several detectors quote it rather
	// than reasoning their own way to a cause.
	Events    observe.EventsSnapshot
	HasEvents bool
	// Pods is the instance roster, including per-container state.
	Pods    observe.PodsSnapshot
	HasPods bool
	// Cluster is the operator's report, read only for the declared
	// instance count.
	Cluster    observe.Snapshot
	HasCluster bool
	// Infrastructure carries the cluster's services, volumes, volume
	// snapshots, and owned child objects including Jobs.
	Infrastructure    observe.InfrastructureSnapshot
	HasInfrastructure bool
	// Poolers and their member pods. A pooler pod is not an instance
	// pod: it runs PgBouncer, is owned through a Deployment, and fails
	// in its own ways.
	Poolers       observe.PoolersSnapshot
	HasPoolers    bool
	PoolerPods    observe.PodsSnapshot
	HasPoolerPods bool
	// FailoverQuorum is the operator's account of whether a failover
	// could proceed.
	FailoverQuorum    observe.FailoverQuorumSnapshot
	HasFailoverQuorum bool
	// PrimaryLease is the Lease the instances use as their
	// primary-election gate from CloudNativePG 1.30.
	PrimaryLease    observe.PrimaryLeaseSnapshot
	HasPrimaryLease bool
	// ImageCatalogs are the catalogs the Cluster draws its image from.
	ImageCatalogs    observe.ImageCatalogsSnapshot
	HasImageCatalogs bool
	// KubeVersion is the API server's own /version report, the one
	// observed fact that arrives by poll rather than by watch.
	KubeVersion    observe.KubeVersionSnapshot
	HasKubeVersion bool
	// CNPGVersion is the console's one CloudNativePG version detector's
	// answer (cnpgstatus.DetectVersion: instanceManagerVersion, then the
	// Pods' cnpg.io/operatorVersion annotation, then the bootstrap
	// image, the commit hash and feature floors). When it names an exact
	// version, every rule pinned to CloudNativePG is gated on it, so the
	// findings and the overview never disagree on the version.
	CNPGVersion    cnpgstatus.Version
	HasCNPGVersion bool
	// Quotas are the namespace's ResourceQuota objects, with usage and
	// exhaustion as the API server reports them. They are what lets a
	// refusal name the quota that made it.
	Quotas    observe.QuotasSnapshot
	HasQuotas bool
	// DatabaseObjects are the declared Database, DatabaseRole,
	// Publication, and Subscription resources with the operator's
	// reconciliation report on each.
	DatabaseObjects    observe.DatabaseObjectsSnapshot
	HasDatabaseObjects bool
	// Evidence is the evidence agent's status, when one is
	// wired. The console never reads object storage itself; this is the
	// viewer's word, carried through.
	Evidence    evidence.Status
	HasEvidence bool

	// History is the object timeline, the one input that carries the
	// past: every other source is a snapshot of now, so a fault visible
	// only in repetition is invisible without it. Absent when the
	// history store is disabled.
	History    history.Snapshot
	HasHistory bool
	// Metrics and PoolerMetrics are the scraped windows. They are query
	// interfaces rather than plain snapshots because the window is a
	// rollup ring, but reading them is still an in-memory operation: no
	// detector reaches the API server or the exporters.
	Metrics       MetricsWindow
	PoolerMetrics MetricsWindow
	// InstanceStatus is the instance managers' own status reports, swept
	// from the status port. Nil when the sweep is switched off; a source
	// that has not swept yet answers so.
	InstanceStatus InstanceStatusSource
	// Volumes is the console's trend and forecast of each instance's
	// volumes. HasVolumes is false when no instance reports them, which
	// is every stock CloudNativePG build: the data decides, not the
	// version.
	Volumes    VolumesSnapshot
	HasVolumes bool
	// Logs is the continuous matcher's read side, nil when log following
	// is off. It is the one input that is not a snapshot: a stream is
	// best effort, so a detector reading it may report what was seen but
	// never how much there was.
	Logs LogObservations
	// Insight is what the Cluster's own exporters report through the
	// pgToolBox insight pack, with the console's derivations over its
	// retained samples. HasInsight is false when instance metrics are
	// not scraped; a Cluster that has not opted into the pack reads as
	// a snapshot with nothing present, which its checks report.
	Insight    insight.Snapshot
	HasInsight bool
	// Parameters are the Cluster's spec.postgresql.parameters, the
	// second source of a threshold the instance did not report.
	Parameters map[string]string
	// Drills is the console's restore-drill record of the Cluster.
	// HasDrills is false when restore drills are not enabled on the
	// console.
	Drills    DrillsSnapshot
	HasDrills bool
}

// InstanceStatusSource is the read side of the instance-status sweep.
type InstanceStatusSource interface {
	// CurrentInstanceStatus returns the snapshot and whether a sweep
	// has published one.
	CurrentInstanceStatus() (instancestatus.Snapshot, bool)
}

// MetricsWindow is the read side of a scraped metrics window, narrowed
// to what a detector needs. It is an interface so a test can supply one
// without a scraper, and so diagnose depends on the shape rather than on
// the store.
//
// A nil window means metrics are disabled or unobserved, which a
// detector must report as "could not run" rather than as no data.
type MetricsWindow interface {
	// Interval is the scrape cadence the window is filled at. It is on
	// the interface because a reading's age only means something
	// against it: a check judging whether a reading still describes now
	// calibrates against how often the console actually asks, rather
	// than against a number written into the catalog.
	Interval() time.Duration
	// Instances names the instances the window holds series for.
	Instances() []string
	// Range returns the retained series for one key at one tier.
	Range(key string, tier metrics.Tier) (times []int64, byInstance map[string][]*float64)
	// InstantReadings returns every instance's latest point-in-time
	// claims, keyed by instance then by instant key. A key an instance
	// never reported is absent rather than zero.
	InstantReadings() map[string]map[string]metrics.Instant
}

// Result is one complete run: what was found, and what was checked.
type Result struct {
	// Findings are ordered most severe first, then by detector ID, so
	// the screen is stable between refreshes that change nothing.
	Findings []Finding
	// Checks are every detector that ran, in registration order. This is
	// the half that keeps the screen honest — it is what turns an empty
	// result from "nothing is wrong" into "these fourteen things were
	// looked at, and two could not be".
	Checks []Check
}

// Detector examines the input and reports findings, plus its own account
// of whether it could run at all.
type Detector interface {
	// Name is the detector's stable name.
	Name() string
	// Layer is where in the stack the detector looks.
	Layer() Layer
	// Describes states what it looks for.
	Describes() string
	// Detect returns any findings, and — when it could not run — the
	// reason, which the screen states instead of a clear result.
	Detect(Input) (findings []Finding, unavailable string)
}

// Detectors is the registered set of hand-written detectors, in the
// order their checks are listed. These are the diagnostics that
// correlate across snapshots; the single-observation, version-scoped
// ones are declared in the catalog packages and passed to Run by the
// caller.
func Detectors() []Detector {
	return []Detector{
		quotaDetector{},
		quotaExhaustedDetector{},
		schedulingDetector{},
		imagePullDetector{},
		volumeDetector{},
		backupCadenceDetector{},
		restoreDrillDetector{},
		wraparoundDetector{},
		idleInTransactionDetector{},
		connectionSaturationDetector{},
		volumeForecastDetector{},
		walArchivingDetector{},
		autoGrowStoppedDetector{},
	}
}

// Run executes every hand-written detector and every given catalog
// rule, and assembles the result. The rules arrive as an argument
// rather than a registry so the catalog can live in its own packages —
// one per component, importing this one — without a dependency cycle.
// A detector reporting an unavailable reason contributes no findings,
// however many it returned: a detector that could not read its input
// has nothing trustworthy to say.
func Run(in Input, rules ...Rule) Result {
	detectors := Detectors()
	result := Result{Checks: make([]Check, 0, len(detectors)+len(rules))}
	for _, detector := range detectors {
		check := Check{Name: detector.Name(), Layer: detector.Layer(), Describes: detector.Describes()}
		findings, unavailable := detector.Detect(in)
		switch {
		case unavailable != "":
			check.Outcome, check.Because = CheckUnavailable, unavailable
			check.SourceOff = sourceOff(unavailable)
		case len(findings) > 0:
			check.Outcome = CheckMatched
			for i := range findings {
				if findings[i].Check == "" {
					findings[i].Check = detector.Name()
				}
			}
			result.Findings = append(result.Findings, findings...)
		default:
			check.Outcome = CheckClear
		}
		result.Checks = append(result.Checks, check)
	}
	for _, rule := range rules {
		check, findings := evaluateRule(rule, in)
		result.Findings = append(result.Findings, findings...)
		result.Checks = append(result.Checks, check)
	}
	sort.SliceStable(result.Findings, func(i, j int) bool {
		if result.Findings[i].Severity != result.Findings[j].Severity {
			return result.Findings[i].Severity > result.Findings[j].Severity
		}
		return result.Findings[i].ID < result.Findings[j].ID
	})
	return result
}
