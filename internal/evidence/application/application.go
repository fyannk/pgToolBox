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

// Package application assembles the provider-neutral runtime and background
// inventory lifecycle.
package application

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fyannk/pgToolBox/internal/evidence/agent"
	"github.com/fyannk/pgToolBox/internal/evidence/config"
	"github.com/fyannk/pgToolBox/internal/evidence/evidenceapi"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/formats"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	azurestore "github.com/fyannk/pgToolBox/internal/evidence/provider/azure"
	gcsstore "github.com/fyannk/pgToolBox/internal/evidence/provider/gcs"
	s3store "github.com/fyannk/pgToolBox/internal/evidence/provider/s3"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	"github.com/fyannk/pgToolBox/internal/evidence/redact"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/web"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 15 * time.Second
	maxHeaderBytes    = 16 * 1024
)

// App is one assembled evidence process: the store reader, the inventory
// scanner and the surface its runtime mode serves (the viewer UI or the
// agent's HTTPS API).
type App struct {
	server       *http.Server
	agentHandler http.Handler
	certificates *agent.CertificateReloader
	// plainHTTP serves the agent without TLS (AGENT_PLAIN_HTTP).
	plainHTTP   bool
	runtimeMode config.RuntimeMode
	readiness   *readiness.ProbeState
	worker      interface{ Run(context.Context) }
}

// readerFactory builds the provider reader. The redactor lets an adapter that
// re-reads rotated credentials teach log redaction the new values.
type readerFactory func(context.Context, config.Config, *redact.Redactor) (store.Reader, error)

// Option adjusts how New assembles the runtime.
type Option func(*options)

type options struct {
	reviewer agent.Reviewer
}

// WithReviewer supplies the TokenReview/SubjectAccessReview client of the
// agent runtime mode; without it, agent mode uses the in-cluster API server.
func WithReviewer(reviewer agent.Reviewer) Option {
	return func(o *options) { o.reviewer = reviewer }
}

// New assembles the process for cfg's runtime mode without starting it.
// producerVersion is reported in the published evidence. In agent mode an
// invalid repository configuration is published as unknown evidence
// instead of failing.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger, producerVersion string, opts ...Option) (*App, error) {
	return newWithFactoryVersion(ctx, cfg, logger, defaultReaderFactory, producerVersion, opts...)
}

func newWithFactory(ctx context.Context, cfg config.Config, logger *slog.Logger, factory readerFactory) (*App, error) {
	return newWithFactoryVersion(ctx, cfg, logger, factory, "test")
}

func newWithFactoryVersion(ctx context.Context, cfg config.Config, logger *slog.Logger, factory readerFactory, producerVersion string, opts ...Option) (*App, error) {
	var settings options
	for _, opt := range opts {
		opt(&settings)
	}
	if factory == nil {
		return nil, errors.New("application requires a reader factory")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	registry, err := formats.Builtins()
	if err != nil {
		return nil, err
	}
	format, err := registry.Select(string(cfg.RepositoryFormat))
	if err != nil {
		return nil, err
	}
	descriptor := format.Descriptor()
	cache, err := inventory.NewCache(inventory.Initial(descriptor))
	if err != nil {
		return nil, err
	}
	if cfg.RuntimeMode.Produces() && cfg.ScopeName() == "" {
		return nil, errors.New("invalid agent evidence configuration")
	}
	probeState := readiness.New(true, 2*cfg.CatalogRefreshInterval+cfg.StoreRequestTimeout, time.Now)
	redactor := redact.New(cfg.SecretValues()...)
	var reader store.Reader
	if cfg.RepositoryError != nil {
		if !cfg.RuntimeMode.Produces() {
			return nil, cfg.RepositoryError
		}
		// The agent serves the evidence API without a store: the published
		// evidence says why it is unknown instead of the Pod crash-looping.
		now := time.Now().UTC()
		reason := redactor.String(cfg.RepositoryError.Error())
		if err := cache.Publish(inventory.FailedSnapshot(descriptor, 1, now, now, fault.InvalidConfig, boundedReason(reason))); err != nil {
			return nil, err
		}
		logger.Error("repository configuration invalid; serving unknown evidence",
			slog.String("category", string(fault.InvalidConfig)), slog.String("detail", reason))
	} else {
		reader, err = factory(ctx, cfg, redactor)
		if err != nil {
			return nil, err
		}
	}
	if cfg.RuntimeMode.Produces() && reader != nil {
		reader, err = newScopeConfinedReader(reader, cfg.ScopePrefixes())
		if err != nil {
			return nil, err
		}
	}
	scannerCache := inventory.SnapshotCache(cache)
	app := &App{runtimeMode: cfg.RuntimeMode, readiness: probeState}
	if cfg.RuntimeMode == config.RuntimeAgent {
		engine, engineErr := newEvidenceEngine(cfg, producerVersion, cache.Load())
		if engineErr != nil {
			return nil, engineErr
		}
		handler, certificates, agentErr := newAgentTransport(ctx, cfg, engine, probeState, settings.reviewer, logger)
		if agentErr != nil {
			return nil, agentErr
		}
		app.agentHandler, app.certificates, app.plainHTTP = handler, certificates, cfg.AgentPlainHTTP
		scannerCache = &evidencePublishingCache{cache: cache, engine: engine}
	}
	var worker interface{ Run(context.Context) }
	if reader != nil {
		configuredScopes := cfg.BarmanServerNames
		var pgbackrestOptions *inventory.PGBackRestOptions
		if cfg.RepositoryFormat == config.FormatPGBackRest {
			configuredScopes = cfg.PGBackRestStanzas
			pgbackrestOptions = &inventory.PGBackRestOptions{Passphrase: cfg.PGBackRestCipher.Bytes()}
			if cfg.RuntimeMode.Produces() {
				stanza, archive := cfg.PGBackRestScope()
				configuredScopes = slices.Compact([]string{stanza, archive})
				slices.Sort(configuredScopes)
				pgbackrestOptions.Stanza, pgbackrestOptions.ArchiveStanza = stanza, archive
				pgbackrestOptions.Retention = pgbackrestRetention(cfg.PGBackRestRetention)
			}
		}
		recentLimit := min(cfg.WALPageSize, inventory.MaxRecentObjects)
		worker, err = inventory.NewScanner(inventory.ScannerOptions{
			Store: reader, Format: format, ConfiguredScopes: configuredScopes,
			Cache: scannerCache, Readiness: probeState, RefreshInterval: cfg.CatalogRefreshInterval,
			MaxObjects: cfg.MaxObjectsPerScan, PageSize: store.MaxPageObjects, AnalyzeBarmanCatalog: cfg.RepositoryFormat == config.FormatBarmanCloud,
			BarmanRecoveryOptions: barmancloud.RecoveryOptions{ExpectedRetentionPolicy: cfg.ExpectedRetentionPolicy, ExpectedMinimumRedundancy: cfg.ExpectedMinimumRedundancy},
			RecentLimit:           recentLimit, Now: time.Now, Logger: logger,
			Redactor: redactor, PGBackRest: pgbackrestOptions,
		})
		if err != nil {
			return nil, err
		}
	}
	app.worker = worker
	if cfg.RuntimeMode.Produces() {
		return app, nil
	}
	handler, err := web.New(web.Options{
		Logger: logger, Provider: string(cfg.Provider), Format: descriptor,
		Inventory: cache.Load,
		Readiness: probeState.Result, TrustedUserHeader: cfg.TrustedUserHeader,
		WALPageSize: cfg.WALPageSize,
	})
	if err != nil {
		return nil, err
	}
	app.server = &http.Server{
		Handler:           handler.Routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	return app, nil
}

type evidencePublishingCache struct {
	cache  *inventory.Cache
	engine *evidenceapi.Engine
}

func (c *evidencePublishingCache) Load() inventory.Snapshot { return c.cache.Load() }

func (c *evidencePublishingCache) Publish(snapshot inventory.Snapshot) error {
	if err := c.engine.Publish(snapshot); err != nil {
		return err
	}
	return c.cache.Publish(snapshot)
}

// prefixConfinedReader narrows a possibly broader provider credential to the
// single format-native scope the agent is configured for. Keys remain
// repository-root-relative because Barman owns and validates that layout.
type prefixConfinedReader struct {
	reader store.Reader
	prefix string
}

var errStoreConfinement = storeConfinementError{}

type storeConfinementError struct{}

func (storeConfinementError) Error() string {
	return "agent store request escaped its configured scope"
}
func (storeConfinementError) Unwrap() error            { return store.ErrInvalidRequest }
func (storeConfinementError) Category() fault.Category { return fault.SafetyLimit }

func newPrefixConfinedReader(reader store.Reader, prefix string) (*prefixConfinedReader, error) {
	if reader == nil || prefix == "" || !strings.HasSuffix(prefix, "/") || len(prefix) > store.MaxKeyBytes {
		return nil, errors.New("invalid agent store confinement")
	}
	return &prefixConfinedReader{reader: reader, prefix: prefix}, nil
}

func (r *prefixConfinedReader) List(ctx context.Context, request store.ListRequest) (store.Page, error) {
	if request.Prefix == "" {
		request.Prefix = r.prefix
	} else if !strings.HasPrefix(request.Prefix, r.prefix) {
		return store.Page{}, errStoreConfinement
	}
	page, err := r.reader.List(ctx, request)
	if err != nil {
		return store.Page{}, err
	}
	for _, object := range page.Objects {
		if !strings.HasPrefix(object.Key, r.prefix) {
			return store.Page{}, errStoreConfinement
		}
	}
	return page, nil
}

func (r *prefixConfinedReader) Open(ctx context.Context, request store.OpenRequest) (io.ReadCloser, error) {
	if !strings.HasPrefix(request.Key, r.prefix) {
		return nil, errStoreConfinement
	}
	return r.reader.Open(ctx, request)
}

func (r *prefixConfinedReader) Stat(ctx context.Context, request store.StatRequest) (store.Object, error) {
	if !strings.HasPrefix(request.Key, r.prefix) {
		return store.Object{}, errStoreConfinement
	}
	object, err := r.reader.Stat(ctx, request)
	if err != nil {
		return store.Object{}, err
	}
	if !strings.HasPrefix(object.Key, r.prefix) {
		return store.Object{}, errStoreConfinement
	}
	return object, nil
}

// unresolvedDestinationBucket stands in for the bucket when the agent
// starts with an invalid repository configuration and no destination could
// be parsed. ParseS3Destination never yields it (it contains spaces and '?'),
// so the resulting fingerprint can never equal a real repository's: a
// consumer sees an identity mismatch beside the invalid_configuration reason,
// never a false match.
const unresolvedDestinationBucket = "? unresolved destination ?"

func newEvidenceEngine(cfg config.Config, producerVersion string, initial inventory.Snapshot) (*evidenceapi.Engine, error) {
	if !cfg.RuntimeMode.Produces() || cfg.ScopeName() == "" || (cfg.Destination == nil && cfg.RepositoryError == nil) {
		return nil, errors.New("invalid agent evidence configuration")
	}
	bucket, account := cfg.DestinationBucket, cfg.Credentials.AzureAccount
	if cfg.Destination == nil {
		bucket = unresolvedDestinationBucket
	}
	if account == "" {
		// Cleared with the credentials of an invalid configuration.
		account = unresolvedDestinationBucket
	}
	endpoint := ""
	if cfg.Endpoint != nil {
		endpoint = cfg.Endpoint.String()
	}
	var clusterName *string
	if cfg.CNPGClusterName != "" {
		value := cfg.CNPGClusterName
		clusterName = &value
	}
	repository := evidencev1alpha1.RepositoryFingerprintInput{
		Provider: string(cfg.Provider), Endpoint: endpoint, Bucket: bucket,
		Prefix: cfg.DestinationPrefix, Format: string(cfg.RepositoryFormat),
		ScopeKind: cfg.ScopeKind(), ScopeName: cfg.ScopeName(),
	}
	if cfg.Provider == config.ProviderAzure {
		// The storage account is part of an Azure repository's identity.
		repository.Account = account
	}
	return evidenceapi.NewEngine(evidenceapi.EngineOptions{
		Projection: evidenceapi.Options{
			ProducerVersion: producerVersion, ClusterNamespace: cfg.CNPGClusterNamespace,
			ClusterUID: cfg.CNPGClusterUID, ClusterName: clusterName,
			Repository: repository,
			// #nosec G115 -- PGBACKREST_REPOSITORY is validated to 1..256.
			PGBackRestArchiveStanza: pgbackrestArchive(cfg), PGBackRestRepository: uint64(max(cfg.PGBackRestRepository, 0)),
		},
		Initial: initial,
	})
}

// Serve blocks until the listener fails or ctx requests graceful shutdown.
func (a *App) Serve(ctx context.Context, listener net.Listener) error {
	if a.runtimeMode != config.RuntimeStandalone || a.server == nil || listener == nil {
		return errors.New("standalone application runtime is unavailable")
	}
	return a.serveRuntime(ctx, func(runtimeCtx context.Context) error {
		return a.serveHTTP(runtimeCtx, listener)
	})
}

// ServeAgent runs the scanner and the evidence/v1alpha1 listener (HTTPS, or
// plain HTTP with AGENT_PLAIN_HTTP) until ctx is canceled.
func (a *App) ServeAgent(ctx context.Context, listener net.Listener) error {
	if a.runtimeMode != config.RuntimeAgent || a.agentHandler == nil || (a.certificates == nil) != a.plainHTTP || listener == nil {
		return errors.New("agent application runtime is unavailable")
	}
	return a.serveRuntime(ctx, func(runtimeCtx context.Context) error {
		if a.plainHTTP {
			return agent.ServePlain(runtimeCtx, listener, a.agentHandler)
		}
		return agent.Serve(runtimeCtx, listener, a.agentHandler, a.certificates)
	})
}

// newAgentTransport assembles the authenticator (bound token or shared
// token) and, unless the agent serves plain HTTP, the reloading serving
// certificate around the engine.
func newAgentTransport(ctx context.Context, cfg config.Config, engine *evidenceapi.Engine, probeState *readiness.ProbeState, reviewer agent.Reviewer, logger *slog.Logger) (http.Handler, *agent.CertificateReloader, error) {
	audience, err := evidencev1alpha1.TokenAudience(cfg.AgentNamespace)
	if err != nil {
		return nil, nil, errors.New("agent namespace is not a DNS-1123 label")
	}
	options := evidenceapi.AgentHandlerOptions{
		Engine: engine, Readiness: probeState, ProducerEpoch: cfg.ProducerEpoch, Logger: logger, Audience: audience,
	}
	switch cfg.AgentAuth {
	case config.AgentAuthSharedToken:
		// No reviewer and no API call: the comparison is local.
		shared, sharedErr := agent.NewSharedTokenAuthenticator(ctx, cfg.AgentTokenFile, logger)
		if sharedErr != nil {
			return nil, nil, sharedErr
		}
		options.Authenticator, options.ParseAuthorization = shared, agent.SharedTokenSyntax
	default:
		if reviewer == nil {
			inCluster, inClusterErr := agent.NewInClusterReviewer()
			if inClusterErr != nil {
				return nil, nil, inClusterErr
			}
			reviewer = inCluster
		}
		authenticator, authErr := agent.NewAuthenticator(agent.AuthenticatorOptions{
			Reviewer: reviewer, Name: cfg.AgentName, Namespace: cfg.AgentNamespace, Logger: logger,
		})
		if authErr != nil {
			return nil, nil, authErr
		}
		options.Authenticator = authenticator
	}
	handler, err := evidenceapi.NewAgentHandler(options)
	if err != nil {
		return nil, nil, err
	}
	if cfg.AgentPlainHTTP {
		return handler, nil, nil
	}
	certificates, err := agent.NewCertificateReloader(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		return nil, nil, err
	}
	return handler, certificates, nil
}

func (a *App) serveRuntime(ctx context.Context, serve func(context.Context) error) error {
	runtimeCtx, cancelRuntime := context.WithCancel(ctx)
	var workers sync.WaitGroup
	if a.worker != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			a.worker.Run(runtimeCtx)
		}()
	}
	defer func() {
		cancelRuntime()
		workers.Wait()
	}()
	return serve(runtimeCtx)
}

func (a *App) serveHTTP(ctx context.Context, listener net.Listener) error {
	a.server.BaseContext = func(net.Listener) context.Context { return ctx }
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- a.server.Serve(listener)
	}()

	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := a.server.Shutdown(shutdownCtx); err != nil {
			_ = a.server.Close()
			return err
		}
		err := <-serveErrors
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func defaultReaderFactory(ctx context.Context, cfg config.Config, redactor *redact.Redactor) (store.Reader, error) {
	switch cfg.Provider {
	case config.ProviderS3:
		return s3store.New(ctx, s3store.Options{
			Bucket: cfg.DestinationBucket, Prefix: cfg.DestinationPrefix,
			Endpoint: cfg.Endpoint, CABundle: cfg.EndpointCABundle, Region: cfg.Credentials.AWSRegion,
			AccessKeyIDFile: cfg.Credentials.AWSAccessKeyIDFile, SecretAccessKeyFile: cfg.Credentials.AWSSecretKeyFile,
			SessionTokenFile: cfg.Credentials.AWSSessionTokenFile, WebIdentityTokenFile: cfg.Credentials.AWSWebIdentityTokenFile,
			RoleARN: cfg.Credentials.AWSRoleARN, RequestTimeout: cfg.StoreRequestTimeout,
			ContainerCredentialsURI:         cfg.Credentials.AWSContainerCredentialsURI,
			ContainerAuthorizationTokenFile: cfg.Credentials.AWSContainerAuthorizationTokenFile,
			DisableInstanceMetadata:         cfg.RuntimeMode == config.RuntimeAgent,
			KeySource:                       cfg.Credentials.AWSKeySource, CredentialsLoaded: redactor.Add,
			SSECustomerKey: cfg.Credentials.S3SSECustomerKey.Bytes(),
		})
	case config.ProviderAzure:
		endpoint := ""
		if cfg.Endpoint != nil {
			endpoint = cfg.Endpoint.String()
		}
		return azurestore.New(ctx, azurestore.Options{Container: cfg.DestinationBucket, Prefix: cfg.DestinationPrefix, Account: cfg.Credentials.AzureAccount, Endpoint: endpoint, ConnectionString: string(cfg.Credentials.AzureConnectionString.Bytes()), AccountKey: cfg.Credentials.AzureAccountKey.Bytes(), SASToken: cfg.Credentials.AzureSASToken.Bytes(), CABundle: cfg.EndpointCABundle, RequestTimeout: cfg.StoreRequestTimeout, WorkloadIdentityOnly: cfg.RuntimeMode == config.RuntimeAgent})
	case config.ProviderGCS:
		return gcsstore.New(ctx, gcsstore.Options{Bucket: cfg.DestinationBucket, Prefix: cfg.DestinationPrefix, CredentialsJSON: cfg.Credentials.GoogleCredentialsJSON.Bytes(), RequestTimeout: cfg.StoreRequestTimeout})
	default:
		return nil, errors.New("unsupported provider")
	}
}

// maxReason is the evidence contract's bound on a reason (bytes).
const maxReason = 256

// boundedReason fits a refusal into the published reason: a longer one
// would fail the snapshot's validation and stop the Pod instead of
// publishing why the evidence is unknown. The log keeps the whole text.
// Control characters become spaces, and a cut ends on a rune boundary.
func boundedReason(reason string) string {
	reason = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, reason)
	if reason = strings.TrimSpace(reason); reason == "" {
		return "the repository configuration is invalid"
	}
	if len(reason) <= maxReason {
		return reason
	}
	const ellipsis = "…"
	cut := maxReason - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut] + ellipsis
}

// pgbackrestRetention is the configured retention in the reader's terms;
// config validated the values.
func pgbackrestRetention(value *config.PGBackRestRetention) *pgbackrest.Retention {
	if value == nil {
		return nil
	}
	return &pgbackrest.Retention{
		Full: value.Full, FullType: pgbackrest.RetentionFullType(value.FullType), Diff: value.Diff,
		Archive: value.Archive, ArchiveType: pgbackrest.BackupType(value.ArchiveType),
	}
}

func pgbackrestArchive(cfg config.Config) string {
	_, archive := cfg.PGBackRestScope()
	return archive
}
