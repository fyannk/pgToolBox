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

// PodSubject is how reliably a condition names the pod its findings are
// about. It exists because a pod-scoped relation asks that of both ends:
// Relation.Holds refuses the pair unless each side names a pod, so a
// relation written against a cause that never names one can never hold,
// and would sit in the catalog looking like a claim while doing nothing.
//
// The three states are needed because the honest answer is not a
// boolean. A log match is always about the container that wrote the
// line; a cluster phase is never about a pod; an event is about whatever
// object Kubernetes recorded it on, which is sometimes a pod and
// sometimes the Cluster.
type PodSubject int

const (
	// PodSubjectNever means the condition's findings name something
	// other than a pod, or name nothing at all.
	//
	// It is the zero value on purpose. A condition nobody has classified
	// reads as unable to carry a pod-scoped relation, so the catalog
	// test refuses the relation and someone has to look — which is the
	// opposite of the silence this whole type exists to prevent. A new
	// condition that does name a pod trips that test the first time a
	// relation points at it, and adding it to PodSubjectOf is the fix.
	PodSubjectNever PodSubject = iota
	// PodSubjectSometimes means it depends on what was observed. A
	// pod-scoped relation to such a cause is not dead: it holds on the
	// findings that name a pod and is reported as a near miss on the
	// ones that do not, which is a real answer either way.
	PodSubjectSometimes
	// PodSubjectAlways means every finding it produces names a pod.
	PodSubjectAlways
)

// PodSubjectOf classifies a condition. A nil condition is a rule that
// fires on its version pins alone, whose findings name no object, so it
// never names a pod.
func PodSubjectOf(when Condition) PodSubject {
	switch condition := when.(type) {
	case nil:
		return PodSubjectNever
	case LogContains, LogFields:
		// The container that wrote the line.
		return PodSubjectAlways
	case ContainerState:
		// The pod the container belongs to.
		return PodSubjectAlways
	case InstantNonZero, InstantZero, InstantShortfall, SeriesAbove, PrimaryDisagreement:
		// The instance the exporter reported for.
		return PodSubjectAlways
	case PrimaryFailing, TimelineDivergence, PrimaryLeaseExpired, PrimaryLeaseHolderMismatch:
		// The instance the operator's status or the lease names.
		return PodSubjectAlways
	case InstanceFlagSet, InstanceArchiveFailing, InstanceReadyWAL, InstanceSlotInactive:
		// The instance whose own report matched.
		return PodSubjectAlways
	case StatusListed:
		// The failed-instances list names pods; the claim lists name
		// claims.
		if condition.List == ListFailedInstances {
			return PodSubjectAlways
		}
		return PodSubjectNever
	case EventMatch:
		// Whatever object the event was recorded on. The default scope
		// is the Cluster and its member pods; a rule naming kinds that
		// exclude pods never names one.
		if len(condition.Kinds) == 0 {
			return PodSubjectSometimes
		}
		for _, kind := range condition.Kinds {
			if kind == "Pod" {
				return PodSubjectSometimes
			}
		}
		return PodSubjectNever
	case AllOf:
		// A composite carries its first branch's subject — the others
		// corroborate it and must agree with it, but the finding is the
		// lead branch's — so that is the branch that decides.
		if len(condition.Of) == 0 {
			return PodSubjectNever
		}
		return PodSubjectOf(condition.Of[0])
	}
	return PodSubjectNever
}
