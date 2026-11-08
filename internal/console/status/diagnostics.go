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
	"context"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/diag"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/diagnose/triage"
)

// clusterRef reads the Cluster for its reference, ErrNotFound when the
// console does not hold it.
func (s *Service) clusterRef(ctx context.Context, namespace, name string) (viewmodel.ClusterRef, error) {
	if !s.cache.Readable(namespace, cache.SourceClusters) {
		return viewmodel.ClusterRef{}, ErrNotFound
	}
	cluster := &cnpgv1.Cluster{}
	if err := s.cache.Reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return viewmodel.ClusterRef{}, ErrNotFound
		}
		// A failed read is not a missing Cluster: the API answers 5xx,
		// not 404.
		return viewmodel.ClusterRef{}, err
	}
	return ref(cluster), nil
}

// Diagnostics is the /diagnostics screen: the latest background run.
func (s *Service) Diagnostics(ctx context.Context, namespace, name string) (viewmodel.Diagnostics, error) {
	r, err := s.clusterRef(ctx, namespace, name)
	if err != nil {
		return viewmodel.Diagnostics{}, err
	}
	out := viewmodel.Diagnostics{Ref: r, CatalogPins: diag.CatalogPins(), Incidents: []viewmodel.Incident{}, Checks: []viewmodel.Check{}}
	d := s.diagnostics()
	if d == nil {
		out.Runner = viewmodel.SourceState{Source: "diagnostics", State: viewmodel.SourceDisabled, Reason: "the diagnostics runner is not running"}
		return out, nil
	}
	out.Runner = d.State()
	run, ok := d.Latest(cache.ClusterKey{Namespace: namespace, Name: name})
	if !ok {
		return out, nil
	}
	at := run.At
	out.RunAt, out.InputGeneration, out.Counts = &at, run.Generation, findingCounts(&run)
	if g, err := s.gather(ctx, cache.ClusterKey{Namespace: namespace, Name: name}, false); err == nil {
		out.Applied = string(g.minor)
	}
	out.Incidents = incidents(run)
	for _, check := range run.Result.Checks {
		out.Checks = append(out.Checks, viewmodel.Check{
			Name: check.Name, Layer: string(check.Layer), Describes: check.Describes,
			Outcome: checkOutcome(check.Outcome), Because: check.Because, SourceOff: check.SourceOff,
		})
	}
	return out, nil
}

func checkOutcome(o diagnose.CheckOutcome) viewmodel.CheckOutcome {
	switch o {
	case diagnose.CheckMatched:
		return viewmodel.CheckMatched
	case diagnose.CheckUnavailable:
		return viewmodel.CheckUnavailable
	case diagnose.CheckNotApplicable:
		return viewmodel.CheckNotApplicable
	default:
		return viewmodel.CheckClear
	}
}

func severity(s diagnose.Severity) viewmodel.Severity {
	switch s {
	case diagnose.SeverityCritical:
		return viewmodel.SeverityCritical
	case diagnose.SeverityHigh:
		return viewmodel.SeverityHigh
	case diagnose.SeverityWarning:
		return viewmodel.SeverityWarning
	default:
		return viewmodel.SeverityNote
	}
}

// incidents groups findings by the catalog's causal relations: a finding
// whose relation holds against another finding of the same run is shown
// beneath it as one incident, keeping its own evidence. Run order (most
// severe first) is preserved for roots.
func incidents(run diag.Run) []viewmodel.Incident {
	findings := run.Result.Findings
	parent := make([]int, len(findings))
	terms := make([]string, len(findings))
	for i := range parent {
		parent[i] = -1
	}
	for i, effect := range findings {
	relations:
		for _, rel := range effect.ConsequenceOf {
			for j, cause := range findings {
				if i == j {
					continue
				}
				if ok, _ := rel.Holds(effect, cause); ok {
					parent[i], terms[i] = j, rel.Terms(effect, cause)
					break relations
				}
			}
		}
	}
	root := func(i int) int {
		for steps := 0; parent[i] >= 0 && steps < len(findings); steps++ {
			i = parent[i]
		}
		return i
	}
	byRoot := map[int]*viewmodel.Incident{}
	var order []int
	for i := range findings {
		r := root(i)
		if parent[i] < 0 || r == i {
			if _, ok := byRoot[i]; !ok {
				byRoot[i] = &viewmodel.Incident{Root: finding(findings[i], run.Since, ""), Severity: severity(findings[i].Severity)}
				order = append(order, i)
			}
			continue
		}
		inc, ok := byRoot[r]
		if !ok {
			inc = &viewmodel.Incident{Root: finding(findings[r], run.Since, ""), Severity: severity(findings[r].Severity)}
			byRoot[r] = inc
			order = append(order, r)
		}
		inc.Consequences = append(inc.Consequences, finding(findings[i], run.Since, terms[i]))
		if worse(severity(findings[i].Severity), inc.Severity) {
			inc.Severity = severity(findings[i].Severity)
		}
	}
	out := make([]viewmodel.Incident, 0, len(order))
	for _, i := range order {
		out = append(out, *byRoot[i])
	}
	return out
}

func worse(a, b viewmodel.Severity) bool {
	rank := map[viewmodel.Severity]int{viewmodel.SeverityNote: 0, viewmodel.SeverityWarning: 1, viewmodel.SeverityHigh: 2, viewmodel.SeverityCritical: 3}
	return rank[a] > rank[b]
}

func finding(f diagnose.Finding, since map[string]time.Time, relation string) viewmodel.Finding {
	out := viewmodel.Finding{
		ID: f.ID, Check: f.Check, Subject: viewmodel.ObjectRef{Kind: f.Subject.Kind, Name: f.Subject.Name},
		Severity: severity(f.Severity), Summary: f.Summary, Detail: f.Detail,
		NextSteps: f.NextSteps, Relation: relation, Link: link(f.Link), LinkLabel: f.LinkLabel,
		Evidence: []viewmodel.Evidence{},
	}
	if !f.At.IsZero() {
		at := f.At.UTC()
		out.At = &at
	}
	if t, ok := since[f.ID]; ok {
		t = t.UTC()
		out.Since = &t
	}
	for _, e := range f.Evidence {
		out.Evidence = append(out.Evidence, viewmodel.Evidence{Origin: e.Origin, Object: e.Object, Detail: bound(e.Detail)})
	}
	return out
}

// link maps the catalog's screen paths onto the SPA's cluster-relative
// routes. The SPA resolves exactly these sections: "",
// instances, instances/<pod>, events, diagnostics, triage, replication,
// backups, storage, objects, history, history/<seq>, metrics,
// operations, logs/<pod>. A catalog path with no SPA screen maps to "" (the
// overview), never to a guessed route.
func link(catalogPath string) string {
	p := strings.Trim(catalogPath, "/")
	section, rest, _ := strings.Cut(p, "/")
	switch section {
	case "":
		return ""
	case "cluster":
		sub, pod, _ := strings.Cut(rest, "/")
		switch sub {
		case "pods":
			if pod != "" {
				return "instances/" + pod
			}
			return "instances"
		case "metrics":
			return "metrics"
		default: // overview and anything else under /cluster
			return ""
		}
	case "pods":
		if rest != "" {
			return "instances/" + rest
		}
		return "instances"
	case "databases", "objects":
		return "objects"
	case "poolers":
		if rest == "metrics" {
			return "metrics"
		}
		return "objects"
	case "backups":
		return "backups"
	case "history":
		if rest != "" {
			return "history/" + rest
		}
		return "history"
	case "instances", "events", "diagnostics", "triage", "replication", "storage", "metrics", "operations", "logs":
		return p
	case "insight":
		// The insight screens: insight alone, or one of its screens.
		switch rest {
		case "", "vacuum", "blocking", "statements", "indexes", "connections":
			return p
		}
		return "insight"
	default:
		return ""
	}
}

// Triage is the /triage screen: every playbook walked against the latest
// run's own input, so the walk and the findings agree.
func (s *Service) Triage(ctx context.Context, namespace, name string) (viewmodel.Triage, error) {
	r, err := s.clusterRef(ctx, namespace, name)
	if err != nil {
		return viewmodel.Triage{}, err
	}
	out := viewmodel.Triage{Ref: r, Playbooks: []viewmodel.Playbook{}}
	d := s.diagnostics()
	if d == nil {
		return out, nil
	}
	run, ok := d.Latest(cache.ClusterKey{Namespace: namespace, Name: name})
	if !ok || run.Failed != "" {
		return out, nil
	}
	at := run.At
	out.RunAt = &at
	for _, pb := range triage.Playbooks() {
		walk := triage.Evaluate(pb, run.Input, run.Result)
		playbook := viewmodel.Playbook{ID: pb.ID, Symptom: pb.Symptom, Describes: pb.Describes, Upstream: pb.Upstream, StartAt: walk.StartAt, Steps: []viewmodel.TriageStep{}}
		for _, step := range walk.Steps {
			ts := viewmodel.TriageStep{
				Question: step.Question, Outcome: triageOutcome(step.Outcome),
				Explain: step.Explain, Yourself: step.Yourself, Because: step.Because,
			}
			for _, f := range step.Findings {
				ts.Findings = append(ts.Findings, f.ID)
			}
			for _, e := range step.Evidence {
				ts.Evidence = append(ts.Evidence, viewmodel.Evidence{Origin: e.Origin, Object: e.Object, Detail: bound(e.Detail)})
			}
			playbook.Steps = append(playbook.Steps, ts)
		}
		out.Playbooks = append(out.Playbooks, playbook)
	}
	return out, nil
}

func triageOutcome(o triage.Outcome) viewmodel.TriageOutcome {
	switch o {
	case triage.OutcomeFound:
		return viewmodel.TriageFound
	case triage.OutcomeRuledOut:
		return viewmodel.TriageRuledOut
	case triage.OutcomeOff:
		return viewmodel.TriageSourceOff
	case triage.OutcomeNotApplicable:
		return viewmodel.TriageNotApplicable
	case triage.OutcomeAsk:
		return viewmodel.TriageAsk
	default:
		return viewmodel.TriageUnknown
	}
}

// Targets is the sweep roster (sweep.Roster): every watched Cluster with
// its member pods, the name the status certificate must carry, and each
// pod's status-port TLS fact from its spec.
func (s *Service) Targets(ctx context.Context) []sweep.Target {
	var out []sweep.Target
	for _, key := range s.Keys(ctx) {
		g, err := s.gather(ctx, key, false)
		if err != nil {
			continue
		}
		t := sweep.Target{
			Key:        g.sweepKey(),
			ClusterUID: string(g.cluster.UID),
			ServerName: cnpgstatus.StatusServerName(g.cluster),
			MetricsTLS: g.cluster.Spec.Monitoring != nil && g.cluster.Spec.Monitoring.TLSConfig != nil && g.cluster.Spec.Monitoring.TLSConfig.Enabled,
		}
		for _, pod := range g.pods {
			t.Pods = append(t.Pods, sweep.PodTarget{
				Name: pod.Name, UID: string(pod.UID), IP: pod.Status.PodIP,
				StatusPortTLS: cnpgstatus.StatusPortTLSForMinor(g.minor, pod),
				Deleting:      pod.DeletionTimestamp != nil,
			})
		}
		out = append(out, t)
	}
	return out
}
