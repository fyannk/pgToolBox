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

package triage

import (
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog"
)

var now = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

// TestEveryStepNamesChecksThatExist is the drift guard: a playbook is a
// reading order over the catalog, and a step naming a check the catalog
// no longer declares would answer "could not be judged" forever.
func TestEveryStepNamesChecksThatExist(t *testing.T) {
	t.Parallel()
	known := map[string]bool{}
	for _, detector := range diagnose.Detectors() {
		known[detector.Name()] = true
	}
	for _, rule := range catalog.Rules() {
		known[rule.ID] = true
	}
	ids := map[string]bool{}
	for _, playbook := range Playbooks() {
		if playbook.ID == "" || playbook.Symptom == "" || len(playbook.Steps) == 0 || playbook.Upstream == "" {
			t.Errorf("playbook is incomplete: %+v", playbook)
		}
		if ids[playbook.ID] {
			t.Errorf("duplicate playbook id %q", playbook.ID)
		}
		ids[playbook.ID] = true
		for _, step := range playbook.Steps {
			if step.Question == "" || step.Explain == "" {
				t.Errorf("%s: step is missing its question or explanation: %+v", playbook.ID, step)
			}
			if len(step.Checks) == 0 && step.Fact == nil && step.Yourself == "" {
				t.Errorf("%s: %q has nothing behind it and nothing to hand the reader", playbook.ID, step.Question)
			}
			seen := map[string]bool{}
			for _, name := range step.Checks {
				if !known[name] {
					t.Errorf("%s: %q names check %q, which nothing declares", playbook.ID, step.Question, name)
				}
				if seen[name] {
					t.Errorf("%s: %q names %q twice", playbook.ID, step.Question, name)
				}
				seen[name] = true
			}
		}
	}
}

// clusterWith is an input whose Cluster reports the given facts on
// CloudNativePG 1.30 with PostgreSQL 17, so version-pinned rules apply.
func clusterWith(facts observe.ClusterFacts) diagnose.Input {
	major := 17
	facts.Present = true
	facts.PostgresMajorVersion = &major
	facts.ObservedSince = map[string]time.Time{}
	for _, key := range facts.HeldKeys() {
		facts.ObservedSince[key] = now.Add(-2 * time.Hour)
	}
	return diagnose.Input{
		Now: now, HasCluster: true, Cluster: observe.Snapshot{Cluster: facts},
		HasPods: true, Pods: observe.PodsSnapshot{Pods: []observe.PodFacts{{
			Name: "orders-1", Containers: []observe.ContainerFacts{
				{Name: "bootstrap-controller", Init: true, Image: "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0"},
				{Name: "postgres", Image: "ghcr.io/cloudnative-pg/postgresql:17.5"},
			},
		}}},
	}
}

// TestEveryPlaybookHasAGoldenScenario proves each playbook finds its
// cause at the step the scenario was built for — the reading order is
// only worth having if a real state lands where the guide says.
func TestEveryPlaybookHasAGoldenScenario(t *testing.T) {
	t.Parallel()
	ready := true
	scenarios := map[string]struct {
		in       diagnose.Input
		startAt  int
		question string
	}{
		"cannot-connect": {
			clusterWith(observe.ClusterFacts{
				Phase: "Cluster in healthy state", CurrentPrimary: "orders-1",
				Conditions: []observe.Condition{{Type: "cnpg.io/hibernation", Status: "True", Reason: "Hibernated"}},
			}),
			0, "Is the cluster deliberately down?",
		},
		"writes-refused": {
			clusterWith(observe.ClusterFacts{
				Phase: "Cluster in healthy state", CurrentPrimary: "orders-1",
				Conditions: []observe.Condition{{Type: "ReplicaClusterFencing", Status: "True"}},
			}),
			0, "Is this cluster a replica, or being demoted to one?",
		},
		"backups-not-happening": {func() diagnose.Input {
			in := clusterWith(observe.ClusterFacts{Phase: "Cluster in healthy state", CurrentPrimary: "orders-1"})
			suspended := true
			in.HasBackups = true
			in.Backups = observe.BackupsSnapshot{ScheduledBackups: []observe.ScheduledBackupFacts{{Name: "nightly", Suspended: &suspended}}}
			return in
		}(), 0, "Is there a schedule, and is it firing?"},
		"will-not-come-up": {
			clusterWith(observe.ClusterFacts{
				Phase:       "Cluster is unrecoverable and needs manual intervention",
				PhaseReason: "No pods are active, the cluster needs manual intervention",
			}),
			0, "Has the operator refused the definition, or stopped before starting?",
		},
		"replica-behind": {func() diagnose.Input {
			four := 4
			in := clusterWith(observe.ClusterFacts{
				Phase: "Cluster in healthy state", CurrentPrimary: "orders-1", TimelineID: &four,
				InstanceTimelines: []observe.InstanceTimeline{{Instance: "orders-2", TimelineID: 3}},
			})
			return in
		}(), 2, "Did the replica fail to rejoin after a promotion?"},
		"operation-stuck": {
			clusterWith(observe.ClusterFacts{
				Phase:       "Waiting for user action",
				PhaseReason: "User must issue a supervised switchover", CurrentPrimary: "orders-1",
			}),
			1, "Is the operator waiting for a person?",
		},
		"operator-silent": {
			clusterWith(observe.ClusterFacts{
				Phase:       "Cluster cannot proceed to reconciliation due to an unknown plugin being required",
				PhaseReason: "Unknown plugin: 'barman-cloud.cloudnative-pg.io'", CurrentPrimary: "orders-1",
			}),
			1, "Has reconciliation stopped on a plugin or the definition?",
		},
	}
	_ = ready
	for _, playbook := range Playbooks() {
		scenario, ok := scenarios[playbook.ID]
		if !ok {
			t.Errorf("playbook %q has no golden scenario", playbook.ID)
			continue
		}
		walk := Evaluate(playbook, scenario.in, diagnose.Run(scenario.in, catalog.Rules()...))
		if walk.StartAt != scenario.startAt {
			t.Errorf("%s: starts at %d, want %d (%q); steps:\n  %s", playbook.ID, walk.StartAt, scenario.startAt,
				scenario.question, describe(walk))
			continue
		}
		if walk.Steps[walk.StartAt].Question != scenario.question {
			t.Errorf("%s: step %d is %q, want %q", playbook.ID, walk.StartAt, walk.Steps[walk.StartAt].Question, scenario.question)
		}
		if found := walk.Steps[walk.StartAt]; len(found.Findings) == 0 && len(found.Evidence) == 0 {
			t.Errorf("%s: the found step carries neither findings nor evidence", playbook.ID)
		}
	}
}

func describe(walk Walk) string {
	lines := make([]string, 0, len(walk.Steps))
	for _, step := range walk.Steps {
		lines = append(lines, step.String())
	}
	return strings.Join(lines, "\n  ")
}

// TestAStepAnswersHonestly proves the five outcomes on one synthetic
// playbook: a matched check finds, all-clear rules out, an unrunnable
// check leaves the step unjudged even beside clear ones, a switched-off
// source and an inapplicable pin each say so, and a step with nothing
// behind it is a question for the reader.
func TestAStepAnswersHonestly(t *testing.T) {
	t.Parallel()
	result := diagnose.Result{
		Findings: []diagnose.Finding{{ID: "a/x", Check: "a", Summary: "A matched."}},
		Checks: []diagnose.Check{
			{Name: "a", Outcome: diagnose.CheckMatched},
			{Name: "b", Outcome: diagnose.CheckClear},
			{Name: "c", Outcome: diagnose.CheckUnavailable, Because: "the roster is stale"},
			{Name: "d", Outcome: diagnose.CheckUnavailable, Because: "log following is switched off", SourceOff: true},
			{Name: "e", Outcome: diagnose.CheckNotApplicable},
		},
	}
	playbook := Playbook{ID: "p", Symptom: "s", Steps: []Step{
		{Question: "found", Checks: []string{"b", "a"}},
		{Question: "ruled out", Checks: []string{"b"}},
		{Question: "unknown", Checks: []string{"b", "c"}},
		{Question: "off", Checks: []string{"d"}},
		{Question: "clear beside off", Checks: []string{"b", "d"}},
		{Question: "na", Checks: []string{"e"}},
		{Question: "ask", Yourself: "kubectl get something"},
		{Question: "fact yes", Fact: ClusterAbsent{}},
		{Question: "fact absent cluster", Fact: NoPrimaryNamed{}, Checks: []string{"b"}},
	}}
	in := diagnose.Input{Now: now, HasCluster: true, Cluster: observe.Snapshot{Cluster: observe.ClusterFacts{Present: false}}}
	walk := Evaluate(playbook, in, result)
	want := []Outcome{OutcomeFound, OutcomeRuledOut, OutcomeUnknown, OutcomeOff, OutcomeUnknown, OutcomeNotApplicable, OutcomeAsk, OutcomeFound, OutcomeFound}
	for i, step := range walk.Steps {
		if step.Outcome != want[i] {
			t.Errorf("%s: %v, want %v (%s)", step.Question, step.Outcome, want[i], step.Because)
		}
	}
	if walk.StartAt != 0 || len(walk.Steps[0].Findings) != 1 || walk.Steps[0].Findings[0].ID != "a/x" {
		t.Errorf("found step = %+v, start %d", walk.Steps[0], walk.StartAt)
	}
	if !strings.Contains(walk.Steps[2].Because, "the roster is stale") {
		t.Errorf("unknown step does not carry the check's own reason: %q", walk.Steps[2].Because)
	}
	if len(walk.Steps[7].Evidence) != 1 {
		t.Errorf("a found fact carries no evidence: %+v", walk.Steps[7])
	}
	if walk.Steps[8].Outcome != OutcomeFound || !strings.Contains(walk.Steps[4].Because, "switched off") {
		t.Errorf("absent cluster = %v; clear beside off says %q", walk.Steps[8].Outcome, walk.Steps[4].Because)
	}
}

// TestFactsReadTheSnapshotsTheyName proves each fact answers from the
// snapshot it is about, in the vocabulary the adapters actually write,
// and cannot answer without it.
func TestFactsReadTheSnapshotsTheyName(t *testing.T) {
	t.Parallel()
	ready := true
	in := diagnose.Input{
		Now:        now,
		HasCluster: true, Cluster: observe.Snapshot{Cluster: observe.ClusterFacts{Present: true, CurrentPrimary: "orders-1"}},
		HasPods: true, Pods: observe.PodsSnapshot{Pods: []observe.PodFacts{{Name: "orders-1", Ready: &ready}}},
		HasInfrastructure: true, Infrastructure: observe.InfrastructureSnapshot{Services: []observe.ServiceFacts{
			{Name: "orders-rw", Role: "read-write"}, {Name: "orders-r", Role: "any instance"},
		}},
		HasBackups: true, Backups: observe.BackupsSnapshot{ScheduledBackups: []observe.ScheduledBackupFacts{{Name: "nightly"}}},
	}
	for _, fact := range []Fact{ClusterAbsent{}, NoPrimaryNamed{}, NoReadyInstance{}, NoWriteService{}, NoBackupSchedule{}} {
		if yes, _, unknown := fact.Answer(in); yes || unknown != "" {
			t.Errorf("%s: answered yes=%v unknown=%q on a healthy cluster", fact.Describe(), yes, unknown)
		}
	}
	empty := diagnose.Input{Now: now}
	for _, fact := range []Fact{ClusterAbsent{}, NoPrimaryNamed{}, NoReadyInstance{}, NoWriteService{}, NoBackupSchedule{}} {
		if _, _, unknown := fact.Answer(empty); unknown == "" {
			t.Errorf("%s: answered without its snapshot", fact.Describe())
		}
	}
	in.Infrastructure.Services = in.Infrastructure.Services[1:]
	if yes, evidence, _ := (NoWriteService{}).Answer(in); !yes || len(evidence) != 1 {
		t.Errorf("no read-write service: yes=%v evidence=%+v", yes, evidence)
	}
}
