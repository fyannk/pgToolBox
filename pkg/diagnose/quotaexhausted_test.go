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

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// quotaInput is an input with one quota observed.
func quotaInput(resources ...observe.QuotaResourceFacts) Input {
	return Input{
		Now:       now,
		HasQuotas: true,
		Quotas: observe.QuotasSnapshot{Quotas: []observe.QuotaFacts{
			{Name: "tight", Resources: resources},
		}},
	}
}

// TestQuotaExhaustedNamesTheQuota proves the cause half of the quota
// story: the finding names the quota, quotes ceiling and usage as the
// API server reports them, and carries guidance. A quota below its
// ceiling is clear, and unobserved quotas are could-not-run.
func TestQuotaExhaustedNamesTheQuota(t *testing.T) {
	t.Parallel()
	detector := quotaExhaustedDetector{}

	if _, unavailable := detector.Detect(Input{Now: now}); unavailable == "" {
		t.Error("unobserved quotas did not report could-not-run")
	}

	headroom := quotaInput(observe.QuotaResourceFacts{
		Resource: "requests.storage", Hard: "10Gi", Used: "1Gi",
	})
	if findings, _ := detector.Detect(headroom); len(findings) != 0 {
		t.Errorf("a quota with headroom was flagged: %+v", findings)
	}

	full := quotaInput(
		observe.QuotaResourceFacts{Resource: "pods", Hard: "10", Used: "3"},
		observe.QuotaResourceFacts{Resource: "requests.storage", Hard: "1Gi", Used: "1Gi", Exhausted: true},
	)
	findings, unavailable := detector.Detect(full)
	if unavailable != "" || len(findings) != 1 {
		t.Fatalf("findings = %+v, unavailable = %q", findings, unavailable)
	}
	finding := findings[0]
	if finding.Severity != SeverityWarning {
		t.Errorf("severity = %v without an instance shortfall, want warning", finding.Severity)
	}
	if !strings.Contains(finding.Summary, `"tight"`) || !strings.Contains(finding.Summary, "requests.storage") {
		t.Errorf("summary names neither quota nor resource: %q", finding.Summary)
	}
	if !strings.Contains(finding.Evidence[0].Detail, "used 1Gi of 1Gi") {
		t.Errorf("ceiling and usage not quoted: %q", finding.Evidence[0].Detail)
	}
	if finding.NextSteps == "" {
		t.Error("no guidance on the one finding whose remedy is always the same shape")
	}
}

// TestQuotaExhaustedEscalatesWithTheShortfall proves the correlation:
// a full quota beside a cluster short of its declared instances is the
// refusal in progress, and reads as critical with both facts quoted.
func TestQuotaExhaustedEscalatesWithTheShortfall(t *testing.T) {
	t.Parallel()
	desired := 3
	in := quotaInput(observe.QuotaResourceFacts{
		Resource: "requests.storage", Hard: "1Gi", Used: "1Gi", Exhausted: true,
	})
	in.HasCluster = true
	in.Cluster = observe.Snapshot{Cluster: observe.ClusterFacts{
		Present: true, DesiredInstances: &desired,
	}}
	in.HasPods = true
	in.Pods = observe.PodsSnapshot{Pods: []observe.PodFacts{{Name: "quota-1"}}}

	findings, _ := quotaExhaustedDetector{}.Detect(in)
	if len(findings) != 1 || findings[0].Severity != SeverityCritical {
		t.Fatalf("findings = %+v, want one critical", findings)
	}
	last := findings[0].Evidence[len(findings[0].Evidence)-1]
	if !strings.Contains(last.Detail, "3 instances declared, 1 pods observed") {
		t.Errorf("the shortfall is not quoted beside the quota: %+v", findings[0].Evidence)
	}
}
