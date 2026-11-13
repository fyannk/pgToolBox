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

// Package leader elects one replica of a console for the work only one
// may do, one election per Purpose: delivering the daily digest (Lease
// DIGEST_LEASE), serving MCP (Lease MCP_LEASE). It runs client-go's
// leader election on a coordination.k8s.io Lease, through a lock of its
// own (leaseLock) so that the console's writes stay where the checker
// (hack/readonlycheck) can pin them: one Create (the Lease does not exist
// yet; RBAC cannot scope create by name) and one Update (renewal,
// hand-over), both on that Lease.
//
// A replica that cannot reach or may not write the Lease never leads:
// the work waits rather than being done twice. It logs one warning per
// transition and says so on the admin page (the Purpose's source).
package leader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// LeaseClient is the slice of the Lease client the election needs: get,
// create and update, matching the console's Role (get and update on the
// Lease by name, create on leases).
type LeaseClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*coordinationv1.Lease, error)
	Create(ctx context.Context, lease *coordinationv1.Lease, opts metav1.CreateOptions) (*coordinationv1.Lease, error)
	Update(ctx context.Context, lease *coordinationv1.Lease, opts metav1.UpdateOptions) (*coordinationv1.Lease, error)
}

// Timings of the election, client-go's usual values: a leader that stops
// renewing is replaced within about LeaseDuration; a graceful stop hands
// the Lease over at once (ReleaseOnCancel).
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// Purpose is what an election is for: its admin page source, the name
// of its Lease in the logs, the work the leader does (a phrase after
// "this replica"), and its gauge (none for nil).
type Purpose struct {
	Source, Noun, Work string
	gauge              prometheus.Gauge
}

// The elections of the console.
var (
	Digest = Purpose{Source: "digestLeader", Noun: "digest", Work: "delivers the daily digests", gauge: leading}
	MCP    = Purpose{Source: "mcpLeader", Noun: "MCP", Work: "serves MCP"}
)

// Options configure an Elector.
type Options struct {
	Client LeaseClient
	// Purpose of the election; Digest when zero.
	Purpose Purpose
	// Namespace and Name are the Lease's.
	Namespace, Name string
	// Identity is this process's, unique among the replicas (the pod name
	// and a random suffix: a restarted pod is a new candidate).
	Identity string
	// Lead runs while this replica leads; its context ends when the
	// leadership does.
	Lead func(ctx context.Context)
	// Timings; the defaults above when zero.
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
	Now                                       func() time.Time
	Logger                                    *slog.Logger
}

// health is how the last call on the Lease went.
type health string

const (
	healthOK        health = "ok"
	healthForbidden health = "forbidden"
	healthFailing   health = "failing"
)

// Elector runs the election.
type Elector struct {
	opts Options

	mu sync.Mutex
	// leading is set between OnStartedLeading and OnStoppedLeading;
	// holder is the last leader observed (any replica); health is the
	// Lease's reachability. The admin source derives from the three.
	leading bool
	holder  string
	health  health
	since   time.Time
}

// New builds an elector; nothing is contacted until Run.
func New(opts Options) *Elector {
	if opts.Purpose.Source == "" {
		opts.Purpose = Digest
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = DefaultLeaseDuration
	}
	if opts.RenewDeadline <= 0 {
		opts.RenewDeadline = DefaultRenewDeadline
	}
	if opts.RetryPeriod <= 0 {
		opts.RetryPeriod = DefaultRetryPeriod
	}
	return &Elector{opts: opts, health: healthOK, since: opts.Now().UTC()}
}

// Run takes part in the election until ctx ends: it leads whenever it
// holds the Lease, then stands again. On a graceful stop it releases the
// Lease, so another replica takes over without waiting for it to expire.
func (e *Elector) Run(ctx context.Context) error {
	lock := &leaseLock{client: e.opts.Client, namespace: e.opts.Namespace, name: e.opts.Name, identity: e.opts.Identity, observe: e.observe}
	// client-go logs through klog; hand it the console's logger.
	ctx = klog.NewContext(ctx, logr.FromSlogHandler(e.opts.Logger.Handler()))
	for ctx.Err() == nil {
		elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			Name:            e.opts.Name,
			LeaseDuration:   e.opts.LeaseDuration,
			RenewDeadline:   e.opts.RenewDeadline,
			RetryPeriod:     e.opts.RetryPeriod,
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: e.lead,
				OnStoppedLeading: e.stopped,
				OnNewLeader:      e.newLeader,
			},
		})
		if err != nil {
			return fmt.Errorf("leader election on Lease %s: %w", e.opts.Name, err)
		}
		elector.Run(ctx)
	}
	return nil
}

func (e *Elector) lead(ctx context.Context) {
	e.mu.Lock()
	e.leading, e.holder = true, e.opts.Identity
	e.mu.Unlock()
	e.setGauge(1)
	e.opts.Logger.Info("this replica now "+e.opts.Purpose.Work, slog.String("lease", e.opts.Name))
	if e.opts.Lead != nil {
		e.opts.Lead(ctx)
	}
}

func (e *Elector) stopped() {
	e.mu.Lock()
	was := e.leading
	e.leading = false
	e.mu.Unlock()
	if was {
		e.setGauge(0)
		e.opts.Logger.Info("this replica no longer "+e.opts.Purpose.Work, slog.String("lease", e.opts.Name))
	}
}

func (e *Elector) setGauge(v float64) {
	if e.opts.Purpose.gauge != nil {
		e.opts.Purpose.gauge.Set(v)
	}
}

func (e *Elector) newLeader(identity string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.holder = identity
}

// observe records the outcome of a call on the Lease: a refusal or a
// failure stands until a call succeeds again. NotFound on a read is the
// Lease not created yet, and a conflict another replica's write: neither
// is a failure.
func (e *Elector) observe(err error) {
	h := healthOK
	switch {
	case err == nil || apierrors.IsNotFound(err) || apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
	case apierrors.IsForbidden(err):
		h = healthForbidden
	case errors.Is(err, context.Canceled):
		return
	default:
		h = healthFailing
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.health == h {
		return
	}
	previous := e.health
	e.health, e.since = h, e.opts.Now().UTC()
	log := e.opts.Logger.With(slog.String("lease", e.opts.Name))
	p := e.opts.Purpose
	switch h {
	case healthForbidden:
		log.Warn("the console may not read or write its " + p.Noun + " Lease: no replica " + p.Work + " until it may")
	case healthFailing:
		log.Warn("the " + p.Noun + " Lease is unreachable; retrying (no replica " + p.Work + " meanwhile)")
	case healthOK:
		log.Info("the "+p.Noun+" Lease is reachable again", slog.String("was", string(previous)))
	}
}

// Leading reports whether this replica holds the Lease.
func (e *Elector) Leading() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.leading
}

// State is the election as a source of the admin page.
func (e *Elector) State() viewmodel.SourceState {
	e.mu.Lock()
	defer e.mu.Unlock()
	since := e.since
	st := viewmodel.SourceState{Source: e.opts.Purpose.Source, Since: &since}
	switch {
	case e.health == healthForbidden:
		st.State, st.Reason = viewmodel.SourceForbidden,
			"the console may not read or write Lease "+e.opts.Name+": no replica "+e.opts.Purpose.Work
	case e.health == healthFailing:
		st.State, st.Reason = viewmodel.SourceUnavailable,
			"Lease "+e.opts.Name+" is unreachable: no replica "+e.opts.Purpose.Work+" until it is"
	case e.leading || e.holder != "":
		// Held, by this replica or another: the work has a doer.
		st.State = viewmodel.SourceLive
	default:
		st.State = viewmodel.SourcePending
	}
	return st
}

// CandidateIdentity is this process's name in an election: the pod name
// (the host name in a pod) and a random suffix, so that a pod restarted
// under the same name is a new candidate and waits for its old self's
// Lease to lapse instead of renewing it.
func CandidateIdentity() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "pgtoolbox-console"
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	return host + "_" + hex.EncodeToString(suffix)
}

// leaseLock is client-go's resourcelock.LeaseLock written against
// LeaseClient, so that its two writes are the console's own code.
type leaseLock struct {
	client                    LeaseClient
	namespace, name, identity string
	observe                   func(error)

	mu    sync.Mutex
	lease *coordinationv1.Lease
}

var _ resourcelock.Interface = (*leaseLock)(nil)

// Get reads the Lease's election record.
func (l *leaseLock) Get(ctx context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	lease, err := l.client.Get(ctx, l.name, metav1.GetOptions{})
	l.observe(err)
	if err != nil {
		return nil, nil, err
	}
	l.mu.Lock()
	l.lease = lease
	l.mu.Unlock()
	record := resourcelock.LeaseSpecToLeaderElectionRecord(&lease.Spec)
	raw, err := json.Marshal(*record)
	if err != nil {
		return nil, nil, err
	}
	return record, raw, nil
}

// Create creates the Lease, held by this replica.
func (l *leaseLock) Create(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: l.name, Namespace: l.namespace},
		Spec:       resourcelock.LeaderElectionRecordToLeaseSpec(&record),
	}
	created, err := l.client.Create(ctx, lease, metav1.CreateOptions{FieldManager: "pgtoolbox-console"})
	l.observe(err)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.lease = created
	l.mu.Unlock()
	return nil
}

// Update writes the record over the Lease last read, with its
// resourceVersion: a replica that lost a race gets a conflict.
func (l *leaseLock) Update(ctx context.Context, record resourcelock.LeaderElectionRecord) error {
	l.mu.Lock()
	if l.lease == nil {
		l.mu.Unlock()
		return errors.New("lease not read yet")
	}
	lease := l.lease.DeepCopy()
	l.mu.Unlock()
	lease.Spec = resourcelock.LeaderElectionRecordToLeaseSpec(&record)
	updated, err := l.client.Update(ctx, lease, metav1.UpdateOptions{FieldManager: "pgtoolbox-console"})
	l.observe(err)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.lease = updated
	l.mu.Unlock()
	return nil
}

// RecordEvent records nothing: the console's identity creates no Event.
func (l *leaseLock) RecordEvent(string) {}

// Identity is this replica's.
func (l *leaseLock) Identity() string { return l.identity }

// Describe names the Lease.
func (l *leaseLock) Describe() string { return l.namespace + "/" + l.name }
