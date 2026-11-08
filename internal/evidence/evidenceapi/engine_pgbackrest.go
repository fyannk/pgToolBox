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

package evidenceapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// PGBackRestSnapshot returns the current pgBackRest snapshot stamped with
// epoch; ok is false when the current publication is not pgBackRest's.
func (e *Engine) PGBackRestSnapshot(epoch uint64) (evidencev1alpha1.RepositoryEvidenceSnapshot, bool, error) {
	stored := e.current.Load()
	if stored == nil || stored.value.PGBackRest == nil {
		return evidencev1alpha1.RepositoryEvidenceSnapshot{}, false, nil
	}
	// Only the snapshot is copied: the collections can be large and are
	// served page by page.
	snapshot, err := cloneJSON(stored.value.PGBackRest.Snapshot)
	if err != nil {
		return evidencev1alpha1.RepositoryEvidenceSnapshot{}, true, err
	}
	snapshot.ProducerEpoch = epoch
	if snapshot.Validate() != nil {
		return evidencev1alpha1.RepositoryEvidenceSnapshot{}, true, ErrInvalidPublication
	}
	return snapshot, true, nil
}

// PGBackRestBackups returns one generation-consistent pgBackRest backup page.
func (e *Engine) PGBackRestBackups(request PageRequest) (evidencev1alpha1.PGBackRestBackupPage, error) {
	return nativePage(e, request, backupRoute, evidencev1alpha1.PGBackRestBackupPageKind,
		func(p *PGBackRestPublication) []evidencev1alpha1.PGBackRestBackup { return p.Backups },
		func(header evidencev1alpha1.PageHeader, items []evidencev1alpha1.PGBackRestBackup) evidencev1alpha1.PGBackRestBackupPage {
			return evidencev1alpha1.PGBackRestBackupPage{PageHeader: header, Items: items}
		})
}

// PGBackRestWALRanges returns one generation-consistent pgBackRest WAL-range page.
func (e *Engine) PGBackRestWALRanges(request PageRequest) (evidencev1alpha1.PGBackRestWALRangePage, error) {
	return nativePage(e, request, walRangeRoute, evidencev1alpha1.PGBackRestWALRangePageKind,
		func(p *PGBackRestPublication) []evidencev1alpha1.PGBackRestWALRange { return p.WALRanges },
		func(header evidencev1alpha1.PageHeader, items []evidencev1alpha1.PGBackRestWALRange) evidencev1alpha1.PGBackRestWALRangePage {
			return evidencev1alpha1.PGBackRestWALRangePage{PageHeader: header, Items: items}
		})
}

// PGBackRestWALGaps returns one generation-consistent pgBackRest WAL-gap page.
func (e *Engine) PGBackRestWALGaps(request PageRequest) (evidencev1alpha1.PGBackRestWALGapPage, error) {
	return nativePage(e, request, walGapRoute, evidencev1alpha1.PGBackRestWALGapPageKind,
		func(p *PGBackRestPublication) []evidencev1alpha1.PGBackRestWALGap { return p.WALGaps },
		func(header evidencev1alpha1.PageHeader, items []evidencev1alpha1.PGBackRestWALGap) evidencev1alpha1.PGBackRestWALGapPage {
			return evidencev1alpha1.PGBackRestWALGapPage{PageHeader: header, Items: items}
		})
}

// PGBackRestRecoveryPaths returns one generation-consistent pgBackRest
// recovery-path page.
func (e *Engine) PGBackRestRecoveryPaths(request PageRequest) (evidencev1alpha1.PGBackRestRecoveryPathPage, error) {
	return nativePage(e, request, recoveryPathRoute, evidencev1alpha1.PGBackRestRecoveryPageKind,
		func(p *PGBackRestPublication) []evidencev1alpha1.PGBackRestRecoveryPath { return p.RecoveryPaths },
		func(header evidencev1alpha1.PageHeader, items []evidencev1alpha1.PGBackRestRecoveryPath) evidencev1alpha1.PGBackRestRecoveryPathPage {
			return evidencev1alpha1.PGBackRestRecoveryPathPage{PageHeader: header, Items: items}
		})
}

func nativePage[I any, P interface{ Validate() error }](e *Engine, request PageRequest, route collectionRoute, kind string, items func(*PGBackRestPublication) []I, build func(evidencev1alpha1.PageHeader, []I) P) (P, error) {
	var zero P
	publication, start, limit, err := e.page(publicationRequest{request: request, route: route, native: true})
	if err != nil {
		return zero, err
	}
	all := items(publication.PGBackRest)
	end := min(start+limit, len(all))
	header, err := e.pageHeader(publication, route, end, limit, len(all))
	if err != nil {
		return zero, err
	}
	pageItems, err := cloneJSON(all[start:end])
	if err != nil {
		return zero, err
	}
	page := build(evidencev1alpha1.PageHeader{
		APIVersion: evidencev1alpha1.APIVersion, Kind: kind, Revision: header.Revision,
		EvidenceGeneration: header.EvidenceGeneration, TotalItems: header.TotalItems, NextCursor: header.NextCursor,
	}, pageItems)
	if page.Validate() != nil {
		return zero, ErrInvalidPublication
	}
	return page, nil
}

// clonePGBackRestPublication deep-copies through the wire encoding, which
// every field round-trips (times are UTC).
func clonePGBackRestPublication(publication *PGBackRestPublication) (*PGBackRestPublication, error) {
	var (
		clone PGBackRestPublication
		err   error
	)
	if clone.Snapshot, err = cloneJSON(publication.Snapshot); err != nil {
		return nil, err
	}
	if clone.Backups, err = cloneJSON(publication.Backups); err != nil {
		return nil, err
	}
	if clone.WALRanges, err = cloneJSON(publication.WALRanges); err != nil {
		return nil, err
	}
	if clone.WALGaps, err = cloneJSON(publication.WALGaps); err != nil {
		return nil, err
	}
	if clone.RecoveryPaths, err = cloneJSON(publication.RecoveryPaths); err != nil {
		return nil, err
	}
	return &clone, nil
}

// cloneJSON deep-copies a value through its wire encoding. A value that
// does not round-trip is an invalid publication, never a panic: this runs
// on the serving path.
func cloneJSON[T any](value T) (T, error) {
	var clone T
	encoded, err := json.Marshal(value)
	if err != nil {
		return clone, fmt.Errorf("%w: encode: %w", ErrInvalidPublication, err)
	}
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return clone, fmt.Errorf("%w: decode: %w", ErrInvalidPublication, err)
	}
	return clone, nil
}

// nativeSource is the optional side of a publicationSource that serves a
// pgBackRest publication; the agent handler asks it first and falls back to
// the Barman routes when ok is false.
type nativeSource interface {
	nativeSnapshot(ctx context.Context, epoch uint64) (evidencev1alpha1.RepositoryEvidenceSnapshot, bool, error)
	nativeCollection(ctx context.Context, path string, request PageRequest) (any, bool, error)
}

func (s engineSource) nativeSnapshot(ctx context.Context, epoch uint64) (evidencev1alpha1.RepositoryEvidenceSnapshot, bool, error) {
	value, ok, err := s.engine.PGBackRestSnapshot(epoch)
	if ok && err == nil {
		err = ctx.Err()
	}
	return value, ok, err
}

func (s engineSource) nativeCollection(ctx context.Context, path string, request PageRequest) (any, bool, error) {
	stored := s.engine.current.Load()
	if stored == nil || stored.value.PGBackRest == nil {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	switch path {
	case evidencev1alpha1.BackupsPath:
		value, err := s.engine.PGBackRestBackups(request)
		return value, true, err
	case evidencev1alpha1.WALRangesPath:
		value, err := s.engine.PGBackRestWALRanges(request)
		return value, true, err
	case evidencev1alpha1.WALGapsPath:
		value, err := s.engine.PGBackRestWALGaps(request)
		return value, true, err
	case evidencev1alpha1.RecoveryPathsPath:
		value, err := s.engine.PGBackRestRecoveryPaths(request)
		return value, true, err
	default:
		return nil, true, errRouteNotFound
	}
}

var errRouteNotFound = errors.New("evidence route not found")
