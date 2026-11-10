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

// PgToolBoxConsole wiring of the operator binary. Kept in its own file, like the
// other controllers'; main.go calls the two functions below.

import (
	"context"
	"flag"
	"os"
	"reflect"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/operator/console"
)

// defaultConsoleImage is the console image a PgToolBoxConsole runs when its spec
// names none; CONSOLE_IMAGE is the packaging's way to set it.
var (
	defaultConsoleImage string
	consoleRepositories string
)

func init() {
	flag.StringVar(&defaultConsoleImage, "default-console-image", os.Getenv("CONSOLE_IMAGE"),
		"The console image a PgToolBoxConsole runs when its spec names none; a digest-pinned reference.")
	flag.StringVar(&consoleRepositories, "console-image-repositories", os.Getenv("CONSOLE_IMAGE_REPOSITORIES"),
		"Comma-separated image repositories a PgToolBoxConsole's spec.image may name (exact, or a prefix ending in '/'). Empty: the default image's repository only.")
}

// pgToolBoxConsoleFeatures are the optional APIs the PgToolBoxConsole controller uses,
// from the probes the PgAdmin preparation already ran.
func pgToolBoxConsoleFeatures(prepared pgAdminPrepared) console.Features {
	return console.Features{
		Routes: prepared.Features.Routes, Gateways: prepared.Features.Gateways, OpenShift: prepared.Features.OpenShift,
	}
}

// pgToolBoxConsoleCacheOptions adds the PgToolBoxConsole controller's informer filters to
// the manager's cache options; a kind another controller already filters
// keeps that filter (every generated object carries managed-by=pgtoolbox).
func pgToolBoxConsoleCacheOptions(options *ctrl.Options, features console.Features) {
	if options.Cache.ByObject == nil {
		options.Cache.ByObject = map[client.Object]cache.ByObject{}
	}
	present := map[reflect.Type]bool{}
	for object := range options.Cache.ByObject {
		present[reflect.TypeOf(object)] = true
	}
	for object, byObject := range console.CacheByObject(features) {
		if !present[reflect.TypeOf(object)] {
			options.Cache.ByObject[object] = byObject
		}
	}
}

// setupPgToolBoxConsole registers the controller; it returns the kinds its
// informers hold (pgtoolbox_cached_objects). hostSuffixes is the loaded
// EXPOSURE_ALLOWED_HOST_SUFFIXES, shared with the PgAdmin controller.
func setupPgToolBoxConsole(ctx context.Context, mgr ctrl.Manager, features console.Features, hostSuffixes []string) ([]client.Object, error) {
	setupLog.Info("PgToolBoxConsole controller", "defaultImage", defaultConsoleImage)
	return console.CachedKinds(features), (&console.Reconciler{
		Client:                   mgr.GetClient(),
		APIReader:                mgr.GetAPIReader(),
		Scheme:                   mgr.GetScheme(),
		Recorder:                 mgr.GetEventRecorder("pgtoolbox-console"),
		DefaultImage:             defaultConsoleImage,
		AllowedImageRepositories: splitList(consoleRepositories),
		AllowedHostSuffixes:      hostSuffixes,
		Features:                 features,
	}).SetupWithManager(ctx, mgr)
}
