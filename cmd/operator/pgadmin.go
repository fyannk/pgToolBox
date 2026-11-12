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

package main

// PgAdmin wiring of the operator binary. Kept in its own file, as the
// EvidenceAgent's is: main.go calls preparePgAdmin
// before the manager, pgAdminManagerOptions on the manager options, and
// setupPgAdmin once the manager exists, all with main's one ctx.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	securityv1 "github.com/openshift/api/security/v1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/configuration"
	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// pgAdminImageRepositories is --pgadmin-image-repositories: the
// repositories a PgAdmin's images may name, over PGADMIN_IMAGE_REPOSITORIES
// (env, operator ConfigMap or Secret). It mirrors the EvidenceAgent's
// --evidence-agent-image-repositories.
var pgAdminImageRepositories string

func init() {
	utilruntime.Must(pgtoolboxv1alpha1.AddToScheme(scheme))
	utilruntime.Must(securityv1.AddToScheme(scheme))
	flag.StringVar(&pgAdminImageRepositories, "pgadmin-image-repositories", "",
		"Comma-separated image repositories a PgAdmin's spec.image and oauth-proxy image may name (exact, or a prefix ending in '/'). "+
			"Overrides PGADMIN_IMAGE_REPOSITORIES. Empty everywhere: each image's default repository only.")
}

// pgAdminPrepared is what preparePgAdmin read before the manager exists.
type pgAdminPrepared struct {
	Config   configuration.Config
	Features pgadmin.Features
	// EvidenceAgentAPIs is the outcome of the EvidenceAgent's optional-API
	// probes, made with the PgAdmin ones before the manager exists.
	EvidenceAgentAPIs evidenceAgentAPIs
	// OptionalKinds is every optional kind's probe, PgAdmin's and
	// EvidenceAgent's: the cache watches each one only in the namespaces
	// where it can be listed (optionalKindCacheOptions).
	OptionalKinds []optionalKind
	// Dropped is the watched namespaces left out at start, and why.
	Dropped []droppedNamespace
	// leaderOnly is what the manager runs once this replica is elected
	// (the dropped-namespace report).
	leaderOnly []manager.Runnable
}

// evidenceAgentAPIs is the outcome of the EvidenceAgent's optional-API
// probes.
type evidenceAgentAPIs struct {
	ObjectStore, Stanza, Archive discovery.Result
}

// startupStepTimeout bounds each start-up step (one or many API reads, see
// startupStep): an API server that accepts the connection and never answers
// fails the start, naming the step, instead of hanging the operator before
// its probes are served. The health endpoint is served only once the
// manager starts, so the manager's liveness probe (first at 15 s, every
// 20 s, three failures: killed about 55 s after the start) counts the
// whole start-up against it, not one step. Every step of it shares
// startupBudgetTotal: the reads before the manager (configuration,
// namespaces, optional APIs), then the manager itself and each
// controller's setup, whose discovery requests go through the timed
// RESTMapper (each request under mapperRequestTimeout, but a slow API
// server can answer many of them in turn). Each step's deadline is the
// smaller of startupStepTimeout and what is left of the budget, and a
// step returns at its deadline even when its requests ignore their
// context, so an API server that is slow on every request, but answers,
// still fails the start with a named error before the kill
// (TestStartupStepBeforeLivenessKill, TestStartupBudgetCoversTheSetups).
const startupStepTimeout = 20 * time.Second

// startupBudgetTotal is the time the whole start-up may take, from the
// first read to the manager's start (see startupStepTimeout). It stays
// under the liveness kill with room for the process start before it and
// the manager's start after it (startupMargin).
const startupBudgetTotal = 40 * time.Second

// startupMargin is what the start-up takes outside the budget: the process
// start and flag parsing before it, and the manager's start after it until
// the health endpoint answers (it is the manager's first server).
const startupMargin = 5 * time.Second

// startupBudget is the start-up's deadline shared by its steps.
type startupBudget struct {
	step     time.Duration
	total    time.Duration
	deadline time.Time
	now      func() time.Time
}

// newStartupBudget starts a budget of total for steps of at most step each.
func newStartupBudget(step, total time.Duration) *startupBudget {
	return &startupBudget{step: step, total: total, deadline: time.Now().Add(total), now: time.Now}
}

// budgetedRead is startupRead under the budget: its deadline is the
// smaller of the step timeout and what is left of the budget; a budget
// already spent fails the step, naming it, without a read.
func budgetedRead[T any](ctx context.Context, budget *startupBudget, name string, read func(context.Context) (T, error)) (T, error) {
	left := budget.deadline.Sub(budget.now())
	if left <= 0 {
		var zero T
		return zero, fmt.Errorf("start-up step %q not started: the start-up budget of %s is spent", name, budget.total)
	}
	value, err := startupRead(ctx, min(budget.step, left), name, read)
	if err != nil && left < budget.step && errors.Is(err, context.DeadlineExceeded) {
		return value, fmt.Errorf("%w (the start-up budget of %s is spent)", err, budget.total)
	}
	return value, err
}

// preparePgAdmin loads the configuration (defaults, env, ConfigMap, Secret;
// one structured error) and probes the optional APIs with a scoped
// List(Limit 1), through a client that reads past any cache.
//
// A namespace left out because its review failed is reviewed again in the
// background (reReviewFailed), which calls restart once it can be watched.
func preparePgAdmin(ctx context.Context, budget *startupBudget, operatorImage, configMapName, secretName string, restart context.CancelCauseFunc) (pgAdminPrepared, error) {
	direct, err := startupClient(ctrl.GetConfigOrDie())
	if err != nil {
		return pgAdminPrepared{}, fmt.Errorf("build API client: %w", err)
	}
	if operatorImage != "" {
		configuration.DefaultOperatorImage = operatorImage
	}
	prepared, err := preparePgAdminWith(ctx, direct, metrics.Registry, budget, configuration.Objects{
		Namespace: os.Getenv("POD_NAMESPACE"), ConfigMap: configMapName, Secret: secretName,
	})
	if err != nil {
		return pgAdminPrepared{}, err
	}
	registerBuildInfo()
	// Beside the start, never in its budget: the objects of a namespace
	// left out say so on themselves where the operator may still write,
	// from the elected replica only; a namespace whose review failed is
	// reviewed again on every replica.
	if len(prepared.Dropped) > 0 {
		prepared.leaderOnly = append(prepared.leaderOnly, droppedReport{api: direct, dropped: prepared.Dropped, now: time.Now})
		go reReviewFailed(ctx, direct, prepared.Dropped, defaultReReviewPolicy(processStarted), restart)
	}
	return prepared, nil
}

// startupClient is a client for the start-up reads only, on a copy of cfg
// with a request timeout: the lazy RESTMapper's discovery requests take no
// context, so without it a hung API server would hold a step past its
// deadline (startupStep returns at the deadline anyway; the timeout ends
// the request it leaves behind). The manager keeps cfg as it is: a client
// timeout would also cut its watches.
func startupClient(cfg *rest.Config) (client.Client, error) {
	return client.New(startupConfig(cfg), client.Options{Scheme: scheme})
}

// startupConfig is the start-up client's copy of cfg, with its request
// timeout, and the access reviews' responses read through
// managerrole.ReviewResponseLimit: one namespace's administrator can grow
// that namespace's rules review without bound (one Role bound many times),
// and must fail that namespace's review alone, not exhaust the memory of
// the operator of every namespace. It speaks JSON: protobuf packs the
// worst shapes of a review (thousands of one-letter verbs or minimal
// rules) several times tighter than JSON for the same decoded heap, so
// the same limit would let several times more heap in.
func startupConfig(cfg *rest.Config) *rest.Config {
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = startupStepTimeout
	cfg.ContentType = runtime.ContentTypeJSON
	cfg.AcceptContentTypes = runtime.ContentTypeJSON
	cfg.Wrap(managerrole.LimitReviewResponses)
	return cfg
}

// mapperRequestTimeout bounds each discovery request of the manager's
// RESTMapper. The controllers' setups (IndexField, then the informer it
// starts) resolve their kinds through it before the manager starts, and
// its discovery requests take no context: without a timeout, a hung API
// server would hold the setup until the liveness probe kills the
// container, with no error named. Discovery documents are small; a
// request still unanswered after this is failed, and the setup with it.
const mapperRequestTimeout = 10 * time.Second

// timedMapperProvider is the manager's MapperProvider: controller-runtime's
// dynamic RESTMapper on a copy of the manager's config whose requests time
// out after timeout, with its own HTTP client (the shared one carries no
// timeout, and must not: a client timeout would cut the watches).
func timedMapperProvider(timeout time.Duration) func(*rest.Config, *http.Client) (meta.RESTMapper, error) {
	return func(cfg *rest.Config, _ *http.Client) (meta.RESTMapper, error) {
		cfg = rest.CopyConfig(cfg)
		cfg.Timeout = timeout
		httpClient, err := rest.HTTPClientFor(cfg)
		if err != nil {
			return nil, fmt.Errorf("build the RESTMapper's HTTP client: %w", err)
		}
		return apiutil.NewDynamicRESTMapper(cfg, httpClient)
	}
}

// startupStep runs one start-up step under its own deadline and names the
// step in its error.
func startupStep(ctx context.Context, timeout time.Duration, name string, run func(context.Context) error) error {
	_, err := startupRead(ctx, timeout, name, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, run(ctx)
	})
	return err
}

// startupRead is startupStep for a step that returns a value. It returns at
// the deadline even when read does not: a request that ignores its context
// (the RESTMapper's discovery) cannot hold the start past it. read is then
// left to finish on its own; its result goes only to a buffered channel
// nobody reads, never to a variable of the caller.
func startupRead[T any](ctx context.Context, timeout time.Duration, name string, read func(context.Context) (T, error)) (T, error) {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := read(stepCtx)
		done <- outcome{value, err}
	}()
	var result outcome
	select {
	case result = <-done:
	case <-stepCtx.Done():
		result.err = stepCtx.Err()
	}
	switch {
	case result.err == nil:
		return result.value, nil
	case ctx.Err() == nil && errors.Is(stepCtx.Err(), context.DeadlineExceeded):
		var zero T
		return zero, fmt.Errorf("start-up step %q did not finish within %s: %w", name, timeout, result.err)
	default:
		var zero T
		return zero, fmt.Errorf("start-up step %q: %w", name, result.err)
	}
}

// preparePgAdminWith is preparePgAdmin past the client: each API read under
// the budget, the configuration info metric registered on registry once the
// watched namespaces are known (it lists those actually watched).
func preparePgAdminWith(ctx context.Context, direct startupAPI, registry prometheus.Registerer, budget *startupBudget, objects configuration.Objects) (pgAdminPrepared, error) {
	loaded, err := budgetedRead(ctx, budget, "load the configuration", func(ctx context.Context) (configuration.Loaded, error) {
		return configuration.Load(ctx, direct, os.LookupEnv, objects)
	})
	if err != nil {
		return pgAdminPrepared{}, err
	}
	if err := loaded.OverrideFlag("PGADMIN_IMAGE_REPOSITORIES", pgAdminImageRepositories); err != nil {
		return pgAdminPrepared{}, err
	}
	// A watched namespace the operator holds no right in (deleted after the
	// install, and its RoleBinding with it), or only some of the rights it
	// needs (its administrator bound the operator to a narrower Role), is
	// left out of the watch rather than failing the start for every
	// namespace: an informer refused in one namespace never syncs.
	type split struct {
		kept    []string
		dropped []droppedNamespace
	}
	namespaces := loaded.Config.WatchNamespaces
	reachable, err := budgetedRead(ctx, budget, "check the watched namespaces", func(ctx context.Context) (split, error) {
		kept, dropped, err := reachableNamespaces(ctx, direct, namespaces)
		return split{kept, dropped}, err
	})
	if err != nil {
		return pgAdminPrepared{}, err
	}
	permitted, err := budgetedRead(ctx, budget, "check the operator's permissions in the watched namespaces", func(ctx context.Context) (split, error) {
		kept, dropped, err := permittedNamespaces(ctx, direct, reachable.kept)
		return split{kept, dropped}, err
	})
	if err != nil {
		return pgAdminPrepared{}, err
	}
	watched := permitted.kept
	dropped := append(slices.Clone(reachable.dropped), permitted.dropped...)
	// An optional API refused in some watched namespaces only is off in
	// those namespaces alone; no namespace is left out for it.
	probed, err := budgetedRead(ctx, budget, "probe the optional APIs", func(ctx context.Context) (optionalAPIs, error) {
		return probeOptionalAPIs(ctx, direct, watched)
	})
	if err != nil {
		return pgAdminPrepared{}, err
	}
	loaded.Config.WatchNamespaces = probed.Namespaces
	if err := loaded.RegisterInfoMetric(registry); err != nil {
		return pgAdminPrepared{}, err
	}
	if err := registerWatchMetrics(registry, countDropped(dropped), probed.Kinds); err != nil {
		return pgAdminPrepared{}, err
	}
	features := probed.Features
	setupLog.Info("PgAdmin optional APIs",
		"routes", features.Routes.State, "gateways", features.Gateways.State,
		"databaseRoles", features.DatabaseRoles.State, "poolers", features.Poolers.State,
		"openshift", features.OpenShift)
	return pgAdminPrepared{Config: loaded.Config, Features: features, EvidenceAgentAPIs: probed.EvidenceAgent, OptionalKinds: probed.Kinds, Dropped: dropped}, nil
}

// pgAdminManagerOptions adds the PgAdmin controller's needs to the shared
// manager's options: label-filtered informers for the kinds it owns (a
// kind another controller already filters keeps that filter: every
// generated object carries managed-by=pgtoolbox, the common selector),
// managedFields stripped, Secret and ConfigMap content never cached, the
// namespaces to watch, and the leader-election behaviour.
func pgAdminManagerOptions(options *ctrl.Options, prepared pgAdminPrepared) {
	if options.Cache.ByObject == nil {
		options.Cache.ByObject = map[client.Object]cache.ByObject{}
	}
	present := map[reflect.Type]bool{}
	for object := range options.Cache.ByObject {
		present[reflect.TypeOf(object)] = true
	}
	for object, byObject := range pgadmin.CacheByObject(prepared.Features) {
		if !present[reflect.TypeOf(object)] {
			options.Cache.ByObject[object] = byObject
		}
	}
	if options.Cache.DefaultTransform == nil {
		options.Cache.DefaultTransform = cache.TransformStripManagedFields()
	}
	if options.Client.Cache == nil {
		options.Client.Cache = &client.CacheOptions{}
	}
	for _, object := range []client.Object{&corev1.Secret{}, &corev1.ConfigMap{}} {
		disabled := false
		for _, existing := range options.Client.Cache.DisableFor {
			disabled = disabled || reflect.TypeOf(existing) == reflect.TypeOf(object)
		}
		if !disabled {
			options.Client.Cache.DisableFor = append(options.Client.Cache.DisableFor, object)
		}
	}
	if namespaces := prepared.Config.WatchNamespaces; len(namespaces) > 0 && options.Cache.DefaultNamespaces == nil {
		options.Cache.DefaultNamespaces = map[string]cache.Config{}
		for _, namespace := range namespaces {
			options.Cache.DefaultNamespaces[namespace] = cache.Config{}
		}
	}
	lease, renew, retry := prepared.Config.LeaseDuration, prepared.Config.RenewDeadline, prepared.Config.RetryPeriod
	shutdown := 30 * time.Second
	options.LeaseDuration, options.RenewDeadline, options.RetryPeriod = &lease, &renew, &retry
	// main exits right after Start returns, so releasing the lease on
	// cancellation hands over at once instead of after a lease period.
	options.LeaderElectionReleaseOnCancel = true
	options.GracefulShutdownTimeout = &shutdown
}

// setupPgAdmin registers the PgAdmin controller and its per-CR gauges
// (pgtoolbox_pgadmin_info, pgtoolbox_pgadmin_condition), computed on
// scrape from the manager's cache.
func setupPgAdmin(ctx context.Context, mgr ctrl.Manager, prepared pgAdminPrepared) error {
	if err := metrics.Registry.Register(pgadmin.Collector{Reader: mgr.GetCache()}); err != nil {
		return err
	}
	return (&pgadmin.Reconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorder("pgadmin"),
		Config:    prepared.Config,
		Features:  prepared.Features,
	}).SetupWithManager(ctx, mgr)
}

// optionalAPIs is what probeOptionalAPIs found: the PgAdmin features, the
// EvidenceAgent's optional APIs, every optional kind's probe, and the
// namespaces watched.
type optionalAPIs struct {
	Features      pgadmin.Features
	EvidenceAgent evidenceAgentAPIs
	Kinds         []optionalKind
	// Namespaces is the namespaces watched (nil: every namespace): those
	// given, always; an optional kind never leaves one out.
	Namespaces []string
}

// optionalKind is one optional kind's probe, with the object its informer
// is keyed by in the cache options.
type optionalKind struct {
	Object client.Object
	Result discovery.Result
}

// metadataObject is the cache key of a kind watched metadata-only.
func metadataObject(gvk schema.GroupVersionKind) client.Object {
	object := &metav1.PartialObjectMetadata{}
	object.SetGroupVersionKind(gvk)
	return object
}

// probeOptionalAPIs checks every optional API, the PgAdmin's and the
// EvidenceAgent's, with a scoped List(Limit 1) in every watched namespace
// (pkg/discovery.ProbeKinds): off when not served or refused in every
// namespace; refused in some namespaces only, off in those namespaces alone
// (watched and used in the others; logged, and counted in
// pgtoolbox_optional_api_refused_namespaces). No namespace is left out for
// an optional kind: a namespace's administrator can narrow or widen the
// operator's Role there, and that must decide nothing for another
// namespace. The start fails on any other error. The CloudNativePG Cluster
// API is required (reachableNamespaces already left out the namespaces that
// refuse it); SecurityContextConstraints are probed cluster-wide and a
// Forbidden answer still means OpenShift.
func probeOptionalAPIs(ctx context.Context, reader client.Reader, namespaces []string) (optionalAPIs, error) {
	probed := optionalAPIs{Namespaces: namespaces}
	var clusters discovery.Result
	features := &probed.Features
	probes := apiProbes(&probed, &clusters)
	gvks := make([]schema.GroupVersionKind, len(probes))
	for i, probe := range probes {
		gvks[i] = probe.gvk
	}
	decision, err := discovery.ProbeKinds(ctx, reader, gvks, namespaces)
	if err != nil {
		return probed, err
	}
	for i, probe := range probes {
		result := decision.Results[i]
		*probe.target = result
		if probe.object == nil {
			continue
		}
		probed.Kinds = append(probed.Kinds, optionalKind{Object: probe.object, Result: result})
		if len(result.RefusedIn) > 0 {
			setupLog.Info("optional API refused in some watched namespaces: off there only (grant it in the operator's Role there, then restart the operator)",
				"kind", probe.gvk.Kind, "group", probe.gvk.Group, "namespaces", result.RefusedIn)
		}
	}
	if !clusters.Usable() {
		return probed, errors.New("the CloudNativePG Cluster API is required: " + clusters.Reason)
	}
	if len(clusters.RefusedIn) > 0 {
		// reachableNamespaces kept only namespaces that allowed it a moment
		// ago: the operator's RBAC changed during the start.
		return probed, fmt.Errorf("listing CloudNativePG Clusters became refused in %v during the start", clusters.RefusedIn)
	}
	openShift, err := discovery.ProbeSCC(ctx, reader)
	if err != nil {
		return probed, err
	}
	features.OpenShift = openShift
	return probed, nil
}

// apiProbe is one kind probeOptionalAPIs probes: where its result goes, and
// the object its informer is keyed by (nil for the required Cluster kind).
type apiProbe struct {
	target *discovery.Result
	gvk    schema.GroupVersionKind
	object client.Object
}

// apiProbes is the kinds probeOptionalAPIs probes, the Cluster kind first,
// each writing its result into probed (clusters for the Cluster kind).
func apiProbes(probed *optionalAPIs, clusters *discovery.Result) []apiProbe {
	features := &probed.Features
	evidence := &probed.EvidenceAgent
	return []apiProbe{
		{clusters, cnpgv1.SchemeGroupVersion.WithKind("Cluster"), nil},
		{&features.Routes, routev1.GroupVersion.WithKind("Route"), &routev1.Route{}},
		{&features.Gateways, schema.GroupVersionKind{Group: gatewayv1.GroupName, Version: "v1", Kind: "HTTPRoute"}, &gatewayv1.HTTPRoute{}},
		{&features.DatabaseRoles, cnpgv1.SchemeGroupVersion.WithKind("DatabaseRole"), &cnpgv1.DatabaseRole{}},
		{&features.Poolers, cnpgv1.SchemeGroupVersion.WithKind("Pooler"), &cnpgv1.Pooler{}},
		{&features.Databases, cnpgv1.SchemeGroupVersion.WithKind("Database"), &cnpgv1.Database{}},
		{&evidence.ObjectStore, discovery.ObjectStoreGVK, metadataObject(discovery.ObjectStoreGVK)},
		{&evidence.Stanza, discovery.StanzaGVK, metadataObject(discovery.StanzaGVK)},
		{&evidence.Archive, discovery.ArchiveGVK, metadataObject(discovery.ArchiveGVK)},
	}
}

// optionalGroupKinds is the optional kinds' GroupKinds: a namespace that
// refuses one turns that feature off there, it is not left out of the
// watch (permittedNamespaces asks for none of them).
func optionalGroupKinds() []schema.GroupKind {
	var kinds []schema.GroupKind
	for _, probe := range apiProbes(&optionalAPIs{}, &discovery.Result{}) {
		if probe.object != nil {
			kinds = append(kinds, probe.gvk.GroupKind())
		}
	}
	return kinds
}

// reachableNamespaces keeps the watched namespaces where the operator may
// list CloudNativePG Clusters (the one kind every controller needs) and logs
// each one it is refused in: a namespace deleted after the install takes the
// chart's RoleBinding with it, and a recreated one needs `helm upgrade` (the
// RoleBinding) and a restart of the operator before it is watched again. It
// returns the namespaces kept and those left out. At least one namespace
// must answer; empty (every namespace) stays empty.
func reachableNamespaces(ctx context.Context, reader client.Reader, namespaces []string) ([]string, []droppedNamespace, error) {
	if len(namespaces) == 0 {
		return nil, nil, nil
	}
	reachable, refused, err := discovery.ReachableNamespaces(ctx, reader, cnpgv1.SchemeGroupVersion.WithKind("Cluster"), namespaces)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range refused {
		setupLog.Error(errors.New(r.Reason), "watched namespace left out: deleted, or the operator is not bound in it (helm upgrade, then restart the operator)",
			"namespace", r.Namespace)
	}
	if len(reachable) == 0 {
		return nil, nil, fmt.Errorf("none of the watched namespaces %v lets the operator list CloudNativePG Clusters: they do not exist or the operator is not bound in them", namespaces)
	}
	return reachable, droppedFromReachable(refused), nil
}

func registerBuildInfo() {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_build_info",
		Help: "Build information of the pgToolBox operator.",
	}, []string{"version"})
	info.WithLabelValues(operatorVersion).Set(1)
	_ = metrics.Registry.Register(info)
}
