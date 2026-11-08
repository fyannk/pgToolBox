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

// Package viewmodel is the typed JSON contract of the console: the
// shapes the API serves (internal/console/api) and the SPA renders
// (ui/). It holds data only — no derivation, no Kubernetes types,
// no I/O — so the contract can be read, golden-tested and turned into
// OpenAPI on its own. The read model that fills these shapes lives in
// pkg/cnpgstatus and internal/console/status.
//
// The honesty vocabulary is built into the types rather
// than left to the renderer:
//
//   - every fact carries its Provenance (who said it, when it was
//     observed, whether that observation is stale);
//   - "unknown" is a Health of its own and never rolls up to ok;
//   - a JSON field that is absent means "not reported", never zero or
//     false: optional scalars are pointers with omitempty, and the UI
//     renders their absence as "unknown";
//   - what the console structurally cannot see is stated as a BlindSpot
//     next to the panel it affects, never left as an empty list.
//
// Compatibility rule: the package is additive. A field is never renamed
// or re-typed once a consumer exists; enum values are extended, never
// repurposed. Times are UTC RFC 3339.
package viewmodel

import (
	"encoding/json"
	"time"
)

// Health is one of the seven health states of the design system.
// The UI pairs each with a glyph and a word; colour is
// never the only signal.
type Health string

// The seven health states.
const (
	// HealthOK is reported healthy, streaming, backup completed.
	HealthOK Health = "ok"
	// HealthProgressing is a switchover, rollout, replica creation or
	// backup in progress.
	HealthProgressing Health = "progressing"
	// HealthWarning is lag above threshold, archiving delayed, a
	// certificate expiring.
	HealthWarning Health = "warning"
	// HealthCritical is a primary unavailable, a failover, a failed
	// backup, a crash loop.
	HealthCritical Health = "critical"
	// HealthInactive is hibernated, fenced, scaled to zero, a suspended
	// schedule: deliberately not running.
	HealthInactive Health = "inactive"
	// HealthUnknown is a stale or absent claim: the observer cannot tell.
	// It is never painted green.
	HealthUnknown Health = "unknown"
	// HealthInfo is neutral: provenance notes, facts with no judgement.
	HealthInfo Health = "info"
)

// AllHealth lists the states in roll-up order, best first.
var AllHealth = []Health{HealthInfo, HealthOK, HealthInactive, HealthProgressing, HealthUnknown, HealthWarning, HealthCritical}

// Rank is the state's position in the roll-up order, 0 = best; a state
// this build does not know ranks as unknown. Clients sort by it (or by
// the healthOrder of GET /api/v1/vocabulary) instead of keeping their
// own copy of the order.
func (h Health) Rank() int { return h.rank() }

// rank orders states for a roll-up. Unknown ranks above every state an
// observer could have confirmed, so a fact the console could not read
// never lets the headline read healthy; warning and critical rank above
// it because an observed fault is worse than an unobserved one.
func (h Health) rank() int {
	for i, state := range AllHealth {
		if state == h {
			return i
		}
	}
	// A state this build does not know is treated as unknown.
	return HealthUnknown.rank()
}

// Valid reports whether h is one of the seven states.
func (h Health) Valid() bool {
	for _, state := range AllHealth {
		if state == h {
			return true
		}
	}
	return false
}

// Worst returns the worst of the given states; HealthUnknown for none,
// because an empty roll-up has confirmed nothing.
func Worst(states ...Health) Health {
	if len(states) == 0 {
		return HealthUnknown
	}
	worst := states[0]
	for _, state := range states[1:] {
		if state.rank() > worst.rank() {
			worst = state
		}
	}
	if !worst.Valid() {
		return HealthUnknown
	}
	return worst
}

// Origin is who made a claim. The three origins of the plan
// are operator, instance and console; Kubernetes (pods, events, PVCs)
// and repository evidence are named separately because they can lie in
// different ways.
type Origin string

// The origins.
const (
	// OriginOperator is the CloudNativePG operator: Cluster.status,
	// child CR status, conditions, operator events. Freezes silently when
	// the operator is down.
	OriginOperator Origin = "operator"
	// OriginKubernetes is the Kubernetes control plane: Pod and PVC
	// status, scheduler and kubelet events.
	OriginKubernetes Origin = "kubernetes"
	// OriginInstance is an instance manager's self-report: /pg/status and
	// the metrics exporter. Up to one sweep interval old.
	OriginInstance Origin = "instance"
	// OriginConsole is the console's own inference: diagnostics rules,
	// cross-checks, roll-ups, "since when" clocks.
	OriginConsole Origin = "console"
	// OriginEvidence is the evidence agent's reading of the backup
	// repository.
	OriginEvidence Origin = "evidence"
)

// Provenance is the origin and age of one fact ("every fact
// is shown with its origin and age").
type Provenance struct {
	Origin Origin `json:"origin" enum:"operator,kubernetes,instance,console,evidence" doc:"Who made the claim."`
	// Source names the field or endpoint read, such as "Cluster.status.phase" or "/pg/status".
	Source string `json:"source,omitempty" doc:"The field or endpoint the fact was read from."`
	// ObservedAt is when the console last read the source successfully.
	ObservedAt *time.Time `json:"observedAt,omitempty" doc:"When the console last read the source successfully; absent when it never has."`
	// Stale is the staleness modifier of the design system: the value is
	// the last good observation and the source is not currently being
	// read.
	Stale bool `json:"stale,omitempty" doc:"The value is the retained last-good observation; the source is not being read now."`
	// StaleReason says why, in words, when Stale.
	StaleReason string `json:"staleReason,omitempty" doc:"Why the source is stale, when it is."`
}

// SourceStateKind is the state of one source the console reads.
type SourceStateKind string

// The source states. Only live licenses a judgement other than unknown.
const (
	// SourceLive is being read and current.
	SourceLive SourceStateKind = "live"
	// SourcePending has not delivered its first observation yet.
	SourcePending SourceStateKind = "pending"
	// SourceStale was read, lost contact, and keeps its last-good value.
	SourceStale SourceStateKind = "stale"
	// SourceUnavailable is configured and failing (network, TLS, API).
	SourceUnavailable SourceStateKind = "unavailable"
	// SourceForbidden is denied by RBAC to the console's ServiceAccount.
	SourceForbidden SourceStateKind = "forbidden"
	// SourceNotInstalled is an optional API absent from the cluster
	// (CRD not installed, CNPG minor without the kind).
	SourceNotInstalled SourceStateKind = "notInstalled"
	// SourceDisabled is switched off by configuration: a decision, not a
	// fault.
	SourceDisabled SourceStateKind = "disabled"
)

// Health is how a source state is drawn. Only live is ok;
// a source still delivering its first observation is progressing; a
// stale one is unknown (its value is the last good one); a source that
// is configured but failing or denied to the console is a warning (an
// observed fault of the console's reach, never of the cluster); an
// optional API that is not installed is info; a source switched off by
// configuration is inactive. A kind this build does not know is unknown.
func (k SourceStateKind) Health() Health {
	switch k {
	case SourceLive:
		return HealthOK
	case SourcePending:
		return HealthProgressing
	case SourceUnavailable, SourceForbidden:
		return HealthWarning
	case SourceNotInstalled:
		return HealthInfo
	case SourceDisabled:
		return HealthInactive
	default:
		return HealthUnknown
	}
}

// AllSourceStates lists the source states.
var AllSourceStates = []SourceStateKind{SourceLive, SourcePending, SourceStale, SourceUnavailable, SourceForbidden, SourceNotInstalled, SourceDisabled}

// SourceState is the console's account of one source: whether it is
// read, since when, and why not. It is what turns an empty panel into
// "could not be read" instead of "nothing there".
type SourceState struct {
	// Source is a stable key, such as "clusters", "pods", "events",
	// "instanceStatus", "metrics", "diagnostics", "objectStores".
	Source string          `json:"source" doc:"Stable key of the source."`
	State  SourceStateKind `json:"state" enum:"live,pending,stale,unavailable,forbidden,notInstalled,disabled" doc:"Whether the source is being read."`
	// Since is when the source entered State.
	Since *time.Time `json:"since,omitempty" doc:"When the source entered this state."`
	// Reason is a bounded, secret-free explanation for any state but live.
	Reason string `json:"reason,omitempty" doc:"Why the source is not live; never contains secret material."`
	// Health is State drawn as one of the seven states; MarshalJSON
	// fills it from State, so producers never set it by hand.
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info" doc:"How the state is drawn: live ok, pending progressing, stale unknown, unavailable and forbidden warning, notInstalled info, disabled inactive."`
}

// MarshalJSON writes the state with its Health derived from State.
func (s SourceState) MarshalJSON() ([]byte, error) {
	type plain SourceState
	p := plain(s)
	p.Health = s.State.Health()
	return json.Marshal(p)
}

// BlindSpot is something the console structurally cannot see, stated on
// the panel it affects, so an absence reads as a limit of
// the observer rather than as a healthy empty list.
type BlindSpot struct {
	// ID is a stable key for i18n, such as "replication.nonStreamingReplica".
	ID string `json:"id" doc:"Stable key of the blind spot, used for translation."`
	// Panel is the view the blind spot affects, such as "replication" or "backups".
	Panel string `json:"panel" doc:"The view this blind spot affects."`
	// Text is the English statement, for clients without a translation.
	Text string `json:"text" doc:"English statement of what cannot be seen."`
}

// ClusterRef names one CloudNativePG Cluster. The UID distinguishes a
// recreated Cluster of the same name.
type ClusterRef struct {
	Namespace string `json:"namespace" doc:"Namespace of the Cluster."`
	Name      string `json:"name" doc:"Name of the Cluster."`
	UID       string `json:"uid,omitempty" doc:"Kubernetes UID of the observed Cluster."`
}

// ObjectRef names a Kubernetes object a fact or finding is about.
type ObjectRef struct {
	Kind string `json:"kind" doc:"Kubernetes kind, such as Pod or Backup."`
	Name string `json:"name,omitempty" doc:"Object name; absent for a statement about the whole Cluster."`
}

// Vocabulary is the static contract the UI sorts and draws by, served
// once (GET /api/v1/vocabulary) so the client keeps no copy of Go's
// orders and mappings.
type Vocabulary struct {
	// HealthOrder is the roll-up order, best first: sort by index.
	HealthOrder []Health `json:"healthOrder"`
	// SourceStates maps each source state to its health.
	SourceStates []SourceStateHealth `json:"sourceStates"`
	// Sections are the cluster-relative routes a Link may name (the
	// first path segment; instances, history and logs take one more).
	Sections []string `json:"sections"`
}

// SourceStateHealth pairs a source state with how it is drawn.
type SourceStateHealth struct {
	State  SourceStateKind `json:"state" enum:"live,pending,stale,unavailable,forbidden,notInstalled,disabled"`
	Health Health          `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
}

// Sections are the cluster-relative routes links may name.
var Sections = []string{"", "instances", "events", "diagnostics", "triage", "replication", "backups", "storage", "objects", "history", "metrics", "operations", "logs", "insight"}

// NewVocabulary returns the vocabulary.
func NewVocabulary() Vocabulary {
	v := Vocabulary{HealthOrder: append([]Health(nil), AllHealth...), Sections: append([]string(nil), Sections...)}
	for _, s := range AllSourceStates {
		v.SourceStates = append(v.SourceStates, SourceStateHealth{State: s, Health: s.Health()})
	}
	return v
}
