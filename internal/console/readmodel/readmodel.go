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

// Package readmodel wires the console's read side — informer cache,
// /pg/status sweep, diagnostics runner, history and the read-model
// service — from one configuration, for cmd/console to start under its
// runner supervisor. It owns no goroutine itself: Runners
// lists what must run, and which of them are core (their exit makes the
// console not ready).
package readmodel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/diag"
	"github.com/fyannk/pgToolBox/internal/console/digest"
	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/history"
	"github.com/fyannk/pgToolBox/internal/console/leader"
	"github.com/fyannk/pgToolBox/internal/console/logfollow"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
	"github.com/fyannk/pgToolBox/internal/console/statesnap"
	"github.com/fyannk/pgToolBox/internal/console/status"
	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/timeline"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/logstream"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/volumetrend"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog"
)

// ReadModel is the assembled read side.
type ReadModel struct {
	Config      config.Console
	Cache       *cache.Manager
	Sweep       *sweep.Sweeper
	Diagnostics *diag.Runner
	// History is nil when HISTORY_ENABLED=false.
	History *history.History
	// Status is what the API serves.
	Status *status.Service
	// Evidence is the evidence-agent consumer; nil when EVIDENCE_ENABLED
	// is false.
	Evidence *evidence.Runner
	// Logs follows member containers for the diagnostics log rules; nil
	// unless LOG_STREAM_ENABLED and ALLOW_LOGS.
	Logs *logfollow.Follower
	// KubeVersion polls the API server version (discovery).
	KubeVersion *observe.KubeVersionPoller
	// Timeline records the incident timeline; nil when history is off.
	Timeline *timeline.Recorder
	// TimelineFile persists the timeline and the digests; nil unless the
	// history is persistent.
	TimelineFile *timeline.File
	// Digest makes the daily digest; nil when DIGEST_ENABLED is false.
	Digest *digest.Service
	// Trends is the long volume trend behind the storage forecasts;
	// TrendsJournal mirrors it next to HISTORY_PATH, nil when history is
	// not persistent (the trend then restarts empty with the console).
	Trends        *volumetrend.Store
	TrendsJournal *volumetrend.Persister
	// Snapshots mirrors the in-memory state (sweep rings, last readings,
	// metrics window, insight, log matches) next to HISTORY_PATH every
	// minute and at shutdown; nil when history is not persistent.
	Snapshots *statesnap.Snapshotter
	// Shared is the state the replicas share through STATE_CONFIGMAP
	// (sign-outs, "since" times, digest delivery marks); nil when it is
	// not set.
	Shared *sharedstate.Store
	// DigestLeader elects the replica that delivers the digests
	// (DIGEST_LEASE); nil when this console delivers alone or delivers
	// nothing.
	DigestLeader *leader.Elector
}

// trendsFile is the volume trend snapshot's name beside the history
// journal, on the same writable volume.
const trendsFile = "volume-trends.gob"

// serverVersion reads /version with the console's client: any
// authenticated identity may (system:discovery).
type serverVersion struct {
	d discovery.ServerVersionInterface
}

func (v serverVersion) FetchServerVersion(context.Context) (string, error) {
	info, err := v.d.ServerVersion()
	if err != nil {
		return "", fmt.Errorf("server version: %w", err)
	}
	return info.GitVersion, nil
}

// Identity is the console's own identity, for the admin page.
type Identity struct {
	Version   string
	Namespace string
	// PublicURL is the console's external origin (PUBLIC_URL), the base
	// of the digest links; empty sends no link.
	PublicURL string
}

// Runner is one long-running part of the read side.
type Runner struct {
	Name string
	Run  func(ctx context.Context) error
	// Core runners keep the console's data current; when one exits the
	// supervisor must restart it and report not-ready meanwhile.
	Core bool
}

// New builds the read side; nothing is contacted until the runners run.
// It fails on an unusable history journal, the only start-time I/O.
func New(cfg config.Console, restConfig *rest.Config, logger *slog.Logger, id Identity) (*ReadModel, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	hist, err := history.Open(cfg.History, logger.With(slog.String("component", "history")))
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	trends := volumetrend.NewStore(volumetrend.Limits{
		Every: cfg.VolumeTrend.Interval, Retention: cfg.VolumeTrend.Retention, MaxSeries: cfg.VolumeTrend.MaxSeries,
	})
	metricTrends.Store(trends)
	var trendsJournal *volumetrend.Persister
	if cfg.History.Persistent() {
		trendsJournal, err = volumetrend.OpenPersister(filepath.Join(filepath.Dir(cfg.History.Path), trendsFile), trends,
			observe.RealClock{}, logger.With(slog.String("component", "volume-trends")))
		if err != nil {
			return nil, fmt.Errorf("volume trends: %w", err)
		}
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	var svcRef *status.Service
	var shared *sharedstate.Store
	if cfg.StateConfigMap != "" {
		if id.Namespace == "" {
			return nil, fmt.Errorf("%s needs the console's namespace (POD_NAMESPACE)", config.EnvStateConfigMap)
		}
		shared = sharedstate.New(sharedstate.Options{
			Client: clientset.CoreV1().ConfigMaps(id.Namespace), Name: cfg.StateConfigMap,
			Live:            func(ctx context.Context) (map[string]bool, bool) { return svcRef.LiveUIDs(ctx) },
			DigestRetention: cfg.Digest.Retention,
			Logger:          logger.With(slog.String("component", "shared-state")),
		})
	}
	if cfg.Digest.Lease != "" && id.Namespace == "" {
		return nil, fmt.Errorf("%s needs the console's namespace (POD_NAMESPACE)", config.EnvDigestLease)
	}
	var tap func(cache.Kind, client.Object, bool)
	var seed func(cache.Kind, []string, []client.Object)
	if hist != nil {
		tap, seed = hist.Tap, hist.Seed
	}
	manager, err := cache.New(cache.Options{
		Config: cfg, RestConfig: restConfig, Tap: tap, Seed: seed,
		Logger: logger.With(slog.String("component", "cache")),
	})
	if err != nil {
		return nil, err
	}
	statusOpts := status.Options{
		Cache: manager, EventsMaxAge: cfg.EventsMaxAge, EventsMaxPerCluster: cfg.EventsMaxPerCluster,
		AllowClusterCatalogs: cfg.AllowClusterCatalogs,
		InsightPackConfigMap: cfg.InsightPackConfigMap,
		Console:              status.ConsoleInfo{Version: id.Version, Namespace: id.Namespace},
		Logger:               logger.With(slog.String("component", "status")),
		Trends:               trends, TrendsPersistent: trendsJournal != nil,
	}
	if hist != nil {
		statusOpts.History = hist
	}
	tl, err := openTimeline(cfg, logger)
	if err != nil {
		return nil, err
	}
	var dg *digest.Service
	if tl != nil {
		statusOpts.Timeline = tl.journal
		if cfg.Digest.Enabled {
			statusOpts.Digest = func() *viewmodel.DigestStatus { return dg.Status() }
		}
	}
	if cfg.MonitoringURL != "" {
		statusOpts.Monitoring = viewmodel.Monitoring{State: "configured", URL: cfg.MonitoringURL}
	}
	if cfg.Prometheus.Enabled() {
		prom, err := status.NewPrometheus(status.PrometheusOptions{
			URL: cfg.Prometheus.URL, TokenFile: cfg.Prometheus.TokenFile, CAFile: cfg.Prometheus.CAFile, Namespaced: cfg.Prometheus.Namespaced,
		})
		if err != nil {
			return nil, err
		}
		statusOpts.Prometheus, statusOpts.PrometheusRange = prom, cfg.Prometheus.Range
	}
	versions := observe.NewKubeVersionStore()
	statusOpts.KubeVersion = versions.CurrentKubeVersion
	versionPoller := observe.NewKubeVersionPoller(serverVersion{clientset.Discovery()}, versions, observe.RealClock{}, logger.With(slog.String("component", "kube-version")))
	var ev *evidence.Runner
	if cfg.Evidence.Enabled {
		// EVIDENCE_AGENTS_FILE (the pgtools chart's agents, without the
		// operator) replaces the EvidenceAgent listing.
		static := make([]evidence.StaticAgent, 0, len(cfg.Evidence.Agents))
		for _, agent := range cfg.Evidence.Agents {
			static = append(static, evidence.StaticAgent{
				Namespace: agent.Namespace, Cluster: agent.Cluster, URL: agent.URL, TokenFile: agent.TokenFile, CAFile: agent.CAFile,
			})
		}
		ev, err = evidence.New(evidence.Options{
			Lister:         manager.Reader(),
			Readable:       func(ns string) bool { return manager.Readable(ns, cache.SourceEvidenceAgents) },
			Namespaces:     cfg.WatchNamespaces,
			TokenDirectory: cfg.Evidence.TokenDirectory,
			Interval:       cfg.Evidence.Interval,
			Static:         static,
			Logger:         logger.With(slog.String("component", "evidence")),
		})
		if err != nil {
			return nil, fmt.Errorf("evidence: %w", err)
		}
		statusOpts.Evidence = ev
	}
	var follower *logfollow.Follower
	if cfg.LogFollow {
		follower = logfollow.New(logfollow.Options{
			Roster: lazyRoster{svc: &svcRef}, Opener: logfollow.ClientsetOpener{Client: clientset},
			Rules: catalog.LogRules(), MaxAge: cfg.LogMatchMaxAge,
			Logger: logger.With(slog.String("component", "log-follow")),
		})
		statusOpts.Logs = func(ns, name string) diagnose.LogObservations { return follower.Observations(ns, name) }
	}
	if shared != nil {
		statusOpts.Shared = shared
	}
	var (
		snapshots *statesnap.Snapshotter
		elector   *leader.Elector
	)
	statusOpts.Persistence = func() []viewmodel.SourceState {
		sources := []viewmodel.SourceState{snapshots.State(), shared.State()}
		if elector != nil {
			sources = append(sources, elector.State())
		}
		return sources
	}
	svc := status.New(statusOpts)
	svcRef = svc

	sweepOpts := sweep.Options{
		Interval: cfg.InstanceStatus.Interval, Timeout: cfg.InstanceStatus.Timeout,
		Concurrency: cfg.InstanceStatus.Concurrency, Retention: cfg.RingRetention, Metrics: cfg.MetricsEnabled,
		Trends: trends,
		Logger: logger.With(slog.String("component", "sweep")),
	}
	if shared != nil {
		sweepOpts.Shared = shared
	}
	if cfg.InstanceStatus.Enabled {
		sweepOpts.Bundle = sweep.NewCABundle(cfg.InstanceStatus.CAFile, sweepOpts.Logger)
	} else {
		sweepOpts.Disabled = config.EnvInstanceStatusEnabled + "=false"
	}
	sw := sweep.New(svc, sweepOpts)
	svc.SetSweep(sw)

	diagOpts := diag.Options{Interval: cfg.DiagnosticsInterval, Debounce: cfg.DiagnosticsDebounce, Logger: logger.With(slog.String("component", "diagnostics"))}
	diagOpts.Lifecycle = diag.NewLifecycleTable(cfg.LifecycleFile, diagOpts.Logger)
	if !cfg.DiagnosticsEnabled {
		diagOpts.Disabled = config.EnvDiagnosticsEnabled + "=false"
	}
	if tl != nil {
		diagOpts.OnRun = func(key cache.ClusterKey, run diag.Run) {
			if findings, unavailable, ok := status.RunFindings(run); ok {
				tl.recorder.Findings(key, run.At, findings, unavailable)
			}
		}
	}
	if shared != nil {
		diagOpts.Shared = shared
	}
	runner := diag.New(svc, manager, diagOpts)
	svc.SetDiagnostics(runner)

	rm := &ReadModel{
		Config: cfg, Cache: manager, Sweep: sw, Diagnostics: runner, History: hist, Status: svc, Evidence: ev,
		Logs: follower, KubeVersion: versionPoller, Trends: trends, TrendsJournal: trendsJournal, Shared: shared,
	}
	if cfg.History.Persistent() {
		snapshots = newSnapshots(filepath.Dir(cfg.History.Path), sw, follower, logger)
		// The only start-time read: what the last run left, held until
		// the sweep and the follower see the same objects again.
		snapshots.Load()
		rm.Snapshots = snapshots
	}
	if tl != nil {
		rm.Timeline, rm.TimelineFile = tl.recorder, tl.file
		if cfg.Digest.Enabled {
			dg, err = newDigest(cfg, id, svc, manager.Ready, tl, shared, logger.With(slog.String("component", "digest")))
			if err != nil {
				return nil, err
			}
			rm.Digest = dg
			if cfg.Digest.Lease != "" && cfg.Digest.Delivers() {
				elector = leader.New(leader.Options{
					Client: clientset.CoordinationV1().Leases(id.Namespace), Namespace: id.Namespace, Name: cfg.Digest.Lease,
					Identity: candidateIdentity(), Lead: dg.Lead,
					Logger: logger.With(slog.String("component", "digest-leader")),
				})
				rm.DigestLeader = elector
			}
		}
	}
	return rm, nil
}

// candidateIdentity is this process's name in the digest election: the
// pod name (the host name in a pod) and a random suffix, so that a pod
// restarted under the same name is a new candidate and waits for its old
// self's Lease to lapse instead of renewing it.
func candidateIdentity() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "pgtoolbox-console"
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	return host + "_" + hex.EncodeToString(suffix)
}

// newSnapshots builds the state snapshots of the in-memory stores, each
// a file of its own beside the history journal.
func newSnapshots(dir string, sw *sweep.Sweeper, follower *logfollow.Follower, logger *slog.Logger) *statesnap.Snapshotter {
	parts := []statesnap.Part{
		statesnap.Gob("sweep", sweep.SnapshotVersion, sweep.MaxSnapshotBytes, sw.Export,
			func(st sweep.PersistedState, _, now time.Time) (int, int) { return sw.Import(st, now) }),
	}
	if follower != nil {
		parts = append(parts, statesnap.Gob("log-matches", logfollow.SnapshotVersion, logfollow.MaxSnapshotBytes,
			func(time.Time) logfollow.PersistedState { return follower.Export() },
			func(st logfollow.PersistedState, _, now time.Time) (int, int) { return follower.Import(st, now) }))
	}
	return statesnap.New(statesnap.Options{
		Dir: dir, Clock: observe.RealClock{}, Logger: logger.With(slog.String("component", "state-snapshots")),
	}, parts...)
}

type timelineParts struct {
	journal  *timeline.Journal
	recorder *timeline.Recorder
	file     *timeline.File
}

// openTimeline builds the incident timeline beside the history: same
// switch, same persistence, a bound derived from the history's (one
// timeline event per revision kept, at least 200 per Cluster).
func openTimeline(cfg config.Console, logger *slog.Logger) (*timelineParts, error) {
	if !cfg.History.Enabled {
		return nil, nil
	}
	clock := observe.RealClock{}
	limits := timeline.Limits{MaxEvents: cfg.History.MaxRevisions, PerCluster: max(cfg.History.MaxRevisions/50, 200)}
	tl := &timelineParts{}
	var (
		previousAlive time.Time
		startDetail   string
	)
	if path := cfg.History.TimelinePath(); path != "" {
		file, err := timeline.Open(path, clock, clock, logger.With(slog.String("component", "timeline")))
		if err != nil {
			return nil, fmt.Errorf("timeline: %w", err)
		}
		journal, err := timeline.NewPersisted(limits, clock, file)
		if err != nil {
			return nil, fmt.Errorf("timeline: %w", err)
		}
		tl.file, tl.journal, previousAlive = file, journal, file.PreviousAlive()
		if aside := file.SetAside(); aside != "" {
			startDetail = "The timeline file was unreadable and was set aside as " + aside + "; the timeline and digests before this start are lost."
		}
	} else {
		tl.journal = timeline.New(limits, clock)
	}
	tl.recorder = timeline.NewRecorderWithStart(tl.journal, previousAlive, startDetail)
	return tl, nil
}

// newDigest builds the daily digest and its delivery channels, each off
// unless configured.
func newDigest(cfg config.Console, id Identity, svc *status.Service, ready func() bool, tl *timelineParts, shared *sharedstate.Store, logger *slog.Logger) (*digest.Service, error) {
	var persister digest.Persister
	if tl.file != nil {
		persister = tl.file
	}
	store, err := digest.NewStore(cfg.Digest.Retention, persister, logger)
	if err != nil {
		return nil, fmt.Errorf("digest: %w", err)
	}
	var channels []digest.Channel
	if cfg.Digest.ChatWebhookFile != "" || cfg.Digest.WebhookFile != "" {
		client := digest.NewHTTPClient()
		if cfg.Digest.ChatWebhookFile != "" {
			channels = append(channels, digest.NewChatChannel(cfg.Digest.ChatWebhookFile, client))
		}
		if cfg.Digest.WebhookFile != "" {
			channels = append(channels, digest.NewWebhookChannel(cfg.Digest.WebhookFile, client))
		}
	}
	if m := cfg.Digest.SMTP; m.Enabled() {
		channels = append(channels, digest.NewMailChannel(digest.MailOptions{
			Address: m.Address, ImplicitTLS: m.ImplicitTLS, Username: m.Username, PasswordFile: m.PasswordFile, From: m.From, To: m.To,
		}))
	}
	o := digest.Options{
		Hour: cfg.Digest.Hour, Retention: cfg.Digest.Retention, Persistent: tl.file != nil,
		Console: id.Namespace, PublicURL: id.PublicURL, Timeout: cfg.Digest.Timeout,
		Clock: observe.RealClock{}, Source: svc, Ready: ready, Journal: tl.journal, Store: store, Channels: channels, Logger: logger,
		// With a Lease, only its holder delivers (Lead).
		Elected: cfg.Digest.Lease != "" && len(channels) > 0,
	}
	if shared != nil {
		o.Marks = shared
	}
	return digest.NewService(o), nil
}

// Runners lists what the supervisor must run.
func (r *ReadModel) Runners() []Runner {
	runners := []Runner{
		{Name: "informer-cache", Run: r.Cache.Run, Core: true},
		{Name: "instance-status-sweep", Run: r.Sweep.Run, Core: true},
		{Name: "diagnostics", Run: r.Diagnostics.Run, Core: true},
		{Name: "status-clocks", Run: func(ctx context.Context) error { return r.Status.Run(ctx, r.Cache) }},
	}
	runners = append(runners, Runner{Name: "kube-version", Run: r.KubeVersion.Run})
	if r.Logs != nil {
		runners = append(runners, Runner{Name: "log-follow", Run: r.Logs.Run})
	}
	if r.Evidence != nil {
		runners = append(runners, Runner{Name: "evidence", Run: r.Evidence.Run})
	}
	if r.History.Persistent() {
		runners = append(runners, Runner{Name: "history-journal", Run: r.History.Run})
	}
	if r.Timeline != nil {
		runners = append(runners, Runner{Name: "timeline", Run: func(ctx context.Context) error { return r.Timeline.Run(ctx, r.Status, r.Cache) }})
	}
	if r.TimelineFile != nil {
		runners = append(runners, Runner{Name: "timeline-journal", Run: r.TimelineFile.Run})
	}
	if r.Digest != nil {
		runners = append(runners, Runner{Name: "digest", Run: r.Digest.Run})
	}
	if r.DigestLeader != nil {
		runners = append(runners, Runner{Name: "digest-leader", Run: r.DigestLeader.Run})
	}
	if r.TrendsJournal != nil {
		runners = append(runners, Runner{Name: "volume-trend-journal", Run: r.TrendsJournal.Run})
	}
	if r.Snapshots != nil {
		runners = append(runners, Runner{Name: "state-snapshots", Run: r.Snapshots.Run})
	}
	if r.Shared != nil {
		runners = append(runners, Runner{Name: "shared-state", Run: r.Shared.Run})
	}
	return runners
}

// Ready is the readiness of the read side: the informer cache synced.
func (r *ReadModel) Ready() bool { return r.Cache.Ready() }

// lazyRoster defers to the read model built after the follower.
type lazyRoster struct{ svc **status.Service }

func (l lazyRoster) Keys(ctx context.Context) []logfollow.Key {
	return (*l.svc).LogRoster().Keys(ctx)
}

func (l lazyRoster) Members(ctx context.Context, key logfollow.Key) []logstream.Member {
	return (*l.svc).LogRoster().Members(ctx, key)
}

func (l lazyRoster) UID(ctx context.Context, key logfollow.Key) string {
	return (*l.svc).LogRoster().UID(ctx, key)
}
