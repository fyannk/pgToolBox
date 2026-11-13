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
	"fmt"
	"sort"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/diag"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// health maps the read model's class onto the view model's state; the
// words are the same seven.
func health(c cnpgstatus.Class) viewmodel.Health {
	h := viewmodel.Health(c)
	if !h.Valid() {
		return viewmodel.HealthUnknown
	}
	return h
}

// requiredSources are the sources whose loss makes the verdict unknown:
// without them the console cannot tell a healthy Cluster from a broken
// one.
var requiredSources = []string{cache.SourceClusters, cache.SourcePods}

// verdict is the Cluster's health roll-up: the worst of every contributor,
// each listed with its provenance, worst first. It never matches a
// string on the phase.
func (s *Service) verdict(g *gathered, run *diag.Run) viewmodel.Verdict {
	now := s.now().UTC()
	operator := func(source string) viewmodel.Provenance {
		return s.provenance(g.key.Namespace, cache.SourceClusters, viewmodel.OriginOperator, source, now)
	}
	var items []viewmodel.Contributor
	add := func(c viewmodel.Contributor) { items = append(items, c) }

	phase, phaseText := phaseDetail(g.phaseMinor, g.cluster.Status.Phase)
	add(viewmodel.Contributor{ID: "phase", Health: health(phase.Class), Label: "Phase", Detail: phaseText, Provenance: operator("Cluster.status.phase")})

	ready := conditionOf(g.cluster, "Ready")
	if ready == nil {
		add(viewmodel.Contributor{ID: "readyCondition", Health: viewmodel.HealthUnknown, Label: "Ready condition", Detail: "not reported", Provenance: operator("Cluster.status.conditions[Ready]")})
	} else {
		reading := cnpgstatus.ClassifyCondition(g.phaseMinor, ready.Type, string(ready.Status), ready.Reason)
		add(viewmodel.Contributor{ID: "readyCondition", Health: health(reading.Class), Label: "Ready condition", Detail: string(ready.Status) + reasonSuffix(ready.Reason) + reasonSuffix(reading.Reason), Provenance: operator("Cluster.status.conditions[Ready]")})
	}

	counts := s.instanceCounts(g)
	add(viewmodel.Contributor{ID: "instances", Health: counts.Health, Label: "Instances ready", Detail: countsDetail(counts), Provenance: operator("Cluster.status.readyInstances"), Link: "instances"})

	hib := cnpgstatus.Hibernation(g.cluster)
	if hib.State != cnpgstatus.HibernationOff {
		h := viewmodel.HealthInactive
		switch hib.State {
		case cnpgstatus.HibernationInProgress:
			h = viewmodel.HealthProgressing
		case cnpgstatus.HibernationUnreadable:
			h = viewmodel.HealthUnknown
		}
		add(viewmodel.Contributor{ID: "hibernation", Health: h, Label: "Hibernation", Detail: hib.State + reasonSuffix(hib.Reason), Provenance: operator("cnpg.io/hibernation")})
	}

	for _, hatch := range cnpgstatus.EscapeHatches(g.cluster) {
		add(viewmodel.Contributor{ID: "escapeHatch:" + hatch.Annotation, Health: health(hatch.Class), Label: hatch.Annotation, Detail: hatch.Effect, Provenance: operator("Cluster.metadata.annotations")})
	}

	archiving := archivingOf(g)
	add(viewmodel.Contributor{ID: "archiving", Health: health(archiving.Class), Label: "WAL archiving", Detail: archivingDetail(archiving), Provenance: s.archivingProvenance(g, now), Link: "backups"})

	backup := s.backupRecency(g)
	add(viewmodel.Contributor{ID: "backupRecency", Health: backup.Health, Label: "Last backup", Detail: backupDetail(backup, now), Provenance: backup.Provenance, Link: "backups"})

	add(s.statusReadContributor(g, now))
	add(diagnosticsContributor(run, s.diagnostics()))

	for _, source := range requiredSources {
		state := s.cache.Source(g.key.Namespace, source)
		if g.failed[source] {
			state = unread([]viewmodel.SourceState{state}, source)[0]
		}
		if state.State == viewmodel.SourceLive {
			continue
		}
		prov := viewmodel.Provenance{Origin: viewmodel.OriginKubernetes, Source: source, Stale: state.State == viewmodel.SourceStale, StaleReason: state.Reason}
		add(viewmodel.Contributor{ID: "source:" + source, Health: viewmodel.HealthUnknown, Label: "Source " + source, Detail: string(state.State) + reasonSuffix(state.Reason), Provenance: prov})
	}

	sort.SliceStable(items, func(i, j int) bool {
		return rankOf(items[i].Health) > rankOf(items[j].Health)
	})
	states := make([]viewmodel.Health, 0, len(items))
	stale := false
	for _, item := range items {
		states = append(states, item.Health)
		stale = stale || item.Provenance.Stale
	}
	return viewmodel.Verdict{Health: viewmodel.Worst(states...), Stale: stale, Contributors: items}
}

func rankOf(h viewmodel.Health) int {
	for i, state := range viewmodel.AllHealth {
		if state == h {
			return i
		}
	}
	return len(viewmodel.AllHealth)
}

// phaseDetail classifies phase for minor and says it for the verdict:
// a phase outside the table names the table it was read against (the
// minor's own, or the newest released one), and a capped one why.
func phaseDetail(minor cnpgstatus.Minor, phase string) (cnpgstatus.PhaseReading, string) {
	reading := cnpgstatus.ClassifyPhase(minor, phase)
	text := phase
	if text == "" {
		text = "not reported"
	} else if !reading.Known {
		text += " (not in the CNPG " + string(cnpgstatus.TableMinor(minor)) + " table)"
	}
	return reading, text + reasonSuffix(reading.Reason)
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

func conditionOf(cluster *cnpgv1.Cluster, conditionType string) *conditionView {
	for i := range cluster.Status.Conditions {
		c := &cluster.Status.Conditions[i]
		if c.Type == conditionType {
			return &conditionView{Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message, LastTransition: c.LastTransitionTime.Time}
		}
	}
	return nil
}

type conditionView struct {
	Type, Status, Reason, Message string
	LastTransition                time.Time
}

// instanceCounts compares spec.instances, the operator's readyInstances
// and the pods Kubernetes reports Ready.
func (s *Service) instanceCounts(g *gathered) viewmodel.InstanceCounts {
	desired := g.cluster.Spec.Instances
	counts := viewmodel.InstanceCounts{Desired: &desired}
	if g.cluster.Status.Phase != "" {
		ready := g.cluster.Status.ReadyInstances
		reported := g.cluster.Status.Instances
		counts.Ready, counts.Reported = &ready, &reported
	}
	if g.listed(cache.SourcePods) {
		podsReady := 0
		for _, pod := range g.pods {
			if podReady(pod) {
				podsReady++
			}
		}
		counts.PodsReady = &podsReady
	}
	hibernated := cnpgstatus.Hibernation(g.cluster).State == cnpgstatus.HibernationHibernated
	switch {
	case hibernated:
		counts.Health = viewmodel.HealthInactive
	case counts.Ready == nil:
		counts.Health = viewmodel.HealthUnknown
	case *counts.Ready >= desired:
		counts.Health = viewmodel.HealthOK
	case *counts.Ready == 0 && desired > 0:
		counts.Health = viewmodel.HealthCritical
	case cnpgstatus.ClassifyPhase(g.phaseMinor, g.cluster.Status.Phase).Class == cnpgstatus.ClassProgressing:
		counts.Health = viewmodel.HealthProgressing
	default:
		counts.Health = viewmodel.HealthWarning
	}
	if counts.PodsReady != nil && counts.Ready != nil && *counts.PodsReady < *counts.Ready && counts.Health == viewmodel.HealthOK {
		// The operator's count lags the kubelet: trust the worse one.
		counts.Health = viewmodel.HealthWarning
	}
	return counts
}

func countsDetail(c viewmodel.InstanceCounts) string {
	if c.Ready == nil || c.Desired == nil {
		return "not reported"
	}
	detail := fmt.Sprintf("%d of %d", *c.Ready, *c.Desired)
	if c.PodsReady != nil && *c.PodsReady != *c.Ready {
		detail += fmt.Sprintf(" (%d pods Ready per Kubernetes)", *c.PodsReady)
	}
	return detail
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// primaryArchiver is the archiving instance's own report, when the
// sweep read it. The archiver is status.currentPrimary: the primary, or
// in a replica cluster the designated primary, a standby (isPrimary
// false) that the instance manager records there itself
// (internal/management/controller/instance_controller.go v1.30.0:1333,
// v1.30.1:1397). Every instance fills the archiver fields from
// pg_stat_archiver (pkg/management/postgres/probes.go v1.30.0 and
// v1.30.1:272, fillArchiverStatus), but readyWalFiles only a primary
// (probes.go v1.30.1:461-463): a designated primary's is not reported
// (ReadyNotReported).
func primaryArchiver(g *gathered) *cnpgstatus.ArchiverLive {
	r := primaryArchiverReading(g)
	if r == nil {
		return nil
	}
	return &cnpgstatus.ArchiverLive{
		IsArchivingWAL: r.IsArchivingWAL, LastFailedWAL: r.LastFailedWAL, ReadyWALFiles: r.ReadyWALFiles, ReadyNotReported: !r.IsPrimary,
		LastArchivedAt: r.LastArchivedWALTime,
		ObservedAt:     r.ObservedAt, Earlier: earlierReady(g, g.cluster.Status.CurrentPrimary, promotedAt(g), r.ObservedAt),
		PluginMaxParallel: pluginMaxParallel(g),
	}
}

// earlierReady are the instance's readyWalFiles samples before the
// reading taken at observed and not before from (the current primary's
// promotion, status.currentPrimaryTimestamp; zero for no limit), oldest
// first, each with the last_archived_time the same sweep read (instance
// clock), when kept. The term bound matters because the backlog rule
// skips gaps: an instance promoted again would otherwise join the
// readings of its earlier term to this one's across its time as a
// standby (CNPG P3-c).
func earlierReady(g *gathered, instance string, from, observed time.Time) []cnpgstatus.ReadySample {
	archived := map[int64]time.Time{}
	none := map[int64]bool{}
	for _, p := range g.archivedRings[instance] {
		switch {
		case p.Value == nil:
		case *p.Value == sweep.ArchivedNone:
			none[p.At.UnixNano()] = true
		case *p.Value >= 0:
			archived[p.At.UnixNano()] = time.UnixMilli(int64(*p.Value)).UTC()
		}
	}
	var out []cnpgstatus.ReadySample
	for _, p := range g.readyRings[instance] {
		if !p.At.Before(observed) || p.At.Before(from) {
			continue
		}
		s := cnpgstatus.ReadySample{At: p.At}
		if p.Value != nil {
			n := int(*p.Value)
			s.Files = &n
			if t, ok := archived[p.At.UnixNano()]; ok {
				s.LastArchivedAt = &t
			}
			s.ArchivedNone = none[p.At.UnixNano()]
		}
		out = append(out, s)
	}
	return out
}

// pluginMaxParallel is the barman-cloud ObjectStore's
// spec.configuration.wal.maxParallel (0 when unset); nil when the
// ObjectStore was not read.
func pluginMaxParallel(g *gathered) *int {
	if g.store == nil {
		return nil
	}
	n := 0
	if v, found, err := unstructured.NestedInt64(g.store.Object, "spec", "configuration", "wal", "maxParallel"); err == nil && found && v > 0 && v < 1<<16 {
		n = int(v)
	}
	return &n
}

// primaryArchiverReading is status.currentPrimary's whole reading; nil
// when currentPrimary is not set or its /pg/status was not read. No other
// instance stands in, not even one reporting itself primary: during a
// failover or a split brain that one is not the archiver the operator
// names, and its pg_stat_archiver says nothing about the WAL the
// repository receives. Unread, the archiver is unknown. One missed read
// (a timeout, a slow answer) does not flip the archiving facts: the last
// good reading of the same pod (the sweep keeps it only for the same pod
// UID, so a restarted pod has none) stands while it is from the sweep
// before (primaryReadGrace); a second miss in a row is unread. A report
// the instance manager answered without reading PostgreSQL (fenced,
// restarting: instancestatus Reading.Masked) is no reading of the
// archiver: its zero archiver fields were not read (round 16, CNPG P2-1).
// archivingOf is the Cluster's archiving fact from its current
// primary's archiver, worded for a primary whose manager answered
// without reaching PostgreSQL (masked) when it is one.
func archivingOf(g *gathered) cnpgstatus.ArchivingReading {
	return cnpgstatus.ArchivingOf(g.cluster, primaryArchiver(g), primaryMasked(g), g.sweepOff)
}

// primaryMasked reports a current primary whose last report is masked
// (instancestatus.Reading.Masked: fenced or restarting).
func primaryMasked(g *gathered) bool {
	if g.cluster == nil || g.cluster.Status.CurrentPrimary == "" {
		return false
	}
	inst, ok := g.swept.Instances[g.cluster.Status.CurrentPrimary]
	return ok && inst.Reading != nil && inst.Reading.Masked()
}

func primaryArchiverReading(g *gathered) *instancestatus.Reading {
	name := g.cluster.Status.CurrentPrimary
	if name == "" {
		return nil
	}
	inst, ok := g.swept.Instances[name]
	if !ok || inst.Reading == nil || inst.Reading.Masked() {
		return nil
	}
	if inst.State == viewmodel.StatusReadOK {
		return inst.Reading
	}
	if g.swept.Interval > 0 && !inst.Reading.ObservedAt.IsZero() && g.swept.SweptAt.Sub(inst.Reading.ObservedAt) <= primaryReadGrace(g.swept.Interval) &&
		!inst.Since.Before(g.swept.SweptAt.Add(-g.swept.Interval/2)) {
		// The failed state began this sweep (Since), and the reading is
		// from the one before.
		return inst.Reading
	}
	return nil
}

// primaryReadGrace is how old the primary's last good reading may be,
// against the sweep, and still stand after a failed read: one sweep
// period, with half a period for the ticker's jitter, so exactly one
// missed sweep is absorbed and two are not.
func primaryReadGrace(interval time.Duration) time.Duration {
	return interval + interval/2
}

// sweepStaleReason is why a reading of the sweep is stale.
const sweepStaleReason = "the sweep has not completed for three intervals"

// sweepStale is the console's one staleness rule for the status sweep:
// its last completed sweep is older than three sweep intervals.
func sweepStale(g *gathered, now time.Time) bool {
	return g.hasSweep && g.swept.Interval > 0 && now.Sub(g.swept.SweptAt) > 3*g.swept.Interval
}

func archivingDetail(a cnpgstatus.ArchivingReading) string {
	switch {
	case a.ConditionStatus != "" && a.Reason != "":
		return "ContinuousArchiving " + a.ConditionStatus + " (" + a.Reason + ")"
	case a.ConditionStatus != "":
		return "ContinuousArchiving " + a.ConditionStatus
	case a.Reason != "":
		return a.Reason
	default:
		return "not reported"
	}
}

func (s *Service) archivingProvenance(g *gathered, now time.Time) viewmodel.Provenance {
	if r := primaryArchiverReading(g); r != nil {
		at := g.swept.SweptAt
		if !r.ObservedAt.IsZero() {
			// The reading's own time: the sweep before, after one miss.
			at = r.ObservedAt.UTC()
		}
		p := viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", ObservedAt: &at}
		if sweepStale(g, now) {
			p.Stale, p.StaleReason = true, sweepStaleReason
		}
		return p
	}
	source := "Cluster.status.conditions[ContinuousArchiving]"
	if g.sweepOff {
		source += " (not checked on the instance)"
	}
	return s.provenance(g.key.Namespace, cache.SourceClusters, viewmodel.OriginOperator, source, now)
}

// statusReadContributor turns the sweep into one roll-up item: every
// instance read is ok; one answering without PostgreSQL (masked) makes it
// a warning; any not read makes it unknown (an observer's
// limit, not a fault of the Cluster); a switched-off sweep is info.
func (s *Service) statusReadContributor(g *gathered, now time.Time) viewmodel.Contributor {
	c := viewmodel.Contributor{ID: "instanceStatus", Label: "Instance self-reports", Link: "instances"}
	sw := s.sweeper()
	if sw == nil {
		c.Health, c.Detail = viewmodel.HealthInfo, "the status sweep is not running"
		return c
	}
	state := sw.State()
	if state.State == viewmodel.SourceDisabled {
		c.Health, c.Detail = viewmodel.HealthInfo, "switched off"+reasonSuffix(state.Reason)
		c.Provenance = viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status"}
		return c
	}
	// A hibernated Cluster runs no instance: nothing to read is what it
	// is, inactive, not unknown (which would outrank the hibernation in
	// the headline; round 8, DBA).
	hibernated := g.cluster != nil && cnpgstatus.Hibernation(g.cluster).State == cnpgstatus.HibernationHibernated
	if !g.hasSweep {
		c.Health, c.Detail = viewmodel.HealthUnknown, "not swept yet"
		if hibernated && len(g.pods) == 0 {
			c.Health, c.Detail = viewmodel.HealthInactive, "the Cluster is hibernated: no instance runs to read"
		}
		c.Provenance = viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status"}
		return c
	}
	at := g.swept.SweptAt
	c.Provenance = viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", ObservedAt: &at}
	// A masked report (fenced or restarting: the instance manager
	// answered without reaching PostgreSQL) is no reading of PostgreSQL
	// (sweep.Instance.ReadingOK), so it is not counted as read (round 18,
	// CNPG P3-2): it is a known state of the instance, a warning, and
	// said apart from those that did not answer.
	read, masked, total := 0, 0, len(g.swept.Instances)
	for _, inst := range g.swept.Instances {
		switch {
		case inst.ReadingOK():
			read++
		case inst.State == viewmodel.StatusReadOK:
			masked++
		}
	}
	c.Detail = fmt.Sprintf("%d of %d instances read", read, total)
	if masked > 0 {
		c.Detail += fmt.Sprintf(", %d answered without PostgreSQL (fenced, restarting or rewinding)", masked)
	}
	switch {
	case total == 0 && hibernated:
		c.Health, c.Detail = viewmodel.HealthInactive, "the Cluster is hibernated: no instance runs to read"
	case total == 0:
		c.Health, c.Detail = viewmodel.HealthUnknown, "no instance pod to read"
	case read == total:
		c.Health = viewmodel.HealthOK
	case read+masked == total:
		c.Health = viewmodel.HealthWarning
	default:
		c.Health = viewmodel.HealthUnknown
	}
	if sweepStale(g, now) {
		c.Provenance.Stale, c.Provenance.StaleReason = true, sweepStaleReason
		if total > 0 || !hibernated {
			// With no instance to read, a stale sweep hides nothing.
			c.Health = viewmodel.HealthUnknown
		}
	}
	return c
}

func diagnosticsContributor(run *diag.Run, d Diagnostics) viewmodel.Contributor {
	c := viewmodel.Contributor{ID: "diagnostics", Label: "Diagnostics", Link: "diagnostics", Provenance: viewmodel.Provenance{Origin: viewmodel.OriginConsole, Source: "diagnostics"}}
	if d == nil {
		c.Health, c.Detail = viewmodel.HealthInfo, "the diagnostics runner is not running"
		return c
	}
	if state := d.State(); state.State == viewmodel.SourceDisabled {
		c.Health, c.Detail = viewmodel.HealthInfo, "switched off"+reasonSuffix(state.Reason)
		return c
	}
	if run == nil {
		c.Health, c.Detail = viewmodel.HealthUnknown, "not run yet"
		return c
	}
	at := run.At
	c.Provenance.ObservedAt = &at
	if run.Failed != "" {
		c.Health, c.Detail = viewmodel.HealthUnknown, run.Failed
		return c
	}
	counts := findingCounts(run)
	// Health has no step between critical and warning: a high finding is
	// red like a critical one, so it does not hide behind a warning.
	switch {
	case counts.Critical > 0, counts.High > 0:
		c.Health = viewmodel.HealthCritical
	case counts.Warning > 0:
		c.Health = viewmodel.HealthWarning
	default:
		c.Health = viewmodel.HealthOK
	}
	c.Detail = fmt.Sprintf("%d critical, %d high, %d warning, %d note; %d checks could not run",
		counts.Critical, counts.High, counts.Warning, counts.Note, counts.Unavailable)
	return c
}

func findingCounts(run *diag.Run) viewmodel.FindingCounts {
	if run == nil {
		return viewmodel.FindingCounts{}
	}
	at := run.At
	counts := viewmodel.FindingCounts{RunAt: &at}
	for _, f := range run.Result.Findings {
		switch f.Severity {
		case diagnose.SeverityCritical:
			counts.Critical++
		case diagnose.SeverityHigh:
			counts.High++
		case diagnose.SeverityWarning:
			counts.Warning++
		default:
			counts.Note++
		}
	}
	for _, check := range run.Result.Checks {
		if check.Outcome == diagnose.CheckUnavailable {
			counts.Unavailable++
		}
	}
	return counts
}

// provenance is a fact's provenance from a cache source's state.
func (s *Service) provenance(namespace, source string, origin viewmodel.Origin, field string, now time.Time) viewmodel.Provenance {
	state := s.cache.Source(namespace, source)
	p := viewmodel.Provenance{Origin: origin, Source: field}
	switch state.State {
	case viewmodel.SourceLive:
		p.ObservedAt = &now
	case viewmodel.SourceStale:
		p.Stale, p.StaleReason = true, state.Reason
		p.ObservedAt = state.Since
	default:
		p.Stale, p.StaleReason = true, string(state.State)+reasonSuffix(state.Reason)
	}
	return p
}
