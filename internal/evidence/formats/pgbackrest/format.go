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

// Package pgbackrest owns the pgBackRest repository format (2.60.0,
// repository format 5): the stanza layout, the strict INI info files and their
// checksums, format-5 encryption, backup.info, archive.info, the summary
// sections of backup.manifest, WAL names and timeline history files, and the
// analysis that joins them into chains, WAL continuity and a recovery window.
// It never reads backup data files or WAL segment content, and runs no
// pgBackRest binary.
package pgbackrest

import (
	"errors"
	"slices"
	"strings"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/repository"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// FormatID and ScopeKind name the format and its scopes in evidence.
const (
	FormatID  = "pgbackrest"
	ScopeKind = "stanza"
)

// Format is the pgBackRest repository format.
type Format struct{}

// New returns the pgBackRest format.
func New() Format { return Format{} }

// Descriptor implements repository.Format; scopes are stanzas.
func (Format) Descriptor() repository.Descriptor {
	return repository.Descriptor{
		ID: FormatID, DisplayName: "pgBackRest", ScopeKind: ScopeKind,
		Capabilities: capabilities(),
	}
}

// NewScopeMatcher implements repository.Format. It maps keys under
// backup/<stanza>/ and archive/<stanza>/ to their stanza; when configured is not empty, only those
// stanzas match. It rejects invalid or duplicate names.
func (Format) NewScopeMatcher(configured []string) (repository.ScopeMatcher, error) {
	set := make(map[string]struct{}, len(configured))
	for _, name := range configured {
		if !evidencev1alpha1.ValidPGBackRestStanza(name) {
			return nil, errors.New("pgbackrest scope matcher: invalid configured stanza")
		}
		if _, exists := set[name]; exists {
			return nil, errors.New("pgbackrest scope matcher: duplicate configured stanza")
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
	if len(parts) != 3 || parts[2] == "" || (parts[0] != "backup" && parts[0] != "archive") || !evidencev1alpha1.ValidPGBackRestStanza(parts[1]) {
		return repository.ScopeMatch{}, false
	}
	if len(m.configured) > 0 {
		if _, ok := m.configuredSet[parts[1]]; !ok {
			return repository.ScopeMatch{}, false
		}
	}
	return repository.ScopeMatch{Name: parts[1], Recognized: true}, true
}

// capabilities lists what this package implements: inventory (the scope
// matcher), the catalog (backup.info), structural validation (checksums,
// .copy agreement, manifests), dependencies (prior and references), WAL
// continuity and timeline traversal (names and history files), encrypted
// metadata (format 5), recovery coverage (the window) and the comparison
// with the configured retention (retention.go).
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

var _ repository.Format = Format{}
