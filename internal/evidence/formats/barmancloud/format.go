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

// Package barmancloud owns Barman Cloud scope layout, backup catalog semantics,
// WAL-name continuity, timeline ancestry, and observed recovery coverage.
package barmancloud

import (
	"errors"
	"slices"
	"strings"
	"unicode"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/repository"
)

// Format is the Barman Cloud repository format.
type Format struct{}

// New returns the Barman Cloud format.
func New() Format { return Format{} }

// Descriptor implements repository.Format; scopes are servers.
func (Format) Descriptor() repository.Descriptor {
	return repository.Descriptor{
		ID: "barman-cloud", DisplayName: "Barman Cloud", ScopeKind: "server",
		Capabilities: capabilities(),
	}
}

// NewScopeMatcher implements repository.Format. It maps keys under
// <server>/base/ and <server>/wals/ to their server; when configured is not empty, only those
// servers match. It rejects invalid or duplicate names.
func (Format) NewScopeMatcher(configured []string) (repository.ScopeMatcher, error) {
	set := make(map[string]struct{}, len(configured))
	for _, name := range configured {
		if !validDiscoveredName(name) {
			return nil, errors.New("barman-cloud scope matcher: invalid configured server")
		}
		if _, exists := set[name]; exists {
			return nil, errors.New("barman-cloud scope matcher: duplicate configured server")
		}
		set[name] = struct{}{}
	}
	return &matcher{configured: slices.Clone(configured), configuredSet: set}, nil
}

type matcher struct {
	configured    []string
	configuredSet map[string]struct{}
}

func (m *matcher) InitialScopes() []string { return slices.Clone(m.configured) }

func (m *matcher) Match(key string) (repository.ScopeMatch, bool) {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) < 2 || !validDiscoveredName(parts[0]) {
		return repository.ScopeMatch{}, false
	}
	_, explicitlyConfigured := m.configuredSet[parts[0]]
	recognized := len(parts) == 3 && parts[2] != "" && (parts[1] == "base" || parts[1] == "wals")
	if len(m.configured) > 0 {
		if !explicitlyConfigured {
			return repository.ScopeMatch{}, false
		}
		return repository.ScopeMatch{Name: parts[0], Recognized: recognized}, true
	}
	if !recognized {
		return repository.ScopeMatch{}, false
	}
	return repository.ScopeMatch{Name: parts[0], Recognized: true}, true
}

func capabilities() []evidence.CapabilityID {
	return []evidence.CapabilityID{
		evidence.ObjectInventory,
		evidence.CatalogListing,
		evidence.StructuralValidation,
		evidence.DependencyValidation,
		evidence.WALContinuity,
		evidence.TimelineTraversal,
		evidence.EncryptedMetadata,
		evidence.RecoveryCoverage,
		evidence.RetentionExpectation,
	}
}

func validDiscoveredName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 128 &&
		!strings.ContainsAny(name, "/\\") && strings.IndexFunc(name, unicode.IsControl) < 0
}

var _ repository.Format = Format{}
