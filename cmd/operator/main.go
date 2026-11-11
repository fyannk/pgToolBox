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

// Command operator is the pgToolBox operator: the PgAdmin, EvidenceAgent,
// PgAccessRequest and PgToolBoxConsole controllers, plus the subcommands the PgAdmin Pod and Jobs run from this
// same binary (install, passfile-sync, pgadmin-register, pgadmin-backup, pgadmin-config-uri).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fyannk/pgToolBox/internal/operator/cachemetrics"
	"github.com/fyannk/pgToolBox/internal/operator/console"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin"
	"github.com/fyannk/pgToolBox/pkg/memlimit"
)

var (
	scheme               = runtime.NewScheme()
	setupLog             = ctrl.Log.WithName("setup")
	operatorVersion      = "development"
	defaultOperatorImage = ""
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	// Platform APIs the operator renders against: CNPG clusters and
	// DatabaseRoles, OpenShift Routes, Gateway API HTTPRoutes. Registered
	// unconditionally; whether each is served is probed at startup
	// (pkg/discovery) before the cache is built.
	utilruntime.Must(cnpgv1.AddToScheme(scheme))
	utilruntime.Must(routev1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

func main() {
	memlimit.FromCgroup()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install", "passfile-sync", "pgadmin-register", "pgadmin-backup", "pgadmin-config-uri":
			os.Exit(runTool(os.Args[1], os.Args[2:]))
		}
	}

	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var operatorImage string
	var metricsCertDir string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metrics endpoint binds to (HTTPS; every scrape needs a token allowed to get /metrics).")
	flag.StringVar(&metricsCertDir, "metrics-cert-dir", "",
		"Directory with tls.crt and tls.key for the metrics endpoint. Empty: a self-signed certificate generated at start.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager.")
	flag.StringVar(&operatorImage, "operator-image", defaultOperatorImage,
		"The operator container image reference, copied into pgAdmin pods by their install init container. Defaults to the value baked at build time.")
	var configMapName, secretName string
	flag.StringVar(&configMapName, "config-map", "pgtoolbox-operator-config",
		"The operator ConfigMap in the operator's namespace (optional; PgAdmin configuration).")
	flag.StringVar(&secretName, "config-secret", "pgtoolbox-operator-config",
		"The operator Secret in the operator's namespace (optional; overrides the ConfigMap).")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	ctx := ctrl.SetupSignalHandler()
	// One budget for the whole start-up, up to the manager's start (the
	// liveness probe is served from then on): startupBudgetTotal.
	budget := newStartupBudget(startupStepTimeout, startupBudgetTotal)
	// The PgAdmin configuration and optional-API probes come first: they
	// shape the cache and the manager's lease timings.
	pgAdminPrepared, err := preparePgAdmin(ctx, budget, operatorImage, configMapName, secretName)
	if err != nil {
		setupLog.Error(err, "unable to prepare the PgAdmin controller")
		os.Exit(1)
	}
	managerOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions(metricsAddr, metricsCertDir),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "pgtoolbox.pgtoolbox.fyannk.dev",
		// The controllers' setups resolve their kinds before the manager
		// starts: a hung API server fails them, not the liveness probe.
		MapperProvider: timedMapperProvider(mapperRequestTimeout),
	}
	consoleFeatures := pgToolBoxConsoleFeatures(pgAdminPrepared)
	managerCacheOptions(&managerOptions, pgAdminPrepared, consoleFeatures)
	restConfig := ctrl.GetConfigOrDie()
	mgr, kinds, err := buildManager(ctx, budget, func() (ctrl.Manager, error) {
		return ctrl.NewManager(restConfig, managerOptions)
	}, managerSetups(pgAdminPrepared, consoleFeatures))
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// pgtoolbox_cached_objects{kind,managed}: what the controllers'
	// informers hold, counted on scrape.
	if err := metrics.Registry.Register(cachemetrics.Collector{
		Reader: mgr.GetCache(), Scheme: mgr.GetScheme(),
		Kinds: cachemetrics.Union(mgr.GetScheme(), kinds...),
	}); err != nil {
		setupLog.Error(err, "unable to register pgtoolbox_cached_objects")
		os.Exit(1)
	}

	if err := addProbes(mgr); err != nil {
		setupLog.Error(err, "unable to set up the health and ready checks")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "version", operatorVersion)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager", "hint", cacheSyncHint(err, pgAdminPrepared.Config.WatchNamespaces))
		os.Exit(1)
	}
}

// managerSetups is every controller's setup, in the order main runs them.
func managerSetups(prepared pgAdminPrepared, consoleFeatures console.Features) []managerSetup {
	return []managerSetup{
		{"EvidenceAgent", func(ctx context.Context, mgr ctrl.Manager) ([]client.Object, error) {
			return setupEvidenceAgent(ctx, mgr, prepared.EvidenceAgentAPIs)
		}},
		{"PgAdmin", func(ctx context.Context, mgr ctrl.Manager) ([]client.Object, error) {
			return pgadmin.CachedKinds(prepared.Features), setupPgAdmin(ctx, mgr, prepared)
		}},
		{"PgAccessRequest", setupAccessRequest},
		{"PgToolBoxConsole", func(ctx context.Context, mgr ctrl.Manager) ([]client.Object, error) {
			return setupPgToolBoxConsole(ctx, mgr, consoleFeatures, prepared.Config.ExposureAllowedHostSuffixes)
		}},
	}
}

// cacheSyncHint is what to check when the manager stopped because an
// informer never synced: controller-runtime's error names the kind (as a
// Go type) but not the namespace, and the cause is almost always one
// watched namespace that refuses that kind to the operator (the start-up
// check left out the namespaces that refused one a moment before, so its
// RBAC changed since). Empty for any other error.
func cacheSyncHint(err error, watched []string) string {
	if !strings.Contains(err.Error(), "timed out waiting for cache to be synced") {
		return ""
	}
	if len(watched) == 0 {
		return "an informer could not list or watch the kind named in the error cluster-wide: check the operator's manager ClusterRoleBinding"
	}
	return fmt.Sprintf("an informer could not list or watch the kind named in the error in one of the watched namespaces %v: check the operator's manager RoleBinding in each (kubectl auth can-i --list --as=system:serviceaccount:<release namespace>:<operator ServiceAccount> -n <namespace>), then restart the operator", watched)
}

// managerSetup is one controller's setup: it registers the controller on
// the manager and returns the kinds its informers hold.
type managerSetup struct {
	controller string
	setup      func(context.Context, ctrl.Manager) ([]client.Object, error)
}

// buildManager creates the manager and runs each controller's setup, each a
// start-up step under budget, so the whole start-up up to the manager's
// start stays within it: a slow API server that answers every discovery
// request of the RESTMapper (each under mapperRequestTimeout) still fails
// the start with the step named, before the liveness probe kills the
// container with no error. A setup gets ctx, not the step's context: what
// it registers outlives the step (the step's deadline is enforced by
// startupRead, which returns at it even when the setup does not). It returns
// the kinds of every setup, in order.
func buildManager(ctx context.Context, budget *startupBudget, newManager func() (ctrl.Manager, error), setups []managerSetup) (ctrl.Manager, [][]client.Object, error) {
	mgr, err := budgetedRead(ctx, budget, "create the manager", func(context.Context) (ctrl.Manager, error) {
		return newManager()
	})
	if err != nil {
		return nil, nil, err
	}
	kinds := make([][]client.Object, 0, len(setups))
	for _, s := range setups {
		objects, err := budgetedRead(ctx, budget, "set up the "+s.controller+" controller", func(context.Context) ([]client.Object, error) {
			return s.setup(ctx, mgr)
		})
		if err != nil {
			return nil, nil, fmt.Errorf("unable to create controller %s: %w", s.controller, err)
		}
		kinds = append(kinds, objects)
	}
	return mgr, kinds, nil
}
