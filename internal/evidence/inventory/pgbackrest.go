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
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// MaxPGBackRestStanzas caps the stanzas the viewer analyses in one scan;
// past it the snapshot is unknown rather than partial.
const MaxPGBackRestStanzas = 64

// PGBackRestOptions turns on the pgBackRest analysis. With a Stanza the
// scanner analyses that stanza only (the agent: its backups under
// backup/<Stanza>/ and its WAL under archive/<ArchiveStanza>/); without one
// it analyses every stanza it lists (the viewer).
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

// pgbackrestBuilder streams the listing into one collector per stanza.
type pgbackrestBuilder struct {
	options    PGBackRestOptions
	previous   map[string]pgbackrest.Report
	generation uint64
	collectors map[string]*pgbackrest.Collector
	truncated  bool
}

func newPGBackRestBuilder(options PGBackRestOptions, previous []pgbackrest.Report, generation uint64) *pgbackrestBuilder {
	builder := &pgbackrestBuilder{
		options: options, generation: generation,
		previous:   make(map[string]pgbackrest.Report, len(previous)),
		collectors: make(map[string]*pgbackrest.Collector),
	}
	for _, report := range previous {
		builder.previous[report.Stanza] = report
	}
	if options.Stanza != "" {
		builder.collectors[options.Stanza] = builder.collector(options.Stanza, options.ArchiveStanza)
	}
	return builder
}

func (b *pgbackrestBuilder) collector(stanza, archiveStanza string) *pgbackrest.Collector {
	options := pgbackrest.Options{
		Stanza: stanza, ArchiveStanza: archiveStanza, Passphrase: b.options.Passphrase,
		MaxManifests: b.options.MaxManifests, Generation: b.generation,
	}
	if previous, ok := b.previous[stanza]; ok {
		options.Previous = &previous
	}
	return pgbackrest.NewCollector(options)
}

// add folds one matched object in. The agent's single collector sees every
// object and keeps its two stanza directories; the viewer opens a collector
// the first time a stanza appears.
func (b *pgbackrestBuilder) add(object store.Object, scope string) {
	if b.options.Stanza != "" {
		b.collectors[b.options.Stanza].Add(object)
		return
	}
	if scope == "" {
		return
	}
	collector, ok := b.collectors[scope]
	if !ok {
		if len(b.collectors) >= MaxPGBackRestStanzas {
			b.truncated = true
			return
		}
		collector = b.collector(scope, "")
		b.collectors[scope] = collector
	}
	collector.Add(object)
}

// analyze reads every collected stanza and returns their reports sorted by
// stanza. A split archive stanza gets its StanzaSplit finding here, and
// each stanza its comparison with the configured retention.
func (b *pgbackrestBuilder) analyze(ctx context.Context, reader store.Reader) []pgbackrest.Report {
	if b.truncated {
		return nil
	}
	stanzas := make([]string, 0, len(b.collectors))
	for stanza := range b.collectors {
		stanzas = append(stanzas, stanza)
	}
	slices.Sort(stanzas)
	reports := make([]pgbackrest.Report, 0, len(stanzas))
	for _, stanza := range stanzas {
		if ctx.Err() != nil {
			return nil
		}
		report := b.collectors[stanza].Analyze(ctx, reader)
		if report.ArchiveStanza != report.Stanza {
			report.AddFinding(pgbackrest.StanzaSplitFinding(report.Stanza, report.ArchiveStanza))
		}
		if b.options.Retention != nil {
			var previous *pgbackrest.Report
			if value, ok := b.previous[stanza]; ok {
				previous = &value
			}
			report.ApplyRetention(*b.options.Retention, previous)
		}
		reports = append(reports, report)
	}
	return reports
}

func validatePGBackRestReports(reports []pgbackrest.Report) error {
	if len(reports) > MaxPGBackRestStanzas {
		return errors.New("inventory snapshot exceeds the pgBackRest stanza limit")
	}
	for index, report := range reports {
		if report.Stanza == "" || (index > 0 && reports[index-1].Stanza >= report.Stanza) {
			return errors.New("pgBackRest reports are not uniquely sorted by stanza")
		}
	}
	return nil
}

// applyPGBackRestEvidence sets the capabilities and the snapshot state from
// the reports' aspects, the most conservative stanza winning each.
func applyPGBackRestEvidence(snapshot *Snapshot, truncated bool) {
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
		switch {
		case truncated:
			reason = "too many pgBackRest stanzas to analyse"
		case len(aspects) == 1:
			aspect := slot.aspect(aspects[0])
			state, reason = aspect.State, aspect.Reason
		case len(aspects) > 1:
			state = evidence.Healthy
			for _, value := range aspects {
				state = pgbackrest.Conservative(state, slot.aspect(value).State)
			}
			reason = "evaluated across " + strconv.Itoa(len(aspects)) + " stanzas"
			if state != evidence.Healthy {
				index := slices.IndexFunc(aspects, func(value pgbackrest.Aspects) bool { return slot.aspect(value).State == state })
				reason = snapshot.PGBackRest[index].Stanza + ": " + slot.aspect(aspects[index]).Reason
			}
		}
		setCapabilityState(snapshot, slot.id, state, boundReason(reason))
	}
	state, reason := evidence.Unknown, "no pgBackRest stanza observed"
	switch {
	case truncated:
		reason = "too many pgBackRest stanzas to analyse"
	case len(aspects) > 0:
		state = evidence.Healthy
		for _, value := range aspects {
			state = pgbackrest.Conservative(state, value.Overall.State)
		}
		index := slices.IndexFunc(aspects, func(value pgbackrest.Aspects) bool { return value.Overall.State == state })
		reason = aspects[index].Overall.Reason
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
