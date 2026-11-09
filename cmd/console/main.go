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

// Command pgtoolbox-console is the CloudNativePG console: one
// Deployment per team namespace, signing people in itself and letting the
// Kubernetes API server decide what they may see. It validates its whole
// environment before opening a listener and shuts down gracefully on
// SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/docsbundle"
	"github.com/fyannk/pgToolBox/internal/console/drills"
	"github.com/fyannk/pgToolBox/internal/console/podlogs"
	"github.com/fyannk/pgToolBox/internal/console/readmodel"
	"github.com/fyannk/pgToolBox/internal/console/server"
	"github.com/fyannk/pgToolBox/internal/console/sse"
	"github.com/fyannk/pgToolBox/internal/console/tracing/otlp"
	"github.com/fyannk/pgToolBox/internal/console/uiassets"
	"github.com/fyannk/pgToolBox/internal/console/wiring"
	"github.com/fyannk/pgToolBox/pkg/memlimit"
)

// version is the release identity, stamped with -X main.version.
var version = "dev"

// tracingFlushTimeout bounds the last export at shutdown, inside the
// chart's 40 s grace period after the listener's own drain.
const tracingFlushTimeout = 5 * time.Second

func main() {
	memlimit.FromCgroup()
	os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr, func() error {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		return run(ctx, os.LookupEnv, os.Stderr)
	}))
}

// cli turns the arguments and the outcome of runFn into an exit code. The
// console is configured by environment only; the one argument it accepts
// asks for its version, anything else is refused rather than ignored.
func cli(args []string, stdout, stderr io.Writer, runFn func() error) int {
	if len(args) > 0 {
		switch args[0] {
		case "--version", "-version":
			_, _ = fmt.Fprintln(stdout, version)
			return 0
		default:
			_, _ = fmt.Fprintf(stderr, "unknown argument %q: pgtoolbox-console is configured by environment variables\n", args[0])
			return 2
		}
	}
	if err := runFn(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// Listener variables (charts/pgtools, "The console binary contract").
const (
	envListenAddr      = "LISTEN_ADDR"
	envProbeListenAddr = "PROBE_LISTEN_ADDR"
	envTLSCertFile     = "TLS_CERT_FILE"
	envTLSKeyFile      = "TLS_KEY_FILE"
)

// Settings is the whole configuration: the read side's (console core)
// and the sign-in and listener settings, validated together.
type Settings struct {
	Console     config.Console
	Auth        auth.Config
	ListenAddr  string
	ProbeAddr   string
	TLSCertFile string
	TLSKeyFile  string
}

// load reads and validates every variable, joining every problem.
func load(lookup func(string) (string, bool)) (Settings, error) {
	var errs []error
	s := Settings{ListenAddr: ":3000", ProbeAddr: ":3001"}
	c, err := config.LoadConsole(config.Lookup(lookup))
	if err != nil {
		errs = append(errs, err)
	}
	s.Console = c
	a, err := auth.LoadConfig(auth.Getenv(lookup))
	if err != nil {
		errs = append(errs, err)
	}
	s.Auth = a
	get := func(k string) string {
		v, _ := lookup(k)
		return strings.TrimSpace(v)
	}
	if v := get(envListenAddr); v != "" {
		s.ListenAddr = v
	}
	if v := get(envProbeListenAddr); v != "" {
		s.ProbeAddr = v
	}
	if s.ListenAddr == s.ProbeAddr {
		errs = append(errs, fmt.Errorf("%s and %s must differ", envListenAddr, envProbeListenAddr))
	}
	s.TLSCertFile, s.TLSKeyFile = get(envTLSCertFile), get(envTLSKeyFile)
	if (s.TLSCertFile == "") != (s.TLSKeyFile == "") {
		errs = append(errs, fmt.Errorf("%s and %s are set together or not at all", envTLSCertFile, envTLSKeyFile))
	}
	return s, errors.Join(errs...)
}

func newLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}

// run validates the environment, assembles the read side and the server,
// and serves until ctx ends.
func run(ctx context.Context, lookup func(string) (string, bool), logOut io.Writer) error {
	s, err := load(lookup)
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	log := newLogger(logOut, s.Console.LogLevel)
	// controller-runtime (its cache, client and the client-go reflectors
	// under them) logs through its own root logger: without this it
	// prints a "SetLogger was never called" stack after 30 s and drops
	// its lines.
	ctrllog.SetLogger(logr.FromSlogHandler(log.Handler()))
	restConfig, err := ctrlconfig.GetConfig()
	if err != nil {
		return fmt.Errorf("load the Kubernetes client configuration: %w", err)
	}
	tr, shutdownTracing, err := otlp.New(ctx, s.Console.Tracing, otlp.Options{Version: version, Lookup: lookup, Log: log})
	if err != nil {
		return err
	}
	defer func() {
		// Flush what is buffered, bounded: a collector that does not answer
		// must not hold the pod past its grace period.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tracingFlushTimeout)
		defer cancel()
		if err := shutdownTracing(sctx); err != nil {
			log.Warn("flushing the last spans failed", "err", err)
		}
	}()
	if w := tr.KubeTransport(); w != nil {
		// Only calls made while serving a traced request get a span.
		restConfig.Wrap(w)
	}
	rm, err := readmodel.New(s.Console, restConfig, log, readmodel.Identity{Version: version, Namespace: s.Auth.Namespace, PublicURL: s.Auth.PublicURL})
	if err != nil {
		return err
	}
	broker := sse.NewBroker()
	runners := make([]server.Runner, 0, 8)
	for _, r := range rm.Runners() {
		runners = append(runners, server.Runner{Name: r.Name, Core: r.Core, Run: r.Run})
	}
	runners = append(runners, server.Runner{Name: "invalidations", Core: true, Run: wiring.Invalidations(wiring.SubscribeCache(rm.Cache), broker)})
	var logReader api.LogReader
	if s.Console.AllowLogs {
		cs, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return fmt.Errorf("building the log client: %w", err)
		}
		logReader = wiring.NewLogs(podlogs.New(rm.Cache.Reader(), wiring.NewPodLogOpener(cs), nil))
	}
	var operator server.OperatorFactory
	if s.Console.OperationsEnabled {
		drill, err := restoreDrills(s, rm, restConfig, log)
		if err != nil {
			return err
		}
		if drill != nil {
			runners = append(runners, server.Runner{Name: "restore-drills", Run: drill.Supervisor.Run})
		}
		operator = wiring.OperatorFactory(rm.Status, drill, log)
	}
	if !uiassets.Embedded() {
		log.Warn("built without the SPA (ui/dist): the UI answers \"SPA not embedded\"")
	}
	mcpOptions := server.MCPOptions{
		Enabled: s.Console.MCP.Enabled, WriteTools: s.Console.MCP.WriteTools,
		MaxResultBytes: s.Console.MCP.MaxResultBytes, CallTimeout: s.Console.MCP.CallTimeout,
		AccessTTL: s.Console.MCP.AccessTTL, ClientMetadataDocuments: s.Console.MCP.ClientMetadataDocuments,
		ClientMetadataHosts: s.Console.MCP.ClientMetadataHosts,
	}
	if mcpOptions.Enabled {
		// The documentation of this release, embedded in the image: the
		// docs tools and resources. Absent (a source build) or unreadable,
		// the endpoint serves the rest without them.
		if docs, err := server.EmbeddedDocs(); errors.Is(err, docsbundle.ErrNotEmbedded) {
			log.Warn("built without the documentation bundle: the MCP endpoint serves no docs tools (images include it; locally run make docs-bundle first)")
		} else if err != nil {
			log.Error("the embedded documentation is unreadable: the MCP endpoint serves no docs tools", "error", err)
		} else {
			mcpOptions.Docs = docs
		}
	}
	source := wiring.NewSource(rm.Status)
	if rm.Digest != nil {
		source = source.WithDigests(rm.Digest)
	}
	log.Info("starting pgtoolbox-console", "version", version, "watchNamespaces", s.Console.WatchNamespaces)
	return server.RunApp(ctx, server.AppOptions{
		RestConfig:  restConfig,
		Auth:        s.Auth,
		ListenAddr:  s.ListenAddr,
		ProbeAddr:   s.ProbeAddr,
		TLSCertFile: s.TLSCertFile,
		TLSKeyFile:  s.TLSKeyFile,
		Source:      source,
		Features: api.Features{
			Logs: s.Console.AllowLogs, History: s.Console.History.Enabled, Metrics: s.Console.MetricsEnabled,
			Evidence: s.Console.Evidence.Enabled, Operations: s.Console.OperationsEnabled, Digest: rm.Digest != nil,
		},
		PgAdminURL:   s.Console.PgAdminURL,
		RunnerStates: rm.Status.SetRunnerStates,
		Logs:         logReader,
		Operator:     operator,
		Access:       accessFactory(rm.Cache, s.Console.OperationsEnabled, log),
		MCP:          mcpOptions,
		Broker:       broker,
		UI:           uiassets.Handler(),
		Runners:      runners,
		Ready: map[string]func() error{"informer-cache": func() error {
			if !rm.Ready() {
				return errors.New("not synced")
			}
			return nil
		}},
		Tracing:    tr,
		Log:        log,
		Version:    version,
		ConfigInfo: s.Console.Info(),
		SignOuts:   signOuts(rm),
		Grants:     grants(rm),
	})
}

// restoreDrills assembles the restore drills when RESTORE_DRILL_ENABLED is
// set: the seal's keys derive from its own key file when
// RESTORE_DRILL_SEAL_KEY_FILE is set, else from the session secret (so
// every replica verifies what any of them created); the record lives in
// the history journal when history is persistent, and finished drills'
// outcomes in the shared-state ConfigMap otherwise.
func restoreDrills(s Settings, rm *readmodel.ReadModel, restConfig *rest.Config, log *slog.Logger) (*wiring.Drills, error) {
	if !s.Console.RestoreDrill.Enabled {
		return nil, nil //nolint:nilnil // disabled is a valid, explicit outcome
	}
	var secrets [][]byte
	var err error
	if path := s.Console.RestoreDrill.SealKeyFile; path != "" {
		if secrets, err = auth.LoadSecrets(path); err != nil {
			return nil, fmt.Errorf("restore drill seal key (%s): %w", config.EnvRestoreDrillSealKeyFile, err)
		}
	} else {
		if secrets, err = auth.LoadSecrets(s.Auth.SessionSecretFile); err != nil {
			return nil, err
		}
		log.Warn("restore drill seals derive from the session secret: whoever reads that Secret can forge a seal; set " +
			config.EnvRestoreDrillSealKeyFile + " to give the seal its own key")
	}
	var persist drills.Persister
	if j := rm.History.DrillJournal(); j != nil {
		persist = j
	}
	var keeper drills.Keeper
	if persist == nil && rm.Shared != nil {
		keeper = wiring.NewSharedDrillKeeper(rm.Shared, time.Now())
	}
	return wiring.NewDrills(s.Console, rm.Status, restConfig, s.Auth.Namespace, secrets, persist, keeper, log)
}

// grants is the shared MCP grant index, or nil (see signOuts).
func grants(rm *readmodel.ReadModel) server.GrantIndex {
	if rm.Shared == nil {
		return nil
	}
	return rm.Shared
}

// signOuts is the shared sign-out set, or nil (a typed nil store in the
// interface would read as configured).
func signOuts(rm *readmodel.ReadModel) auth.SignOutStore {
	if rm.Shared == nil {
		return nil
	}
	return rm.Shared
}
