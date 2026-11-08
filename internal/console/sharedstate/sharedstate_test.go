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

package sharedstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// apiServer is one ConfigMap with the API server's optimistic
// concurrency: an update must carry the resourceVersion it read, and
// every change is broadcast to the open watches.
type apiServer struct {
	mu       sync.Mutex
	cm       *corev1.ConfigMap
	rv       int
	watchers []*watch.RaceFreeFakeWatcher
	// forbidden refuses every call; conflicts is how many more updates
	// to refuse as if another writer had won.
	forbidden bool
	conflicts int
	updates   int
	// compacted refuses a watch from any resourceVersion, as the API
	// server does once that version is older than the history it keeps.
	compacted bool
}

var configMaps = schema.GroupResource{Resource: "configmaps"}

func newAPIServer() *apiServer {
	return &apiServer{cm: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", Namespace: "team-a", ResourceVersion: "1"}}, rv: 1}
}

func (a *apiServer) Get(_ context.Context, name string, _ metav1.GetOptions) (*corev1.ConfigMap, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.forbidden:
		return nil, apierrors.NewForbidden(configMaps, name, errors.New("no"))
	case a.cm == nil || a.cm.Name != name:
		return nil, apierrors.NewNotFound(configMaps, name)
	}
	return a.cm.DeepCopy(), nil
}

func (a *apiServer) Watch(_ context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.forbidden {
		return nil, apierrors.NewForbidden(configMaps, "", errors.New("no"))
	}
	if a.compacted && opts.ResourceVersion != "" {
		return nil, apierrors.NewResourceExpired("too old resource version: " + opts.ResourceVersion)
	}
	w := watch.NewRaceFreeFake()
	a.watchers = append(a.watchers, w)
	return w, nil
}

func (a *apiServer) Update(_ context.Context, cm *corev1.ConfigMap, _ metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.forbidden:
		return nil, apierrors.NewForbidden(configMaps, cm.Name, errors.New("no"))
	case a.cm == nil:
		return nil, apierrors.NewNotFound(configMaps, cm.Name)
	case a.conflicts > 0 || cm.ResourceVersion != a.cm.ResourceVersion:
		if a.conflicts > 0 {
			a.conflicts--
			a.bumpLocked(a.cm.DeepCopy())
		}
		return nil, apierrors.NewConflict(configMaps, cm.Name, errors.New("stale"))
	}
	a.updates++
	a.bumpLocked(cm.DeepCopy())
	return a.cm.DeepCopy(), nil
}

// bumpLocked stores cm as the next version and tells the watches.
func (a *apiServer) bumpLocked(cm *corev1.ConfigMap) {
	a.rv++
	cm.ResourceVersion = strconv.Itoa(a.rv)
	a.cm = cm
	for _, w := range a.watchers {
		if !w.IsStopped() {
			w.Modify(cm.DeepCopy())
		}
	}
}

func (a *apiServer) data(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cm.Data[key]
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *testClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func newStore(api ConfigMapClient, clock *testClock, log *slog.Logger) *Store {
	return New(Options{
		Client: api, Name: "console-state", Now: clock.Now, Logger: log,
		Debounce: 5 * time.Millisecond, UrgentDebounce: time.Millisecond,
		Backoff: func(int) time.Duration { return 5 * time.Millisecond },
	})
}

func run(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestSinceMergeRules(t *testing.T) {
	clock := &testClock{now: t0}
	s := newStore(nil, clock, nil)
	early, late := t0.Add(-time.Hour), t0
	steps := []struct {
		name  string
		state string
		local time.Time
		want  time.Time
	}{
		{"first observation is shared", "ok", late, late},
		{"same state, earlier observation wins", "ok", early, early},
		{"same state, later observation reads the earlier", "ok", late, early},
		{"new state, later: replaces", "failing", late.Add(time.Minute), late.Add(time.Minute)},
		{"old state, earlier than the shared transition: kept local", "ok", early, early},
		{"the shared transition stays", "failing", late.Add(2 * time.Minute), late.Add(time.Minute)},
	}
	for _, step := range steps {
		if got := s.InstanceSince("pod-uid", step.state, step.local); !got.Equal(step.want) {
			t.Fatalf("%s: got %v, want %v", step.name, got, step.want)
		}
	}
	// Two objects never share a clock.
	if got := s.PhaseSince("cluster-uid", "ok", late); !got.Equal(late) {
		t.Fatalf("phase since = %v, want its own", got)
	}
}

func TestFindingsSinceClearsWhatARunNoLongerReports(t *testing.T) {
	s := newStore(nil, &testClock{now: t0}, nil)
	first := t0.Add(-time.Hour)
	got := s.FindingsSince("c", map[string]time.Time{"a": first, "b": first}, first)
	if !got["a"].Equal(first) || !got["b"].Equal(first) {
		t.Fatalf("got %v", got)
	}
	// A restarted replica reports "a" since now: it inherits the hour.
	if got := s.FindingsSince("c", map[string]time.Time{"a": t0}, t0); !got["a"].Equal(first) {
		t.Fatalf("a since %v, want %v", got["a"], first)
	}
	// "b" was not reported: cleared at t0. When it comes back, its clock
	// starts again rather than reaching back to the first episode.
	back := t0.Add(time.Minute)
	if got := s.FindingsSince("c", map[string]time.Time{"a": back, "b": back}, back); !got["b"].Equal(back) || !got["a"].Equal(first) {
		t.Fatalf("got %v, want b since %v and a since %v", got, back, first)
	}
}

func TestSignOutsExpire(t *testing.T) {
	clock := &testClock{now: t0}
	s := newStore(nil, clock, nil)
	s.RecordSignOut("0123456789abcdef0123456789abcdef", t0.Add(time.Hour))
	if !s.SignedOut("0123456789abcdef0123456789abcdef") || s.SignedOut("ffffffffffffffffffffffffffffffff") {
		t.Fatal("signed-out set wrong")
	}
	clock.Add(time.Hour)
	if s.SignedOut("0123456789abcdef0123456789abcdef") {
		t.Fatal("a sign-out outlived the session")
	}
	var nilStore *Store
	nilStore.RecordSignOut("x", t0)
	if nilStore.SignedOut("x") || !nilStore.InstanceSince("p", "s", t0).Equal(t0) {
		t.Fatal("a nil store shares nothing")
	}
}

func TestTheConfigMapNeverHoldsASessionIDOrAReason(t *testing.T) {
	api := newAPIServer()
	clock := &testClock{now: t0}
	s := newStore(api, clock, nil)
	run(t, s)
	sid := "0123456789abcdef0123456789abcdef"
	s.RecordSignOut(sid, t0.Add(time.Hour))
	s.InstanceSince("pod-uid", "unreachable\x00dial tcp 10.0.0.7:8000: i/o timeout", t0)
	eventually(t, "a write", func() bool { return api.data(KeySince) != "" && api.data(KeySessions) != "" })
	all := api.data(KeySessions) + api.data(KeySince)
	for _, secret := range []string{sid, "10.0.0.7", "timeout"} {
		if strings.Contains(all, secret) {
			t.Fatalf("the ConfigMap holds %q: %s", secret, all)
		}
	}
}

func TestTwoReplicasConverge(t *testing.T) {
	api := newAPIServer()
	clock := &testClock{now: t0}
	a, b := newStore(api, clock, nil), newStore(api, clock, nil)
	run(t, a)
	run(t, b)
	eventually(t, "both replicas reading", func() bool {
		return a.State().State == viewmodel.SourceLive && b.State().State == viewmodel.SourceLive
	})

	// A has seen the instance fail for an hour; B has just started.
	early := t0.Add(-time.Hour)
	a.InstanceSince("pod-uid", "unreachable", early)
	eventually(t, "B to learn A's clock", func() bool {
		return b.InstanceSince("pod-uid", "unreachable", t0).Equal(early)
	})

	// A sign-out on A is refused by B.
	sid := "fedcba9876543210fedcba9876543210"
	a.RecordSignOut(sid, t0.Add(time.Hour))
	eventually(t, "B to refuse A's signed-out session", func() bool { return b.SignedOut(sid) })

	// Both now write the same bytes and stop writing.
	eventually(t, "convergence", func() bool {
		a.mu.Lock()
		ad := a.encodeLocked()
		a.mu.Unlock()
		b.mu.Lock()
		bd := b.encodeLocked()
		b.mu.Unlock()
		return maps.Equal(ad, bd) && api.data(KeySince) == ad[KeySince]
	})
	api.mu.Lock()
	settled := api.updates
	api.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.updates != settled {
		t.Fatalf("replicas keep writing after converging: %d writes, then %d", settled, api.updates)
	}
}

// A ConfigMap left unchanged longer than the API server keeps history has
// a resourceVersion it no longer accepts a watch from: the replicas must
// still hear each other.
func TestAWatchSurvivesACompactedHistory(t *testing.T) {
	api := newAPIServer()
	api.compacted = true
	clock := &testClock{now: t0}
	a, b := newStore(api, clock, nil), newStore(api, clock, nil)
	run(t, a)
	run(t, b)
	sid := "fedcba9876543210fedcba9876543210"
	a.RecordSignOut(sid, t0.Add(time.Hour))
	eventually(t, "B to refuse A's signed-out session", func() bool { return b.SignedOut(sid) })
	if s := b.State(); s.State != viewmodel.SourceLive {
		t.Fatalf("B is %s, want live", s.State)
	}
}

func TestConflictIsReadMergedAndRetried(t *testing.T) {
	api := newAPIServer()
	// Another writer's sign-out lands first, and the next two updates lose
	// the race as well.
	other := sessionsDoc{Version: formatVersion, SignedOut: map[string]int64{sessionHash("other-session"): t0.Add(time.Hour).Unix()}}
	raw, _ := json.Marshal(other)
	api.cm.Data = map[string]string{KeySessions: string(raw)}
	clock := &testClock{now: t0}
	s := newStore(api, clock, nil)
	s.mu.Lock()
	s.mergeLocked(api.cm.DeepCopy())
	s.setModeLocked(modeLive, "")
	s.mu.Unlock()
	api.conflicts = 2
	s.RecordSignOut("mine", t0.Add(time.Hour))
	if err := s.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	var doc sessionsDoc
	if err := json.Unmarshal([]byte(api.data(KeySessions)), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.SignedOut) != 2 {
		t.Fatalf("signed out = %v, want both writers' sessions", doc.SignedOut)
	}
}

func TestBudgetDropsOldestSinceNeverASignOut(t *testing.T) {
	var logs bytes.Buffer
	clock := &testClock{now: t0}
	s := New(Options{Name: "console-state", Now: clock.Now, Budget: 4096, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	for i := range 20 {
		s.RecordSignOut(fmt.Sprintf("session-%02d", i), t0.Add(time.Hour))
	}
	for i := range 200 {
		s.InstanceSince(fmt.Sprintf("pod-%03d", i), "ok", t0.Add(time.Duration(i)*time.Second))
	}
	s.MarkDigestSent("20261105T0700Z", "chat", t0)
	s.mu.Lock()
	s.pruneLocked()
	size := s.encodedSizeLocked()
	s.mu.Unlock()
	if size > 4096 {
		t.Fatalf("documents take %d bytes, budget 4096", size)
	}
	if !s.SignedOut("session-00") || !s.SignedOut("session-19") {
		t.Fatal("a sign-out was dropped for the budget")
	}
	if _, sent := s.DigestSent("20261105T0700Z", "chat"); !sent {
		t.Fatal("a delivery mark was dropped for the budget")
	}
	s.mu.Lock()
	_, oldest := s.facts["i/pod-000"]
	_, newest := s.facts["i/pod-199"]
	s.mu.Unlock()
	if oldest || !newest {
		t.Fatal("want the oldest since dropped first, the newest kept")
	}
	if !strings.Contains(logs.String(), "over its size budget") {
		t.Fatalf("log = %s", logs.String())
	}
}

func TestPruneForgetsObjectsGoneFromTheCache(t *testing.T) {
	s := newStore(nil, &testClock{now: t0}, nil)
	s.InstanceSince("pod-gone", "ok", t0)
	s.InstanceSince("pod-here", "ok", t0)
	s.FindingsSince("cluster-gone", map[string]time.Time{"x": t0}, t0)
	s.setLive(map[string]bool{"pod-here": true})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.facts) != 1 || s.facts["i/pod-here"].Since == 0 {
		t.Fatalf("facts = %v, want only pod-here", s.facts)
	}
}

func TestMissingOrForbiddenDegradesToLocal(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*apiServer)
		want  viewmodel.SourceStateKind
	}{
		{"missing", func(a *apiServer) { a.cm = nil }, viewmodel.SourceUnavailable},
		{"forbidden", func(a *apiServer) { a.forbidden = true }, viewmodel.SourceForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newAPIServer()
			tc.setup(api)
			var logs bytes.Buffer
			var mu sync.Mutex
			s := newStore(api, &testClock{now: t0}, slog.New(slog.NewJSONHandler(syncWriter{&mu, &logs}, nil)))
			run(t, s)
			eventually(t, "the degraded state", func() bool { return s.State().State == tc.want })
			// Still works locally.
			early := t0.Add(-time.Hour)
			s.InstanceSince("pod", "ok", early)
			if got := s.InstanceSince("pod", "ok", t0); !got.Equal(early) {
				t.Fatalf("local since = %v", got)
			}
			s.RecordSignOut("sid", t0.Add(time.Hour))
			if !s.SignedOut("sid") {
				t.Fatal("a local sign-out is not honoured")
			}
			time.Sleep(30 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if n := strings.Count(logs.String(), "each replica keeps its own"); n != 1 {
				t.Fatalf("warned %d times, want once: %s", n, logs.String())
			}
		})
	}
}

type syncWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestForeignVersionIsLeftAlone(t *testing.T) {
	api := newAPIServer()
	api.cm.Data = map[string]string{KeySince: `{"version":2,"facts":{"x":1}}`}
	s := newStore(api, &testClock{now: t0}, nil)
	s.mu.Lock()
	s.mergeLocked(api.cm.DeepCopy())
	s.setModeLocked(modeLive, "")
	s.mu.Unlock()
	s.InstanceSince("pod", "ok", t0)
	s.RecordSignOut("sid", t0.Add(time.Hour))
	if err := s.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := api.data(KeySince); got != `{"version":2,"facts":{"x":1}}` {
		t.Fatalf("a newer console's document was rewritten: %s", got)
	}
	if api.data(KeySessions) == "" {
		t.Fatal("the document this console understands was not written")
	}
}

// The delivery marks' merge rule: a union, the earliest delivery time of
// a mark wins, and a mark once set is never unset by a replica that did
// not see it; marks past the retention go, malformed ones are not taken.
func TestDigestMarksMergeRules(t *testing.T) {
	clock := &testClock{now: t0}
	s := New(Options{Name: "console-state", Now: clock.Now, DigestRetention: 30 * 24 * time.Hour})
	id := "20261105T0700Z"
	s.MarkDigestSent(id, "chat", t0)
	// A later mark of the same delivery (a retry that was not needed, a
	// second replica) does not move the time.
	s.MarkDigestSent(id, "chat", t0.Add(time.Minute))
	if at, ok := s.DigestSent(id, "chat"); !ok || !at.Equal(t0) {
		t.Fatalf("chat sent %v %v, want %v", at, ok, t0)
	}
	if _, ok := s.DigestSent(id, "mail"); ok {
		t.Fatal("mail was never sent")
	}
	remote := digestsDoc{Version: formatVersion, Sent: map[string]int64{
		id + "/chat":                   t0.Add(-time.Minute).Unix(),         // earlier: wins
		id + "/mail":                   t0.Unix(),                           // new: taken
		"20261001T0700Z/webhook":       t0.Add(-40 * 24 * time.Hour).Unix(), // past retention + slack: pruned
		"20261104T0700Z/webhook":       t0.Add(-24 * time.Hour).Unix(),      // kept
		"../etc/chat":                  t0.Unix(),                           // malformed ID
		id + "/Chat":                   t0.Unix(),                           // malformed channel
		id + "/webhook":                0,                                   // no time
		"20261105T0700Z":               t0.Unix(),                           // no channel
		strings.Repeat("1", 65) + "/x": t0.Unix(),                           // too long
	}}
	raw, err := json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.mergeLocked(&corev1.ConfigMap{Data: map[string]string{KeyDigests: string(raw)}})
	got := maps.Clone(s.sent)
	s.mu.Unlock()
	want := map[string]int64{
		id + "/chat":             t0.Add(-time.Minute).Unix(),
		id + "/mail":             t0.Unix(),
		"20261104T0700Z/webhook": t0.Add(-24 * time.Hour).Unix(),
	}
	if !maps.Equal(got, want) {
		t.Fatalf("marks = %v, want %v", got, want)
	}
	// A document without the marks (an older console's write) unsets
	// nothing.
	s.mu.Lock()
	s.mergeLocked(&corev1.ConfigMap{Data: map[string]string{KeyDigests: `{"version":1,"sent":{}}`}})
	n := len(s.sent)
	s.mu.Unlock()
	if n != 3 {
		t.Fatalf("a merge unset marks: %d left", n)
	}
	// Past the retention, the marks go.
	clock.Add(33 * 24 * time.Hour)
	s.mu.Lock()
	s.pruneLocked()
	n = len(s.sent)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d marks outlived the retention", n)
	}
	var nilStore *Store
	nilStore.MarkDigestSent(id, "chat", t0)
	if _, ok := nilStore.DigestSent(id, "chat"); ok || !nilStore.MarksKnown() {
		t.Fatal("a nil store shares no marks and has nothing to wait for")
	}
}

// A mark set on one replica reaches the other through the ConfigMap, and
// the second replica's marks are known only once it has read it.
func TestDigestMarksReachTheOtherReplica(t *testing.T) {
	api := newAPIServer()
	clock := &testClock{now: t0}
	a, b := newStore(api, clock, nil), newStore(api, clock, nil)
	if b.MarksKnown() {
		t.Fatal("marks known before the ConfigMap was read")
	}
	run(t, a)
	run(t, b)
	eventually(t, "both replicas reading", func() bool { return a.MarksKnown() && b.MarksKnown() })
	a.MarkDigestSent("20261105T0700Z", "webhook", t0)
	eventually(t, "B to see A's mark", func() bool {
		_, ok := b.DigestSent("20261105T0700Z", "webhook")
		return ok
	})
	if !strings.Contains(api.data(KeyDigests), `"20261105T0700Z/webhook"`) {
		t.Fatalf("digests document = %s", api.data(KeyDigests))
	}
}

func TestDigestMarksKnownWhenTheConfigMapCannotBeRead(t *testing.T) {
	for name, setup := range map[string]func(*apiServer){
		"missing":   func(a *apiServer) { a.cm = nil },
		"forbidden": func(a *apiServer) { a.forbidden = true },
	} {
		t.Run(name, func(t *testing.T) {
			api := newAPIServer()
			setup(api)
			s := newStore(api, &testClock{now: t0}, nil)
			run(t, s)
			// Nothing to wait for: the local marks are all there is.
			eventually(t, "marks known", s.MarksKnown)
		})
	}
}

func TestDisabled(t *testing.T) {
	s := New(Options{})
	if s.Enabled() || s.State().State != viewmodel.SourceDisabled || !s.MarksKnown() {
		t.Fatalf("state = %+v", s.State())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
}
