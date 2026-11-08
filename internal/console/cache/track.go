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

package cache

import (
	"context"
	"sort"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	toolscache "k8s.io/client-go/tools/cache"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// sourceKey is one source in one namespace; cluster-scoped kinds use the
// empty namespace.
type sourceKey struct {
	namespace, source string
}

// tracker holds the state of every source. It is the console's account
// of what it can and cannot see: a broken watch is stale, never healthy
// (AGENTS.md, console invariants).
type tracker struct {
	mu     sync.RWMutex
	now    func() time.Time
	states map[sourceKey]viewmodel.SourceState
	// failing holds, per source, the informers (by id) whose last list or
	// watch failed. The informer layer does not say which namespace a list
	// belongs to, so a fault is attributed to the source in every
	// namespace it is watched in: over-reporting staleness is the honest
	// direction.
	failing map[string]map[int]bool
}

func newTracker(now func() time.Time) *tracker {
	return &tracker{now: now, states: map[sourceKey]viewmodel.SourceState{}, failing: map[string]map[int]bool{}}
}

// set moves one source to a state; Since changes only on a transition.
func (t *tracker) set(namespace, source string, state viewmodel.SourceStateKind, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.setLocked(sourceKey{namespace, source}, state, reason)
}

func (t *tracker) setLocked(key sourceKey, state viewmodel.SourceStateKind, reason string) {
	current, ok := t.states[key]
	if ok && current.State == state && current.Reason == reason {
		return
	}
	since := t.now().UTC()
	if ok && current.State == state {
		since = *current.Since
	}
	t.states[key] = viewmodel.SourceState{Source: key.source, State: state, Since: &since, Reason: reason}
}

// watchedOutcome records one list/watch call of an informer of source:
// failure makes every namespace of the source stale (keeping its
// last-good objects), success makes it live again once nothing fails.
func (t *tracker) watchedOutcome(source string, informer int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
			// A relist is the normal answer to an expired resource version,
			// not a lost source.
			return
		}
		if t.failing[source] == nil {
			t.failing[source] = map[int]bool{}
		}
		t.failing[source][informer] = true
		reason := "watch lost: " + redact.Safe(err)
		if apierrors.IsForbidden(err) {
			reason = "the console ServiceAccount is no longer allowed to watch this source"
		}
		for key, state := range t.states {
			if key.source == source && (state.State == viewmodel.SourceLive || state.State == viewmodel.SourceStale) {
				t.setLocked(key, viewmodel.SourceStale, reason)
			}
		}
		return
	}
	delete(t.failing[source], informer)
	if len(t.failing[source]) > 0 {
		return
	}
	for key, state := range t.states {
		if key.source == source && state.State == viewmodel.SourceStale {
			t.setLocked(key, viewmodel.SourceLive, "")
		}
	}
}

func (t *tracker) get(namespace, source string) (viewmodel.SourceState, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	state, ok := t.states[sourceKey{namespace, source}]
	return state, ok
}

// forNamespace lists the namespace's sources (and the cluster-scoped
// ones), sorted by source key.
func (t *tracker) forNamespace(namespace string) []viewmodel.SourceState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []viewmodel.SourceState
	for key, state := range t.states {
		if key.namespace == namespace || key.namespace == "" {
			out = append(out, state)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

// trackedListWatch reports every list and watch outcome of one informer
// to the tracker, so a source whose API server path breaks turns stale
// on the next attempt rather than staying "live" on its last good list.
type trackedListWatch struct {
	inner   toolscache.ListerWatcherWithContext
	outcome func(error)
	source  string
}

var (
	_ toolscache.ListerWatcher            = trackedListWatch{}
	_ toolscache.ListerWatcherWithContext = trackedListWatch{}
)

func (lw trackedListWatch) ListWithContext(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
	obj, err := lw.inner.ListWithContext(ctx, options)
	if ctx.Err() == nil {
		lw.outcome(err)
		informerCalls.WithLabelValues(lw.source, "list", outcomeLabel(err)).Inc()
	}
	return obj, err
}

func (lw trackedListWatch) WatchWithContext(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
	w, err := lw.inner.WatchWithContext(ctx, options)
	if ctx.Err() == nil {
		informerCalls.WithLabelValues(lw.source, "watch", outcomeLabel(err)).Inc()
	}
	if err != nil && ctx.Err() == nil {
		lw.outcome(err)
	}
	return w, err
}

// List and Watch satisfy the context-free interface the informer
// constructor is typed with; the informer calls the context forms.
func (lw trackedListWatch) List(options metav1.ListOptions) (runtime.Object, error) {
	return lw.ListWithContext(context.Background(), options)
}

func (lw trackedListWatch) Watch(options metav1.ListOptions) (watch.Interface, error) {
	return lw.WatchWithContext(context.Background(), options)
}
