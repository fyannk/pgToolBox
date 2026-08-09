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

// pgtoolbox-proxy is the authentication and coarse authorization proxy of
// a pgToolBox console: OIDC or local sign-in in front of the in-pod
// HTTP upstreams.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/fyannk/pgtoolbox/internal/proxy/config"
	"github.com/fyannk/pgtoolbox/internal/proxy/local"
	"github.com/fyannk/pgtoolbox/internal/proxy/oidc"
	"github.com/fyannk/pgtoolbox/internal/proxy/openshift"
	"github.com/fyannk/pgtoolbox/internal/proxy/server"
)

// configFileEnv names the environment variable holding the rendered
// configuration path.
const configFileEnv = "PROXY_CONFIG_FILE"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "pgtoolbox-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	flag.StringVar(&configPath, "config", os.Getenv(configFileEnv),
		"proxy configuration file (default $"+configFileEnv+")")
	flag.Parse()
	if configPath == "" {
		return fmt.Errorf("no configuration file: set %s or pass -config", configFileEnv)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, warns, err := config.Load(configPath)
	if err != nil {
		// Validation errors reference fields only, never secret values.
		return fmt.Errorf("invalid configuration: %w", err)
	}
	for _, w := range warns {
		logger.Warn("config warning", "warning", w)
	}
	logger.Info("configuration loaded",
		"modes", cfg.Provider.Modes,
		"cookieSecrets", len(cfg.Session.CookieSecrets),
		"users", len(cfg.Users),
		"routes", len(cfg.Routes))

	rt, err := server.BuildRuntime(cfg)
	if err != nil {
		return err
	}
	env := server.NewEnv(rt, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	mux := server.New(env)
	providers, err := setupProviders(ctx, env, cfg)
	if err != nil {
		return err
	}
	for _, provider := range providers {
		provider.Register(mux)
		env.Available = append(env.Available, provider.Mode())
	}

	if cfg.AccessRequest.Enabled {
		creator, err := server.NewInClusterAccessRequestCreator()
		if err != nil {
			logger.Warn("access requests enabled but in-cluster config unavailable; showing contact-administrator page", "error", err)
		} else {
			env.AccessRequests = creator
		}
	}

	watcher := config.NewWatcher(configPath, logger, func(newCfg *config.Config) {
		newRt, err := server.BuildRuntime(newCfg)
		if err != nil {
			logger.Error("config reload failed, keeping previous configuration", "error", err)
			return
		}
		if !slices.Equal(newCfg.Provider.Modes, cfg.Provider.Modes) {
			logger.Warn("provider change requires a restart; keeping providers", "modes", cfg.Provider.Modes)
		}
		env.Swap(newRt)
	})
	go func() {
		if err := watcher.Run(ctx); err != nil {
			logger.Error("config watcher stopped", "error", err)
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}

// setupProvider builds the configured authentication provider. Provider
// settings are fixed at startup; changing them on reload requires a
// restart.
// setupProviders builds one provider per enabled mode.
//
// A provider that cannot start does not take the others with it. Building
// an external provider reaches out to it — discovery, endpoints, a client
// secret — so "the identity provider is unreachable" is a startup error,
// and failing the process on it would mean an outage at the IdP is also an
// outage of the local form that exists precisely to survive one. The
// failure is logged at error level and the proxy serves what is left; only
// when nothing is left does it refuse to start.
func setupProviders(ctx context.Context, env *server.Env, cfg *config.Config) ([]server.Provider, error) {
	var providers []server.Provider
	for _, mode := range cfg.Provider.Modes {
		provider, err := setupProvider(ctx, env, cfg, mode)
		if err != nil {
			env.Logger.Error("authentication provider is unavailable and will not be offered",
				"provider", mode, "error", err)
			continue
		}
		providers = append(providers, provider)
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("no authentication provider could be started")
	}
	return providers, nil
}

func setupProvider(ctx context.Context, env *server.Env, cfg *config.Config, mode string) (server.Provider, error) {
	switch mode {
	case config.ModeOIDC:
		secret, err := readSecretFile(cfg.Provider.OIDC.ClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("reading OIDC client secret: %w", err)
		}
		return oidc.New(ctx, env, cfg.Provider.OIDC, secret)
	case config.ModeLocal:
		return local.New(env)
	case config.ModeOpenShift:
		return openshift.New(ctx, env, cfg.Provider.OpenShift)
	default:
		return nil, fmt.Errorf("provider %q is not supported by this build", mode)
	}
}

// readSecretFile reads a secret file; the contents are returned but never
// logged. The path comes from the operator-rendered configuration file.
func readSecretFile(path string) (string, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path from operator-rendered config
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("secret file is empty")
	}
	return s, nil
}
