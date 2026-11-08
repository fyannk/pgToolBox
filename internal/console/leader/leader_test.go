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

package leader

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/fyannk/pgToolBox/internal/console/digest"
	"github.com/fyannk/pgToolBox/internal/console/timeline"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

var leases = schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}

// apiServer is one namespace's Leases with the API server's optimistic
// concurrency: an update must carry the resourceVersion it read.
type apiServer struct {
	mu        sync.Mutex
	lease     *coordinationv1.Lease
	rv        int
	forbidden bool
	writes    int
}

func (a *apiServer) Get(_ context.Context, name string, _ metav1.GetOptions) (*coordinationv1.Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.forbidden:
		return nil, apierrors.NewForbidden(leases, name, errors.New("no"))
	case a.lease == nil:
		return nil, apierrors.NewNotFound(leases, name)
	}
	return a.lease.DeepCopy(), nil
}

func (a *apiServer) Create(_ context.Context, lease *coordinationv1.Lease, _ metav1.CreateOptions) (*coordinationv1.Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.forbidden:
		return nil, apierrors.NewForbidden(leases, lease.Name, errors.New("no"))
	case a.lease != nil:
		return nil, apierrors.NewAlreadyExists(leases, lease.Name)
	}
	a.rv++
	a.writes++
	a.lease = lease.DeepCopy()
	a.lease.ResourceVersion = strconv.Itoa(a.rv)
	return a.lease.DeepCopy(), nil
}

func (a *apiServer) Update(_ context.Context, lease *coordinationv1.Lease, _ metav1.UpdateOptions) (*coordinationv1.Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.forbidden:
		return nil, apierrors.NewForbidden(leases, lease.Name, errors.New("no"))
	case a.lease == nil:
		return nil, apierrors.NewNotFound(leases, lease.Name)
	case lease.ResourceVersion != a.lease.ResourceVersion:
		return nil, apierrors.NewConflict(leases, lease.Name, errors.New("stale"))
	}
	a.rv++
	a.writes++
	a.lease = lease.DeepCopy()
	a.lease.ResourceVersion = strconv.Itoa(a.rv)
	return a.lease.DeepCopy(), nil
}

func (a *apiServer) holder() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lease == nil || a.lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *a.lease.Spec.HolderIdentity
}

func fast(api LeaseClient, identity string, lead func(context.Context), logger *slog.Logger) *Elector {
	return New(Options{
		Client: api, Namespace: "team-a", Name: "console-digest", Identity: identity, Lead: lead, Logger: logger,
		LeaseDuration: 600 * time.Millisecond, RenewDeadline: 400 * time.Millisecond, RetryPeriod: 50 * time.Millisecond,
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// start runs e until the returned stop is called.
func start(e *Elector) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

// marks is the delivery marks the state ConfigMap shares.
type marks struct {
	mu   sync.Mutex
	sent map[string]time.Time
}

func (m *marks) DigestSent(id, channel string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.sent[id+"/"+channel]
	return at, ok
}

func (m *marks) MarkDigestSent(id, channel string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sent[id+"/"+channel]; !ok {
		m.sent[id+"/"+channel] = at
	}
}

func (m *marks) MarksKnown() bool { return true }

// countingChannel counts the messages sent, across replicas.
type countingChannel struct{ n *atomic.Int32 }

func (c countingChannel) Name() string { return "chat" }

func (c countingChannel) Send(context.Context, digest.Message) error {
	c.n.Add(1)
	return nil
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Wait(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

type emptySource struct{}

func (emptySource) ClusterList(context.Context) (viewmodel.ClusterList, error) {
	return viewmodel.ClusterList{}, nil
}

func (emptySource) DigestEntries(context.Context, time.Time, time.Time) map[viewmodel.ClusterRef][]viewmodel.TimelineEntry {
	return nil
}

func (emptySource) SourceFacts() []timeline.SourceFacts { return nil }

// consoleReplica is one replica's digest service holding today's digest.
func consoleReplica(t *testing.T, shared *marks, sent *atomic.Int32) *digest.Service {
	t.Helper()
	store, err := digest.NewStore(30*24*time.Hour, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	due := digest.Slot(time.Now().Add(-digest.DefaultGrace), 7)
	d, err := digest.Generate(context.Background(), emptySource{}, nil, due, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	store.Add(d, time.Now())
	return digest.NewService(digest.Options{
		Hour: 7, Retention: 30 * 24 * time.Hour, Persistent: true, Console: "team-a", Backoff: []time.Duration{0},
		Clock: realClock{}, Source: emptySource{}, Store: store, Channels: []digest.Channel{countingChannel{sent}},
		Marks: shared, Elected: true,
	})
}

// Two in-process replicas elect one leader; only it delivers. When it
// stops, the other takes the Lease over (released at once) and sends
// nothing the first had sent.
func TestOneLeaderDeliversAndFailoverDoesNotResend(t *testing.T) {
	api := &apiServer{}
	shared := &marks{sent: map[string]time.Time{}}
	var sent atomic.Int32
	var leaders atomic.Int32
	var maxLeaders atomic.Int32
	wrap := func(s *digest.Service) func(context.Context) {
		return func(ctx context.Context) {
			if n := leaders.Add(1); n > maxLeaders.Load() {
				maxLeaders.Store(n)
			}
			defer leaders.Add(-1)
			s.Lead(ctx)
		}
	}
	a := fast(api, "console-0_a", wrap(consoleReplica(t, shared, &sent)), nil)
	b := fast(api, "console-1_b", wrap(consoleReplica(t, shared, &sent)), nil)
	stopA := start(a)
	eventually(t, "A to lead", a.Leading)
	stopB := start(b)
	defer stopB()
	eventually(t, "the leader's delivery", func() bool { return sent.Load() == 1 })
	eventually(t, "B to see the leader", func() bool { return b.State().State == viewmodel.SourceLive })
	if b.Leading() || b.State().State != viewmodel.SourceLive || api.holder() != "console-0_a" {
		t.Fatalf("B leading=%v state=%+v holder=%q", b.Leading(), b.State(), api.holder())
	}

	stopA() // a roll: A releases the Lease on its way out
	if a.Leading() {
		t.Fatal("A still leads after stopping")
	}
	eventually(t, "B to take over", b.Leading)
	time.Sleep(100 * time.Millisecond) // B's catch-up has run
	if got := sent.Load(); got != 1 {
		t.Fatalf("the digest was sent %d times across the failover, want once", got)
	}
	if maxLeaders.Load() != 1 {
		t.Fatalf("%d replicas led at once", maxLeaders.Load())
	}
}

// Without the right to the Lease no replica leads, so nothing is sent
// twice; the admin page and the log (once) say why.
func TestForbiddenLeaseNeverLeads(t *testing.T) {
	api := &apiServer{forbidden: true}
	var logs bytes.Buffer
	var mu sync.Mutex
	var led atomic.Bool
	e := fast(api, "console-0_a", func(context.Context) { led.Store(true) }, slog.New(slog.NewTextHandler(syncWriter{&mu, &logs}, nil)))
	stop := start(e)
	eventually(t, "the forbidden state", func() bool { return e.State().State == viewmodel.SourceForbidden })
	time.Sleep(200 * time.Millisecond) // a few more retries
	stop()
	if led.Load() || e.Leading() {
		t.Fatal("a replica led without the right to the Lease")
	}
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(logs.String(), "may not read or write its digest Lease"); n != 1 {
		t.Fatalf("warned %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(e.State().Reason, "no replica delivers") {
		t.Fatalf("reason = %q", e.State().Reason)
	}
}

// The lock writes exactly the Lease: created when absent, then updated
// with the resourceVersion read (a stale one is a conflict, not a
// second holder).
func TestLeaseLockWritesTheOneLease(t *testing.T) {
	api := &apiServer{}
	var errs []error
	lock := &leaseLock{client: api, namespace: "team-a", name: "console-digest", identity: "me", observe: func(err error) { errs = append(errs, err) }}
	if _, _, err := lock.Get(t.Context()); !apierrors.IsNotFound(err) {
		t.Fatalf("get before create: %v", err)
	}
	if err := lock.Update(t.Context(), myRecord()); err == nil {
		t.Fatal("an update before any read must fail")
	}
	if err := lock.Create(t.Context(), myRecord()); err != nil {
		t.Fatal(err)
	}
	if err := lock.Update(t.Context(), myRecord()); err != nil {
		t.Fatal(err)
	}
	// Another writer moves the Lease on: this lock's copy is stale.
	api.mu.Lock()
	api.rv++
	api.lease.ResourceVersion = strconv.Itoa(api.rv)
	api.mu.Unlock()
	if err := lock.Update(t.Context(), myRecord()); !apierrors.IsConflict(err) {
		t.Fatalf("stale update: %v", err)
	}
	if api.writes != 2 || api.holder() != "me" || api.lease.Name != "console-digest" || api.lease.Namespace != "team-a" {
		t.Fatalf("writes=%d lease=%+v", api.writes, api.lease)
	}
	if lock.Describe() != "team-a/console-digest" || lock.Identity() != "me" {
		t.Fatal("describe/identity")
	}
}

func myRecord() resourcelock.LeaderElectionRecord {
	now := metav1.Now()
	return resourcelock.LeaderElectionRecord{HolderIdentity: "me", LeaseDurationSeconds: 15, AcquireTime: now, RenewTime: now}
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
