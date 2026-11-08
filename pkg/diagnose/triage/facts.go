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
	"fmt"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// The facts are absences the catalog has no rule for, because an
// absence is not a fault the operator reports — it is the shape of the
// cluster. Each reads one snapshot, says when it cannot, and quotes
// what it read.

// ClusterAbsent is the API server confirming there is no Cluster.
type ClusterAbsent struct{}

// Describe states the question.
func (ClusterAbsent) Describe() string {
	return "the API server reports no Cluster object of this name"
}

// Answer reads the snapshot the question is about.
func (ClusterAbsent) Answer(in diagnose.Input) (bool, []diagnose.Evidence, string) {
	switch {
	case !in.HasCluster:
		return false, nil, "the Cluster has not been observed yet"
	case in.Cluster.Stale:
		return false, nil, "the Cluster snapshot is stale"
	case !in.Cluster.Cluster.Present:
		return true, []diagnose.Evidence{{
			Origin: "Kubernetes-observed", Object: "Cluster",
			Detail: "the pinned get returned not found",
		}}, ""
	}
	return false, nil, ""
}

// NoPrimaryNamed is the operator naming no current primary.
type NoPrimaryNamed struct{}

// Describe states the question.
func (NoPrimaryNamed) Describe() string { return "the operator names no current primary" }

// Answer reads the snapshot the question is about.
func (NoPrimaryNamed) Answer(in diagnose.Input) (bool, []diagnose.Evidence, string) {
	switch {
	case !in.HasCluster:
		return false, nil, "the Cluster has not been observed yet"
	case in.Cluster.Stale:
		return false, nil, "the Cluster snapshot is stale"
	case !in.Cluster.Cluster.Present:
		// No Cluster at all is the strongest form of no primary named.
		return true, []diagnose.Evidence{{
			Origin: "Kubernetes-observed", Object: "Cluster",
			Detail: "the pinned get returned not found",
		}}, ""
	}
	cluster := in.Cluster.Cluster
	if cluster.CurrentPrimary != "" {
		return false, nil, ""
	}
	detail := "currentPrimary empty"
	if cluster.TargetPrimary != "" {
		detail += ", targetPrimary " + cluster.TargetPrimary
	}
	if cluster.Phase != "" {
		detail += ", phase " + cluster.Phase
	}
	return true, []diagnose.Evidence{{Origin: "operator-reported", Object: "Cluster status", Detail: detail}}, ""
}

// NoReadyInstance is the roster holding no pod the kubelet reports
// ready.
type NoReadyInstance struct{}

// Describe states the question.
func (NoReadyInstance) Describe() string { return "no instance pod is ready" }

// Answer reads the snapshot the question is about.
func (NoReadyInstance) Answer(in diagnose.Input) (bool, []diagnose.Evidence, string) {
	switch {
	case !in.HasPods:
		return false, nil, "the pod roster has not been observed yet"
	case in.Pods.Stale:
		return false, nil, "the pod roster is stale"
	case in.Pods.Truncated:
		return false, nil, "the pod roster is bounded, so a ready pod may have been cut from it"
	}
	ready := 0
	for _, pod := range in.Pods.Pods {
		if pod.Ready != nil && *pod.Ready {
			ready++
		}
	}
	if ready > 0 {
		return false, nil, ""
	}
	return true, []diagnose.Evidence{{
		Origin: "Kubernetes-observed", Object: "instance pods",
		Detail: fmt.Sprintf("%d pods observed, none ready", len(in.Pods.Pods)),
	}}, ""
}

// NoWriteService is the cluster's read-write Service not existing.
type NoWriteService struct{}

// Describe states the question.
func (NoWriteService) Describe() string { return "the cluster has no read-write Service" }

// Answer reads the snapshot the question is about.
func (NoWriteService) Answer(in diagnose.Input) (bool, []diagnose.Evidence, string) {
	switch {
	case !in.HasInfrastructure:
		return false, nil, "the cluster's Services have not been observed yet"
	case in.Infrastructure.Stale:
		return false, nil, "the cluster's Services are stale"
	case in.Infrastructure.Truncated:
		return false, nil, "the cluster's objects are bounded, so the Service may have been cut from the list"
	}
	for _, service := range in.Infrastructure.Services {
		// The adapter names the role from the operator's -rw suffix.
		if service.Role == "read-write" {
			return false, nil, ""
		}
	}
	return true, []diagnose.Evidence{{
		Origin: "Kubernetes-observed", Object: "Services",
		Detail: fmt.Sprintf("%d services observed, none the read-write one", len(in.Infrastructure.Services)),
	}}, ""
}

// NoBackupSchedule is the cluster having no ScheduledBackup at all.
type NoBackupSchedule struct{}

// Describe states the question.
func (NoBackupSchedule) Describe() string { return "the cluster has no backup schedule" }

// Answer reads the snapshot the question is about.
func (NoBackupSchedule) Answer(in diagnose.Input) (bool, []diagnose.Evidence, string) {
	switch {
	case !in.HasBackups:
		return false, nil, "the backup catalog has not been observed yet"
	case in.Backups.Stale:
		return false, nil, "the backup catalog is stale"
	case in.Backups.SchedulesTruncated:
		return false, nil, "the schedule list is bounded, so a schedule may have been cut from it"
	}
	if len(in.Backups.ScheduledBackups) > 0 {
		return false, nil, ""
	}
	return true, []diagnose.Evidence{{
		Origin: "operator-reported", Object: "ScheduledBackup objects",
		Detail: "none reference this cluster",
	}}, ""
}
