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

// Metrics, the admin page and the pgAdmin link-outs.

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/opstate"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
)

// ConsoleInfo describes the console itself for the admin page.
type ConsoleInfo struct {
	Version   string
	Namespace string
}

// SetRunnerStates wires the supervisor's runner report (the supervisor
// is built after the read model).
func (s *Service) SetRunnerStates(f func() []viewmodel.RunnerState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runners = f
}

// Metrics is the /metrics screen.
func (s *Service) Metrics(ctx context.Context, namespace, name string) (viewmodel.Metrics, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return viewmodel.Metrics{}, err
	}
	out := viewmodel.Metrics{
		Ref: ref(g.cluster), GeneratedAt: s.now().UTC(), Tiles: []viewmodel.MetricTile{}, Charts: []viewmodel.MetricChart{},
		Monitoring: s.opts.Monitoring,
		BlindSpots: []viewmodel.BlindSpot{{
			ID: "metrics.notAMonitoringSystem", Panel: "metrics",
			Text: "The console keeps a short window of the values its checks read; it is not a monitoring system.",
		}},
	}
	if out.Monitoring.State == "" {
		out.Monitoring.State = "notConfigured"
	}
	groupTitle := map[string]string{}
	for _, gd := range metrics.Instance.Groups {
		groupTitle[gd.Key] = gd.Title
	}
	out.ChartSource = viewmodel.ChartSourceRing
	if s.prom != nil {
		e := s.prom.charts(ctx, g.key.Namespace, g.key.Name, groupTitle)
		state := e.state
		out.Prometheus = &state
		if len(e.charts) > 0 {
			out.Charts, out.ChartSource = e.charts, viewmodel.ChartSourcePrometheus
			out.RangeSeconds, out.StepSeconds = int(s.prom.rng/time.Second), int(e.step/time.Second)
		}
	}
	sw := s.sweeper()
	var store *metrics.Store
	if sw != nil {
		store = sw.Metrics(g.sweepKey())
	}
	if store == nil {
		out.Source = viewmodel.SourceState{Source: "metrics", State: viewmodel.SourceDisabled, Reason: "METRICS_ENABLED is false, or the Cluster was not swept yet"}
		if sw != nil && sw.State().State == viewmodel.SourceLive {
			out.Source.State, out.Source.Reason = viewmodel.SourcePending, "the Cluster was not swept yet"
		}
		return out, nil
	}
	out.Source = sw.State()
	out.Source.Source = "metrics"
	out.IntervalSeconds, out.RetentionSeconds = int(store.Interval()/time.Second), int(store.Retention()/time.Second)
	catalog := store.Catalog()
	for _, gd := range catalog.Groups {
		groupTitle[gd.Key] = gd.Title
	}
	readings := store.InstantReadings()
	instances := members(g)
	for _, def := range catalog.Instants {
		for _, inst := range instances {
			tile := viewmodel.MetricTile{
				Key: def.Key, Label: def.Title, Group: groupTitle[def.Group], Unit: def.Unit, Instance: inst,
				Provenance: viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: ":9187/metrics"},
			}
			if r, ok := readings[inst][def.Key]; ok {
				v, at := r.Value, time.Unix(r.At, 0).UTC()
				tile.Value, tile.At, tile.Provenance.ObservedAt = &v, &at, &at
				if swept, ok := g.swept.Instances[inst]; ok && swept.State != viewmodel.StatusReadOK {
					tile.Provenance.Stale, tile.Provenance.StaleReason = true, swept.Reason
				}
			}
			out.Tiles = append(out.Tiles, tile)
		}
	}
	if out.ChartSource == viewmodel.ChartSourcePrometheus {
		return out, nil
	}
	for _, def := range catalog.Series {
		times, by := store.Range(def.Key, metrics.TierRaw)
		if len(times) == 0 {
			continue
		}
		chart := viewmodel.MetricChart{Key: def.Key, Label: def.Title, Group: groupTitle[def.Group], Unit: def.Unit, Note: bound(def.Note.Watch)}
		for _, t := range times {
			chart.Times = append(chart.Times, time.Unix(t, 0).UTC())
		}
		names := make([]string, 0, len(by))
		for n := range by {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			chart.Lines = append(chart.Lines, viewmodel.MetricLine{Instance: n, Values: by[n]})
		}
		out.Charts = append(out.Charts, chart)
	}
	return out, nil
}

// members are the Cluster's instance names: member pods and the
// operator's instance names, sorted.
func members(g *gathered) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range g.pods {
		if !seen[p.Name] {
			seen[p.Name] = true
			out = append(out, p.Name)
		}
	}
	for _, n := range g.cluster.Status.InstanceNames {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// readyHealth draws a pgToolBox object from its Ready condition
// (api/v1alpha1/conditions.go, the frozen catalogue): absent or behind
// its generation is unknown, never ok.
func readyHealth(conditions []metav1.Condition, generation, observed int64) (viewmodel.Health, bool) {
	stale := observed < generation
	c := meta.FindStatusCondition(conditions, pgtoolboxv1alpha1.ConditionReady)
	switch {
	case c == nil || stale:
		return viewmodel.HealthUnknown, stale
	case c.Status == "True":
		return viewmodel.HealthOK, false
	case c.Status == "False" && slices.Contains([]string{string(pgtoolboxv1alpha1.ReasonInitializing), string(pgtoolboxv1alpha1.ReasonReconciling), string(pgtoolboxv1alpha1.ReasonRolloutInProgress)}, c.Reason):
		return viewmodel.HealthProgressing, false
	case c.Status == "False" && slices.Contains([]string{string(pgtoolboxv1alpha1.ReasonReconciliationPaused), string(pgtoolboxv1alpha1.ReasonScaledToZero)}, c.Reason):
		return viewmodel.HealthInactive, false
	case c.Status == "False":
		return viewmodel.HealthCritical, false
	default:
		return viewmodel.HealthUnknown, false
	}
}

func objectConditions(conditions []metav1.Condition) []viewmodel.Condition {
	out := make([]viewmodel.Condition, 0, len(conditions))
	for _, c := range conditions {
		h := conditionHealth(c)
		t := c.LastTransitionTime.UTC()
		out = append(out, viewmodel.Condition{
			Type: c.Type, Status: string(c.Status), Reason: bound(c.Reason), Message: bound(c.Message),
			LastTransition: &t, Health: h, Known: c.Type == pgtoolboxv1alpha1.ConditionReady,
		})
	}
	return out
}

// conditionHealth draws one condition of a pgToolBox object by its
// polarity in api/v1alpha1/conditions.go: Stalled is abnormal-true,
// Progressing True is work in flight, every other type is normal-true.
func conditionHealth(c metav1.Condition) viewmodel.Health {
	switch {
	case c.Status != metav1.ConditionTrue && c.Status != metav1.ConditionFalse:
		return viewmodel.HealthUnknown
	case c.Type == pgtoolboxv1alpha1.ConditionStalled && c.Status == metav1.ConditionTrue:
		return viewmodel.HealthCritical
	case c.Type == pgtoolboxv1alpha1.ConditionStalled:
		return viewmodel.HealthOK
	case c.Type == pgtoolboxv1alpha1.ConditionProgressing && c.Status == metav1.ConditionTrue:
		return viewmodel.HealthProgressing
	case c.Type == pgtoolboxv1alpha1.ConditionProgressing:
		return viewmodel.HealthOK
	case c.Status == metav1.ConditionTrue:
		return viewmodel.HealthOK
	default:
		return viewmodel.HealthWarning
	}
}

// safeURL keeps a status URL only when it is an absolute https URL
// without user information: the console renders it as a link on its own
// origin, and status is not proof of who wrote it.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(raw) > 2048 {
		return ""
	}
	return u.String()
}

// PgAdminLinks are the PgAdmins of the namespace whose status lists the
// Cluster as a registered server; an error when the PgAdmin list failed.
func (s *Service) PgAdminLinks(ctx context.Context, namespace, name string) ([]viewmodel.PgAdminLink, error) {
	if _, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false); err != nil {
		return nil, err
	}
	out := []viewmodel.PgAdminLink{}
	if !s.cache.Readable(namespace, cache.SourcePgAdmins) {
		return out, nil
	}
	list := &pgtoolboxv1alpha1.PgAdminList{}
	if err := s.cache.Reader().List(ctx, list, client.InNamespace(namespace)); err != nil {
		// The caller serves its view without links and says they were
		// not read, rather than "no pgAdmin".
		return out, fmt.Errorf("listing PgAdmins in %s: %w", namespace, err)
	}
	for i := range list.Items {
		p := &list.Items[i]
		for _, srv := range p.Status.Servers {
			if srv.Cluster != name {
				continue
			}
			h, _ := readyHealth(p.Status.Conditions, p.Generation, p.Status.ObservedGeneration)
			out = append(out, viewmodel.PgAdminLink{Name: p.Name, URL: safeURL(p.Status.URL), Health: h, ServerState: string(srv.State)})
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Admin is the admin page before per-namespace authorization.
func (s *Service) Admin(ctx context.Context) (viewmodel.Admin, error) {
	now := s.now().UTC()
	out := viewmodel.Admin{GeneratedAt: now, Namespaces: []viewmodel.AdminNamespace{}}
	reader := s.cache.Reader()
	for _, ns := range s.cache.Namespaces() {
		row := viewmodel.AdminNamespace{
			Name: ns, PgAdmins: []viewmodel.PgAdminRow{}, EvidenceAgents: []viewmodel.EvidenceAgentRow{},
			Sources: []viewmodel.SourceState{s.cache.Source(ns, cache.SourcePgAdmins), s.cache.Source(ns, cache.SourceEvidenceAgents)},
		}
		if s.cache.Readable(ns, cache.SourcePgAdmins) {
			list := &pgtoolboxv1alpha1.PgAdminList{}
			if reader.List(ctx, list, client.InNamespace(ns)) != nil {
				row.Sources = unread(row.Sources, cache.SourcePgAdmins)
			} else {
				for i := range list.Items {
					p := &list.Items[i]
					h, stale := readyHealth(p.Status.Conditions, p.Generation, p.Status.ObservedGeneration)
					r := viewmodel.PgAdminRow{
						Name: p.Name, Health: h, URL: safeURL(p.Status.URL), Version: bound(p.Status.PgAdminVersion), Image: bound(p.Status.Image),
						Servers: []viewmodel.PgAdminServer{}, Conditions: objectConditions(p.Status.Conditions), Stale: stale,
						Provenance: s.provenance(ns, cache.SourcePgAdmins, viewmodel.OriginOperator, "PgAdmin.status", now),
					}
					for _, srv := range p.Status.Servers {
						r.Servers = append(r.Servers, viewmodel.PgAdminServer{Cluster: srv.Cluster, Endpoint: srv.Endpoint, State: string(srv.State), Message: bound(srv.Message)})
					}
					row.PgAdmins = append(row.PgAdmins, r)
				}
			}
		}
		if s.cache.Readable(ns, cache.SourceEvidenceAgents) {
			list := &pgtoolboxv1alpha1.EvidenceAgentList{}
			if reader.List(ctx, list, client.InNamespace(ns)) != nil {
				row.Sources = unread(row.Sources, cache.SourceEvidenceAgents)
			} else {
				for i := range list.Items {
					a := &list.Items[i]
					h, stale := readyHealth(a.Status.Conditions, a.Generation, a.Status.ObservedGeneration)
					r := viewmodel.EvidenceAgentRow{
						Name: a.Name, Health: h, Endpoint: a.Status.Endpoint, Fingerprint: bound(a.Status.Fingerprint),
						Conditions: objectConditions(a.Status.Conditions), Stale: stale,
						Provenance: s.provenance(ns, cache.SourceEvidenceAgents, viewmodel.OriginOperator, "EvidenceAgent.status", now),
					}
					if a.Status.ProducerEpoch > 0 {
						epoch := a.Status.ProducerEpoch
						r.ProducerEpoch = &epoch
					}
					row.EvidenceAgents = append(row.EvidenceAgents, r)
				}
			}
		}
		out.Namespaces = append(out.Namespaces, row)
	}
	info := s.opts.Console
	health := &viewmodel.ConsoleHealth{
		Version: info.Version, Namespace: info.Namespace, WatchNamespaces: slices.Clone(s.cache.Namespaces()),
		Runners: []viewmodel.RunnerState{}, Sources: []viewmodel.SourceState{},
		Provenance: viewmodel.Provenance{Origin: viewmodel.OriginConsole, Source: "console", ObservedAt: &now},
	}
	s.mu.RLock()
	runners := s.runners
	s.mu.RUnlock()
	if runners != nil {
		health.Runners = runners()
	}
	if sw := s.sweeper(); sw != nil {
		health.Sources = append(health.Sources, sw.State())
	}
	if d := s.diagnostics(); d != nil {
		health.Sources = append(health.Sources, d.State())
	}
	if s.opts.History != nil {
		health.Sources = append(health.Sources, viewmodel.SourceState{Source: "history", State: viewmodel.SourceLive})
	} else {
		health.Sources = append(health.Sources, viewmodel.SourceState{Source: "history", State: viewmodel.SourceDisabled})
	}
	if s.opts.Evidence != nil {
		health.Sources = append(health.Sources, s.opts.Evidence.State())
	}
	if s.opts.Persistence != nil {
		health.Sources = append(health.Sources, s.opts.Persistence()...)
	}
	if s.opts.Digest != nil {
		health.Digest = s.opts.Digest()
	}
	out.Console = health
	return out, nil
}

// OperationState is the informer cache's view of a Cluster for the
// operation offers and trackers (operations.State).
func (s *Service) OperationState(ctx context.Context, namespace, name string) (opstate.ClusterState, error) {
	g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false)
	if err != nil {
		return opstate.ClusterState{}, err
	}
	st := opstate.ClusterState{Cluster: g.cluster.DeepCopy(), Backups: g.backups, Minor: g.minor}
	for _, p := range g.pods {
		st.Pods = append(st.Pods, *p.DeepCopy())
	}
	for _, r := range s.instanceRows(g, false) {
		inst := opstate.InstanceState{Name: r.Name, Health: r.Health}
		if r.Live != nil {
			inst.LagBytes = r.Live.LagBytes
		}
		st.Instances = append(st.Instances, inst)
	}
	return st, nil
}
