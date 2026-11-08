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

package auth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
)

// The revocation list is a ConfigMap in the console's own
// namespace that the installing admin maintains; every replica watches it,
// so two replicas agree without a session database. The console's
// ServiceAccount may get and watch exactly that ConfigMap (resourceNames)
// and nothing else of its kind. Keys:
//
//	notBefore  RFC 3339 time: every session signed in earlier is revoked
//	users      one "<username> <RFC 3339 time>" per line: that user's
//	           sessions signed in earlier are revoked
//	sessions   one session ID per line (from the login.success audit line),
//	           or a bearer reference "bearer-<16 hex>" (from the mcp.call
//	           audit line's session) to refuse one raw token, or a grant
//	           ID "grant-<32 hex>" (an MCP client's console grant, from the
//	           mcp.grant audit line or the Connected assistants list); a
//	           session ID also ends the grants minted from that session
//
// Blank lines and lines starting with # are ignored. A list that does not
// parse is refused whole and the last good list stays in force: a typo
// must neither revoke everyone nor un-revoke anyone.
const (
	revocationKeyNotBefore = "notBefore"
	revocationKeyUsers     = "users"
	revocationKeySessions  = "sessions"
	maxRevocationEntries   = 10000
)

// RevocationList is one parsed snapshot.
type RevocationList struct {
	NotBefore time.Time
	Users     map[string]time.Time
	Sessions  map[string]struct{}
}

// Revoked reports whether a session of user, signed in at issuedAt with
// ID sid, is revoked.
func (l *RevocationList) Revoked(sid, user string, issuedAt time.Time) bool {
	if l == nil {
		return false
	}
	if !l.NotBefore.IsZero() && issuedAt.Before(l.NotBefore) {
		return true
	}
	if t, ok := l.Users[user]; ok && issuedAt.Before(t) {
		return true
	}
	_, ok := l.Sessions[sid]
	return ok
}

// ParseRevocationList parses the ConfigMap data.
func ParseRevocationList(data map[string]string) (*RevocationList, error) {
	l := &RevocationList{Users: map[string]time.Time{}, Sessions: map[string]struct{}{}}
	var errs []error
	if v := strings.TrimSpace(data[revocationKeyNotBefore]); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not an RFC 3339 time", revocationKeyNotBefore, v))
		}
		l.NotBefore = t
	}
	n := 0
	lines := func(key string, fn func(no int, line string)) {
		sc := bufio.NewScanner(strings.NewReader(data[key]))
		no := 0
		for sc.Scan() {
			no++
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			n++
			if n > maxRevocationEntries {
				errs = append(errs, fmt.Errorf("more than %d entries", maxRevocationEntries))
				return
			}
			fn(no, line)
		}
		if err := sc.Err(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}
	}
	lines(revocationKeyUsers, func(no int, line string) {
		fs := strings.Fields(line)
		if len(fs) != 2 {
			errs = append(errs, fmt.Errorf("%s line %d: want \"<username> <RFC 3339 time>\"", revocationKeyUsers, no))
			return
		}
		t, err := time.Parse(time.RFC3339, fs[1])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s line %d: %q is not an RFC 3339 time", revocationKeyUsers, no, fs[1]))
			return
		}
		if prev, ok := l.Users[fs[0]]; !ok || t.After(prev) {
			l.Users[fs[0]] = t
		}
	})
	lines(revocationKeySessions, func(no int, line string) {
		if !isSessionReference(line) {
			errs = append(errs, fmt.Errorf("%s line %d: not a session ID or bearer reference", revocationKeySessions, no))
			return
		}
		l.Sessions[line] = struct{}{}
	})
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return l, nil
}

// isSessionReference accepts a session ID (32 lowercase hex) or a bearer
// reference (BearerReference: "bearer-" and 16 lowercase hex).
func isSessionReference(s string) bool {
	hexOnly := func(v string, n int) bool {
		return len(v) == n && !strings.ContainsFunc(v, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) })
	}
	if ref, ok := strings.CutPrefix(s, bearerReferencePrefix); ok {
		return hexOnly(ref, 16)
	}
	if IsGrantReference(s) {
		return true
	}
	return hexOnly(s, 32)
}

// ConfigMapWatcher is the slice of the ConfigMap client the list needs:
// get and watch, matching the name-scoped Role.
type ConfigMapWatcher interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
}

// Revocations keeps the current list up to date from a watch. It is a
// runner: Run blocks until ctx ends.
type Revocations struct {
	client ConfigMapWatcher
	name   string
	log    *slog.Logger

	current atomic.Pointer[RevocationList]
	synced  atomic.Bool
	once    sync.Once
	ready   chan struct{}
	// backoff between failed watch attempts; tests shorten it.
	backoff func(attempt int) time.Duration
}

// NewRevocations watches ConfigMap name through client. With an empty
// name the list is disabled: always synced, never revoking.
func NewRevocations(client ConfigMapWatcher, name string, log *slog.Logger) *Revocations {
	r := &Revocations{client: client, name: name, log: log, ready: make(chan struct{})}
	r.backoff = func(attempt int) time.Duration {
		d := time.Second << min(attempt, 6)
		return min(d, time.Minute)
	}
	if name == "" {
		r.markSynced()
	}
	return r
}

// Enabled reports whether a list is configured.
func (r *Revocations) Enabled() bool { return r != nil && r.name != "" }

// Synced reports whether the list was read at least once; readiness waits
// for it, because serving before would ignore revocations.
func (r *Revocations) Synced() bool { return r == nil || r.synced.Load() }

// Ready is closed once Synced.
func (r *Revocations) Ready() <-chan struct{} { return r.ready }

// Revoked consults the current list.
func (r *Revocations) Revoked(sid, user string, issuedAt time.Time) bool {
	if r == nil {
		return false
	}
	return r.current.Load().Revoked(sid, user, issuedAt)
}

func (r *Revocations) markSynced() {
	r.synced.Store(true)
	r.once.Do(func() { close(r.ready) })
}

func (r *Revocations) apply(cm *corev1.ConfigMap) {
	var data map[string]string
	if cm != nil {
		data = cm.Data
	}
	l, err := ParseRevocationList(data)
	if err != nil {
		r.log.Error("revocation list refused; the previous list stays in force", "configmap", r.name, "error", err)
		revocationValid.Set(0)
		// Without a good list ever read, readiness stays false rather
		// than serving with no revocations at all.
		return
	}
	revocationValid.Set(1)
	r.current.Store(l)
	r.markSynced()
}

// Run watches the ConfigMap until ctx ends: get, then watch from that
// resourceVersion; on any watch end or error, get again. There is no
// polling: between events the loop blocks on the watch.
func (r *Revocations) Run(ctx context.Context) error {
	if r.name == "" {
		<-ctx.Done()
		return nil
	}
	attempt := 0
	for ctx.Err() == nil {
		err := r.watchOnce(ctx)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // cancellation is the clean end of Run; the watch error it caused is not a failure
		}
		if err != nil {
			attempt++
			r.log.Warn("revocation list watch failed; retrying", "configmap", r.name, "error", err, "attempt", attempt)
		} else {
			attempt = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.backoff(attempt)):
		}
	}
	return nil
}

func (r *Revocations) watchOnce(ctx context.Context) error {
	cm, err := r.client.Get(ctx, r.name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		r.apply(nil)
	case err != nil:
		return fmt.Errorf("get: %w", err)
	default:
		r.apply(cm)
	}
	// No resourceVersion: the watch starts at the current state and first
	// sends the ConfigMap as Added. Starting from cm.ResourceVersion fails
	// with "too old resource version" forever once the list has been left
	// unchanged longer than the API server keeps history.
	w, err := r.client.Watch(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", r.name).String(),
	})
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			switch ev.Type {
			case watch.Added, watch.Modified:
				if cm, ok := ev.Object.(*corev1.ConfigMap); ok && cm.Name == r.name {
					r.apply(cm)
				}
			case watch.Deleted:
				if cm, ok := ev.Object.(*corev1.ConfigMap); ok && cm.Name == r.name {
					r.apply(nil)
				}
			case watch.Error:
				return fmt.Errorf("watch error: %w", apierrors.FromObject(ev.Object))
			case watch.Bookmark:
			}
		}
	}
}
