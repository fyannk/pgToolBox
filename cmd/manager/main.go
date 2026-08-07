/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"flag"
	"os"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/controller/pgconsole"
	"github.com/fyannk/pgtoolbox/internal/controller/pgtoolboxaccessrequest"
	"github.com/fyannk/pgtoolbox/internal/controller/pgtoolboxrole"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var (
	scheme               = runtime.NewScheme()
	setupLog             = ctrl.Log.WithName("setup")
	operatorVersion      = "development"
	defaultOperatorImage = ""
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(pgtoolboxv1alpha1.AddToScheme(scheme))
	// Platform APIs the operator renders against: CNPG clusters and
	// DatabaseRoles, OpenShift Routes and Ingress configuration, Gateway
	// API HTTPRoutes. Registered unconditionally; availability is
	// discovered at startup once controllers land.
	utilruntime.Must(cnpgv1.AddToScheme(scheme))
	utilruntime.Must(routev1.AddToScheme(scheme))
	utilruntime.Must(configv1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "admin-sync-init":
			if err := runAdminSyncInit(os.Args[2:]); err != nil {
				setupLog.Error(err, "admin-sync-init failed")
				os.Exit(1)
			}
			return
		case "admin-sync-sidecar":
			if err := runAdminSyncSidecar(os.Args[2:]); err != nil {
				setupLog.Error(err, "admin-sync-sidecar failed")
				os.Exit(1)
			}
			return
		}
	}

	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var operatorImage string
	var defaultImages pgconsole.DefaultImages
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager.")
	flag.StringVar(&operatorImage, "operator-image", defaultOperatorImage,
		"The operator container image reference, used by the admin-sync init container. Defaults to the value baked at build time.")
	flag.StringVar(&defaultImages.PgConsole, "default-pgconsole-image", "",
		"The pgconsole image used when a PgConsole spec names none.")
	flag.StringVar(&defaultImages.Proxy, "default-pgtoolbox-proxy-image", "",
		"The pgtoolbox-proxy image used when a PgConsole spec names none.")
	flag.StringVar(&defaultImages.PgAdmin, "default-pgadmin-image", "",
		"The pgAdmin image used when a PgConsole spec names none.")
	flag.StringVar(&defaultImages.ObjectStoreViewer, "default-objectstoreviewer-image", "",
		"The ObjectStoreViewer image used when a PgConsole spec names none.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "pgtoolbox.pgtoolbox.fyannk.dev",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	availability := discoverAPIs(mgr)
	setupLog.Info("API discovery",
		"routes", availability.RouteAPIAvailable,
		"gatewayAPI", availability.GatewayAPIAvailable,
		"barmanObjectStores", availability.BarmanObjectStoreAvailable)

	if err := (&pgconsole.Reconciler{
		Runtime: shared.Runtime{
			Client:              mgr.GetClient(),
			APIReader:           mgr.GetAPIReader(),
			Scheme:              mgr.GetScheme(),
			Recorder:            mgr.GetEventRecorderFor("pgconsole"),
			RouteAPIAvailable:   availability.RouteAPIAvailable,
			GatewayAPIAvailable: availability.GatewayAPIAvailable,
		},
		DefaultImages:              defaultImages,
		BarmanObjectStoreAvailable: availability.BarmanObjectStoreAvailable,
		OperatorImage:              operatorImage,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PgConsole")
		os.Exit(1)
	}

	if err := (&pgtoolboxrole.Reconciler{
		Runtime: shared.Runtime{
			Client:              mgr.GetClient(),
			APIReader:           mgr.GetAPIReader(),
			Scheme:              mgr.GetScheme(),
			Recorder:            mgr.GetEventRecorderFor("pgtoolboxrole"),
			RouteAPIAvailable:   availability.RouteAPIAvailable,
			GatewayAPIAvailable: availability.GatewayAPIAvailable,
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PgToolBoxRole")
		os.Exit(1)
	}

	if err := (&pgtoolboxaccessrequest.Reconciler{
		Runtime: shared.Runtime{
			Client:              mgr.GetClient(),
			APIReader:           mgr.GetAPIReader(),
			Scheme:              mgr.GetScheme(),
			Recorder:            mgr.GetEventRecorderFor("pgtoolboxaccessrequest"),
			RouteAPIAvailable:   availability.RouteAPIAvailable,
			GatewayAPIAvailable: availability.GatewayAPIAvailable,
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PgToolBoxAccessRequest")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "version", operatorVersion)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// apiAvailability records which optional platform APIs the cluster serves,
// discovered once at startup. Exposure kinds and the evidence composition
// gate on these rather than failing against an absent API.
type apiAvailability struct {
	RouteAPIAvailable          bool
	GatewayAPIAvailable        bool
	BarmanObjectStoreAvailable bool
}

// discoverAPIs probes the cluster for the optional APIs the operator can
// render against. A discovery failure is not fatal: the affected API is
// reported unavailable, matching the behaviour of a cluster without it.
func discoverAPIs(mgr ctrl.Manager) apiAvailability {
	var availability apiAvailability
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to build discovery client; optional APIs reported unavailable")
		return availability
	}
	availability.RouteAPIAvailable = apiServed(discoveryClient, routev1.GroupVersion.WithKind("Route"))
	availability.GatewayAPIAvailable = apiServed(discoveryClient, schema.GroupVersionKind{
		Group:   gatewayv1.GroupName,
		Version: "v1",
		Kind:    "HTTPRoute",
	})
	availability.BarmanObjectStoreAvailable = apiServed(discoveryClient, schema.GroupVersionKind{
		Group:   "barmancloud.cnpg.io",
		Version: "v1",
		Kind:    "ObjectStore",
	})
	return availability
}

// apiServed reports whether the cluster serves the given kind.
func apiServed(discoveryClient discovery.DiscoveryInterface, gvk schema.GroupVersionKind) bool {
	resources, err := discoveryClient.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		return false
	}
	for i := range resources.APIResources {
		if resources.APIResources[i].Kind == gvk.Kind {
			return true
		}
	}
	return false
}
