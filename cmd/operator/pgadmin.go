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
	"os"
	"reflect"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	securityv1 "github.com/openshift/api/security/v1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/configuration"
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
}

// startupStepTimeout bounds each start-up step (one or many API reads, see
// startupStep): an API server that accepts the connection and never answers
// fails the start, naming the step, instead of hanging the operator before
// its probes are served. The health endpoint is served only once the
// manager starts, so the manager's liveness probe (first at 15 s, every
// 20 s, three failures: killed about 55 s after the start) counts the
// start-up against it; the deadline stays well under that, so the named
// error is what the log shows (TestStartupStepBeforeLivenessKill).
const startupStepTimeout = 20 * time.Second

// preparePgAdmin loads the configuration (defaults, env, ConfigMap, Secret;
// one structured error) and probes the optional APIs with a scoped
// List(Limit 1), through a client that reads past any cache.
func preparePgAdmin(ctx context.Context, operatorImage, configMapName, secretName string) (pgAdminPrepared, error) {
	direct, err := startupClient(ctrl.GetConfigOrDie())
	if err != nil {
		return pgAdminPrepared{}, fmt.Errorf("build API client: %w", err)
	}
	if operatorImage != "" {
		configuration.DefaultOperatorImage = operatorImage
	}
	prepared, err := preparePgAdminWith(ctx, direct, metrics.Registry, startupStepTimeout, configuration.Objects{
		Namespace: os.Getenv("POD_NAMESPACE"), ConfigMap: configMapName, Secret: secretName,
	})
	if err != nil {
		return pgAdminPrepared{}, err
	}
	registerBuildInfo()
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
// timeout.
func startupConfig(cfg *rest.Config) *rest.Config {
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = startupStepTimeout
	return cfg
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
// timeout, the configuration info metric registered on registry once the
// watched namespaces are known (it lists those actually watched).
func preparePgAdminWith(ctx context.Context, direct client.Reader, registry prometheus.Registerer, timeout time.Duration, objects configuration.Objects) (pgAdminPrepared, error) {
	loaded, err := startupRead(ctx, timeout, "load the configuration", func(ctx context.Context) (configuration.Loaded, error) {
		return configuration.Load(ctx, direct, os.LookupEnv, objects)
	})
	if err != nil {
		return pgAdminPrepared{}, err
	}
	if err := loaded.OverrideFlag("PGADMIN_IMAGE_REPOSITORIES", pgAdminImageRepositories); err != nil {
		return pgAdminPrepared{}, err
	}
	// A watched namespace the operator holds no right in (deleted after the
	// install, and its RoleBinding with it) is left out of the watch
	// rather than failing the start or turning features off elsewhere.
	namespaces := loaded.Config.WatchNamespaces
	watched, err := startupRead(ctx, timeout, "check the watched namespaces", func(ctx context.Context) ([]string, error) {
		return reachableNamespaces(ctx, direct, namespaces)
	})
	if err != nil {
		return pgAdminPrepared{}, err
	}
	loaded.Config.WatchNamespaces = watched
	if err := loaded.RegisterInfoMetric(registry); err != nil {
		return pgAdminPrepared{}, err
	}
	features, err := startupRead(ctx, timeout, "probe the optional APIs", func(ctx context.Context) (pgadmin.Features, error) {
		return probeFeatures(ctx, direct, watched)
	})
	if err != nil {
		return pgAdminPrepared{}, err
	}
	setupLog.Info("PgAdmin optional APIs",
		"routes", features.Routes.State, "gateways", features.Gateways.State,
		"databaseRoles", features.DatabaseRoles.State, "poolers", features.Poolers.State,
		"openshift", features.OpenShift)
	return pgAdminPrepared{Config: loaded.Config, Features: features}, nil
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

// probeFeatures checks every optional API with a scoped List(Limit 1) in
// every watched namespace (pkg/discovery): off when not served or
// forbidden, start fails on any other error. The CloudNativePG Cluster API
// is required; SecurityContextConstraints are probed cluster-wide and a
// Forbidden answer still means OpenShift.
func probeFeatures(ctx context.Context, reader client.Reader, namespaces []string) (pgadmin.Features, error) {
	var features pgadmin.Features
	probes := []struct {
		target *discovery.Result
		gvk    schema.GroupVersionKind
	}{
		{&features.Routes, routev1.GroupVersion.WithKind("Route")},
		{&features.Gateways, schema.GroupVersionKind{Group: gatewayv1.GroupName, Version: "v1", Kind: "HTTPRoute"}},
		{&features.DatabaseRoles, cnpgv1.SchemeGroupVersion.WithKind("DatabaseRole")},
		{&features.Poolers, cnpgv1.SchemeGroupVersion.WithKind("Pooler")},
		{&features.Databases, cnpgv1.SchemeGroupVersion.WithKind("Database")},
	}
	for _, probe := range probes {
		result, err := discovery.ProbeNamespaces(ctx, reader, probe.gvk, namespaces)
		if err != nil {
			return features, err
		}
		*probe.target = result
	}
	openShift, err := discovery.ProbeSCC(ctx, reader)
	if err != nil {
		return features, err
	}
	features.OpenShift = openShift
	clusters, err := discovery.ProbeNamespaces(ctx, reader, cnpgv1.SchemeGroupVersion.WithKind("Cluster"), namespaces)
	if err != nil {
		return features, err
	}
	if !clusters.Usable() {
		return features, errors.New("the CloudNativePG Cluster API is required: " + clusters.Reason)
	}
	return features, nil
}

// reachableNamespaces keeps the watched namespaces where the operator may
// list CloudNativePG Clusters (the one kind every controller needs) and logs
// each one it is refused in: a namespace deleted after the install takes the
// chart's RoleBinding with it, and a recreated one needs `helm upgrade` (the
// RoleBinding) and a restart of the operator before it is watched again. At
// least one namespace must answer; empty (every namespace) stays empty.
func reachableNamespaces(ctx context.Context, reader client.Reader, namespaces []string) ([]string, error) {
	if len(namespaces) == 0 {
		return nil, nil
	}
	reachable, refused, err := discovery.ReachableNamespaces(ctx, reader, cnpgv1.SchemeGroupVersion.WithKind("Cluster"), namespaces)
	if err != nil {
		return nil, err
	}
	for _, r := range refused {
		setupLog.Error(errors.New(r.Reason), "watched namespace left out: deleted, or the operator is not bound in it (helm upgrade, then restart the operator)",
			"namespace", r.Namespace)
	}
	if len(reachable) == 0 {
		return nil, fmt.Errorf("none of the watched namespaces %v lets the operator list CloudNativePG Clusters: they do not exist or the operator is not bound in them", namespaces)
	}
	return reachable, nil
}

func registerBuildInfo() {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_build_info",
		Help: "Build information of the pgToolBox operator.",
	}, []string{"version"})
	info.WithLabelValues(operatorVersion).Set(1)
	_ = metrics.Registry.Register(info)
}
