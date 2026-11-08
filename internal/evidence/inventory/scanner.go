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
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	"github.com/fyannk/pgToolBox/internal/evidence/redact"
	"github.com/fyannk/pgToolBox/internal/evidence/repository"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// Clock returns the current time; tests replace it to make timestamps
// deterministic.
type Clock func() time.Time

// SnapshotCache is the scanner-owned publication surface. Implementations
// validate and replace snapshots atomically and preserve the previous snapshot
// when Publish fails.
type SnapshotCache interface {
	Load() Snapshot
	Publish(Snapshot) error
}

// ScannerOptions configures NewScanner. Every field except Now,
// AnalyzeBarmanCatalog, BarmanRecoveryOptions and Redactor is required.
type ScannerOptions struct {
	Store                 store.Reader
	Format                repository.Format
	ConfiguredScopes      []string
	Cache                 SnapshotCache
	Readiness             *readiness.ProbeState
	RefreshInterval       time.Duration
	MaxObjects            int
	PageSize              int
	RecentLimit           int
	Now                   Clock
	Logger                *slog.Logger
	AnalyzeBarmanCatalog  bool
	BarmanRecoveryOptions barmancloud.RecoveryOptions
	// PGBackRest, with a pgBackRest format, analyses stanzas after each
	// complete listing; nil only inventories them.
	PGBackRest *PGBackRestOptions
	// Redactor scrubs configured secret values and credential-shaped text
	// from provider root causes before they are logged. Nil still applies
	// the generic credential patterns.
	Redactor *redact.Redactor
}

// Scanner periodically lists the whole repository, builds an inventory
// snapshot and publishes it to the cache; it also probes store reachability.
type Scanner struct {
	store                 store.Reader
	format                repository.Format
	configuredScopes      []string
	cache                 SnapshotCache
	readiness             *readiness.ProbeState
	refreshInterval       time.Duration
	maxObjects            int
	pageSize              int
	recentLimit           int
	now                   Clock
	logger                *slog.Logger
	analyzeBarmanCatalog  bool
	barmanRecoveryOptions barmancloud.RecoveryOptions
	pgbackrest            *PGBackRestOptions
	redactor              *redact.Redactor

	refreshMu  sync.Mutex
	generation atomic.Uint64
}

// NewScanner validates options and returns a scanner; it does not start it.
func NewScanner(options ScannerOptions) (*Scanner, error) {
	if options.Store == nil || options.Format == nil || options.Cache == nil || options.Readiness == nil || options.Logger == nil ||
		options.RefreshInterval <= 0 || options.MaxObjects < 1 || options.PageSize < 1 || options.PageSize > store.MaxPageObjects ||
		options.RecentLimit < 1 || options.RecentLimit > MaxRecentObjects || len(options.ConfiguredScopes) > MaxScopes {
		return nil, errors.New("invalid inventory scanner options")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Scanner{
		store: options.Store, format: options.Format, configuredScopes: slices.Clone(options.ConfiguredScopes),
		cache: options.Cache, readiness: options.Readiness, refreshInterval: options.RefreshInterval,
		maxObjects: options.MaxObjects, pageSize: options.PageSize, recentLimit: options.RecentLimit, now: now,
		logger:                options.Logger,
		analyzeBarmanCatalog:  options.AnalyzeBarmanCatalog,
		barmanRecoveryOptions: options.BarmanRecoveryOptions,
		pgbackrest:            clonePGBackRestOptions(options.PGBackRest),
		redactor:              options.Redactor,
	}, nil
}

// Run probes and refreshes once, then again every refresh interval until ctx
// is canceled.
func (s *Scanner) Run(ctx context.Context) {
	s.runCycle(ctx)
	ticker := time.NewTicker(s.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runCycle(ctx)
		}
	}
}

// maxLoggedCauseBytes bounds one logged provider root cause.
const maxLoggedCauseBytes = 512

func (s *Scanner) runCycle(ctx context.Context) {
	// Root causes are logged once per distinct cause per cycle: the probe and
	// the refresh usually fail for the same reason.
	logged := make(map[string]struct{}, 2)
	if err := s.probeError(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.logRootCause(ctx, "store probe failed", err, logged)
	}
	if err := s.Refresh(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.logger.WarnContext(ctx, "inventory refresh failed",
			slog.String("category", string(fault.Categorize(err))),
			slog.Uint64("refresh_generation", s.generation.Load()),
		)
		s.logRootCause(ctx, "inventory refresh failure cause", err, logged)
	}
}

// logRootCause logs the redacted provider or projection cause behind a
// categorized error. Categorized errors never render their cause, so this
// is the only place an operator can see, for example, a DNS or TLS failure
// that the stable vocabulary reports as "unavailable".
func (s *Scanner) logRootCause(ctx context.Context, message string, err error, logged map[string]struct{}) {
	category := fault.Categorize(err)
	cause := rootCause(err)
	if cause == "" {
		return
	}
	cause = s.redactor.String(cause)
	if len(cause) > maxLoggedCauseBytes {
		cause = strings.ToValidUTF8(cause[:maxLoggedCauseBytes], "") + "..."
	}
	// Service errors deduplicate on their stable identity (operation and
	// error code): their text carries a fresh request ID on every call.
	key := string(category) + "\x00" + cause
	var keyed interface{ CauseKey() string }
	if errors.As(err, &keyed) && keyed.CauseKey() != "" {
		key = string(category) + "\x00" + keyed.CauseKey()
	}
	if _, seen := logged[key]; seen {
		return
	}
	logged[key] = struct{}{}
	s.logger.WarnContext(ctx, message, slog.String("category", string(category)), slog.String("cause", cause))
}

// rootCause returns the retained cause text of the first error in the chain
// that carries one (provider adapters and scan errors), or "" otherwise.
func rootCause(err error) string {
	var carrier interface{ RootCause() string }
	if errors.As(err, &carrier) {
		return carrier.RootCause()
	}
	return ""
}

// Probe lists one object to check reachability, records the result in the
// readiness state and returns the failure category, or fault.Unknown on
// success.
func (s *Scanner) Probe(ctx context.Context) fault.Category {
	if err := s.probeError(ctx); err != nil {
		return fault.Categorize(err)
	}
	return fault.Unknown
}

func (s *Scanner) probeError(ctx context.Context) error {
	_, err := s.store.List(ctx, store.ListRequest{Limit: 1})
	now := s.now().UTC()
	if err != nil {
		s.readiness.MarkFailure(now, fault.Categorize(err))
		return err
	}
	s.readiness.MarkReachable(now)
	return nil
}

// Refresh runs one full scan and publishes the result. On a failed scan or a
// rejected snapshot it instead publishes the previous snapshot marked stale
// (or a failed initial snapshot) with the failure category; a canceled
// context publishes nothing. Calls are serialized.
func (s *Scanner) Refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	generation := s.generation.Add(1)
	started := s.now().UTC()
	snapshot, err := s.scan(ctx, generation, started)
	if err == nil {
		publishErr := s.cache.Publish(snapshot)
		if publishErr == nil {
			s.logger.InfoContext(ctx, "inventory refresh completed",
				slog.Uint64("refresh_generation", generation),
				slog.Uint64("evidence_generation", snapshot.Evidence.Generation),
				slog.Int64("objects", snapshot.ObjectCount),
				slog.Int64("pages", snapshot.PagesExamined),
				slog.Int64("duration_ms", s.now().UTC().Sub(started).Milliseconds()),
				slog.String("state", string(snapshot.Evidence.State)),
			)
			return nil
		}
		// A complete scan whose result cannot be published (the evidence
		// projection rejects a value read from the repository) must not leave
		// the previous publication looking fresh. Mark it stale and unknown
		// under a format category so consumers see the failure.
		s.publishFailure(ctx, generation, started, fault.Incompatible)
		return &scanError{kind: fault.Incompatible, cause: publishErr}
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	s.publishFailure(ctx, generation, started, fault.Categorize(err))
	return err
}

func (s *Scanner) scan(ctx context.Context, generation uint64, started time.Time) (Snapshot, error) {
	matcher, err := s.format.NewScopeMatcher(s.configuredScopes)
	if err != nil {
		return Snapshot{}, err
	}
	descriptor := s.format.Descriptor()
	builder, err := newBuilder(descriptor, matcher.InitialScopes(), s.recentLimit, s.analyzeBarmanCatalog)
	if err != nil {
		return Snapshot{}, err
	}
	if s.pgbackrest != nil && descriptor.ID == pgbackrest.FormatID {
		builder.pgbackrest = newPGBackRestBuilder(*s.pgbackrest, s.cache.Load().PGBackRest, generation)
	}
	cursor := ""
	maxPages := (s.maxObjects+s.pageSize-1)/s.pageSize + 100
	for pageNumber := 0; ; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		if pageNumber >= maxPages {
			return Snapshot{}, &scanError{kind: fault.SafetyLimit}
		}
		remaining := s.maxObjects - int(builder.objectCount)
		if remaining <= 0 {
			return Snapshot{}, &scanError{kind: fault.SafetyLimit}
		}
		limit := min(s.pageSize, remaining)
		page, err := s.store.List(ctx, store.ListRequest{Cursor: cursor, Limit: limit})
		if err != nil {
			return Snapshot{}, err
		}
		if len(page.Objects) > limit || len(page.NextCursor) > store.MaxCursorBytes {
			return Snapshot{}, &scanError{kind: fault.SafetyLimit}
		}
		builder.pagesExamined++
		for _, object := range page.Objects {
			match, matched := matcher.Match(object.Key)
			if err := builder.add(object, match, matched); err != nil {
				return Snapshot{}, err
			}
		}
		if page.NextCursor == "" {
			break
		}
		if builder.objectCount >= int64(s.maxObjects) || page.NextCursor == cursor {
			return Snapshot{}, &scanError{kind: fault.SafetyLimit}
		}
		cursor = page.NextCursor
	}
	snapshot := builder.complete(generation, started, s.now().UTC())
	if s.analyzeBarmanCatalog && descriptor.ID == "barman-cloud" {
		snapshot.BarmanCatalog = barmancloud.Analyze(ctx, s.store, builder.catalogObjects)
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		applyBarmanEvidence(&snapshot)
		previous := s.cache.Load()
		snapshot.BarmanWAL = builder.barmanWAL.Finish(snapshot.BarmanCatalog.Backups, previous.BarmanWAL, generation)
		applyBarmanWALEvidence(&snapshot)
		historyObjects, historyTruncated := builder.barmanWAL.HistoryObjects()
		snapshot.BarmanRecovery = barmancloud.AnalyzeRecovery(ctx, s.store, snapshot.BarmanCatalog.Backups, snapshot.BarmanWAL, historyObjects, historyTruncated, s.barmanRecoveryOptions)
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		applyBarmanRecoveryEvidence(&snapshot)
	}
	if builder.pgbackrest != nil {
		snapshot.PGBackRest = builder.pgbackrest.analyze(ctx, s.store)
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		applyPGBackRestEvidence(&snapshot, builder.pgbackrest.truncated)
	}
	return snapshot, nil
}

func (s *Scanner) publishFailure(ctx context.Context, generation uint64, started time.Time, category fault.Category) {
	previous := s.cache.Load()
	now := s.now().UTC()
	reason := "refresh failed: " + string(category)
	if previous.TotalsKnown {
		previous.RefreshGeneration = generation
		previous.LastRefreshAt = now
		previous.LastAttemptAt = started
		previous.RefreshFailure = category
		previous.Evidence.Stale = true
		previous.Evidence.State = evidence.Unknown
		previous.Evidence.Reason = reason
		for index := range previous.Evidence.Capabilities {
			if previous.Evidence.Capabilities[index].Support == evidence.Supported {
				previous.Evidence.Capabilities[index].State = evidence.Unknown
				previous.Evidence.Capabilities[index].Reason = "stale after " + reason
			}
		}
		s.logFailurePublication(ctx, s.cache.Publish(previous), generation)
		return
	}
	failed := FailedSnapshot(s.format.Descriptor(), generation, started, now, category, reason)
	s.logFailurePublication(ctx, s.cache.Publish(failed), generation)
}

// FailedSnapshot is the stale, incomplete, unknown snapshot published when no
// complete snapshot exists yet and an attempt failed with category. reason is
// shown verbatim and must already be redacted.
func FailedSnapshot(descriptor repository.Descriptor, generation uint64, started, now time.Time, category fault.Category, reason string) Snapshot {
	failed := Initial(descriptor)
	failed.RefreshGeneration = generation
	failed.LastRefreshAt = now
	failed.LastAttemptAt = started
	failed.RefreshFailure = category
	failed.Evidence.Generation = generation
	failed.Evidence.StartedAt = started
	failed.Evidence.Completeness = evidence.Incomplete
	failed.Evidence.Stale = true
	failed.Evidence.Reason = reason
	for index := range failed.Evidence.Capabilities {
		if failed.Evidence.Capabilities[index].ID != evidence.ObjectInventory {
			failed.Evidence.Capabilities[index].Reason = "scan incomplete"
		}
	}
	return failed
}

// logFailurePublication records that even the stale failure snapshot could
// not be published. The cache then still serves its previous value, so this
// must be visible to operators.
func (s *Scanner) logFailurePublication(ctx context.Context, err error, generation uint64) {
	if err == nil {
		return
	}
	s.logger.ErrorContext(ctx, "inventory failure snapshot publication failed",
		slog.String("category", string(fault.Categorize(err))),
		slog.Uint64("refresh_generation", generation),
	)
}

type builder struct {
	descriptor     repository.Descriptor
	pagesExamined  int64
	objectCount    int64
	storedBytes    int64
	unscoped       int64
	scopes         map[string]*Scope
	recent         recentHeap
	recentLimit    int
	catalogObjects []store.Object
	barmanWAL      *barmancloud.WALCollector
	pgbackrest     *pgbackrestBuilder
}

func newBuilder(descriptor repository.Descriptor, initialScopes []string, recentLimit int, analyzeBarman bool) (*builder, error) {
	if len(initialScopes) > MaxScopes {
		return nil, &scanError{kind: fault.SafetyLimit}
	}
	builderValue := &builder{
		descriptor: descriptor, scopes: make(map[string]*Scope, len(initialScopes)), recentLimit: recentLimit,
	}
	if analyzeBarman && descriptor.ID == "barman-cloud" {
		builderValue.barmanWAL = barmancloud.NewWALCollector()
	}
	for _, name := range initialScopes {
		if invalidScopeName(name) {
			return nil, &scanError{kind: fault.Incompatible}
		}
		if _, exists := builderValue.scopes[name]; exists {
			return nil, &scanError{kind: fault.Incompatible}
		}
		builderValue.scopes[name] = &Scope{Name: name}
	}
	heap.Init(&builderValue.recent)
	return builderValue, nil
}

func (b *builder) add(object store.Object, match repository.ScopeMatch, matched bool) error {
	if object.Key == "" || len(object.Key) > store.MaxKeyBytes || object.Size < 0 || b.objectCount == math.MaxInt64 || object.Size > math.MaxInt64-b.storedBytes {
		return &scanError{kind: fault.SafetyLimit}
	}
	b.objectCount++
	b.storedBytes += object.Size
	scopeName := ""
	if matched {
		scopeName = match.Name
		if invalidScopeName(scopeName) {
			return &scanError{kind: fault.Incompatible}
		}
		scopeValue, exists := b.scopes[scopeName]
		if !exists {
			if len(b.scopes) >= MaxScopes {
				return &scanError{kind: fault.SafetyLimit}
			}
			scopeValue = &Scope{Name: scopeName}
			b.scopes[scopeName] = scopeValue
		}
		if scopeValue.ObjectCount == math.MaxInt64 || object.Size > math.MaxInt64-scopeValue.StoredBytes {
			return &scanError{kind: fault.SafetyLimit}
		}
		scopeValue.ObjectCount++
		scopeValue.StoredBytes += object.Size
		scopeValue.Recognized = scopeValue.Recognized || match.Recognized
	} else {
		b.unscoped++
	}
	b.addRecent(RecentObject{Key: object.Key, Scope: scopeName, Size: object.Size, LastModified: object.LastModified.UTC()})
	if matched {
		_, _, ok := barmanBackupObject(object.Key)
		if ok {
			if len(b.catalogObjects) >= MaxCatalogObjects {
				return &scanError{kind: fault.SafetyLimit}
			}
			b.catalogObjects = append(b.catalogObjects, object)
		}
	}
	if matched && b.barmanWAL != nil {
		b.barmanWAL.Add(object)
	}
	if matched && b.pgbackrest != nil {
		b.pgbackrest.add(object, scopeName)
	}
	return nil
}

func barmanBackupObject(key string) (string, string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 4 || parts[0] == "" || parts[1] != "base" || parts[2] == "" {
		return "", "", false
	}
	return parts[0], parts[2], true
}

func applyBarmanEvidence(snapshot *Snapshot) {
	state := evidence.Healthy
	reason := "Barman backup catalog evaluated"
	if len(snapshot.BarmanCatalog.Backups) == 0 {
		state, reason = evidence.Unknown, "no Barman backup metadata observed"
	} else {
		for _, backup := range snapshot.BarmanCatalog.Backups {
			switch backup.State {
			case evidence.Unknown:
				state, reason = evidence.Unknown, "Barman catalog contains unknown backup evidence"
			case evidence.Unhealthy:
				if state != evidence.Unknown {
					state, reason = evidence.Unhealthy, "Barman catalog contains failed or incomplete backup evidence"
				}
			case evidence.Warning:
				if state == evidence.Healthy {
					state, reason = evidence.Warning, "Barman catalog contains in-progress backup evidence"
				}
			case evidence.Healthy:
				// Lowers nothing.
			}
		}
	}
	for index := range snapshot.Evidence.Capabilities {
		capability := &snapshot.Evidence.Capabilities[index]
		if capability.ID == evidence.CatalogListing || capability.ID == evidence.StructuralValidation {
			capability.Support, capability.State, capability.Reason = evidence.Supported, state, reason
		}
	}
	snapshot.Evidence.State, snapshot.Evidence.Reason = state, reason
}

func applyBarmanWALEvidence(snapshot *Snapshot) {
	state := evidence.Unknown
	reason := "no Barman WAL scopes evaluated"
	if len(snapshot.BarmanWAL.Servers) > 0 {
		state = evidence.Healthy
		reason = "Barman WAL continuity evaluated"
		for _, server := range snapshot.BarmanWAL.Servers {
			state = conservativeState(state, server.State)
		}
		switch state {
		case evidence.Unknown:
			reason = "Barman WAL continuity contains unknown evidence"
		case evidence.Unhealthy:
			reason = "Barman WAL continuity contains confirmed gaps"
		case evidence.Warning:
			reason = "Barman WAL continuity contains candidate gaps"
		case evidence.Healthy:
			// No reason to give.
		}
	}
	for index := range snapshot.Evidence.Capabilities {
		capability := &snapshot.Evidence.Capabilities[index]
		if capability.ID == evidence.WALContinuity {
			capability.Support, capability.State, capability.Reason = evidence.Supported, state, reason
		}
	}
	snapshot.Evidence.State = conservativeState(snapshot.Evidence.State, state)
	switch snapshot.Evidence.State {
	case evidence.Unknown:
		snapshot.Evidence.Reason = "Barman catalog or WAL continuity contains unknown evidence"
	case evidence.Unhealthy:
		snapshot.Evidence.Reason = "Barman catalog or WAL continuity contains unhealthy evidence"
	case evidence.Warning:
		snapshot.Evidence.Reason = "Barman catalog or WAL continuity contains warning evidence"
	default:
		snapshot.Evidence.Reason = "Barman catalog and WAL continuity evaluated"
	}
}

func conservativeState(left, right evidence.State) evidence.State {
	if left == evidence.Unknown || right == evidence.Unknown {
		return evidence.Unknown
	}
	if left == evidence.Unhealthy || right == evidence.Unhealthy {
		return evidence.Unhealthy
	}
	if left == evidence.Warning || right == evidence.Warning {
		return evidence.Warning
	}
	return evidence.Healthy
}

func applyBarmanRecoveryEvidence(snapshot *Snapshot) {
	timelineState, coverageState, retentionState := evidence.Unknown, evidence.Unknown, evidence.Unknown
	if len(snapshot.BarmanRecovery.Servers) > 0 {
		timelineState, coverageState, retentionState = evidence.Healthy, evidence.Healthy, evidence.Healthy
		for _, server := range snapshot.BarmanRecovery.Servers {
			timelineState = conservativeState(timelineState, server.TimelineState)
			coverageState = conservativeState(coverageState, server.CoverageState)
			retentionState = conservativeState(retentionState, server.Retention.State)
		}
	}
	setCapability := func(id evidence.CapabilityID, state evidence.State, reason string) {
		for index := range snapshot.Evidence.Capabilities {
			if snapshot.Evidence.Capabilities[index].ID == id {
				snapshot.Evidence.Capabilities[index].Support = evidence.Supported
				snapshot.Evidence.Capabilities[index].State = state
				snapshot.Evidence.Capabilities[index].Reason = reason
			}
		}
	}
	setCapability(evidence.TimelineTraversal, timelineState, recoveryCapabilityReason("timeline history", timelineState))
	setCapability(evidence.RecoveryCoverage, coverageState, recoveryCapabilityReason("observed recovery coverage", coverageState))
	setCapability(evidence.RetentionExpectation, retentionState, recoveryCapabilityReason("retention evidence", retentionState))
	snapshot.Evidence.State = conservativeState(snapshot.Evidence.State, conservativeState(timelineState, conservativeState(coverageState, retentionState)))
	if snapshot.Evidence.State != evidence.Healthy {
		snapshot.Evidence.Reason = "Barman catalog, WAL, timeline, recovery, or retention evidence is not healthy"
	} else {
		snapshot.Evidence.Reason = "Barman catalog, WAL, timeline, recovery, and retention evidence evaluated"
	}
}

func recoveryCapabilityReason(name string, state evidence.State) string {
	switch state {
	case evidence.Healthy:
		return name + " evaluated"
	case evidence.Warning:
		return name + " contains provisional evidence"
	case evidence.Unhealthy:
		return name + " contains definite structural failure"
	default:
		return name + " contains unknown evidence"
	}
}

func invalidScopeName(name string) bool {
	return name == "" || len(name) > 128 || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\") || strings.IndexFunc(name, unicode.IsControl) >= 0
}

func (b *builder) addRecent(object RecentObject) {
	if len(b.recent) < b.recentLimit {
		heap.Push(&b.recent, object)
		return
	}
	if moreRecent(object, b.recent[0]) {
		b.recent[0] = object
		heap.Fix(&b.recent, 0)
	}
}

func (b *builder) complete(generation uint64, started, completed time.Time) Snapshot {
	envelope := evidence.InitialSnapshot(b.descriptor.ID, b.descriptor.ScopeKind, b.descriptor.Capabilities)
	envelope.Generation = generation
	envelope.StartedAt = started
	envelope.CompletedAt = completed
	envelope.Completeness = evidence.Complete
	envelope.State = evidence.Unknown
	envelope.Reason = "backup semantics not evaluated"
	SetInventoryCapability(&envelope, evidence.Healthy, "complete object listing")
	scopes := make([]Scope, 0, len(b.scopes))
	for _, scopeValue := range b.scopes {
		scopes = append(scopes, *scopeValue)
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].Name < scopes[j].Name })
	recent := make([]RecentObject, len(b.recent))
	copy(recent, b.recent)
	sort.Slice(recent, func(i, j int) bool { return moreRecent(recent[i], recent[j]) })
	return Snapshot{
		Evidence: envelope, RefreshGeneration: generation, LastRefreshAt: completed, LastAttemptAt: started,
		PagesExamined: b.pagesExamined, ObjectsExamined: b.objectCount,
		TotalsKnown: true, ObjectCount: b.objectCount, StoredBytes: b.storedBytes,
		UnscopedObjectCount: b.unscoped, Scopes: scopes, RecentObjects: recent,
	}
}

type recentHeap []RecentObject

func (h recentHeap) Len() int { return len(h) }
func (h recentHeap) Less(i, j int) bool {
	return moreRecent(h[j], h[i])
}
func (h recentHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *recentHeap) Push(value any) { *h = append(*h, value.(RecentObject)) }
func (h *recentHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func moreRecent(left, right RecentObject) bool {
	if !left.LastModified.Equal(right.LastModified) {
		return left.LastModified.After(right.LastModified)
	}
	return left.Key < right.Key
}

// scanError carries a stable category and, optionally, the internal cause it
// summarizes. Error never renders the cause; RootCause exposes it only for
// redacted logging.
type scanError struct {
	kind  fault.Category
	cause error
}

func (e *scanError) Error() string            { return fmt.Sprintf("inventory scan failed: %s", e.kind) }
func (e *scanError) Category() fault.Category { return e.kind }
func (e *scanError) RootCause() string {
	if e.cause == nil {
		return ""
	}
	return e.cause.Error()
}
