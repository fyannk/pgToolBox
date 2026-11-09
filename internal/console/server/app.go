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

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/mcp"
	"github.com/fyannk/pgToolBox/internal/console/mcpoauth"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/sse"
	"github.com/fyannk/pgToolBox/internal/console/tracing"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// AppOptions are what cmd/console hands over after reading its
// configuration: the REST configuration of the console's ServiceAccount,
// the sign-in configuration, the listeners, and the read side built by
// the console core (its Source, its runners and readiness checks, and
// the broker it publishes invalidations to).
type AppOptions struct {
	RestConfig *rest.Config
	Auth       auth.Config
	ListenAddr string
	ProbeAddr  string
	// TLSCertFile/TLSKeyFile enable HTTPS on the main listener.
	TLSCertFile string
	TLSKeyFile  string
	// ShutdownDelay keeps the listener open, not ready, after the
	// shutdown signal (SHUTDOWN_DELAY).
	ShutdownDelay time.Duration

	Source api.Source
	// Logs, Operator and Features complete the API; nil Logs or Operator serve 501.
	Logs     api.LogReader
	Operator OperatorFactory
	Features api.Features
	// PgAdminURL is a fixed pgAdmin link (PGADMIN_URL); empty offers none.
	PgAdminURL string
	// SignOuts shares sign-outs between replicas (STATE_CONFIGMAP); nil
	// keeps them per replica.
	SignOuts auth.SignOutStore
	// Grants is the shared index of MCP grants (STATE_CONFIGMAP); nil
	// keeps it in this replica's memory (one replica, or revocations that
	// reach only the replica that took them).
	Grants GrantIndex
	// Access builds the access-request endpoints; nil serves 501.
	Access AccessFactory
	// MCP configures the MCP endpoint; off by default.
	MCP MCPOptions
	// Tracing is OpenTelemetry tracing: server spans, the
	// user client's API calls and the MCP tool calls; nil is off.
	Tracing *tracing.Tracing
	// RunnerStates receives the supervisor's runner report for the admin
	// page; nil ignores it.
	RunnerStates func(func() []viewmodel.RunnerState)
	Broker       *sse.Broker
	Runners      []Runner
	// Ready are readiness checks of the read side (caches synced).
	Ready map[string]func() error
	UI    http.Handler
	Log   *slog.Logger
	// Version is the release identity for pgtoolbox_console_build_info.
	Version string
	// ConfigInfo adds the read side's effective settings to
	// pgtoolbox_console_config_info (never a secret value).
	ConfigInfo map[string]string
}

// MCPOptions are the MCP endpoint's settings (MCP_*).
type MCPOptions struct {
	Enabled bool
	// WriteTools adds the MCP write tools when operations
	// are served.
	WriteTools     bool
	MaxResultBytes int
	CallTimeout    time.Duration
	// AccessTTL of the console's OAuth access tokens
	// (MCP_OAUTH_ACCESS_TTL); mcpoauth.DefaultAccessTTL when zero.
	AccessTTL time.Duration
	// ClientMetadataDocuments accepts CIMD client IDs
	// (MCP_OAUTH_CLIENT_METADATA_DOCUMENTS), from ClientMetadataHosts
	// only when set (MCP_OAUTH_CLIENT_METADATA_HOSTS).
	ClientMetadataDocuments bool
	ClientMetadataHosts     []string
	// Docs is the embedded documentation; nil serves no docs tools.
	Docs mcp.Docs
}

// OperatorFactory builds the operation service once the SAR authorizer
// exists: writes go to the API server as the caller (token passthrough
// over restConfig's address and CA, never its credentials).
type OperatorFactory func(restConfig *rest.Config, az *authz.Authorizer) (api.Operator, error)

// AccessFactory builds the access-request endpoints: reads from the
// console's cache, writes as the caller (token passthrough over
// restConfig's address and CA, never its credentials).
type AccessFactory func(restConfig *rest.Config) (*api.Access, error)

// RunApp assembles and runs the console until ctx ends: the sign-in layer
// (sealed cookies, the user client, SelfSubjectReview identities, the
// revocation-list watch), the SAR authorizer, the JSON API and stream,
// the probe listener with the authenticated /metrics, and the runner
// supervisor. Configuration and secret-file errors fail before any
// listener opens.
func RunApp(ctx context.Context, o AppOptions) error {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.RestConfig == nil || o.Source == nil {
		return errors.New("server: RunApp needs a REST configuration and a Source")
	}
	if o.Broker == nil {
		o.Broker = sse.NewBroker()
	}
	publishInfo(o.Version, o.Auth.Info(), o.ConfigInfo)
	secrets, err := auth.LoadSecrets(o.Auth.SessionSecretFile)
	if err != nil {
		return err
	}
	codec, err := auth.NewCodec(secrets, o.Auth.CookieSecure, auth.Limits{MaxAge: o.Auth.MaxAge, IdleTimeout: o.Auth.IdleTimeout})
	if err != nil {
		return err
	}
	factory, err := auth.NewClientFactory(o.RestConfig, o.Tracing.KubeTransport())
	if err != nil {
		return err
	}
	identity := auth.NewCachedReviewer(auth.NewSelfSubjectReviewer(factory), 0, 0)
	// The revocation watch gets its own client budget,
	// apart from the read side's informers.
	consoleCfg := rest.CopyConfig(o.RestConfig)
	consoleCfg.RateLimiter = nil
	consoleCfg.QPS, consoleCfg.Burst = 100, 200
	console, err := kubernetes.NewForConfig(consoleCfg)
	if err != nil {
		return fmt.Errorf("building the console client: %w", err)
	}
	az := authz.NewAuthorizer(auth.SelfReviewer{Factory: factory}, authz.Options{})
	revocations := auth.NewRevocations(console.CoreV1().ConfigMaps(o.Auth.Namespace), o.Auth.RevocationConfigMap, o.Log)

	idpCtx, cancel := context.WithTimeout(ctx, o.Auth.IdPTimeout+5*time.Second)
	defer cancel()
	authHandler, err := auth.New(idpCtx, auth.Options{Config: o.Auth, Codec: codec, Identity: identity, Revocations: revocations, SignOuts: o.SignOuts, Logger: o.Log})
	if err != nil {
		return err
	}
	var operator api.Operator
	if o.Operator != nil {
		if operator, err = o.Operator(o.RestConfig, az); err != nil {
			return err
		}
	}
	var access *api.Access
	if o.Access != nil {
		if access, err = o.Access(o.RestConfig); err != nil {
			return err
		}
	}
	// Per-person DatabaseRoles are a write: served only where operations
	// are enabled, as the caller (token passthrough).
	var databaseRoles api.DatabaseRoleCreator
	if operator != nil {
		c, err := ops.NewUserClient(auth.UserConfig(o.RestConfig, o.Tracing.KubeTransport()))
		if err != nil {
			return err
		}
		databaseRoles = ops.NewUserWriter(c, 0)
	}
	deps := api.Deps{
		Source: o.Source, Authz: az, Broker: o.Broker, Sessions: authHandler,
		Logs: o.Logs, Operator: operator, DatabaseRoles: databaseRoles, Features: o.Features, Access: access,
		PgAdminURL: o.PgAdminURL, Log: o.Log, PublicURL: o.Auth.PublicURL,
		ClientOf: func(r *http.Request) auth.Client { return auth.ClientOf(r, o.Auth.TrustedProxyHops) },
	}
	var (
		oauthServer *mcpoauth.Server
		setup       *assistantSetup
		index       GrantIndex
	)
	if o.MCP.Enabled {
		// One index for the grants and the write tools' confirmations:
		// both spend single-use values every replica must see.
		index = grantIndex(o)
		if oauthServer, err = newOAuthServer(o, authHandler, index); err != nil {
			return err
		}
		setup = &assistantSetup{}
		deps.Assistant = setup
		if oauthServer != nil {
			deps.Grants = grantManager{oauthServer}
		}
		// What the session reports: the SPA offers the assistants page only
		// on a console that serves MCP.
		deps.Features.MCP, deps.Features.MCPWriteTools = true, o.MCP.WriteTools
	}
	apiHandler, _, reads := api.NewWithReads(deps)
	var mcpEndpoint Mounter
	if o.MCP.Enabled {
		e, err := mcp.New(mcp.Options{
			Reads: reads, Authenticator: mcpAuthenticator(oauthServer, authHandler), OAuth: mcpOAuth(oauthServer),
			PublicURL: o.Auth.PublicURL, TrustedProxyHops: o.Auth.TrustedProxyHops, Docs: o.MCP.Docs,
			Attempts:   authHandler.AttemptLimiter(),
			WriteTools: o.MCP.WriteTools, Sealer: authHandler.Codec(), Confirmations: index, Sessions: authHandler,
			MaxResultBytes: o.MCP.MaxResultBytes, CallTimeout: o.MCP.CallTimeout,
			Tracing: o.Tracing, Version: o.Version, Log: o.Log,
		})
		if err != nil {
			return err
		}
		mcpEndpoint = e
		if err := setup.init(e, authHandler.TokenLoginEnabled()); err != nil {
			return err
		}
	}
	metrics, err := MetricsHandler(identity, az, logr.FromSlogHandler(o.Log.Handler()), o.Auth.TrustedProxyHops)
	if err != nil {
		return err
	}

	sup := NewSupervisor(o.Log)
	if o.RunnerStates != nil {
		o.RunnerStates(sup.States)
	}
	if revocations.Enabled() {
		sup.Add(Runner{Name: "revocation-list", Core: true, Run: revocations.Run})
	}
	for _, r := range o.Runners {
		sup.Add(r)
	}
	ready := &Readiness{}
	ready.Add("revocation-list", func() error {
		if !revocations.Synced() {
			return errors.New("not read yet")
		}
		return nil
	})
	ready.Add("runners", sup.Check)
	for name, fn := range o.Ready {
		ready.Add(name, fn)
	}

	srv := New(Options{
		Addr: o.ListenAddr, ProbeAddr: o.ProbeAddr, TLSCertFile: o.TLSCertFile, TLSKeyFile: o.TLSKeyFile,
		Auth: authHandler, PublicURL: o.Auth.PublicURL, API: apiHandler, UI: o.UI,
		Ready: ready, Broker: o.Broker, Metrics: metrics, Log: o.Log, MCP: mcpEndpoint, Tracing: o.Tracing,
		ShutdownDelay: o.ShutdownDelay,
	})
	supCtx, stopRunners := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { sup.Run(supCtx) })
	err = srv.Run(ctx)
	stopRunners()
	wg.Wait()
	return err
}
