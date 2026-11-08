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

package catalog

import (
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

var testNow = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// outcomeOf finds one named check in a result.
func outcomeOf(t *testing.T, result diagnose.Result, name string) diagnose.Check {
	t.Helper()
	for _, check := range result.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("check %q did not account for itself", name)
	return diagnose.Check{}
}

// TestKubernetesEOLRidesTheObservedServerVersion walks the rule through
// its answers: could-not-run while the version is unobserved, matched on a
// retired line with the server's report and the table's date quoted, clear
// on a supported one and on a line newer than the table knows.
func TestKubernetesEOLRidesTheObservedServerVersion(t *testing.T) {
	t.Parallel()

	in := diagnose.Input{Now: testNow, Lifecycle: Lifecycle()}
	if got := outcomeOf(t, diagnose.Run(in, Rules()...), "k8s-eol"); got.Outcome != diagnose.CheckUnavailable {
		t.Errorf("outcome = %v with no observed version, want could-not-run", got.Outcome)
	}

	in.HasKubeVersion = true
	in.KubeVersion = observe.KubeVersionSnapshot{GitVersion: "v1.32.4"}
	result := diagnose.Run(in, Rules()...)
	if got := outcomeOf(t, result, "k8s-eol"); got.Outcome != diagnose.CheckMatched {
		t.Fatalf("outcome = %v on 1.32.4, want matched", got.Outcome)
	}
	for _, finding := range result.Findings {
		if finding.ID != "k8s-eol" {
			continue
		}
		if !strings.Contains(finding.Evidence[0].Detail, "v1.32.4") ||
			!strings.Contains(finding.Evidence[1].Detail, "2026-02-28") {
			t.Errorf("the server's report and the table's date are not quoted: %+v", finding.Evidence)
		}
	}

	for _, v := range []string{"v1.35.0", "v1.36.1"} {
		in.KubeVersion.GitVersion = v
		if got := outcomeOf(t, diagnose.Run(in, Rules()...), "k8s-eol"); got.Outcome != diagnose.CheckClear {
			t.Errorf("outcome = %v on %s, want clear", got.Outcome, v)
		}
	}
}

// The boundary moves with the clock, not with a release: 14 is supported
// on its last day and past end of life the day after, flagged as soon in
// the six months before; a major older than every line is past it too.
func TestPostgresEOLFollowsTheTableAndTheClock(t *testing.T) {
	t.Parallel()
	run := func(major int, now time.Time) (eol, soon diagnose.CheckOutcome) {
		in := diagnose.Input{Now: now, Lifecycle: Lifecycle(), HasCluster: true}
		in.Cluster.Cluster = observe.ClusterFacts{Present: true, PostgresMajorVersion: &major}
		result := diagnose.Run(in, Rules()...)
		return outcomeOf(t, result, "postgres-eol").Outcome, outcomeOf(t, result, "postgres-eol-soon").Outcome
	}
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	cases := []struct {
		major     int
		now       string
		eol, soon diagnose.CheckOutcome
	}{
		{14, "2026-01-01", diagnose.CheckClear, diagnose.CheckClear},
		{14, "2026-11-11", diagnose.CheckClear, diagnose.CheckMatched},
		{14, "2026-11-12", diagnose.CheckMatched, diagnose.CheckClear},
		{12, "2026-01-01", diagnose.CheckMatched, diagnose.CheckClear},
		{19, "2026-01-01", diagnose.CheckClear, diagnose.CheckClear},
	}
	for _, c := range cases {
		if eol, soon := run(c.major, day(c.now)); eol != c.eol || soon != c.soon {
			t.Errorf("PostgreSQL %d on %s: eol %v soon %v, want %v %v", c.major, c.now, eol, soon, c.eol, c.soon)
		}
	}
}

// A table a deployment supplies is refused whole on a typo.
func TestParseLifecycleRefusesTypos(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		`{"lines":{"PostgreSQL":[{"line":"14","eol":"12/11/2026"}]}}`,
		`{"lines":{"PostgreSQL":[{"line":"fourteen"}]}}`,
		`{"reviewed":"yesterday"}`,
		`not json`,
	} {
		if _, err := ParseLifecycle([]byte(bad), "test"); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	l, err := ParseLifecycle([]byte(`{"reviewed":"2026-11-02","lines":{"CloudNativePG":[{"line":"1.30","eol":"2027-05-01"}]}}`), "ConfigMap x")
	if err != nil || l.Source != "ConfigMap x" || len(l.Lines[diagnose.ComponentCNPG]) != 1 {
		t.Errorf("good table read as %+v, %v", l, err)
	}
}

// TestCrashLoopCatchesTheSidecar proves the gap the container rules
// close: a crash-looping plugin sidecar produces a finding even though
// the instance container is healthy and no CloudNativePG status says
// anything.
func TestCrashLoopCatchesTheSidecar(t *testing.T) {
	t.Parallel()
	restarts := 12
	in := diagnose.Input{
		Now:     testNow,
		HasPods: true,
		Pods: observe.PodsSnapshot{Pods: []observe.PodFacts{{
			Name: "orders-1",
			Containers: []observe.ContainerFacts{
				{Name: "postgres", State: "running"},
				{
					Name: "plugin-barman-cloud", State: "waiting", Reason: "CrashLoopBackOff",
					Restarts: &restarts,
				},
			},
		}}},
	}
	result := diagnose.Run(in, Rules()...)
	if got := outcomeOf(t, result, "k8s-container-crashloop"); got.Outcome != diagnose.CheckMatched {
		t.Fatalf("outcome = %v, want matched on the crash-looping sidecar", got.Outcome)
	}
	found := false
	for _, finding := range result.Findings {
		if finding.ID == "k8s-container-crashloop/orders-1/plugin-barman-cloud" {
			found = true
		}
	}
	if !found {
		t.Errorf("no finding names the sidecar: %+v", result.Findings)
	}
}

// TestMountFailureQuotesTheKubelet proves the event-backed mount rule
// carries the kubelet's message, which is where the missing Secret is
// named.
func TestMountFailureQuotesTheKubelet(t *testing.T) {
	t.Parallel()
	in := diagnose.Input{
		Now:       testNow,
		HasEvents: true,
		Events: observe.EventsSnapshot{Events: []observe.EventFacts{{
			Type: "Warning", Reason: "FailedMount", Kind: "Pod", Object: "orders-1",
			Message: `MountVolume.SetUp failed for volume "certs" : secret "orders-server" not found`,
		}}},
	}
	result := diagnose.Run(in, Rules()...)
	if got := outcomeOf(t, result, "k8s-volume-mount-failed"); got.Outcome != diagnose.CheckMatched {
		t.Fatalf("outcome = %v, want matched", got.Outcome)
	}
	for _, finding := range result.Findings {
		if finding.ID != "k8s-volume-mount-failed" {
			continue
		}
		if !strings.Contains(finding.Evidence[0].Detail, `secret "orders-server" not found`) {
			t.Errorf("kubelet message not quoted: %+v", finding.Evidence)
		}
	}
}
