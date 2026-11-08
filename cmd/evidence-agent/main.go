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

// Command evidence-agent is the backup-evidence agent. It reads one backup
// repository and serves what it finds: standalone, as a read-only viewer
// UI; as the in-cluster agent an EvidenceAgent deploys (RUNTIME_MODE=agent:
// evidence/v1alpha1 over HTTPS with bound-token authentication); or as the
// pgtools chart's team agent (RUNTIME_MODE=agent with AGENT_RESOLVE=cluster:
// it resolves its own repository from the Cluster and authenticates the
// console by a shared token).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/fyannk/pgToolBox/internal/backupresolve"
	"github.com/fyannk/pgToolBox/internal/evidence/application"
	"github.com/fyannk/pgToolBox/internal/evidence/config"
	"github.com/fyannk/pgToolBox/internal/evidence/selfresolve"
	"github.com/fyannk/pgToolBox/pkg/memlimit"
)

// version is injected at build time with -ldflags "-X main.version=<tag>" by
// the Makefile, the Dockerfile (ARG VERSION), and the release workflow.
var version = "development"

// releaseVersion returns the injected version when it is a bounded, printable
// release identifier, and "development" otherwise: the value is published as
// producer.version on the evidence wire, which accepts at most 64 bytes.
func releaseVersion(raw string) string {
	if raw == "" || len(raw) > 64 {
		return "development"
	}
	for index, character := range raw {
		alphanumeric := (character >= '0' && character <= '9') || (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
		if !alphanumeric && (index == 0 || !strings.ContainsRune(".+_-", character)) {
			return "development"
		}
	}
	return raw
}

func main() {
	memlimit.FromCgroup()
	level, levelErr := config.ParseLogLevel(os.Getenv("LOG_LEVEL"))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	// The self-resolving watch and client log through controller-runtime's
	// root logger: hand it this one (otherwise a "SetLogger was never
	// called" stack after 30 s, and its lines dropped).
	ctrllog.SetLogger(logr.FromSlogHandler(logger.Handler()))
	err := levelErr
	if err == nil {
		err = dispatch(os.Args[1:], logger)
	}
	if err != nil {
		attributes := []any{slog.String("category", errorCategory(err))}
		if config.IsError(err) {
			attributes = append(attributes, slog.String("detail", err.Error()))
		}
		logger.Error("application stopped", attributes...)
		os.Exit(1)
	}
}

func dispatch(arguments []string, logger *slog.Logger) error {
	if len(arguments) == 0 {
		return run(logger)
	}
	return errors.New("unsupported command")
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.LoadOS()
	if err != nil {
		return err
	}
	if cfg.RuntimeMode == config.RuntimeAgent && cfg.AgentResolve == config.AgentResolveCluster {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		if cfg, err = selfResolve(ctx, cancel, cfg, logger); err != nil {
			return err
		}
	}
	app, err := application.New(ctx, cfg, logger, releaseVersion(version))
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	if cfg.RuntimeMode == config.RuntimeAgent {
		logger.Info("agent application listening",
			slog.String("runtime_mode", string(cfg.RuntimeMode)),
			slog.String("version", releaseVersion(version)),
			slog.String("listen_addr", cfg.ListenAddr),
			slog.String("agent", cfg.AgentNamespace+"/"+cfg.AgentName),
			slog.Uint64("producer_epoch", cfg.ProducerEpoch),
			slog.String("auth", string(cfg.AgentAuth)),
			slog.Bool("plain_http", cfg.AgentPlainHTTP),
			slog.Bool("self_resolving", cfg.AgentResolve == config.AgentResolveCluster),
		)
		return app.ServeAgent(ctx, listener)
	}
	logger.Info("application listening",
		slog.String("version", releaseVersion(version)),
		slog.String("listen_addr", cfg.ListenAddr),
		slog.String("provider", string(cfg.Provider)),
		slog.String("repository_format", string(cfg.RepositoryFormat)),
	)
	return app.Serve(ctx, listener)
}

// selfResolve resolves the team agent's repository from its Cluster and
// starts the watch that ends the process (exit 0, the kubelet restarts it)
// when that resolution changes. A resolution that fails is not fatal: the
// agent serves it as unknown evidence with the reason, which is what the
// console shows, rather than crash-looping with the reason only in its log.
func selfResolve(ctx context.Context, stop context.CancelFunc, cfg config.Config, logger *slog.Logger) (config.Config, error) {
	resolver, watcher, err := selfresolve.NewInCluster(selfresolve.Options{
		Namespace: cfg.CNPGClusterNamespace, Cluster: cfg.CNPGClusterName, AllowedSecrets: cfg.AgentAllowedSecrets,
		AllowInsecureEndpoint: cfg.AgentAllowInsecureEndpoint, WorkloadIdentity: cfg.AgentWorkloadIdentity, Logger: logger,
		PgBackRest: pgBackRestSettings(cfg), PgBackRestEndpoint: cfg.AgentPgBackRestEndpoint,
	})
	if err != nil {
		return cfg, err
	}
	outcome := resolver.Resolve(ctx)
	cfg = config.ApplyResolved(cfg, outcome.Repository, selfresolve.Epoch(time.Now()), outcome.Failure)
	if outcome.Failure != nil {
		// The failure names objects and values, never a credential.
		logger.Warn("the backup repository could not be resolved; serving unknown evidence",
			slog.String("cluster", cfg.CNPGClusterNamespace+"/"+cfg.CNPGClusterName),
			slog.String("reason", outcome.Failure.Error()),
			slog.Duration("retry_after", outcome.RetryAfter))
	} else {
		attributes := []any{slog.String("cluster", cfg.CNPGClusterNamespace+"/"+cfg.CNPGClusterName)}
		if location := outcome.PgBackRest; location != nil {
			attributes = append(attributes, slog.String("pgbackrest_object", location.Kind+" "+location.ObjectName),
				slog.String("stanza", outcome.Repository.Stanza), slog.Int("repository", outcome.Repository.Repository))
		} else {
			attributes = append(attributes, slog.String("object_store", outcome.ObjectStoreName))
		}
		logger.Info("backup repository resolved from the Cluster", attributes...)
	}
	go func() {
		if reason := watcher.Wait(ctx, outcome); reason != "" {
			logger.Info("restarting to resolve the backup repository again", slog.String("reason", reason))
			stop()
		}
	}()
	return cfg, nil
}

// pgBackRestSettings are the chart entry's pgbackrest settings, nil when
// it sets none (repo1, the CA the plugin's object names).
func pgBackRestSettings(cfg config.Config) *backupresolve.PgBackRestSettings {
	ca := cfg.AgentPgBackRestEndpointCA
	if cfg.AgentPgBackRestRepository == 0 && ca.Name == "" {
		return nil
	}
	settings := &backupresolve.PgBackRestSettings{Repository: cfg.AgentPgBackRestRepository}
	if ca.Name != "" {
		settings.EndpointCA = &backupresolve.SecretKeyRef{Name: ca.Name, Key: ca.Key}
	}
	return settings
}

func errorCategory(err error) string {
	if config.IsError(err) {
		return "invalid_configuration"
	}
	return "runtime"
}
