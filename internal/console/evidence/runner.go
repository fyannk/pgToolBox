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

package evidence

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// SourceKey is the runner's own source in the console's source states.
const SourceKey = "evidence"

// Runner defaults and floors.
const (
	// DefaultInterval is the refresh period when Options.Interval is zero.
	DefaultInterval = 60 * time.Second
	// MinimumInterval is the shortest accepted refresh period.
	MinimumInterval = 10 * time.Second
	// DefaultConcurrency is how many agents are read at once.
	DefaultConcurrency = 4
	// MaximumConcurrency caps Options.Concurrency.
	MaximumConcurrency = 32
	// triggerGap is the least time between a pass and one Trigger starts,
	// so a burst of EvidenceAgent events costs one pass.
	triggerGap = 5 * time.Second
)

// Display-safe reasons the runner states itself (the failure kinds have
// theirs in fetch.go).
const (
	reasonDisabled       = "the console is configured not to read evidence agents"
	reasonNotListed      = "the console has not read the EvidenceAgent objects yet"
	reasonNotWatched     = "the console does not watch this namespace"
	reasonUnreadable     = "EvidenceAgent objects are not readable in this namespace (the kind is not installed or not permitted to the console)"
	reasonNoAgent        = "no EvidenceAgent in this namespace names this Cluster"
	reasonNotRead        = "the evidence agent has not been read yet"
	reasonNoEndpoint     = "the EvidenceAgent has not published status.endpoint yet"
	reasonNoCABundle     = "the EvidenceAgent has not published status.caBundle yet"
	reasonNoEpoch        = "the EvidenceAgent has not published status.producerEpoch yet (rollout in progress): the answer cannot be tied to the current configuration"
	reasonOtherEpoch     = "the evidence agent answered from another configuration than status.producerEpoch (rollout in progress)"
	reasonAgentStale     = "the evidence agent reports that its latest repository scan failed"
	reasonListFailed     = "the EvidenceAgent objects could not be listed from the console's cache"
	reasonMultipleAgents = "%d EvidenceAgents name this Cluster; the oldest, %s, is used"
)

// AgentLister is the runner's view of the console's informer cache: a
// controller-runtime client.Reader satisfies it.
type AgentLister interface {
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

// Options configure a Runner.
type Options struct {
	// Disabled builds a runner that reads nothing: every Result is
	// EvidenceDisabled and State is SourceDisabled. Nothing else is
	// required then.
	Disabled bool
	// Lister lists EvidenceAgent objects; the console's cache, which
	// watches them under cache.SourceEvidenceAgents.
	Lister AgentLister
	// Readable reports whether the cache holds a list of EvidenceAgents in
	// a namespace (cache.Manager.Readable(ns, cache.SourceEvidenceAgents)).
	// A namespace that is not readable yields noAgent, never a fault. Nil
	// means every namespace is readable.
	Readable func(namespace string) bool
	// Namespaces are the watched namespaces.
	Namespaces []string
	// TokenDirectory holds the projected bound tokens, one per watched
	// namespace at evidencev1alpha1.TokenPath(TokenDirectory, namespace),
	// each of audience evidencev1alpha1.TokenAudience(namespace); default
	// evidencev1alpha1.DefaultTokenDirectory. A namespace's token is
	// re-read on every pass that has an agent to call there.
	TokenDirectory string
	// Interval is the refresh period; default DefaultInterval, at least
	// MinimumInterval.
	Interval time.Duration
	// Concurrency bounds the agents read at once; default
	// DefaultConcurrency, at most MaximumConcurrency.
	Concurrency int
	// Static lists fixed agents (EVIDENCE_AGENTS_FILE, the pgtools chart
	// without the operator). Set, the runner lists no EvidenceAgent and
	// needs no Lister, Readable or TokenDirectory: see static.go.
	Static []StaticAgent
	// Logger receives state transitions; never a token, a CA or a body.
	Logger *slog.Logger
	// Now is the clock of ObservedAt and Since; default time.Now.
	Now func() time.Time
}

// agentRef is what ForCluster needs of a listed EvidenceAgent.
type agentRef struct {
	name    string
	cluster string
	created time.Time
}

// agentState is the retained result of one agent.
type agentState struct {
	uid    types.UID
	result Result
}

// cachedClient is one agent's HTTPS client, rebuilt when its endpoint or
// CA bundle changes.
type cachedClient struct {
	fingerprint [sha256.Size]byte
	client      *http.Client
}

// Runner reads every EvidenceAgent of the watched namespaces in the
// background and keeps the latest good Result per agent. It implements
// Reader and is safe for concurrent use.
type Runner struct {
	disabled bool
	// static holds the fixed agents by "namespace/cluster"; nil in the
	// EvidenceAgent mode.
	static      map[string]StaticAgent
	lister      AgentLister
	readable    func(string) bool
	namespaces  []string
	tokenPaths  map[string]string
	interval    time.Duration
	concurrency int
	logger      *slog.Logger
	now         func() time.Time
	// dial replaces the transport's dialer in tests.
	dial dialFunc

	trigger chan struct{}

	// refreshMu serializes passes; clients is touched by passes only.
	refreshMu sync.Mutex
	clients   map[string]*cachedClient

	mu         sync.RWMutex
	listed     bool
	agents     map[string][]agentRef
	unreadable map[string]bool
	states     map[string]*agentState
	source     viewmodel.SourceState
}

var _ Reader = (*Runner)(nil)

// New validates the options and builds a Runner.
func New(opts Options) (*Runner, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	r := &Runner{
		disabled: opts.Disabled, logger: logger, now: now,
		trigger: make(chan struct{}, 1),
		clients: map[string]*cachedClient{}, agents: map[string][]agentRef{},
		unreadable: map[string]bool{}, states: map[string]*agentState{},
	}
	if opts.Disabled {
		r.source = viewmodel.SourceState{Source: SourceKey, State: viewmodel.SourceDisabled, Reason: reasonDisabled}
		return r, nil
	}
	if len(opts.Static) == 0 && opts.Lister == nil {
		return nil, errors.New("evidence: a lister is required")
	}
	namespaces := slices.Clone(opts.Namespaces)
	slices.Sort(namespaces)
	namespaces = slices.Compact(namespaces)
	if len(namespaces) == 0 || slices.Contains(namespaces, "") {
		return nil, errors.New("evidence: at least one watched namespace is required, and none may be empty")
	}
	tokenDirectory := opts.TokenDirectory
	if tokenDirectory == "" {
		tokenDirectory = evidencev1alpha1.DefaultTokenDirectory
	}
	tokenPaths := make(map[string]string, len(namespaces))
	if len(opts.Static) > 0 {
		// The bound-token projection is not used in this mode.
		static, err := staticAgents(opts.Static, namespaces)
		if err != nil {
			return nil, err
		}
		r.static = static
	} else {
		for _, namespace := range namespaces {
			path, err := evidencev1alpha1.TokenPath(tokenDirectory, namespace)
			if err != nil {
				return nil, fmt.Errorf("evidence: the token directory must be absolute and clean, and every watched namespace a DNS-1123 label: %w", err)
			}
			tokenPaths[namespace] = path
		}
	}
	interval := opts.Interval
	if interval == 0 {
		interval = DefaultInterval
	}
	if interval < MinimumInterval {
		return nil, fmt.Errorf("evidence: the refresh interval must be at least %s", MinimumInterval)
	}
	concurrency := opts.Concurrency
	if concurrency == 0 {
		concurrency = DefaultConcurrency
	}
	if concurrency < 1 || concurrency > MaximumConcurrency {
		return nil, fmt.Errorf("evidence: concurrency must be between 1 and %d", MaximumConcurrency)
	}
	r.lister, r.readable, r.namespaces = opts.Lister, opts.Readable, namespaces
	r.tokenPaths, r.interval, r.concurrency = tokenPaths, interval, concurrency
	since := now().UTC()
	r.source = viewmodel.SourceState{Source: SourceKey, State: viewmodel.SourcePending, Since: &since, Reason: reasonNotListed}
	if r.static != nil {
		// The fixed list is known before the first pass.
		r.listed, r.agents = true, staticRefs(r.static)
		r.source.Reason = reasonNotRead
	}
	return r, nil
}

// Run refreshes immediately, then every interval and on Trigger, until
// ctx ends. It returns nil on cancellation; a pass never fails the loop.
func (r *Runner) Run(ctx context.Context) error {
	if r.disabled {
		<-ctx.Done()
		return nil
	}
	defer r.closeClients()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		r.refresh(ctx)
		last := time.Now()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-r.trigger:
			if wait := triggerGap - time.Since(last); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
			}
		}
	}
}

// Trigger asks for a pass soon (an EvidenceAgent changed). It never
// blocks; triggers arriving during a pass collapse into one.
func (r *Runner) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// State is the runner's own source state.
func (r *Runner) State() viewmodel.SourceState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.source
}

// ForCluster returns the evidence for a Cluster: the Result of the
// EvidenceAgent whose spec.clusterRef names it (the oldest when several
// do, as Reason says). Snapshot and BackupIDs are shared and read-only.
func (r *Runner) ForCluster(namespace, cluster string) Result {
	if r.disabled {
		return Result{State: viewmodel.EvidenceDisabled, Reason: reasonDisabled}
	}
	if _, watched := slices.BinarySearch(r.namespaces, namespace); !watched {
		return Result{State: viewmodel.EvidenceNoAgent, Reason: reasonNotWatched}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.listed {
		return Result{State: viewmodel.EvidencePending, Reason: reasonNotListed}
	}
	if r.unreadable[namespace] {
		return Result{State: viewmodel.EvidenceNoAgent, Reason: reasonUnreadable}
	}
	var candidates []agentRef
	for _, ref := range r.agents[namespace] {
		if ref.cluster == cluster {
			candidates = append(candidates, ref)
		}
	}
	if len(candidates) == 0 {
		if r.static != nil {
			return Result{State: viewmodel.EvidenceNoAgent, Reason: reasonNoStaticAgent}
		}
		return Result{State: viewmodel.EvidenceNoAgent, Reason: reasonNoAgent}
	}
	chosen := candidates[0]
	result := Result{Agent: chosen.name, State: viewmodel.EvidencePending, Reason: reasonNotRead}
	if state := r.states[stateKey(namespace, chosen.name)]; state != nil {
		result = state.result
	}
	if len(candidates) > 1 {
		note := fmt.Sprintf(reasonMultipleAgents, len(candidates), chosen.name)
		if result.Reason == "" {
			result.Reason = note
		} else {
			result.Reason = note + "; " + result.Reason
		}
	}
	return result
}

func stateKey(namespace, name string) string { return namespace + "/" + name }

// outcome is what one pass learned about one agent.
type outcome struct {
	// pending is set when the agent is not ready to be called.
	pending string
	// err is set when the call was refused or failed.
	err      error
	snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot
	backups  backups
}

// job is one agent of a pass.
type job struct {
	agent   *pgtoolboxv1alpha1.EvidenceAgent
	session *agentSession
	out     outcome
	// static marks a fixed agent: no producerEpoch to compare, and its
	// failures read with the static reasons.
	static bool
}

// refresh is one pass: token, listing, bounded concurrent fetches, and
// one merge of the results under the lock.
func (r *Runner) refresh(ctx context.Context) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if r.static != nil {
		r.refreshStatic(ctx)
		return
	}

	listed := map[string][]*pgtoolboxv1alpha1.EvidenceAgent{}
	unreadable := map[string]bool{}
	kept := map[string]bool{}
	listFailed := false
	for _, namespace := range r.namespaces {
		if r.readable != nil && !r.readable(namespace) {
			unreadable[namespace] = true
			continue
		}
		var list pgtoolboxv1alpha1.EvidenceAgentList
		if err := r.lister.List(ctx, &list, client.InNamespace(namespace)); err != nil {
			if ctx.Err() != nil {
				return
			}
			listFailed, kept[namespace] = true, true
			r.logger.Warn("listing EvidenceAgents failed; keeping the previous listing",
				slog.String("namespace", namespace), slog.String("error", err.Error()))
			continue
		}
		for index := range list.Items {
			if list.Items[index].Namespace == namespace {
				listed[namespace] = append(listed[namespace], &list.Items[index])
			}
		}
	}

	var jobs []*job
	live := map[string]bool{}
	tokenFailures := map[string]failure{}
	for _, namespace := range r.namespaces {
		if len(listed[namespace]) == 0 {
			continue
		}
		// One token per namespace: the agents of this namespace accept
		// only its audience.
		token, tokenFailure := r.readToken(namespace)
		if tokenFailure != "" {
			tokenFailures[namespace] = tokenFailure
		}
		for _, agent := range listed[namespace] {
			key := stateKey(agent.Namespace, agent.Name)
			live[key] = true
			jobs = append(jobs, r.prepare(agent, key, token, tokenFailure))
		}
	}
	r.pruneClients(live, kept)
	r.runJobs(ctx, jobs)
	if ctx.Err() != nil {
		return
	}
	r.merge(jobs, listed, unreadable, kept, tokenFailures, listFailed)
}

// readToken re-reads a namespace's projected token; the kubelet rotates it
// in place.
func (r *Runner) readToken(namespace string) (evidencev1alpha1.BearerToken, failure) {
	path := r.tokenPaths[namespace]
	token, err := evidencev1alpha1.ReadBearerTokenFile(path)
	if err == nil {
		return token, ""
	}
	if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
		return evidencev1alpha1.BearerToken{}, failureTokenMissing
	}
	return evidencev1alpha1.BearerToken{}, failureTokenInvalid
}

// prepare decides whether an agent can be called and builds its session.
func (r *Runner) prepare(agent *pgtoolboxv1alpha1.EvidenceAgent, key string, token evidencev1alpha1.BearerToken, tokenFailure failure) *job {
	j := &job{agent: agent}
	switch {
	case agent.Status.Endpoint == "":
		j.out.pending = reasonNoEndpoint
		return j
	case len(agent.Status.CABundle) == 0:
		j.out.pending = reasonNoCABundle
		return j
	case !StatusWrittenByOperator(agent):
		// Defense in depth: a status.endpoint or
		// status.caBundle another field manager wrote is never called,
		// so the console's token is never sent to it.
		j.out.err = fail(failureStatusWriter, nil)
		return j
	case tokenFailure != "":
		j.out.err = fail(tokenFailure, nil)
		return j
	}
	origin, serverName, err := parseEndpoint(agent.Status.Endpoint, agent.Namespace)
	if err != nil {
		j.out.err = fail(failureEndpoint, err)
		return j
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(string(agent.UID) + "\x00" + agent.Status.Endpoint + "\x00"))
	_, _ = hash.Write(agent.Status.CABundle)
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hash.Sum(nil))
	cached := r.clients[key]
	if cached == nil || cached.fingerprint != fingerprint {
		if cached != nil {
			cached.client.CloseIdleConnections()
			delete(r.clients, key)
		}
		httpClient, err := newAgentClient(agent.Status.CABundle, serverName, r.dial)
		if err != nil {
			j.out.err = err
			return j
		}
		cached = &cachedClient{fingerprint: fingerprint, client: httpClient}
		r.clients[key] = cached
	}
	j.session = &agentSession{client: cached.client, origin: origin, token: token}
	return j
}

// runJobs fetches the callable agents, at most r.concurrency at once,
// and counts every attempted fetch.
func (r *Runner) runJobs(ctx context.Context, jobs []*job) {
	slots := make(chan struct{}, r.concurrency)
	var wait sync.WaitGroup
	for _, j := range jobs {
		if j.session == nil {
			if j.out.err != nil {
				countFetch(string(kindOf(j.out.err)))
			}
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				j.out.err = fail(failureCanceled, ctx.Err())
				return
			}
			defer func() { <-slots }()
			snapshot, collection, err := j.session.fetch(ctx, j.agent)
			j.out.snapshot, j.out.backups, j.out.err = snapshot, collection, err
			if ctx.Err() != nil {
				return
			}
			switch {
			case err != nil:
				countFetch(string(kindOf(err)))
			case !j.static && !epochCurrent(j.agent, snapshot):
				countFetch(outcomeStaleEpoch)
			default:
				countFetch(outcomeSuccess)
			}
		}()
	}
	wait.Wait()
}

// merge publishes one pass under the lock and logs transitions.
func (r *Runner) merge(jobs []*job, listed map[string][]*pgtoolboxv1alpha1.EvidenceAgent, unreadable, kept map[string]bool, tokenFailures map[string]failure, listFailed bool) {
	now := r.now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()

	states := map[string]*agentState{}
	agents := map[string][]agentRef{}
	for namespace := range kept {
		agents[namespace] = r.agents[namespace]
		for _, ref := range r.agents[namespace] {
			key := stateKey(namespace, ref.name)
			if state, ok := r.states[key]; ok {
				states[key] = state
			}
		}
	}
	for namespace, items := range listed {
		refs := make([]agentRef, 0, len(items))
		for _, agent := range items {
			refs = append(refs, agentRef{name: agent.Name, cluster: agent.Spec.ClusterRef.Name, created: agent.CreationTimestamp.UTC()})
		}
		slices.SortFunc(refs, func(a, b agentRef) int {
			if c := a.created.Compare(b.created); c != 0 {
				return c
			}
			return strings.Compare(a.name, b.name)
		})
		agents[namespace] = refs
	}
	for _, j := range jobs {
		key := stateKey(j.agent.Namespace, j.agent.Name)
		previous := r.states[key]
		next := advance(previous, j.agent, j.out, now, j.static)
		states[key] = next
		r.logTransition(j.agent, previous, next, j.out.err)
	}
	publishAgentStates(r.states, states)
	r.states, r.agents, r.unreadable, r.listed = states, agents, unreadable, true

	state, reason := viewmodel.SourceLive, ""
	switch {
	case len(tokenFailures) > 0:
		state, reason = viewmodel.SourceUnavailable, r.tokenFailureReason(tokenFailures)
	case listFailed:
		state, reason = viewmodel.SourceUnavailable, reasonListFailed
	}
	if r.source.State != state || r.source.Reason != reason {
		since := now
		r.source = viewmodel.SourceState{Source: SourceKey, State: state, Since: &since, Reason: reason}
	}
}

// tokenFailureReason states the token failures of a pass: the reason of
// the first failing namespace, naming the failing namespaces when the
// console watches more than one.
func (r *Runner) tokenFailureReason(failures map[string]failure) string {
	namespaces := make([]string, 0, len(failures))
	for namespace := range failures {
		namespaces = append(namespaces, namespace)
	}
	slices.Sort(namespaces)
	reason := failures[namespaces[0]].reason()
	if r.static != nil {
		reason = failures[namespaces[0]].staticReason()
	}
	if len(r.namespaces) > 1 {
		reason += " for namespace " + strings.Join(namespaces, ", ")
	}
	return reason
}

// advance computes an agent's next Result from its previous one and
// what this pass learned. A failed or refused refresh never replaces a
// retained snapshot: it turns it stale with a display-safe reason.
//
// A static agent (EVIDENCE_AGENTS_FILE) has no EvidenceAgent status, so
// no producerEpoch to compare its answer with: the agent restarts on any
// change of its configuration, and the console accepts what it answers,
// still bound to the Cluster's namespace and name by checkIdentity.
func advance(previous *agentState, agent *pgtoolboxv1alpha1.EvidenceAgent, out outcome, now time.Time, static bool) *agentState {
	next := &agentState{uid: agent.UID}
	if previous != nil && previous.uid == agent.UID {
		next.result = previous.result
	}
	next.result.Agent = agent.Name
	retained := next.result.Snapshot != nil
	degrade := func(reason string) *agentState {
		if retained {
			next.result.State = viewmodel.EvidenceStale
		} else {
			next.result.State = viewmodel.EvidenceUnavailable
		}
		next.result.Reason = reason
		return next
	}
	switch {
	case out.pending != "":
		if !retained {
			next.result.State, next.result.Reason = viewmodel.EvidencePending, out.pending
			return next
		}
		return degrade(out.pending)
	case out.err != nil:
		if static {
			return degrade(kindOf(out.err).staticReason())
		}
		return degrade(kindOf(out.err).reason())
	case out.snapshot == nil:
		return degrade(failureContract.reason())
	}
	observed := now
	if out.backups.recovery.ScannedAt == nil {
		scanned := observed
		if out.snapshot.CompletedAt != nil {
			scanned = out.snapshot.CompletedAt.UTC()
		}
		out.backups.recovery.ScannedAt = &scanned
	}
	fresh := Result{
		Agent: agent.Name, State: viewmodel.EvidenceLive, ObservedAt: &observed, Snapshot: out.snapshot,
		BackupIDs: out.backups.ids, BackupsComplete: out.backups.complete,
		Backups: out.backups.items, Recovery: keepRecovery(next.result.Recovery, out.backups.recovery),
		RefreshInterval: DefaultAgentRefreshInterval,
	}
	if agent.Spec.RefreshInterval != nil && agent.Spec.RefreshInterval.Duration > 0 {
		fresh.RefreshInterval = agent.Spec.RefreshInterval.Duration
	}
	if pgbackrest := agent.Status.PgBackRest; pgbackrest != nil && !static {
		fresh.PgBackRestPlugin, fresh.PgBackRestObject = pgbackrest.Plugin, pgbackrest.ObjectName
	}
	if !static && !epochCurrent(agent, out.snapshot) {
		reason := reasonOtherEpoch
		if agent.Status.ProducerEpoch <= 0 {
			reason = reasonNoEpoch
		}
		if !retained || !epochCurrent(agent, next.result.Snapshot) {
			next.result = fresh
		}
		next.result.State, next.result.Reason = viewmodel.EvidenceStale, reason
		return next
	}
	next.result = fresh
	if out.snapshot.Stale {
		next.result.State, next.result.Reason = viewmodel.EvidenceStale, reasonAgentStale
	}
	return next
}

// keepRecovery keeps the last good recovery paths when reading them
// failed transiently: they turn stale with the failure's reason instead
// of vanishing, as a failed refresh keeps a retained snapshot. A
// definitive answer (unsupported, not scanned) replaces them. The kept
// paths keep their own scan time (ScannedAt), which the timeline's edges
// are timed from.
func keepRecovery(previous, current Recovery) Recovery {
	if current.State != RecoveryUnavailable || previous.State != RecoveryAvailable || len(previous.Paths) == 0 {
		return current
	}
	previous.Stale, previous.StaleReason = true, current.Reason
	return previous
}

// logTransition logs a change of state or reason, never a token, a CA
// or a body; the transport cause is debug-only.
func (r *Runner) logTransition(agent *pgtoolboxv1alpha1.EvidenceAgent, previous, next *agentState, cause error) {
	if previous != nil && previous.result.State == next.result.State && previous.result.Reason == next.result.Reason {
		return
	}
	attributes := []any{
		slog.String("namespace", agent.Namespace), slog.String("evidenceAgent", agent.Name),
		slog.String("state", string(next.result.State)), slog.String("reason", next.result.Reason),
	}
	switch next.result.State {
	case viewmodel.EvidenceStale, viewmodel.EvidenceUnavailable:
		r.logger.Warn("evidence agent state changed", attributes...)
		if cause != nil {
			r.logger.Debug("evidence agent fetch failure detail", slog.String("namespace", agent.Namespace),
				slog.String("evidenceAgent", agent.Name), slog.String("error", errorDetail(cause)))
		}
	default:
		r.logger.Info("evidence agent state changed", attributes...)
	}
}

// errorDetail is the wrapped cause of a fetch error, for debug logs.
func errorDetail(err error) string {
	var typed *fetchError
	if errors.As(err, &typed) && typed.cause != nil {
		return typed.cause.Error()
	}
	return err.Error()
}

// pruneClients drops the clients of agents no longer listed.
func (r *Runner) pruneClients(live, kept map[string]bool) {
	for key, cached := range r.clients {
		namespace, _, _ := strings.Cut(key, "/")
		if live[key] || kept[namespace] {
			continue
		}
		cached.client.CloseIdleConnections()
		delete(r.clients, key)
	}
}

// closeClients releases every idle connection at shutdown.
func (r *Runner) closeClients() {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	for key, cached := range r.clients {
		cached.client.CloseIdleConnections()
		delete(r.clients, key)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	publishAgentStates(r.states, nil)
}
