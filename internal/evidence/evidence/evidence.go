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

// Package evidence contains only facts with identical meaning across every
// repository format. Format-native backup models remain in their modules.
package evidence

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// State is the normative four-state evidence vocabulary.
type State = evidencev1alpha1.State

// The evidence states, re-exported from evidencev1alpha1 where each is
// documented.
const (
	Healthy   = evidencev1alpha1.Healthy
	Warning   = evidencev1alpha1.Warning
	Unhealthy = evidencev1alpha1.Unhealthy
	Unknown   = evidencev1alpha1.Unknown
)

// Support states whether a configured format has proven a capability.
type Support = evidencev1alpha1.Support

// The capability support values, re-exported from evidencev1alpha1 where
// each is documented.
const (
	Supported      = evidencev1alpha1.Supported
	Unsupported    = evidencev1alpha1.Unsupported
	SupportUnknown = evidencev1alpha1.SupportUnknown
)

// CapabilityID identifies format-neutral evidence operations only.
type CapabilityID = evidencev1alpha1.CapabilityID

// The capability identifiers, re-exported from evidencev1alpha1 where each is
// documented.
const (
	ObjectInventory      = evidencev1alpha1.ObjectInventory
	CatalogListing       = evidencev1alpha1.CatalogListing
	StructuralValidation = evidencev1alpha1.StructuralValidation
	DependencyValidation = evidencev1alpha1.DependencyValidation
	WALContinuity        = evidencev1alpha1.WALContinuity
	TimelineTraversal    = evidencev1alpha1.TimelineTraversal
	EncryptedMetadata    = evidencev1alpha1.EncryptedMetadata
	RecoveryCoverage     = evidencev1alpha1.RecoveryCoverage
	RetentionExpectation = evidencev1alpha1.RetentionExpectation
)

// Capability preserves both implementation support and current evidence.
type Capability struct {
	ID      CapabilityID
	Support Support
	State   State
	Reason  string
}

// Completeness records whether all required evidence was collected.
type Completeness = evidencev1alpha1.Completeness

// The completeness values, re-exported from evidencev1alpha1 where each is
// documented.
const (
	Complete   = evidencev1alpha1.Complete
	Incomplete = evidencev1alpha1.Incomplete
	Unscanned  = evidencev1alpha1.Unscanned
)

// Scope uses format-native kind/name without imposing a Barman server shape.
type Scope struct {
	Kind string
	Name string
}

// Snapshot is the conservative common envelope rendered by shared HTTP code.
type Snapshot struct {
	RepositoryFormat string
	Compatibility    State
	Scope            Scope
	Generation       uint64
	StartedAt        time.Time
	CompletedAt      time.Time
	Completeness     Completeness
	Stale            bool
	State            State
	Reason           string
	Capabilities     []Capability
}

// InitialSnapshot is explicitly unknown until an atomic complete scan exists.
func InitialSnapshot(repositoryFormat, scopeKind string, capabilities []CapabilityID) Snapshot {
	result := Snapshot{
		RepositoryFormat: repositoryFormat,
		Compatibility:    Unknown,
		Scope:            Scope{Kind: scopeKind},
		Completeness:     Unscanned,
		State:            Unknown,
		Reason:           "no completed scan",
		Capabilities:     make([]Capability, 0, len(capabilities)),
	}
	for _, capability := range capabilities {
		result.Capabilities = append(result.Capabilities, Capability{
			ID: capability, Support: SupportUnknown, State: Unknown, Reason: "no completed scan",
		})
	}
	slices.SortFunc(result.Capabilities, func(a, b Capability) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return result
}

// Validate rejects envelopes that could overstate incomplete evidence.
func (s Snapshot) Validate() error {
	if invalidText(s.RepositoryFormat, 128) || invalidText(s.Scope.Kind, 128) || len(s.Scope.Name) > 256 || strings.IndexFunc(s.Scope.Name, unicode.IsControl) >= 0 {
		return errors.New("evidence envelope requires format and scope kind")
	}
	if !validState(s.State) || !validState(s.Compatibility) {
		return errors.New("evidence envelope contains an invalid state")
	}
	if s.Completeness != Complete && s.Completeness != Incomplete && s.Completeness != Unscanned {
		return errors.New("evidence envelope contains invalid completeness")
	}
	if s.Completeness != Complete && s.State == Healthy {
		return errors.New("incomplete evidence cannot be healthy")
	}
	if s.Completeness == Unscanned && s.State != Unknown {
		return errors.New("unscanned evidence must be unknown")
	}
	if invalidText(s.Reason, 256) {
		return errors.New("evidence envelope reason must be bounded")
	}
	seen := make(map[CapabilityID]struct{}, len(s.Capabilities))
	for _, capability := range s.Capabilities {
		if invalidText(string(capability.ID), 128) || !validState(capability.State) || !validSupport(capability.Support) || invalidText(capability.Reason, 256) {
			return errors.New("evidence envelope contains an invalid capability")
		}
		if capability.Support != Supported && capability.State != Unknown {
			return fmt.Errorf("capability %s must be unknown without proven support", capability.ID)
		}
		if _, ok := seen[capability.ID]; ok {
			return fmt.Errorf("duplicate capability %s", capability.ID)
		}
		seen[capability.ID] = struct{}{}
	}
	return nil
}

func validState(state State) bool {
	return state == Healthy || state == Warning || state == Unhealthy || state == Unknown
}

func validSupport(support Support) bool {
	return support == Supported || support == Unsupported || support == SupportUnknown
}

func invalidText(value string, maximum int) bool {
	return value == "" || len(value) > maximum || strings.IndexFunc(value, unicode.IsControl) >= 0
}
