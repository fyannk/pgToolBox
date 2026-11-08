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

// Package logfollow feeds the diagnostics catalog's log rules in the
// console: one pkg/cnpgstatus/logstream runner and matcher per watched
// Cluster, following the running containers of its member pods with the
// console's ServiceAccount (the chart grants pods/log only with
// ALLOW_LOGS). The matcher keeps only what matched, never log text, so
// no corpus of database logs accumulates; a container whose stream is
// not open is reported Unread and its rules cannot clear:
// a console that is not listening has not ruled anything out.
package logfollow

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// Key names one Cluster.
type Key struct{ Namespace, Name string }

// Roster is the read model's account of what to follow.
type Roster interface {
	// Keys lists the watched Clusters.
	Keys(ctx context.Context) []Key
	// Members lists the running containers of the Cluster's member pods
	// (membership proven by the controller owner reference).
	Members(ctx context.Context, key Key) []logstream.Member
	// UID is the Cluster's UID, empty when it is not in the cache: what a
	// restored snapshot is matched against.
	UID(ctx context.Context, key Key) string
}

// Opener opens one container's follow stream in a namespace.
type Opener interface {
	FollowLogs(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error)
}

// Options wire a Follower.
type Options struct {
	Roster Roster
	Opener Opener
	Rules  []logstream.Rule
	// MaxAge expires an observation nothing has repeated.
	MaxAge time.Duration
	// Every is how often the set of Clusters is reconciled.
	Every  time.Duration
	Now    func() time.Time
	Logger *slog.Logger
}

type cluster struct {
	matcher *logstream.Matcher
	cancel  context.CancelFunc
	done    chan struct{}
	// uid is the Cluster's UID when the runner started.
	uid string
}

// Follower runs one runner per Cluster.
type Follower struct {
	opts Options
	mu   sync.Mutex
	set  map[Key]*cluster
	// pending holds what a snapshot restored until its Cluster is
	// followed (persist.go).
	pending   map[Key]PersistedCluster
	pendingAt time.Time
}

// New returns a Follower.
func New(o Options) *Follower {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Every <= 0 {
		o.Every = 30 * time.Second
	}
	return &Follower{opts: o, set: map[Key]*cluster{}}
}

type clock struct{ now func() time.Time }

func (c clock) Now() time.Time { return c.now() }

type namespaced struct {
	ns     string
	opener Opener
}

func (n namespaced) FollowLogs(ctx context.Context, pod, container string) (io.ReadCloser, error) {
	return n.opener.FollowLogs(ctx, n.ns, pod, container)
}

// Run reconciles the per-Cluster runners until ctx ends.
func (f *Follower) Run(ctx context.Context) error {
	t := time.NewTicker(f.opts.Every)
	defer t.Stop()
	f.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			f.stopAll()
			return nil
		case <-t.C:
			f.reconcile(ctx)
		}
	}
}

func (f *Follower) reconcile(ctx context.Context) {
	want := map[Key]bool{}
	for _, k := range f.opts.Roster.Keys(ctx) {
		want[k] = true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, c := range f.set {
		if !want[k] {
			c.cancel()
			<-c.done
			delete(f.set, k)
		}
	}
	f.expirePendingLocked()
	for k := range want {
		if _, ok := f.set[k]; ok {
			continue
		}
		matcher := logstream.NewMatcher(f.opts.Rules, f.opts.MaxAge, f.opts.Now)
		uid := f.opts.Roster.UID(ctx, k)
		f.restoreLocked(k, uid, matcher)
		key := k
		members := func() []logstream.Member { return f.opts.Roster.Members(ctx, key) }
		runner := logstream.NewRunner(logstream.NewSource(members, namespaced{ns: k.Namespace, opener: f.opts.Opener}), matcher,
			clock{f.opts.Now}, f.opts.Logger.With(slog.String("cluster", k.Namespace+"/"+k.Name)))
		cctx, cancel := context.WithCancel(ctx)
		c := &cluster{matcher: matcher, cancel: cancel, done: make(chan struct{}), uid: uid}
		go func() {
			defer close(c.done)
			_ = runner.Run(cctx)
		}()
		f.set[k] = c
	}
}

func (f *Follower) stopAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, c := range f.set {
		c.cancel()
		<-c.done
		delete(f.set, k)
	}
}

// Observations returns the Cluster's log observations for the
// diagnostics input; nil while the Cluster is not followed yet, which
// the catalog reports as "could not run".
func (f *Follower) Observations(namespace, name string) diagnose.LogObservations {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.set[Key{Namespace: namespace, Name: name}]; ok {
		return c.matcher
	}
	return nil
}

// ClientsetOpener follows logs with a clientset (the console's
// ServiceAccount). It asks for no backfill: with TailLines unset the API
// server would replay the container's whole log on every reconnect, and
// the matcher would count the past as the present.
type ClientsetOpener struct{ Client kubernetes.Interface }

// FollowLogs implements Opener.
func (o ClientsetOpener) FollowLogs(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error) {
	fromNow := int64(0)
	return o.Client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, Follow: true, TailLines: &fromNow}).Stream(ctx)
}
