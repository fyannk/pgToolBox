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
)

// quotaExhaustedDetector reports a namespace ResourceQuota whose usage
// has reached its ceiling. This is the cause half of the quota story:
// the event-backed resource-quota detector quotes a refusal after it
// happened, and the scheduling detector shows the pod stuck on the
// object that could not be created — but neither can name the quota,
// its ceiling, or its usage. This one can, because the quota itself is
// observed.
//
// An exhausted quota with everything running is capacity, not damage,
// so the base severity is a warning. It escalates to critical when the
// cluster is visibly short of its declared instances at the same time:
// the two facts together are the refusal in progress.
//
// It implements Detector.
type quotaExhaustedDetector struct{}

func (quotaExhaustedDetector) Name() string { return "quota-exhausted" }

func (quotaExhaustedDetector) Layer() Layer { return LayerKubernetes }

func (quotaExhaustedDetector) Describes() string {
	return "a namespace quota whose usage has reached its ceiling"
}

func (d quotaExhaustedDetector) Detect(in Input) ([]Finding, string) {
	if !in.HasQuotas {
		return nil, "resource quotas have not been observed yet — deployments predating the " +
			"resourcequotas grant need the updated Role"
	}
	if in.Quotas.Stale {
		return nil, "the resource quotas are stale, so current usage is unknown"
	}

	shortfall, short := instanceShortfall(in)

	var findings []Finding
	for _, quota := range in.Quotas.Quotas {
		var exhausted []string
		var evidence []Evidence
		for _, line := range quota.Resources {
			if !line.Exhausted {
				continue
			}
			exhausted = append(exhausted, line.Resource)
			evidence = append(evidence, Evidence{
				Origin: "Kubernetes-reported",
				Object: "ResourceQuota/" + quota.Name,
				Detail: fmt.Sprintf("%s: used %s of %s — the ceiling is reached", line.Resource, line.Used, line.Hard),
			})
		}
		if len(exhausted) == 0 {
			continue
		}
		finding := Finding{
			ID:       "quota-exhausted/" + quota.Name,
			Check:    "quota-exhausted",
			Subject:  EntityRef{Kind: "ResourceQuota", Name: quota.Name},
			Severity: SeverityWarning,
			Summary: fmt.Sprintf("The namespace quota %q is exhausted for %s.",
				quota.Name, strings.Join(exhausted, ", ")),
			Detail: "Anything that needs another unit of these is refused at admission " +
				"until room exists. The refusal is silent from the cluster's side: the " +
				"operator keeps retrying, and the objects simply never appear.",
			Evidence: evidence,
			NextSteps: "Raise the quota's ceiling, or lower what this namespace asks of it " +
				"— fewer instances, smaller volumes, freed leftovers. The operator " +
				"retries refused objects on its own once room exists; nothing needs " +
				"recreating by hand.",
			Link:      "/cluster/pods",
			LinkLabel: "Pods",
		}
		if short {
			// The refusal in progress: the quota is full and the cluster
			// is short of instances at the same time.
			finding.Severity = SeverityCritical
			finding.Evidence = append(finding.Evidence, shortfall)
		}
		findings = append(findings, finding)
	}
	return findings, ""
}
