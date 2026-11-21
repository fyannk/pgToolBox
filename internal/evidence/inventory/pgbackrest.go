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

package inventory

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// PGBackRestOptions turns on the pgBackRest analysis of one stanza: its
// backups under backup/<Stanza>/ and its WAL under archive/<ArchiveStanza>/
// (Stanza when empty). Stanza is required.
type PGBackRestOptions struct {
	Stanza        string
	ArchiveStanza string
	// Passphrase opens an encrypted repository's info files and manifests.
	// It is never logged or published.
	Passphrase   []byte
	MaxManifests int
	// Retention is the repository's configured retention (the agent's
	// PGBACKREST_RETENTION_*); each stanza is compared with it. Nil leaves
	// retention unevaluated.
	Retention *pgbackrest.Retention
}

func clonePGBackRestOptions(options *PGBackRestOptions) *PGBackRestOptions {
	if options == nil {
		return nil
	}
	clone := *options
	clone.Passphrase = slices.Clone(options.Passphrase)
	if options.Retention != nil {
		retention := *options.Retention
		clone.Retention = &retention
	}
	return &clone
}

// pgbackrestBuilder streams the listing into the stanza's collector.
type pgbackrestBuilder struct {
	options    PGBackRestOptions
	previous   map[string]pgbackrest.Report
	generation uint64
	collector  *pgbackrest.Collector
}

func newPGBackRestBuilder(options PGBackRestOptions, previous []pgbackrest.Report, generation uint64) *pgbackrestBuilder {
	builder := &pgbackrestBuilder{
		options: options, generation: generation,
		previous: make(map[string]pgbackrest.Report, len(previous)),
	}
	for _, report := range previous {
		builder.previous[report.Stanza] = report
	}
	collectorOptions := pgbackrest.Options{
		Stanza: options.Stanza, ArchiveStanza: options.ArchiveStanza, Passphrase: options.Passphrase,
		MaxManifests: options.MaxManifests, Generation: generation,
	}
	if report, ok := builder.previous[options.Stanza]; ok {
		collectorOptions.Previous = &report
	}
	builder.collector = pgbackrest.NewCollector(collectorOptions)
	return builder
}

// add folds one listed object into the stanza's collector, which keeps its
// two stanza directories.
func (b *pgbackrestBuilder) add(object store.Object) {
	b.collector.Add(object)
}

// analyze reads the stanza and returns its report. A split archive stanza
// gets its StanzaSplit finding here, and the stanza its comparison with the
// configured retention.
func (b *pgbackrestBuilder) analyze(ctx context.Context, reader store.Reader) []pgbackrest.Report {
	report := b.collector.Analyze(ctx, reader)
	if ctx.Err() != nil {
		return nil
	}
	if report.ArchiveStanza != report.Stanza {
		report.AddFinding(pgbackrest.StanzaSplitFinding(report.Stanza, report.ArchiveStanza))
	}
	if b.options.Retention != nil {
		var previous *pgbackrest.Report
		if value, ok := b.previous[report.Stanza]; ok {
			previous = &value
		}
		report.ApplyRetention(*b.options.Retention, previous)
	}
	return []pgbackrest.Report{report}
}

func validatePGBackRestReports(reports []pgbackrest.Report) error {
	if len(reports) > 1 {
		return errors.New("inventory snapshot holds more than one pgBackRest stanza")
	}
	for index, report := range reports {
		if report.Stanza == "" || (index > 0 && reports[index-1].Stanza >= report.Stanza) {
			return errors.New("pgBackRest reports are not uniquely sorted by stanza")
		}
	}
	return nil
}

// applyPGBackRestEvidence sets the capabilities and the snapshot state from
// the stanza report's aspects.
func applyPGBackRestEvidence(snapshot *Snapshot) {
	type slot struct {
		id     evidence.CapabilityID
		aspect func(pgbackrest.Aspects) pgbackrest.Aspect
	}
	slots := []slot{
		{evidence.CatalogListing, func(a pgbackrest.Aspects) pgbackrest.Aspect { return a.Catalog }},
		{evidence.StructuralValidation, func(a pgbackrest.Aspects) pgbackrest.Aspect { return a.Catalog }},
		{evidence.DependencyValidation, func(a pgbackrest.Aspects) pgbackrest.Aspect { return a.Chains }},
		{evidence.WALContinuity, func(a pgbackrest.Aspects) pgbackrest.Aspect { return a.WAL }},
		{evidence.TimelineTraversal, func(a pgbackrest.Aspects) pgbackrest.Aspect { return a.Timeline }},
		{evidence.EncryptedMetadata, func(a pgbackrest.Aspects) pgbackrest.Aspect { return a.Encryption }},
		{evidence.RecoveryCoverage, func(a pgbackrest.Aspects) pgbackrest.Aspect { return a.Coverage }},
		{evidence.RetentionExpectation, func(a pgbackrest.Aspects) pgbackrest.Aspect { return a.Retention }},
	}
	aspects := make([]pgbackrest.Aspects, len(snapshot.PGBackRest))
	for index, report := range snapshot.PGBackRest {
		aspects[index] = report.Aspects()
	}
	for _, slot := range slots {
		state, reason := evidence.Unknown, "no pgBackRest stanza observed"
		if len(aspects) == 1 {
			aspect := slot.aspect(aspects[0])
			state, reason = aspect.State, aspect.Reason
		}
		setCapabilityState(snapshot, slot.id, state, boundReason(reason))
	}
	state, reason := evidence.Unknown, "no pgBackRest stanza observed"
	if len(aspects) == 1 {
		state, reason = aspects[0].Overall.State, aspects[0].Overall.Reason
	}
	snapshot.Evidence.State, snapshot.Evidence.Reason = state, reason
}

func setCapabilityState(snapshot *Snapshot, id evidence.CapabilityID, state evidence.State, reason string) {
	for index := range snapshot.Evidence.Capabilities {
		capability := &snapshot.Evidence.Capabilities[index]
		if capability.ID == id {
			capability.Support, capability.State, capability.Reason = evidence.Supported, state, reason
		}
	}
}

// boundReason keeps a capability reason within the evidence bound.
func boundReason(reason string) string {
	const limit = 240
	if len(reason) <= limit {
		return reason
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return strings.TrimSpace(reason[:cut])
}
