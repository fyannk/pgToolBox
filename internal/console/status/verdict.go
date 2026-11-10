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

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/diag"
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

	archiving := cnpgstatus.Archiving(g.cluster, primaryArchiver(g))
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
// v1.30.1:272, fillArchiverStatus).
func primaryArchiver(g *gathered) *cnpgstatus.ArchiverLive {
	r := primaryArchiverReading(g)
	if r == nil {
		return nil
	}
	return &cnpgstatus.ArchiverLive{IsArchivingWAL: r.IsArchivingWAL, LastFailedWAL: r.LastFailedWAL, ReadyWALFiles: r.ReadyWALFiles}
}

// primaryArchiverReading is status.currentPrimary's whole reading; nil
// when currentPrimary is not set or its /pg/status was not read. No other
// instance stands in, not even one reporting itself primary: during a
// failover or a split brain that one is not the archiver the operator
// names, and its pg_stat_archiver says nothing about the WAL the
// repository receives. Unread, the archiver is unknown.
func primaryArchiverReading(g *gathered) *instancestatus.Reading {
	name := g.cluster.Status.CurrentPrimary
	if name == "" {
		return nil
	}
	if inst, ok := g.swept.Instances[name]; ok && inst.State == viewmodel.StatusReadOK && inst.Reading != nil {
		return inst.Reading
	}
	return nil
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
	if primaryArchiver(g) != nil {
		at := g.swept.SweptAt
		p := viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", ObservedAt: &at}
		if sweepStale(g, now) {
			p.Stale, p.StaleReason = true, sweepStaleReason
		}
		return p
	}
	return s.provenance(g.key.Namespace, cache.SourceClusters, viewmodel.OriginOperator, "Cluster.status.conditions[ContinuousArchiving]", now)
}

// statusReadContributor turns the sweep into one roll-up item: every
// instance read is ok; any not read makes it unknown (an observer's
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
	if !g.hasSweep {
		c.Health, c.Detail = viewmodel.HealthUnknown, "not swept yet"
		c.Provenance = viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status"}
		return c
	}
	at := g.swept.SweptAt
	c.Provenance = viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "/pg/status", ObservedAt: &at}
	read, total := 0, len(g.swept.Instances)
	for _, inst := range g.swept.Instances {
		if inst.State == viewmodel.StatusReadOK {
			read++
		}
	}
	c.Detail = fmt.Sprintf("%d of %d instances read", read, total)
	switch {
	case total == 0:
		c.Health, c.Detail = viewmodel.HealthUnknown, "no instance pod to read"
	case read == total:
		c.Health = viewmodel.HealthOK
	default:
		c.Health = viewmodel.HealthUnknown
	}
	if sweepStale(g, now) {
		c.Provenance.Stale, c.Provenance.StaleReason = true, sweepStaleReason
		c.Health = viewmodel.HealthUnknown
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
