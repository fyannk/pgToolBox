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
	"flag"
	"os"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/fyannk/pgToolBox/internal/operator/cachemetrics"
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
	// The PgAdmin configuration and optional-API probes come first: they
	// shape the cache and the manager's lease timings.
	pgAdminPrepared, err := preparePgAdmin(ctx, operatorImage, configMapName, secretName)
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
	}
	consoleFeatures := pgToolBoxConsoleFeatures(pgAdminPrepared)
	managerCacheOptions(&managerOptions, pgAdminPrepared, consoleFeatures)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), managerOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	evidenceAgentKinds, err := setupEvidenceAgent(ctx, mgr, pgAdminPrepared.Config.WatchNamespaces)
	if err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EvidenceAgent")
		os.Exit(1)
	}

	if err := setupPgAdmin(ctx, mgr, pgAdminPrepared); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PgAdmin")
		os.Exit(1)
	}

	accessRequestKinds, err := setupAccessRequest(ctx, mgr)
	if err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PgAccessRequest")
		os.Exit(1)
	}

	pgToolBoxConsoleKinds, err := setupPgToolBoxConsole(ctx, mgr, consoleFeatures, pgAdminPrepared.Config.ExposureAllowedHostSuffixes)
	if err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PgToolBoxConsole")
		os.Exit(1)
	}

	// pgtoolbox_cached_objects{kind,managed}: what the controllers'
	// informers hold, counted on scrape.
	if err := metrics.Registry.Register(cachemetrics.Collector{
		Reader: mgr.GetCache(), Scheme: mgr.GetScheme(),
		Kinds: cachemetrics.Union(mgr.GetScheme(), pgadmin.CachedKinds(pgAdminPrepared.Features), evidenceAgentKinds, accessRequestKinds, pgToolBoxConsoleKinds),
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
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
