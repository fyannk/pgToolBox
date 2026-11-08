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

// Package repository owns explicit format selection and conservative common
// descriptors. Rich catalogs remain in format-specific packages.
package repository

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

// Format exposes only identity and common capabilities at the shared boundary.
// Later format packages keep their typed catalog behind this interface.
type Format interface {
	Descriptor() Descriptor
	NewScopeMatcher(configured []string) (ScopeMatcher, error)
}

// ScopeMatch is the only format-owned fact needed by the shared inventory
// scanner. Recognized means that the key demonstrates the selected format's
// scope layout; it does not prove catalog compatibility or backup health.
type ScopeMatch struct {
	Name       string
	Recognized bool
}

// ScopeMatcher classifies opaque relative object keys without provider types.
type ScopeMatcher interface {
	InitialScopes() []string
	Match(key string) (ScopeMatch, bool)
}

// Descriptor uses the repository format's native scope term.
type Descriptor struct {
	ID           string
	DisplayName  string
	ScopeKind    string
	Capabilities []evidence.CapabilityID
}

// StaticFormat is sufficient for the no-scan runtime foundation.
type StaticFormat struct {
	Value Descriptor
}

// Descriptor implements Format; it returns a copy of Value.
func (f StaticFormat) Descriptor() Descriptor {
	descriptor := f.Value
	descriptor.Capabilities = slices.Clone(f.Value.Capabilities)
	return descriptor
}

// NewScopeMatcher implements Format with a matcher that reports the
// configured scopes and matches no key.
func (f StaticFormat) NewScopeMatcher(configured []string) (ScopeMatcher, error) {
	return staticMatcher{scopes: slices.Clone(configured)}, nil
}

type staticMatcher struct {
	scopes []string
}

func (m staticMatcher) InitialScopes() []string { return slices.Clone(m.scopes) }

func (staticMatcher) Match(string) (ScopeMatch, bool) { return ScopeMatch{}, false }

// Registry never detects or falls back between formats.
type Registry struct {
	formats map[string]Format
}

// NewRegistry returns a registry of formats keyed by descriptor ID; it rejects
// nil formats, incomplete descriptors and duplicate IDs.
func NewRegistry(formats ...Format) (*Registry, error) {
	registry := &Registry{formats: make(map[string]Format, len(formats))}
	for _, format := range formats {
		if format == nil {
			return nil, errors.New("repository registry: nil format")
		}
		descriptor := format.Descriptor()
		if err := validateDescriptor(descriptor); err != nil {
			return nil, err
		}
		if _, exists := registry.formats[descriptor.ID]; exists {
			return nil, fmt.Errorf("repository registry: duplicate format %q", descriptor.ID)
		}
		registry.formats[descriptor.ID] = format
	}
	return registry, nil
}

// Select performs an exact lookup. It deliberately has no detection input.
func (r *Registry) Select(id string) (Format, error) {
	format, ok := r.formats[id]
	if !ok {
		return nil, fmt.Errorf("repository format %q is not registered", id)
	}
	return format, nil
}

// IDs returns the registered format IDs in sorted order.
func (r *Registry) IDs() []string {
	result := make([]string, 0, len(r.formats))
	for id := range r.formats {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func validateDescriptor(descriptor Descriptor) error {
	if descriptor.ID == "" || descriptor.DisplayName == "" || descriptor.ScopeKind == "" {
		return errors.New("repository registry: descriptor identity is incomplete")
	}
	seen := make(map[evidence.CapabilityID]struct{}, len(descriptor.Capabilities))
	for _, capability := range descriptor.Capabilities {
		if capability == "" {
			return fmt.Errorf("repository registry: format %q has an empty capability", descriptor.ID)
		}
		if _, exists := seen[capability]; exists {
			return fmt.Errorf("repository registry: format %q repeats capability %q", descriptor.ID, capability)
		}
		seen[capability] = struct{}{}
	}
	return nil
}
