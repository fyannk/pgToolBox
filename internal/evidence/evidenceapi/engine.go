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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Paging limits, re-exported from evidencev1alpha1 where each is documented.
const (
	DefaultPageSize    = evidencev1alpha1.DefaultPageSize
	MaximumPageSize    = evidencev1alpha1.MaximumPageSize
	MaximumCursorBytes = evidencev1alpha1.MaximumCursorBytes
	cursorKeyBytes     = 32
	cursorVersion      = 1
)

var (
	// ErrInvalidRequest means the page request or its cursor is malformed,
	// out of bounds or does not belong to the requested collection.
	ErrInvalidRequest = errors.New("invalid evidence page request")
	// ErrPublicationChanged means the request names a revision that is no
	// longer the current publication.
	ErrPublicationChanged = errors.New("evidence publication changed")
	// ErrInvalidPublication means no valid publication is available to serve
	// or a publication was rejected.
	ErrInvalidPublication = errors.New("invalid evidence publication")
)

type collectionRoute string

const (
	backupRoute       collectionRoute = "backups"
	walRangeRoute     collectionRoute = "wal-ranges"
	walGapRoute       collectionRoute = "wal-gaps"
	recoveryPathRoute collectionRoute = "recovery-paths"
)

// EngineOptions contains the credential-free projection inputs and the first
// immutable inventory snapshot. CursorEntropy is injectable only so tests can
// prove deterministic and restart-invalidated cursors; production uses
// crypto/rand.Reader. An empty Projection.ProducerInstance is always drawn
// from crypto/rand.Reader, so two engines never share one.
type EngineOptions struct {
	Projection    Options
	Initial       inventory.Snapshot
	CursorEntropy io.Reader
}

// PageRequest binds one page to the revision observed from /snapshot. A zero
// limit selects DefaultPageSize. Cursor is empty only for the first page.
type PageRequest struct {
	Revision uint64
	Limit    int
	Cursor   string
}

// Engine atomically publishes one projected snapshot and all of its typed
// collections. It owns no provider or HTTP behavior.
type Engine struct {
	projection Options
	cursorKey  [cursorKeyBytes]byte
	publishMu  sync.Mutex
	current    atomic.Pointer[enginePublication]
}

type enginePublication struct {
	value Publication
}

type cursorPayload struct {
	Version            uint64          `json:"version"`
	Route              collectionRoute `json:"route"`
	Revision           uint64          `json:"revision"`
	EvidenceGeneration uint64          `json:"evidence_generation"`
	Position           uint64          `json:"sort_position"`
	Limit              uint64          `json:"limit"`
}

// NewEngine creates a process-local cursor key and publishes the initial
// snapshot before returning.
func NewEngine(options EngineOptions) (*Engine, error) {
	entropy := options.CursorEntropy
	if entropy == nil {
		entropy = rand.Reader
	}
	engine := &Engine{projection: cloneOptions(options.Projection)}
	if _, err := io.ReadFull(entropy, engine.cursorKey[:]); err != nil {
		return nil, fmt.Errorf("initialize evidence cursor key: %w", err)
	}
	if engine.projection.ProducerInstance == "" {
		// Revision and generation restart with the process; the instance lets
		// a consumer tell two processes' equal generation numbers apart.
		var instance [evidencev1alpha1.ProducerInstanceLength / 2]byte
		if _, err := io.ReadFull(rand.Reader, instance[:]); err != nil {
			return nil, fmt.Errorf("initialize evidence producer instance: %w", err)
		}
		engine.projection.ProducerInstance = hex.EncodeToString(instance[:])
	}
	if err := engine.Publish(options.Initial); err != nil {
		return nil, err
	}
	return engine, nil
}

// Publish projects and validates one complete publication before atomically
// replacing the current value. A failed, conflicting, or regressive publish
// leaves the current publication untouched.
func (e *Engine) Publish(source inventory.Snapshot) error {
	projected, err := Project(source, e.projection)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidPublication, err)
	}
	projected, err = clonePublication(projected)
	if err != nil {
		return err
	}

	e.publishMu.Lock()
	defer e.publishMu.Unlock()
	current := e.current.Load()
	if current != nil {
		currentRevision := current.value.revision()
		newRevision := projected.revision()
		if newRevision < currentRevision {
			return ErrInvalidPublication
		}
		if newRevision == currentRevision {
			if reflect.DeepEqual(current.value, projected) {
				return nil
			}
			return ErrInvalidPublication
		}
	}
	e.current.Store(&enginePublication{value: projected})
	return nil
}

// Snapshot returns a mutation-isolated copy of the current snapshot resource.
func (e *Engine) Snapshot() evidencev1alpha1.RepositoryEvidenceSnapshot {
	publication := e.current.Load()
	if publication == nil {
		return evidencev1alpha1.RepositoryEvidenceSnapshot{}
	}
	return cloneSnapshot(publication.value.Snapshot)
}

// Backups returns one generation-consistent backup page.
func (e *Engine) Backups(request PageRequest) (evidencev1alpha1.BarmanBackupPage, error) {
	publication, start, limit, err := e.page(publicationRequest{request: request, route: backupRoute})
	if err != nil {
		return evidencev1alpha1.BarmanBackupPage{}, err
	}
	end := min(start+limit, len(publication.Backups))
	header, err := e.pageHeader(publication, backupRoute, end, limit, len(publication.Backups))
	if err != nil {
		return evidencev1alpha1.BarmanBackupPage{}, err
	}
	page := evidencev1alpha1.BarmanBackupPage{
		PageHeader: header,
		Items:      cloneBackups(publication.Backups[start:end]),
	}
	if err := page.Validate(); err != nil {
		return evidencev1alpha1.BarmanBackupPage{}, ErrInvalidPublication
	}
	return page, nil
}

// WALRanges returns one generation-consistent compact WAL-range page.
func (e *Engine) WALRanges(request PageRequest) (evidencev1alpha1.BarmanWALRangePage, error) {
	publication, start, limit, err := e.page(publicationRequest{request: request, route: walRangeRoute})
	if err != nil {
		return evidencev1alpha1.BarmanWALRangePage{}, err
	}
	end := min(start+limit, len(publication.WALRanges))
	header, err := e.pageHeader(publication, walRangeRoute, end, limit, len(publication.WALRanges))
	if err != nil {
		return evidencev1alpha1.BarmanWALRangePage{}, err
	}
	page := evidencev1alpha1.BarmanWALRangePage{
		PageHeader: header,
		Items:      cloneWALRanges(publication.WALRanges[start:end]),
	}
	if err := page.Validate(); err != nil {
		return evidencev1alpha1.BarmanWALRangePage{}, ErrInvalidPublication
	}
	return page, nil
}

// WALGaps returns one generation-consistent WAL-gap page.
func (e *Engine) WALGaps(request PageRequest) (evidencev1alpha1.BarmanWALGapPage, error) {
	publication, start, limit, err := e.page(publicationRequest{request: request, route: walGapRoute})
	if err != nil {
		return evidencev1alpha1.BarmanWALGapPage{}, err
	}
	end := min(start+limit, len(publication.WALGaps))
	header, err := e.pageHeader(publication, walGapRoute, end, limit, len(publication.WALGaps))
	if err != nil {
		return evidencev1alpha1.BarmanWALGapPage{}, err
	}
	page := evidencev1alpha1.BarmanWALGapPage{
		PageHeader: header,
		Items:      cloneWALGaps(publication.WALGaps[start:end]),
	}
	if err := page.Validate(); err != nil {
		return evidencev1alpha1.BarmanWALGapPage{}, ErrInvalidPublication
	}
	return page, nil
}

// RecoveryPaths returns one generation-consistent observed-coverage page.
func (e *Engine) RecoveryPaths(request PageRequest) (evidencev1alpha1.BarmanRecoveryPathPage, error) {
	publication, start, limit, err := e.page(publicationRequest{request: request, route: recoveryPathRoute})
	if err != nil {
		return evidencev1alpha1.BarmanRecoveryPathPage{}, err
	}
	end := min(start+limit, len(publication.RecoveryPaths))
	header, err := e.pageHeader(publication, recoveryPathRoute, end, limit, len(publication.RecoveryPaths))
	if err != nil {
		return evidencev1alpha1.BarmanRecoveryPathPage{}, err
	}
	page := evidencev1alpha1.BarmanRecoveryPathPage{
		PageHeader: header,
		Items:      cloneRecoveryPaths(publication.RecoveryPaths[start:end]),
	}
	if err := page.Validate(); err != nil {
		return evidencev1alpha1.BarmanRecoveryPathPage{}, ErrInvalidPublication
	}
	return page, nil
}

type publicationRequest struct {
	request PageRequest
	route   collectionRoute
	// native selects the pgBackRest publication: a Barman route of a
	// pgBackRest publication, or the reverse, is an invalid publication.
	native bool
}

// revision and evidenceGeneration read the publication's snapshot, whichever
// profile it is.
func (p Publication) revision() uint64 {
	if p.PGBackRest != nil {
		return p.PGBackRest.Snapshot.Revision
	}
	return p.Snapshot.Revision
}

func (p Publication) evidenceGeneration() uint64 {
	if p.PGBackRest != nil {
		return p.PGBackRest.Snapshot.EvidenceGeneration
	}
	return p.Snapshot.EvidenceGeneration
}

func (e *Engine) page(input publicationRequest) (Publication, int, int, error) {
	request := input.request
	if request.Revision == 0 || len(request.Cursor) > MaximumCursorBytes || request.Limit < 0 || request.Limit > MaximumPageSize {
		return Publication{}, 0, 0, ErrInvalidRequest
	}
	limit := request.Limit
	if limit == 0 {
		limit = DefaultPageSize
	}
	stored := e.current.Load()
	if stored == nil {
		return Publication{}, 0, 0, ErrInvalidPublication
	}
	publication := stored.value
	if (publication.PGBackRest != nil) != input.native {
		return Publication{}, 0, 0, ErrInvalidPublication
	}
	if request.Revision != publication.revision() {
		return Publication{}, 0, 0, ErrPublicationChanged
	}
	if publication.evidenceGeneration() == 0 {
		return Publication{}, 0, 0, ErrInvalidRequest
	}
	start := 0
	// #nosec G115 -- limit is validated to 1..maxPageItems, the collection
	// length is a slice length, and the guard rejects any Position at or
	// beyond it, so every conversion below is of a bounded non-negative value.
	if request.Cursor != "" {
		payload, err := e.decodeCursor(request.Cursor)
		if err != nil || payload.Version != cursorVersion || payload.Route != input.route || payload.Revision != request.Revision || payload.EvidenceGeneration != publication.evidenceGeneration() || payload.Limit != uint64(limit) || payload.Position == 0 || payload.Position >= uint64(maxCollectionLength(publication, input.route)) {
			return Publication{}, 0, 0, ErrInvalidRequest
		}
		// #nosec G115 -- the guard above rejects any Position at or beyond the
		// collection length, so the value is a valid slice index.
		start = int(payload.Position)
	}
	return publication, start, limit, nil
}

func (e *Engine) pageHeader(publication Publication, route collectionRoute, end, limit, total int) (evidencev1alpha1.PageHeader, error) {
	// #nosec G115 -- total, end, and limit are non-negative bounded page
	// arithmetic over a slice length.
	totalItems := uint64(total)
	var nextCursor *string
	if end < total {
		// #nosec G115 -- end and limit are non-negative bounded page arithmetic
		// over a slice length.
		encoded, err := e.encodeCursor(cursorPayload{
			Version: cursorVersion, Route: route,
			Revision: publication.revision(), EvidenceGeneration: publication.evidenceGeneration(),
			Position: uint64(end), Limit: uint64(limit),
		})
		if err != nil {
			return evidencev1alpha1.PageHeader{}, fmt.Errorf("%w: encode cursor: %w", ErrInvalidPublication, err)
		}
		if len(encoded) > MaximumCursorBytes {
			return evidencev1alpha1.PageHeader{}, ErrInvalidPublication
		}
		nextCursor = &encoded
	}
	return evidencev1alpha1.PageHeader{
		APIVersion: evidencev1alpha1.APIVersion,
		Kind:       collectionKind(route), Revision: publication.revision(),
		EvidenceGeneration: publication.evidenceGeneration(),
		TotalItems:         &totalItems, NextCursor: nextCursor,
	}, nil
}

func (e *Engine) encodeCursor(payload cursorPayload) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signature := hmac.New(sha256.New, e.cursorKey[:])
	_, _ = signature.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(signature.Sum(nil)), nil
}

func (e *Engine) decodeCursor(cursor string) (cursorPayload, error) {
	payloadPart, signaturePart, ok := strings.Cut(cursor, ".")
	if !ok || payloadPart == "" || signaturePart == "" || strings.Contains(signaturePart, ".") {
		return cursorPayload{}, ErrInvalidRequest
	}
	data, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return cursorPayload{}, ErrInvalidRequest
	}
	provided, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return cursorPayload{}, ErrInvalidRequest
	}
	expected := hmac.New(sha256.New, e.cursorKey[:])
	_, _ = expected.Write(data)
	if !hmac.Equal(provided, expected.Sum(nil)) {
		return cursorPayload{}, ErrInvalidRequest
	}
	var payload cursorPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return cursorPayload{}, ErrInvalidRequest
	}
	return payload, nil
}

func collectionKind(route collectionRoute) string {
	switch route {
	case backupRoute:
		return evidencev1alpha1.BarmanBackupPageKind
	case walRangeRoute:
		return evidencev1alpha1.BarmanWALRangePageKind
	case walGapRoute:
		return evidencev1alpha1.BarmanWALGapPageKind
	case recoveryPathRoute:
		return evidencev1alpha1.BarmanRecoveryPageKind
	default:
		panic("unknown evidence collection route")
	}
}

func maxCollectionLength(publication Publication, route collectionRoute) int {
	if native := publication.PGBackRest; native != nil {
		switch route {
		case backupRoute:
			return len(native.Backups)
		case walRangeRoute:
			return len(native.WALRanges)
		case walGapRoute:
			return len(native.WALGaps)
		case recoveryPathRoute:
			return len(native.RecoveryPaths)
		default:
			return 0
		}
	}
	switch route {
	case backupRoute:
		return len(publication.Backups)
	case walRangeRoute:
		return len(publication.WALRanges)
	case walGapRoute:
		return len(publication.WALGaps)
	case recoveryPathRoute:
		return len(publication.RecoveryPaths)
	default:
		return 0
	}
}

func cloneOptions(options Options) Options {
	options.ClusterName = clonePointer(options.ClusterName)
	return options
}

func clonePublication(publication Publication) (Publication, error) {
	publication.Snapshot = cloneSnapshot(publication.Snapshot)
	publication.Backups = cloneBackups(publication.Backups)
	publication.WALRanges = cloneWALRanges(publication.WALRanges)
	publication.WALGaps = cloneWALGaps(publication.WALGaps)
	publication.RecoveryPaths = cloneRecoveryPaths(publication.RecoveryPaths)
	if publication.PGBackRest != nil {
		clone, err := clonePGBackRestPublication(publication.PGBackRest)
		if err != nil {
			return Publication{}, err
		}
		publication.PGBackRest = clone
	}
	return publication, nil
}

func cloneSnapshot(snapshot evidencev1alpha1.RepositoryEvidenceSnapshot) evidencev1alpha1.RepositoryEvidenceSnapshot {
	snapshot.Identity.Cluster.Name = clonePointer(snapshot.Identity.Cluster.Name)
	snapshot.StartedAt = clonePointer(snapshot.StartedAt)
	snapshot.CompletedAt = clonePointer(snapshot.CompletedAt)
	snapshot.LastAttemptAt = clonePointer(snapshot.LastAttemptAt)
	snapshot.Capabilities = slices.Clone(snapshot.Capabilities)
	snapshot.Producer.Features = slices.Clone(snapshot.Producer.Features)
	snapshot.Inventory.ObjectCount = clonePointer(snapshot.Inventory.ObjectCount)
	snapshot.Inventory.StoredBytes = clonePointer(snapshot.Inventory.StoredBytes)
	snapshot.Inventory.UnscopedObjectCount = clonePointer(snapshot.Inventory.UnscopedObjectCount)
	snapshot.Inventory.LastFailureCategory = clonePointer(snapshot.Inventory.LastFailureCategory)
	if snapshot.Details.BarmanCloud != nil {
		details := *snapshot.Details.BarmanCloud
		details.BackupItems = clonePointer(details.BackupItems)
		details.WALRangeItems = clonePointer(details.WALRangeItems)
		details.WALGapItems = clonePointer(details.WALGapItems)
		details.RecoveryPathItems = clonePointer(details.RecoveryPathItems)
		details.StructurallyUsableBackups = clonePointer(details.StructurallyUsableBackups)
		details.BackupStates = clonePointer(details.BackupStates)
		details.WALCounts = clonePointer(details.WALCounts)
		details.LatestArchiveReceiptAt = clonePointer(details.LatestArchiveReceiptAt)
		details.Retention.VisibleBackups = clonePointer(details.Retention.VisibleBackups)
		details.Retention.StructurallyUsableBackups = clonePointer(details.Retention.StructurallyUsableBackups)
		details.Retention.OldestCompletionAt = clonePointer(details.Retention.OldestCompletionAt)
		details.Retention.NewestCompletionAt = clonePointer(details.Retention.NewestCompletionAt)
		details.Retention.MinimumRedundancy = clonePointer(details.Retention.MinimumRedundancy)
		snapshot.Details.BarmanCloud = &details
	}
	return snapshot
}

func cloneBackups(values []evidencev1alpha1.BarmanBackup) []evidencev1alpha1.BarmanBackup {
	result := slices.Clone(values)
	for index := range result {
		item := &result[index]
		item.Status = clonePointer(item.Status)
		item.BackupType = clonePointer(item.BackupType)
		item.SystemID = clonePointer(item.SystemID)
		item.PostgreSQLVersion = clonePointer(item.PostgreSQLVersion)
		item.Timeline = clonePointer(item.Timeline)
		item.WALSegmentSizeBytes = clonePointer(item.WALSegmentSizeBytes)
		item.BeginWAL = clonePointer(item.BeginWAL)
		item.EndWAL = clonePointer(item.EndWAL)
		item.BeginLSN = clonePointer(item.BeginLSN)
		item.EndLSN = clonePointer(item.EndLSN)
		item.BeginAt = clonePointer(item.BeginAt)
		item.EndAt = clonePointer(item.EndAt)
		item.LogicalBytes = clonePointer(item.LogicalBytes)
		item.DeduplicatedBytes = clonePointer(item.DeduplicatedBytes)
		item.StoredArtifactBytes = clonePointer(item.StoredArtifactBytes)
		item.Compression = clonePointer(item.Compression)
		item.Encryption = clonePointer(item.Encryption)
		item.ArtifactCount = clonePointer(item.ArtifactCount)
		item.TablespaceCount = clonePointer(item.TablespaceCount)
	}
	return result
}

func cloneWALRanges(values []evidencev1alpha1.BarmanWALRange) []evidencev1alpha1.BarmanWALRange {
	result := slices.Clone(values)
	for index := range result {
		result[index].LatestReceiptAt = clonePointer(result[index].LatestReceiptAt)
		result[index].EndReceiptAt = clonePointer(result[index].EndReceiptAt)
	}
	return result
}

func cloneWALGaps(values []evidencev1alpha1.BarmanWALGap) []evidencev1alpha1.BarmanWALGap {
	return slices.Clone(values)
}

func cloneRecoveryPaths(values []evidencev1alpha1.BarmanRecoveryPath) []evidencev1alpha1.BarmanRecoveryPath {
	result := slices.Clone(values)
	for index := range result {
		item := &result[index]
		item.LowerBoundAt = clonePointer(item.LowerBoundAt)
		item.StartTimeline = clonePointer(item.StartTimeline)
		item.StartPosition = clonePointer(item.StartPosition)
		item.StartWAL = clonePointer(item.StartWAL)
		item.FrontierTimeline = clonePointer(item.FrontierTimeline)
		item.FrontierPosition = clonePointer(item.FrontierPosition)
		item.FrontierWAL = clonePointer(item.FrontierWAL)
		item.FrontierReceiptAt = clonePointer(item.FrontierReceiptAt)
		item.AssumptionCodes = slices.Clone(item.AssumptionCodes)
	}
	return result
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
