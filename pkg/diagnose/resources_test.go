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
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/evidence"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// TestPoolerShortReadsTheOperatorsCounts proves the pooler condition:
// could-not-run without or with a stale pooler set, clear on a full or
// undeclared pooler, and a per-pooler finding quoting declared against
// ready when one is short.
func TestPoolerShortReadsTheOperatorsCounts(t *testing.T) {
	t.Parallel()
	rule := Rule{ID: "pooler", Summary: "Pooler.", When: PoolerShort{}}
	two := int32(2)
	if check, _ := evaluateRule(rule, Input{Now: now}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with no poolers observed, want could-not-run", check.Outcome)
	}
	in := Input{Now: now, HasPoolers: true, Poolers: observe.PoolersSnapshot{Poolers: []observe.PoolerFacts{
		{Name: "orders-rw", DesiredInstances: &two, ReadyInstances: 2},
		{Name: "orders-ro", ReadyInstances: 0},
	}}}
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with a full pooler and an undeclared one, want clear", check.Outcome)
	}
	in.Poolers.Poolers[0].ReadyInstances = 0
	in.Poolers.Poolers[0].Phase = "Pooler instances are not ready"
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched || len(findings) != 1 || findings[0].ID != "pooler/orders-rw" {
		t.Fatalf("outcome = %v, findings %+v; want one on orders-rw", check.Outcome, findings)
	}
	if findings[0].Subject != (EntityRef{Kind: "Pooler", Name: "orders-rw"}) ||
		!strings.Contains(findings[0].Summary, "0 of 2") ||
		!strings.Contains(findings[0].Evidence[0].Detail, "2 instances declared, 0 ready") ||
		!strings.Contains(findings[0].Evidence[0].Detail, "not ready") {
		t.Errorf("finding does not quote the operator's counts and phase: %+v", findings[0])
	}
	in.Poolers.Stale = true
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v on a stale pooler set, want could-not-run", check.Outcome)
	}
}

// TestQuorumStandbysShortComparesTheTwoNumbers proves the quorum
// condition: absence of the resource is clear, a truncated standby list
// cannot be compared, and a standby count below the required number is
// the finding, quoting both.
func TestQuorumStandbysShortComparesTheTwoNumbers(t *testing.T) {
	t.Parallel()
	rule := Rule{ID: "quorum", Summary: "Quorum.", When: QuorumStandbysShort{}}
	if check, _ := evaluateRule(rule, Input{Now: now}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with no quorum observed, want could-not-run", check.Outcome)
	}
	in := Input{Now: now, HasFailoverQuorum: true, FailoverQuorum: observe.FailoverQuorumSnapshot{
		Quorum: observe.FailoverQuorumFacts{Present: false},
	}}
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with no quorum resource, want clear", check.Outcome)
	}
	in.FailoverQuorum.Quorum = observe.FailoverQuorumFacts{
		Present: true, Method: "any",
		Primary: "orders-1", StandbyNumber: 2, Standbys: []string{"orders-2", "orders-3"},
	}
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with enough standbys, want clear", check.Outcome)
	}
	in.FailoverQuorum.Quorum.Standbys = []string{"orders-2"}
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v with one standby of two, want matched", check.Outcome)
	}
	if !strings.Contains(findings[0].Summary, "wait for 2") || !strings.Contains(findings[0].Summary, "only 1") ||
		!strings.Contains(findings[0].Evidence[0].Detail, "standbyNames [orders-2]") {
		t.Errorf("finding does not quote both numbers: %+v", findings[0])
	}
	in.FailoverQuorum.Quorum.StandbysTruncated = true
	if check, _ := evaluateRule(rule, in); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v with a truncated standby list, want could-not-run", check.Outcome)
	}
}

// TestImageCatalogConditionsFollowTheReference proves the two catalog
// conditions and the lookup they share: no reference is clear, a
// namespaced catalog that is absent is the missing finding, one that
// lacks the major is the other finding, and each way the cluster-scoped
// lookup can decline is could-not-run rather than a claim.
func TestImageCatalogConditionsFollowTheReference(t *testing.T) {
	t.Parallel()
	missing := Rule{ID: "missing", Summary: "Missing.", When: ImageCatalogMissing{}}
	lacks := Rule{ID: "lacks", Summary: "Lacks.", When: ImageCatalogLacksMajor{}}
	outcome := func(rule Rule, in Input) CheckOutcome {
		check, _ := evaluateRule(rule, in)
		return check.Outcome
	}

	in := clusterInput(17)
	if outcome(missing, in) != CheckClear || outcome(lacks, in) != CheckClear {
		t.Error("a Cluster naming no catalog should be clear on both checks: there is nothing to look up")
	}
	in.Cluster.Cluster.ImageCatalogRef = &observe.ImageCatalogRef{Kind: "ImageCatalog", Name: "postgres"}
	if outcome(missing, in) != CheckUnavailable || outcome(lacks, in) != CheckUnavailable {
		t.Error("a reference with the catalogs never observed should be could-not-run on both checks")
	}
	in.HasImageCatalogs = true

	check, findings := evaluateRule(missing, in)
	if check.Outcome != CheckMatched || len(findings) != 1 || len(findings[0].Evidence) != 2 {
		t.Fatalf("absent namespaced catalog: outcome %v, findings %+v", check.Outcome, findings)
	}
	if outcome(lacks, in) != CheckClear {
		t.Error("the lacks-major check should stay clear when the catalog is absent — that is the other finding")
	}
	in.ImageCatalogs.Truncated = true
	if outcome(missing, in) != CheckUnavailable {
		t.Error("a truncated catalog set cannot prove absence")
	}
	in.ImageCatalogs.Truncated = false

	in.ImageCatalogs.Catalogs = []observe.ImageCatalogFacts{{
		Name:   "postgres",
		Images: []observe.CatalogImageFacts{{Major: 15, Image: "pg:15"}, {Major: 16, Image: "pg:16"}},
	}}
	if outcome(missing, in) != CheckClear {
		t.Error("a present catalog should clear the missing check")
	}
	check, findings = evaluateRule(lacks, in)
	if check.Outcome != CheckMatched || len(findings) != 1 ||
		!strings.Contains(findings[0].Summary, "PostgreSQL 17") ||
		!strings.Contains(findings[0].Evidence[1].Detail, "[15, 16]") {
		t.Fatalf("catalog lacking the major: outcome %v, findings %+v", check.Outcome, findings)
	}
	in.ImageCatalogs.Catalogs[0].Images = append(in.ImageCatalogs.Catalogs[0].Images, observe.CatalogImageFacts{Major: 17})
	if outcome(lacks, in) != CheckClear {
		t.Error("a catalog carrying the major should clear the lacks check")
	}

	in.Cluster.Cluster.ImageCatalogRef = &observe.ImageCatalogRef{Kind: "ClusterImageCatalog", Name: "postgres"}
	for state, want := range map[observe.ClusterCatalogState]CheckOutcome{
		observe.ClusterCatalogAbsent:   CheckMatched,
		observe.ClusterCatalogPresent:  CheckClear,
		observe.ClusterCatalogDisabled: CheckUnavailable,
		observe.ClusterCatalogUnknown:  CheckUnavailable,
	} {
		in.ImageCatalogs.ClusterCatalogState = state
		if got := outcome(missing, in); got != want {
			t.Errorf("cluster-scoped catalog %s: missing check = %v, want %v", state, got, want)
		}
	}
}

// TestRepositoryStateRestatesTheSidecar proves the repository condition
// names every way the channel can be silent, matches only the evidence agent's
// own state, and quotes state, reason code and generation.
func TestRepositoryStateRestatesTheSidecar(t *testing.T) {
	t.Parallel()
	rule := Rule{ID: "wal", Summary: "WAL.", When: RepositoryState{Aspect: RepositoryWAL}}
	because := func(in Input) (CheckOutcome, string) {
		check, _ := evaluateRule(rule, in)
		return check.Outcome, check.Because
	}
	healthy := evidence.Report{
		Completeness: "complete", ScopeName: "orders", EvidenceGeneration: 7,
		Facts: &evidence.RepositoryFacts{Format: "barman-cloud", WAL: evidence.StateFact{State: "healthy", Code: "wal-contiguous"}},
	}

	for name, tc := range map[string]struct {
		in   Input
		want string
	}{
		"not configured": {Input{Now: now}, "not configured"},
		"never answered": {Input{
			Now: now, HasEvidence: true,
			Evidence: evidence.Status{Failure: evidence.FailureUnavailable},
		}, "unavailable"},
		"contact lost": {Input{Now: now, HasEvidence: true, Evidence: evidence.Status{
			HasReport: true,
			Snapshot:  evidence.Snapshot{Stale: true, Report: healthy},
		}}, "contact"},
		"no scan": {Input{Now: now, HasEvidence: true, Evidence: evidence.Status{
			HasReport: true,
			Snapshot:  evidence.Snapshot{Report: evidence.Report{Completeness: "no-completed-scan", Facts: healthy.Facts}},
		}}, "not completed a scan"},
		"agent stale": {Input{Now: now, HasEvidence: true, Evidence: evidence.Status{
			HasReport: true,
			Snapshot:  evidence.Snapshot{Report: evidence.Report{Completeness: "complete", SourceStale: true, Facts: healthy.Facts}},
		}}, "stale against the repository"},
		"unknown variant": {Input{Now: now, HasEvidence: true, Evidence: evidence.Status{
			HasReport: true,
			Snapshot:  evidence.Snapshot{Report: evidence.Report{Completeness: "complete"}},
		}}, "variant"},
	} {
		if got, why := because(tc.in); got != CheckUnavailable || !strings.Contains(why, tc.want) {
			t.Errorf("%s: outcome %v (%q), want could-not-run mentioning %q", name, got, why, tc.want)
		}
	}

	in := Input{Now: now, HasEvidence: true, Evidence: evidence.Status{
		HasReport: true,
		Snapshot:  evidence.Snapshot{Report: healthy},
	}}
	if got, _ := because(in); got != CheckClear {
		t.Errorf("outcome = %v on a healthy WAL state, want clear", got)
	}
	unhealthy := healthy
	unhealthy.Facts = &evidence.RepositoryFacts{Format: "barman-cloud", WAL: evidence.StateFact{State: "unhealthy", Code: "wal-gap-confirmed"}}
	unhealthy.Completeness = "incomplete"
	in.Evidence.Snapshot.Report = unhealthy
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v on an unhealthy WAL state, want matched", check.Outcome)
	}
	detail := findings[0].Evidence[0].Detail
	if !strings.Contains(detail, "state unhealthy") || !strings.Contains(detail, "wal-gap-confirmed") ||
		!strings.Contains(detail, "generation 7") || !strings.Contains(detail, "incomplete") {
		t.Errorf("evidence does not restate the evidence agent's state, code, generation and completeness: %q", detail)
	}
	if findings[0].Subject != (EntityRef{Kind: "Repository", Name: "orders"}) || findings[0].Evidence[0].Origin != "repository-evidence" {
		t.Errorf("finding is not attributed to the repository evidence: %+v", findings[0])
	}
}

// TestRepositoryRulesReadPgBackRestFacts proves the repository rules read
// the format-neutral facts of a pgBackRest repository: WAL matches on its
// state, retention says the format reports none instead of clearing, and
// a finding rule matches its code only and names the subject.
func TestRepositoryRulesReadPgBackRestFacts(t *testing.T) {
	t.Parallel()
	report := evidence.Report{
		Completeness: "complete", ScopeName: "main", EvidenceGeneration: 3, Format: "pgbackrest",
		Facts: &evidence.RepositoryFacts{
			Format: "pgbackrest",
			WAL:    evidence.StateFact{State: "unhealthy", Code: "wal-gap"},
			Findings: []evidence.FindingFact{
				{Code: "CopyMismatch", Severity: "warning", Subject: "backup/main/backup.info"},
				{Code: "StanzaSplit", Severity: "warning", Subject: "main"},
			},
		},
	}
	in := Input{Now: now, HasEvidence: true, Evidence: evidence.Status{HasReport: true, Snapshot: evidence.Snapshot{Report: report}}}

	check, findings := evaluateRule(Rule{ID: "wal", Summary: "WAL.", When: RepositoryState{Aspect: RepositoryWAL}}, in)
	if check.Outcome != CheckMatched || len(findings) != 1 || findings[0].Subject.Name != "main" {
		t.Fatalf("WAL rule on pgBackRest facts = %v %+v, want one match on stanza main", check.Outcome, findings)
	}
	check, _ = evaluateRule(Rule{ID: "retention", Summary: "Retention.", When: RepositoryState{Aspect: RepositoryRetention}}, in)
	if check.Outcome != CheckUnavailable || !strings.Contains(check.Because, "no retention comparison for a pgbackrest repository") {
		t.Errorf("retention rule on pgBackRest = %v (%q), want could-not-run naming the format", check.Outcome, check.Because)
	}
	split := Rule{ID: "split", Summary: "Split.", When: RepositoryFinding{Code: "StanzaSplit"}}
	check, findings = evaluateRule(split, in)
	if check.Outcome != CheckMatched || len(findings) != 1 || !strings.Contains(findings[0].Evidence[0].Detail, "StanzaSplit (warning) on main") {
		t.Fatalf("StanzaSplit rule = %v %+v, want one match restating code, severity and subject", check.Outcome, findings)
	}
	clean := report
	clean.Facts = &evidence.RepositoryFacts{Format: "pgbackrest", Findings: []evidence.FindingFact{{Code: "CopyMismatch", Severity: "warning", Subject: "x"}}}
	in.Evidence.Snapshot.Report = clean
	if check, _ = evaluateRule(split, in); check.Outcome != CheckClear {
		t.Errorf("StanzaSplit rule without the finding = %v, want clear", check.Outcome)
	}
	clean.Facts.FindingsTruncated = true
	in.Evidence.Snapshot.Report = clean
	if check, _ = evaluateRule(split, in); check.Outcome != CheckUnavailable {
		t.Errorf("StanzaSplit rule on a cut findings list = %v, want could-not-run", check.Outcome)
	}
}

// TestRetentionRulesReadPgBackRestRetention: once the agent compares a
// stanza with its retention, the retention rule reads that comparison like
// Barman's, and the finding rules match their codes.
func TestRetentionRulesReadPgBackRestRetention(t *testing.T) {
	t.Parallel()
	report := evidence.Report{
		Completeness: "complete", ScopeName: "main", EvidenceGeneration: 4, Format: "pgbackrest",
		Facts: &evidence.RepositoryFacts{
			Format: "pgbackrest",
			Retention: &evidence.RetentionFacts{
				Result:     evidence.StateFact{State: "unhealthy", Code: "retention-unhealthy"},
				PGBackRest: &evidence.PGBackRestRetentionFacts{Configured: true, FullType: "count"},
			},
			Findings: []evidence.FindingFact{
				{Code: "RetentionNotApplied", Severity: "warning", Subject: "20261101-000000F"},
				{Code: "RetentionShortfall", Severity: "error", Subject: "backup/main/"},
			},
		},
	}
	in := Input{Now: now, HasEvidence: true, Evidence: evidence.Status{HasReport: true, Snapshot: evidence.Snapshot{Report: report}}}
	check, findings := evaluateRule(Rule{ID: "retention", Summary: "Retention.", When: RepositoryState{Aspect: RepositoryRetention}}, in)
	if check.Outcome != CheckMatched || len(findings) != 1 || !strings.Contains(findings[0].Evidence[0].Detail, "retention-unhealthy") {
		t.Fatalf("retention rule on an unhealthy pgBackRest retention = %v %+v", check.Outcome, findings)
	}
	report.Facts.Retention.Result = evidence.StateFact{State: "healthy", Code: "retention-healthy"}
	in.Evidence.Snapshot.Report = report
	if check, _ = evaluateRule(Rule{ID: "retention", Summary: "Retention.", When: RepositoryState{Aspect: RepositoryRetention}}, in); check.Outcome != CheckClear {
		t.Errorf("retention rule on a healthy pgBackRest retention = %v, want clear", check.Outcome)
	}
	notApplied := Rule{ID: "not-applied", Summary: "Not applied.", When: RepositoryFinding{Code: "RetentionNotApplied"}}
	if check, findings = evaluateRule(notApplied, in); check.Outcome != CheckMatched || !strings.Contains(findings[0].Evidence[0].Detail, "RetentionNotApplied (warning) on 20261101-000000F") {
		t.Errorf("RetentionNotApplied rule = %v %+v", check.Outcome, findings)
	}
	notConfigured := Rule{ID: "not-configured", Summary: "Not configured.", When: RepositoryFinding{Code: "RetentionNotConfigured"}}
	if check, _ = evaluateRule(notConfigured, in); check.Outcome != CheckClear {
		t.Errorf("RetentionNotConfigured rule without the finding = %v, want clear", check.Outcome)
	}
}

// TestSeriesAboveReadsThePoolerWindow proves the pooler flag switches
// the window, the catalog and the link: the instance window's absence
// no longer matters, and the pooler window's absence is the reason.
func TestSeriesAboveReadsThePoolerWindow(t *testing.T) {
	t.Parallel()
	rule := Rule{ID: "wait", Summary: "Wait.", When: SeriesAbove{Key: "maxwait", Threshold: 5, Pooler: true}}
	if check, _ := evaluateRule(rule, Input{Now: now, Metrics: staticWindow{}}); check.Outcome != CheckUnavailable ||
		!strings.Contains(check.Because, "pooler metrics") {
		t.Errorf("outcome = %v (%q) without a pooler window, want could-not-run naming it", check.Outcome, check.Because)
	}
	ten := 10.0
	in := Input{Now: now, PoolerMetrics: seriesWindow{
		times: []int64{now.Unix()},
		raw:   map[string][]*float64{"orders-rw-1": {&ten}},
	}}
	check, findings := evaluateRule(rule, in)
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v, want matched on the pooler window", check.Outcome)
	}
	if !strings.Contains(findings[0].Evidence[0].Detail, "cnpg_pgbouncer_pools_maxwait = 10") ||
		findings[0].Link != "/poolers/metrics" || !strings.Contains(check.Describes, "pooler instance") {
		t.Errorf("finding does not read the pooler catalog: %+v (%s)", findings[0], check.Describes)
	}
}
