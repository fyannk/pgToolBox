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

package config

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/fyannk/pgToolBox/internal/backupresolve"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	defaultListenAddr             = ":3000"
	defaultTrustedUserHeader      = "X-Forwarded-User"
	defaultCatalogRefreshInterval = 5 * time.Minute
	defaultStoreRequestTimeout    = 10 * time.Second
	defaultScanConcurrency        = 4
	defaultMaxObjectsPerScan      = 1_000_000
	defaultWALPageSize            = 200
	maxSecretFileBytes            = 64 * 1024
	maxConfigFileBytes            = 1024 * 1024
	maxConfiguredScopes           = 1_000
)

// RuntimeMode selects one explicit process surface.
type RuntimeMode string

const (
	// RuntimeStandalone serves the viewer's own web UI; it is the default.
	RuntimeStandalone RuntimeMode = ""
	// RuntimeAgent is the long-running in-cluster HTTPS producer of
	// evidence/v1alpha1, deployed by an EvidenceAgent or the team chart:
	// bound-token (TokenReview + SubjectAccessReview) or shared-token
	// authentication.
	RuntimeAgent RuntimeMode = "agent"
)

// Produces reports whether the mode publishes the evidence contract rather
// than serving the standalone viewer UI.
func (m RuntimeMode) Produces() bool {
	return m == RuntimeAgent
}

const (
	defaultAgentListenAddr = ":8443"
	maxAgentNameBytes      = 63
	maxAllowedSecrets      = 64
)

// AgentAuth selects how the agent authenticates its callers (AGENT_AUTH).
type AgentAuth string

const (
	// AgentAuthServiceAccount is the operator's profile: the console
	// presents a bound ServiceAccount token, checked with a TokenReview and
	// a SubjectAccessReview (a ClusterRole the operator manages).
	AgentAuthServiceAccount AgentAuth = "serviceaccount"
	// AgentAuthSharedToken is the pgtools chart's profile:
	// one random token in a Secret the console and the agent both mount,
	// compared in constant time; no API call and no cluster-scoped grant.
	AgentAuthSharedToken AgentAuth = "sharedtoken"
)

// AgentResolve selects where the agent's repository coordinates come from
// (AGENT_RESOLVE).
type AgentResolve string

const (
	// AgentResolveEnvironment is the operator's profile: the operator
	// resolves the Cluster and renders the repository variables.
	AgentResolveEnvironment AgentResolve = ""
	// AgentResolveCluster is the pgtools chart's profile:
	// the agent reads CNPG_CLUSTER_NAME in its own namespace and resolves
	// Cluster -> ObjectStore -> credential Secrets itself
	// (internal/evidence/selfresolve, through the resolver the operator
	// uses), with a namespaced Role. Config then carries no repository
	// coordinates until ApplyResolved.
	AgentResolveCluster AgentResolve = "cluster"
)

// repositoryVariables are the repository coordinates and credentials the
// operator renders. With AGENT_RESOLVE=cluster the agent reads them from
// the Cluster and its ObjectStore instead, so setting one is a conflict.
//
// AWS_REGION, AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE are not in the
// list: the EKS identity webhook injects them into a Pod whose
// ServiceAccount carries a role (IRSA), and a workload-identity repository
// (s3Credentials.inheritFromIAMRole) reads them through the SDK's default
// chain. AWS_REGION is used only by such a repository, when its
// configuration names no region.
var repositoryVariables = []string{
	"REPOSITORY_FORMAT", "PROVIDER", "DESTINATION_PATH", "ENDPOINT_URL", "ENDPOINT_CA_FILE",
	"BARMAN_SERVER_NAMES", "PGBACKREST_STANZAS", "PGBACKREST_CIPHER_PASS_FILE", "STORE_CREDENTIAL_MODE",
	"PGBACKREST_STANZA", "PGBACKREST_ARCHIVE_STANZA", "PGBACKREST_REPOSITORY",
	"PGBACKREST_RETENTION_FULL", "PGBACKREST_RETENTION_FULL_TYPE", "PGBACKREST_RETENTION_DIFF",
	"PGBACKREST_RETENTION_ARCHIVE", "PGBACKREST_RETENTION_ARCHIVE_TYPE",
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_ACCESS_KEY_ID_FILE",
	"AWS_SECRET_ACCESS_KEY_FILE", "AWS_SESSION_TOKEN_FILE", "S3_SSE_CUSTOMER_KEY_FILE",
	"AZURE_STORAGE_ACCOUNT", "AZURE_STORAGE_CONNECTION_STRING_FILE", "AZURE_STORAGE_ACCOUNT_KEY_FILE", "AZURE_STORAGE_SAS_TOKEN_FILE",
	"GOOGLE_APPLICATION_CREDENTIALS", "CNPG_CLUSTER_UID", "PRODUCER_EPOCH",
}

// RepositoryFormat identifies the only repository grammar allowed in a scope.
type RepositoryFormat string

const (
	// FormatBarmanCloud is a repository written by barman-cloud.
	FormatBarmanCloud RepositoryFormat = "barman-cloud"
	// FormatPGBackRest is a pgBackRest repository.
	FormatPGBackRest RepositoryFormat = "pgbackrest"
)

// Provider identifies the object-store transport independently of format.
type Provider string

const (
	// ProviderS3 is Amazon S3 or an S3-compatible service.
	ProviderS3 Provider = "s3"
	// ProviderAzure is Azure Blob Storage.
	ProviderAzure Provider = "azure"
	// ProviderGCS is Google Cloud Storage.
	ProviderGCS Provider = "gcs"
)

// CredentialMode states where an eventual provider adapter obtains credentials.
type CredentialMode string

const (
	// CredentialAmbient uses no configured secret: the provider SDK's default
	// chain, or for the agent its Pod's workload identity only.
	CredentialAmbient CredentialMode = "ambient"
	// CredentialStaticFiles reads S3 access keys from mounted files.
	CredentialStaticFiles CredentialMode = "static-files"
	// CredentialAWSWebIdentity assumes AWS_ROLE_ARN with the token in
	// AWS_WEB_IDENTITY_TOKEN_FILE.
	CredentialAWSWebIdentity CredentialMode = "aws-web-identity"
	// CredentialConnectionFile reads an Azure connection string from a file.
	CredentialConnectionFile CredentialMode = "connection-string-file"
	// CredentialAccountKeyFile reads an Azure storage account key from a file.
	CredentialAccountKeyFile CredentialMode = "account-key-file"
	// CredentialSASTokenFile reads an Azure SAS token from a file.
	//
	// #nosec G101 -- these are credential *mode* names, not credentials.
	CredentialSASTokenFile CredentialMode = "sas-token-file"
	// CredentialJSONFile reads a Google service account key from a JSON file.
	CredentialJSONFile CredentialMode = "json-file"
	// CredentialKubernetesSecrets is set by ApplyResolved, never by
	// STORE_CREDENTIAL_MODE: the S3 keys are read from the Secrets the
	// ObjectStore names, through the API, when the store needs them.
	CredentialKubernetesSecrets CredentialMode = "kubernetes-secrets" //nolint:gosec // a mode name, not a credential
)

// KeySource supplies S3 keys read at run time (AGENT_RESOLVE=cluster: from
// the Secrets the ObjectStore names). The S3 adapter asks again at its
// credential recheck interval, so a rotated Secret is followed without a
// restart. Callers must not log the values.
type KeySource interface {
	Keys(ctx context.Context) (accessKeyID, secretAccessKey, sessionToken []byte, err error)
}

// Secret deliberately has no exported representation and always formats redacted.
type Secret struct {
	value []byte
}

// Bytes returns a copy for a provider adapter. Callers must not log the result.
func (s Secret) Bytes() []byte {
	return bytes.Clone(s.value)
}

// Empty reports whether no value was configured.
func (s Secret) Empty() bool { return len(s.value) == 0 }

func (s Secret) String() string { return "[REDACTED]" }

// GoString keeps %#v from printing the value.
func (s Secret) GoString() string { return "[REDACTED]" }

// Credentials contains only credentials selected for the configured provider.
type Credentials struct {
	Mode CredentialMode

	AWSAccessKeyID  Secret
	AWSSecretKey    Secret
	AWSSessionToken Secret
	// The absolute credential file paths (not secret) let the S3 adapter
	// re-read rotated keys; the values above are the startup read that
	// validates them and seeds the redactor.
	AWSAccessKeyIDFile      string
	AWSSecretKeyFile        string
	AWSSessionTokenFile     string
	AWSRegion               string
	AWSRoleARN              string
	AWSWebIdentityTokenFile string
	// AWSContainerCredentialsURI and AWSContainerAuthorizationTokenFile are
	// EKS Pod Identity's (CredentialAmbient in the agent): the agent's AWS
	// workload identity is IRSA (the two above) or this, never the node's.
	AWSContainerCredentialsURI         string
	AWSContainerAuthorizationTokenFile string
	// AWSKeySource is set with CredentialKubernetesSecrets only.
	AWSKeySource KeySource
	// S3SSECustomerKey is the raw 256-bit key of S3 server-side encryption
	// with customer keys (SSE-C), whatever the credential mode: every
	// object read sends it (S3_SSE_CUSTOMER_KEY_FILE, or the
	// configuration's s3Credentials.sseCustomerKey).
	S3SSECustomerKey Secret

	AzureAccount          string
	AzureConnectionString Secret
	AzureAccountKey       Secret
	AzureSASToken         Secret

	GoogleCredentialsJSON Secret
}

// Config is the agent's environment contract: one repository per agent.
type Config struct {
	RuntimeMode RuntimeMode

	RepositoryFormat RepositoryFormat
	Provider         Provider
	Destination      *url.URL
	// DestinationBucket and DestinationPrefix are the bucket or container and
	// the decoded, slash-trimmed key prefix of Destination. For S3 they come
	// from evidencev1alpha1.ParseS3Destination, the parser pgtoolbox and
	// The console share, so all three hash the same repository identity.
	DestinationBucket string
	DestinationPrefix string
	Endpoint          *url.URL
	EndpointCABundle  []byte

	BarmanServerNames []string
	// PGBackRestStanzas is the viewer's (standalone) list; the producing
	// modes read one stanza instead.
	PGBackRestStanzas []string
	// PGBackRestCipher is the repository passphrase (repo-cipher-pass) read
	// from PGBACKREST_CIPHER_PASS_FILE. It opens the info files only; never
	// logged or served.
	PGBackRestCipher Secret
	// PGBackRestStanza (PGBACKREST_STANZA) holds the backups of a producing
	// agent's repository; PGBackRestArchiveStanza
	// (PGBACKREST_ARCHIVE_STANZA) holds its WAL, PGBackRestStanza when the
	// variable is unset; PGBackRestRepository
	// (PGBACKREST_REPOSITORY, default 1) is the repository number, reported
	// only: DESTINATION_PATH already points at that repository's root.
	PGBackRestStanza        string
	PGBackRestArchiveStanza string
	PGBackRestRepository    int
	// PGBackRestRetention is the repository's configured retention
	// (PGBACKREST_RETENTION_*), which the agent compares the stanza with;
	// nil when none of the variables is set.
	PGBackRestRetention *PGBackRestRetention

	ListenAddr        string
	TrustedUserHeader string
	AllowDownload     bool

	CNPGClusterNamespace string
	CNPGClusterUID       string
	CNPGClusterName      string

	// Agent mode only. TLSCertFile and TLSKeyFile are the serving key pair
	// (re-read on rotation), both empty with AgentPlainHTTP; AgentName and
	// AgentNamespace name the EvidenceAgent whose "get" the
	// SubjectAccessReview checks (or, without the operator, the agent's
	// Deployment); ProducerEpoch is stamped into every served snapshot
	// (derived by ApplyResolved with AGENT_RESOLVE=cluster).
	TLSCertFile    string
	TLSKeyFile     string
	AgentName      string
	AgentNamespace string
	ProducerEpoch  uint64
	// AgentAuth selects the authenticator; AgentTokenFile is the shared
	// token's file with AgentAuthSharedToken.
	AgentAuth      AgentAuth
	AgentTokenFile string
	// AgentPlainHTTP serves plain HTTP instead of TLS (pgtools'
	// evidence.tls.mode=none).
	AgentPlainHTTP bool
	// AgentResolve and AgentAllowedSecrets: with AgentResolveCluster the
	// agent resolves its repository itself and may read only the Secrets
	// listed here (the chart's Role names the same list).
	AgentResolve        AgentResolve
	AgentAllowedSecrets []string
	// AgentAllowInsecureEndpoint and AgentWorkloadIdentity are the chart
	// entry's opt-ins (AGENT_ALLOW_INSECURE_ENDPOINT, AGENT_WORKLOAD_IDENTITY),
	// checked against the resolved configuration.
	AgentAllowInsecureEndpoint bool
	AgentWorkloadIdentity      bool
	// AgentPgBackRestRepository and AgentPgBackRestEndpointCA are the chart
	// entry's pgbackrest.repository (0: the default, repo1) and
	// pgbackrest.endpointCA (AGENT_PGBACKREST_REPOSITORY,
	// AGENT_PGBACKREST_ENDPOINT_CA_SECRET and _KEY), for a Cluster backed
	// up by a pgBackRest plugin.
	AgentPgBackRestRepository int
	AgentPgBackRestEndpointCA SecretKey
	// AgentPgBackRestEndpoint is the chart entry's pgbackrest.endpoint
	// (AGENT_PGBACKREST_ENDPOINT): the host[:port] the repository's
	// endpoint must be, checked against the resolved configuration.
	AgentPgBackRestEndpoint string
	// podAWSRegion is the Pod's AWS_REGION with AGENT_RESOLVE=cluster: the
	// region of an S3 workload-identity repository whose configuration
	// names none.
	podAWSRegion string
	// podAWSIdentity is what the identity webhook injected into the Pod
	// (AGENT_RESOLVE=cluster), for an ambient S3 repository.
	podAWSIdentity awsWorkloadIdentity

	CatalogRefreshInterval time.Duration
	StoreRequestTimeout    time.Duration
	ScanConcurrency        int
	MaxObjectsPerScan      int
	WALPageSize            int

	ExpectedRetentionPolicy   string
	ExpectedMinimumRedundancy *int
	Credentials               Credentials

	// RepositoryError is set only in agent mode, when the process-level
	// contract (runtime, authentication, identity, scope) is valid but the
	// repository coordinates are not: DESTINATION_PATH, ENDPOINT_URL,
	// ENDPOINT_CA_FILE, AWS_REGION, or the S3 credential variables. The
	// agent then starts without a store and publishes unknown evidence with
	// this redacted error instead of exiting, so the console reads why the
	// repository is unreadable rather than a crash-looping Pod. Load still
	// returns every error in standalone mode.
	RepositoryError error
}

// SecretValues returns copies solely for constructing a boundary redactor.
func (c Config) SecretValues() [][]byte {
	secrets := []Secret{
		c.PGBackRestCipher,
		c.Credentials.AWSAccessKeyID,
		c.Credentials.AWSSecretKey,
		c.Credentials.AWSSessionToken,
		c.Credentials.AzureConnectionString,
		c.Credentials.AzureAccountKey,
		c.Credentials.AzureSASToken,
		c.Credentials.GoogleCredentialsJSON,
	}
	values := make([][]byte, 0, len(secrets)+2)
	for _, secret := range secrets {
		if !secret.Empty() {
			values = append(values, secret.Bytes())
		}
	}
	if key := c.Credentials.S3SSECustomerKey; !key.Empty() {
		// The raw key, and its base64 form: the one a header or the
		// Secret carries.
		values = append(values, key.Bytes(), []byte(base64.StdEncoding.EncodeToString(key.value)))
	}
	return values
}

// Error is safe to return or log: it never embeds an environment value or path.
type Error struct {
	Variable string
	Code     string
}

func (e *Error) Error() string {
	if e.Variable == "" {
		return "configuration invalid: " + e.Code
	}
	return fmt.Sprintf("configuration invalid: %s: %s", e.Variable, e.Code)
}

// Load reads only the documented variables through getenv. In tests and other
// injected environments, an empty value is treated as absent.
func Load(getenv func(string) string) (Config, error) {
	return load(getenv, func(key string) (string, bool) {
		value := getenv(key)
		return value, value != ""
	})
}

func load(getenv func(string) string, lookupEnv func(string) (string, bool)) (Config, error) {
	runtimeMode, err := parseRuntimeMode(getenv("RUNTIME_MODE"))
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		RuntimeMode:            runtimeMode,
		CatalogRefreshInterval: defaultCatalogRefreshInterval,
		StoreRequestTimeout:    defaultStoreRequestTimeout,
		ScanConcurrency:        defaultScanConcurrency,
		MaxObjectsPerScan:      defaultMaxObjectsPerScan,
		WALPageSize:            defaultWALPageSize,
	}
	switch runtimeMode {
	case RuntimeStandalone:
		cfg.ListenAddr = defaultListenAddr
		cfg.TrustedUserHeader = defaultTrustedUserHeader
	case RuntimeAgent:
		cfg.ListenAddr = defaultAgentListenAddr
	}

	selfResolving := runtimeMode == RuntimeAgent && AgentResolve(getenv("AGENT_RESOLVE")) == AgentResolveCluster
	if err := loadRepositoryShape(&cfg, selfResolving, getenv); err != nil {
		return Config{}, err
	}
	if !selfResolving {
		if err := deferRepositoryError(&cfg, loadDestination(&cfg, cfg.Provider, getenv)); err != nil {
			return Config{}, err
		}
	}
	if err := loadListener(&cfg, getenv, lookupEnv); err != nil {
		return Config{}, err
	}
	if err := loadLimits(&cfg, getenv); err != nil {
		return Config{}, err
	}
	if err := loadExpectations(&cfg, getenv); err != nil {
		return Config{}, err
	}
	if err := loadFormatConfig(&cfg, getenv); err != nil {
		return Config{}, err
	}
	if err := loadRuntimeConfig(&cfg, getenv); err != nil {
		return Config{}, err
	}
	if cfg.RepositoryError == nil && !selfResolving {
		if err := deferRepositoryError(&cfg, loadCredentials(&cfg, getenv)); err != nil {
			return Config{}, err
		}
	}
	if cfg.RepositoryError != nil {
		// No credential or coordinate may reach a store adapter.
		cfg.Credentials = Credentials{}
		cfg.Endpoint, cfg.EndpointCABundle = nil, nil
	}
	return cfg, nil
}

// loadRepositoryShape sets the repository format and provider. A
// self-resolving agent takes the repository from the Cluster
// (ApplyResolved), so a variable naming it here would be a second,
// conflicting source and is refused.
func loadRepositoryShape(cfg *Config, selfResolving bool, getenv func(string) string) error {
	if selfResolving {
		for _, variable := range repositoryVariables {
			if getenv(variable) != "" {
				return configError(variable, "does not apply with AGENT_RESOLVE=cluster: the agent resolves the repository from the Cluster")
			}
		}
		// The only shape the shared resolver yields today; ApplyResolved
		// fills in the coordinates.
		cfg.RepositoryFormat, cfg.Provider = FormatBarmanCloud, ProviderS3
		return nil
	}
	format, err := parseFormat(getenv("REPOSITORY_FORMAT"))
	if err != nil {
		return err
	}
	cfg.RepositoryFormat = format
	provider, err := parseProvider(getenv("PROVIDER"))
	if err != nil {
		return err
	}
	cfg.Provider = provider
	return nil
}

// deferRepositoryError defers a repository-coordinate error in a mode
// that produces evidence (see Config.RepositoryError): the first one is
// kept and nil returned. Every other error is returned unchanged.
func deferRepositoryError(cfg *Config, err error) error {
	if err == nil || !cfg.RuntimeMode.Produces() || !IsError(err) {
		return err
	}
	if cfg.RepositoryError == nil {
		cfg.RepositoryError = err
	}
	return nil
}

// loadListener reads the HTTP listener per mode: the agent has no trusted
// user header, the standalone viewer may set or clear one.
func loadListener(cfg *Config, getenv func(string) string, lookupEnv func(string) (string, bool)) error {
	switch cfg.RuntimeMode {
	case RuntimeAgent:
		if _, present := lookupEnv("TRUSTED_USER_HEADER"); present {
			return configError("TRUSTED_USER_HEADER", "does not apply to agent")
		}
		if raw := getenv("LISTEN_ADDR"); raw != "" {
			cfg.ListenAddr = raw
		}
		if err := validateListenAddr(cfg.ListenAddr); err != nil {
			return err
		}
	default:
		if raw := getenv("LISTEN_ADDR"); raw != "" {
			cfg.ListenAddr = raw
		}
		if err := validateListenAddr(cfg.ListenAddr); err != nil {
			return err
		}
		if raw, present := lookupEnv("TRUSTED_USER_HEADER"); present {
			cfg.TrustedUserHeader = raw
		}
		if cfg.TrustedUserHeader != "" && !validHeaderName(cfg.TrustedUserHeader) {
			return configError("TRUSTED_USER_HEADER", "must be an HTTP field name or empty")
		}
	}
	return nil
}

// loadLimits reads the download switch (never on where evidence is
// produced) and the bounded tunables of the scan.
func loadLimits(cfg *Config, getenv func(string) string) error {
	var err error
	if cfg.AllowDownload, err = parseBool(getenv("ALLOW_DOWNLOAD"), false, "ALLOW_DOWNLOAD"); err != nil {
		return err
	}
	if cfg.RuntimeMode.Produces() && cfg.AllowDownload {
		return configError("ALLOW_DOWNLOAD", "must be false in "+string(cfg.RuntimeMode))
	}
	if cfg.CatalogRefreshInterval, err = parseDuration(getenv("CATALOG_REFRESH_INTERVAL"), defaultCatalogRefreshInterval, 30*time.Second, 24*time.Hour, "CATALOG_REFRESH_INTERVAL"); err != nil {
		return err
	}
	if cfg.StoreRequestTimeout, err = parseDuration(getenv("STORE_REQUEST_TIMEOUT"), defaultStoreRequestTimeout, time.Second, 5*time.Minute, "STORE_REQUEST_TIMEOUT"); err != nil {
		return err
	}
	if cfg.ScanConcurrency, err = parseInt(getenv("SCAN_CONCURRENCY"), defaultScanConcurrency, 1, 64, "SCAN_CONCURRENCY"); err != nil {
		return err
	}
	if cfg.MaxObjectsPerScan, err = parseInt(getenv("MAX_OBJECTS_PER_SCAN"), defaultMaxObjectsPerScan, 1_000, 10_000_000, "MAX_OBJECTS_PER_SCAN"); err != nil {
		return err
	}
	if cfg.WALPageSize, err = parseInt(getenv("WAL_PAGE_SIZE"), defaultWALPageSize, 1, 1_000, "WAL_PAGE_SIZE"); err != nil {
		return err
	}
	return nil
}

// loadExpectations reads the retention policy and minimum redundancy the
// Barman recovery evidence is checked against; both are optional.
func loadExpectations(cfg *Config, getenv func(string) string) error {
	cfg.ExpectedRetentionPolicy = getenv("EXPECTED_RETENTION_POLICY")
	if len(cfg.ExpectedRetentionPolicy) > 256 || containsControl(cfg.ExpectedRetentionPolicy) {
		return configError("EXPECTED_RETENTION_POLICY", "must be at most 256 characters without control characters")
	}
	if raw := getenv("EXPECTED_MINIMUM_REDUNDANCY"); raw != "" {
		value, err := parseInt(raw, 0, 0, 100_000, "EXPECTED_MINIMUM_REDUNDANCY")
		if err != nil {
			return err
		}
		cfg.ExpectedMinimumRedundancy = &value
	}
	return nil
}

func loadDestination(cfg *Config, provider Provider, getenv func(string) string) error {
	destination, err := parseDestination(provider, getenv("DESTINATION_PATH"))
	if err != nil {
		return err
	}
	cfg.Destination = destination
	cfg.DestinationBucket = destination.Host
	cfg.DestinationPrefix = strings.Trim(destination.Path, "/")

	if raw := getenv("ENDPOINT_URL"); raw != "" {
		// S3: a compatible store; Azure: an emulator (Azurite) origin, the
		// account then being the first path segment.
		if provider == ProviderGCS {
			return configError("ENDPOINT_URL", "supported only for s3 and azure")
		}
		cfg.Endpoint, err = parseEndpoint(raw)
		if err != nil {
			return err
		}
	}

	if path := getenv("ENDPOINT_CA_FILE"); path != "" {
		// S3: a compatible store's CA; Azure: the Blob endpoint's
		// (Azurite over TLS, Azure Stack). barman-cloud uses no CA for
		// Google Cloud Storage, which has no endpoint to set either.
		if provider == ProviderGCS {
			return configError("ENDPOINT_CA_FILE", "supported only for s3 and azure")
		}
		cfg.EndpointCABundle, err = readBoundedFile("ENDPOINT_CA_FILE", path, maxConfigFileBytes, false)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.EndpointCABundle) {
			return configError("ENDPOINT_CA_FILE", "must contain at least one PEM certificate")
		}
	}
	return nil
}

// agentVariables apply only to RUNTIME_MODE=agent.
var agentVariables = []string{
	"TLS_CERT_FILE", "TLS_KEY_FILE", "AGENT_NAME", "AGENT_NAMESPACE", "PRODUCER_EPOCH",
	"AGENT_AUTH", "AGENT_TOKEN_FILE", "AGENT_PLAIN_HTTP", "AGENT_RESOLVE", "AGENT_ALLOWED_SECRETS",
	"AGENT_ALLOW_INSECURE_ENDPOINT", "AGENT_WORKLOAD_IDENTITY", "AGENT_PGBACKREST_REPOSITORY",
	"AGENT_PGBACKREST_ENDPOINT_CA_SECRET", "AGENT_PGBACKREST_ENDPOINT_CA_KEY", "AGENT_PGBACKREST_ENDPOINT",
}

func loadRuntimeConfig(cfg *Config, getenv func(string) string) error {
	if cfg.RuntimeMode == RuntimeStandalone {
		for _, variable := range append([]string{"CNPG_CLUSTER_NAMESPACE", "CNPG_CLUSTER_UID", "CNPG_CLUSTER_NAME", "STORE_CREDENTIAL_MODE"}, agentVariables...) {
			if getenv(variable) != "" {
				return configError(variable, "requires agent")
			}
		}
		return nil
	}
	mode := string(cfg.RuntimeMode)
	// Both renderings set it (the AWS SDK reads it); the S3 adapter disables
	// instance metadata in the agent regardless, so a Pod that turns it back
	// on is a misconfiguration, not a choice.
	if value := getenv("AWS_EC2_METADATA_DISABLED"); value != "" && !strings.EqualFold(value, "true") {
		return configError("AWS_EC2_METADATA_DISABLED", "must be true or empty: the agent never uses the node's instance metadata")
	}
	if AgentResolve(getenv("AGENT_RESOLVE")) == AgentResolveCluster {
		return loadSelfResolvingAgent(cfg, getenv)
	}
	switch {
	case cfg.RepositoryFormat == FormatPGBackRest:
		// One stanza per agent; loadFormatConfig required PGBACKREST_STANZA.
	case cfg.RepositoryFormat != FormatBarmanCloud:
		return configError("REPOSITORY_FORMAT", mode+" requires barman-cloud or pgbackrest")
	case len(cfg.BarmanServerNames) != 1:
		return configError("BARMAN_SERVER_NAMES", mode+" requires exactly one server")
	}
	if err := loadAgentConfig(cfg, getenv); err != nil {
		return err
	}
	cfg.CNPGClusterNamespace = getenv("CNPG_CLUSTER_NAMESPACE")
	cfg.CNPGClusterUID = getenv("CNPG_CLUSTER_UID")
	cfg.CNPGClusterName = getenv("CNPG_CLUSTER_NAME")
	if invalidIdentity(cfg.CNPGClusterNamespace, 63, true) {
		return configError("CNPG_CLUSTER_NAMESPACE", "must be a bounded non-empty identifier")
	}
	if invalidIdentity(cfg.CNPGClusterUID, 128, true) {
		return configError("CNPG_CLUSTER_UID", "must be a bounded non-empty identifier")
	}
	if invalidIdentity(cfg.CNPGClusterName, 253, false) {
		return configError("CNPG_CLUSTER_NAME", "must be a bounded identifier or empty")
	}
	if cfg.AgentNamespace != cfg.CNPGClusterNamespace {
		// Same-namespace only: the agent reads its own namespace's
		// repository with its own namespace's credentials.
		return configError("AGENT_NAMESPACE", "must equal CNPG_CLUSTER_NAMESPACE")
	}
	return nil
}

func loadAgentConfig(cfg *Config, getenv func(string) string) error {
	if err := loadAgentTransport(cfg, getenv); err != nil {
		return err
	}
	if getenv("AGENT_RESOLVE") != "" {
		return configError("AGENT_RESOLVE", "must be cluster or empty")
	}
	for _, variable := range []string{
		"AGENT_ALLOWED_SECRETS", "AGENT_ALLOW_INSECURE_ENDPOINT", "AGENT_WORKLOAD_IDENTITY",
		"AGENT_PGBACKREST_REPOSITORY", "AGENT_PGBACKREST_ENDPOINT_CA_SECRET", "AGENT_PGBACKREST_ENDPOINT_CA_KEY",
		"AGENT_PGBACKREST_ENDPOINT",
	} {
		if getenv(variable) != "" {
			// The operator applies the EvidenceAgent's own choices before
			// it renders anything.
			return configError(variable, "requires AGENT_RESOLVE=cluster")
		}
	}
	epoch, err := evidencev1alpha1.ParseProducerEpoch(getenv("PRODUCER_EPOCH"))
	if err != nil {
		return configError("PRODUCER_EPOCH", "must be a decimal integer of at least 1")
	}
	cfg.ProducerEpoch = epoch
	return nil
}

// loadAgentTransport reads what every agent profile has: its identity,
// how it authenticates callers and whether it serves TLS.
func loadAgentTransport(cfg *Config, getenv func(string) string) error {
	switch auth := AgentAuth(getenv("AGENT_AUTH")); auth {
	case "", AgentAuthServiceAccount:
		cfg.AgentAuth = AgentAuthServiceAccount
		if getenv("AGENT_TOKEN_FILE") != "" {
			return configError("AGENT_TOKEN_FILE", "requires AGENT_AUTH=sharedtoken")
		}
	case AgentAuthSharedToken:
		cfg.AgentAuth = auth
		// The file itself may appear later (a Secret of the same release):
		// the authenticator refuses every request until it is valid.
		cfg.AgentTokenFile = getenv("AGENT_TOKEN_FILE")
		if !validAbsolutePath(cfg.AgentTokenFile) {
			return configError("AGENT_TOKEN_FILE", "must be an absolute clean path")
		}
	default:
		return configError("AGENT_AUTH", "must be serviceaccount, sharedtoken or empty")
	}
	plain, err := parseBool(getenv("AGENT_PLAIN_HTTP"), false, "AGENT_PLAIN_HTTP")
	if err != nil {
		return err
	}
	cfg.AgentPlainHTTP = plain
	if plain {
		// Plain HTTP only inside one namespace with a shared token: whoever
		// can tamper with that path can already read the object-store keys
		// the agent holds, and what travels is backup metadata and a
		// token. A bound ServiceAccount token is never sent in
		// clear.
		if cfg.AgentAuth != AgentAuthSharedToken {
			return configError("AGENT_PLAIN_HTTP", "requires AGENT_AUTH=sharedtoken")
		}
		for _, variable := range []string{"TLS_CERT_FILE", "TLS_KEY_FILE"} {
			if getenv(variable) != "" {
				return configError(variable, "cannot be combined with AGENT_PLAIN_HTTP=true")
			}
		}
	} else {
		cfg.TLSCertFile = getenv("TLS_CERT_FILE")
		if !validAbsolutePath(cfg.TLSCertFile) {
			return configError("TLS_CERT_FILE", "must be an absolute clean path (or set AGENT_PLAIN_HTTP=true)")
		}
		cfg.TLSKeyFile = getenv("TLS_KEY_FILE")
		if !validAbsolutePath(cfg.TLSKeyFile) {
			return configError("TLS_KEY_FILE", "must be an absolute clean path (or set AGENT_PLAIN_HTTP=true)")
		}
	}
	cfg.AgentName = getenv("AGENT_NAME")
	if !validDNSLabel(cfg.AgentName, maxAgentNameBytes) {
		return configError("AGENT_NAME", "must be a DNS-1123 label")
	}
	cfg.AgentNamespace = getenv("AGENT_NAMESPACE")
	if !validDNSLabel(cfg.AgentNamespace, 63) {
		return configError("AGENT_NAMESPACE", "must be a DNS-1123 label")
	}
	return nil
}

// loadSelfResolvingAgent is AGENT_RESOLVE=cluster: the process contract
// only. The repository coordinates, the Cluster's UID and the producer
// epoch are set by ApplyResolved once the Cluster has been read.
func loadSelfResolvingAgent(cfg *Config, getenv func(string) string) error {
	cfg.AgentResolve = AgentResolveCluster
	if err := loadAgentTransport(cfg, getenv); err != nil {
		return err
	}
	cfg.CNPGClusterName = getenv("CNPG_CLUSTER_NAME")
	if !validDNSSubdomain(cfg.CNPGClusterName) {
		return configError("CNPG_CLUSTER_NAME", "must name the Cluster to resolve (a DNS-1123 subdomain)")
	}
	cfg.CNPGClusterNamespace = getenv("CNPG_CLUSTER_NAMESPACE")
	switch cfg.CNPGClusterNamespace {
	case "":
		cfg.CNPGClusterNamespace = cfg.AgentNamespace
	case cfg.AgentNamespace:
	default:
		// Same-namespace only: the Role that lets the agent read the
		// Cluster and its Secrets is in the agent's own namespace.
		return configError("CNPG_CLUSTER_NAMESPACE", "must be empty or equal AGENT_NAMESPACE")
	}
	raw := getenv("AGENT_ALLOWED_SECRETS")
	seen := map[string]bool{}
	for item := range strings.SplitSeq(raw, ",") {
		name := strings.TrimSpace(item)
		if name == "" || seen[name] {
			continue
		}
		if !validDNSSubdomain(name) {
			return configError("AGENT_ALLOWED_SECRETS", "must be a comma-separated list of Secret names")
		}
		seen[name] = true
		cfg.AgentAllowedSecrets = append(cfg.AgentAllowedSecrets, name)
	}
	// Empty is valid: a workload-identity repository reads no Secret.
	if len(cfg.AgentAllowedSecrets) > maxAllowedSecrets {
		return configError("AGENT_ALLOWED_SECRETS", "must name at most 64 Secrets the agent may read: it never reads another")
	}
	sort.Strings(cfg.AgentAllowedSecrets)
	var err error
	if cfg.AgentAllowInsecureEndpoint, err = parseOptIn("AGENT_ALLOW_INSECURE_ENDPOINT", getenv); err != nil {
		return err
	}
	if cfg.AgentWorkloadIdentity, err = parseOptIn("AGENT_WORKLOAD_IDENTITY", getenv); err != nil {
		return err
	}
	if err := loadAgentPgBackRest(cfg, getenv); err != nil {
		return err
	}
	cfg.podAWSRegion = getenv("AWS_REGION")
	cfg.podAWSIdentity = awsWorkloadIdentity{
		roleARN: getenv("AWS_ROLE_ARN"), tokenFile: getenv("AWS_WEB_IDENTITY_TOKEN_FILE"),
		containerURI: getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"), containerTokenFile: getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"),
	}
	return nil
}

// loadAgentPgBackRest reads the chart entry's pgbackrest settings: the
// repository number (1 to 256, as spec.pgbackrest.repository) and the
// endpoint CA, a key of a Secret the allow-list must name.
func loadAgentPgBackRest(cfg *Config, getenv func(string) string) error {
	if raw := getenv("AGENT_PGBACKREST_REPOSITORY"); raw != "" {
		repository, err := strconv.Atoi(raw)
		if err != nil || repository < 1 || repository > 256 {
			return configError("AGENT_PGBACKREST_REPOSITORY", "must be a repository number from 1 to 256")
		}
		cfg.AgentPgBackRestRepository = repository
	}
	if pin := getenv("AGENT_PGBACKREST_ENDPOINT"); pin != "" {
		if _, _, ok := backupresolve.ParseEndpointPin(pin); !ok {
			return configError("AGENT_PGBACKREST_ENDPOINT", "must be a host[:port]: a DNS name, an IPv4 address or a bracketed IPv6 address")
		}
		cfg.AgentPgBackRestEndpoint = pin
	}
	name, key := getenv("AGENT_PGBACKREST_ENDPOINT_CA_SECRET"), getenv("AGENT_PGBACKREST_ENDPOINT_CA_KEY")
	switch {
	case name == "" && key == "":
		return nil
	case !validDNSSubdomain(name):
		return configError("AGENT_PGBACKREST_ENDPOINT_CA_SECRET", "must name a Secret, with AGENT_PGBACKREST_ENDPOINT_CA_KEY")
	case !validSecretKey(key):
		return configError("AGENT_PGBACKREST_ENDPOINT_CA_KEY", "must name a key of AGENT_PGBACKREST_ENDPOINT_CA_SECRET")
	case !slices.Contains(cfg.AgentAllowedSecrets, name):
		return configError("AGENT_PGBACKREST_ENDPOINT_CA_SECRET", "must be in AGENT_ALLOWED_SECRETS: the agent reads no other Secret")
	}
	cfg.AgentPgBackRestEndpointCA = SecretKey{Name: name, Key: key}
	return nil
}

// SecretKey is one key of one Secret in the agent's namespace.
type SecretKey struct {
	Name, Key string
}

// validSecretKey accepts a Secret data key ([-._a-zA-Z0-9]+, at most 253).
func validSecretKey(key string) bool {
	if key == "" || len(key) > 253 || key == "." || key == ".." {
		return false
	}
	for _, character := range key {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9',
			character == '-', character == '.', character == '_':
		default:
			return false
		}
	}
	return true
}

// parseOptIn reads an explicit opt-in: true, or false/empty.
func parseOptIn(variable string, getenv func(string) string) (bool, error) {
	switch getenv(variable) {
	case "true":
		return true, nil
	case "", "false":
		return false, nil
	default:
		return false, configError(variable, "must be true, false or empty")
	}
}

// validDNSSubdomain accepts a lower-case RFC 1123 subdomain (an object name).
func validDNSSubdomain(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for label := range strings.SplitSeq(value, ".") {
		if !validDNSLabel(label, 63) {
			return false
		}
	}
	return true
}

// unresolvedClusterUID stands in for the Cluster's UID while it could not
// be read (AGENT_RESOLVE=cluster). It is not a UUID, so it never equals a
// real Cluster's UID.
const unresolvedClusterUID = "unresolved"

// ResolvedRepository is what AGENT_RESOLVE=cluster read from the Cluster,
// its Barman configuration and the referenced Secrets: plain values,
// validated by ApplyResolved through the same parsers as the environment.
type ResolvedRepository struct {
	ClusterUID string
	ServerName string
	// Provider is s3, azure or gcs; empty is s3.
	Provider Provider
	// DestinationPath is in the agent's grammar: s3://bucket/prefix,
	// azure://container/prefix or gs://bucket/prefix.
	DestinationPath string
	// EndpointURL is the S3 compatible origin or the Azure emulator origin.
	EndpointURL string
	// EndpointCABundle is the PEM the configuration's endpointCA names (S3
	// or Azure).
	EndpointCABundle []byte
	// Region is the S3 signing region (from the region Secret, or derived
	// from an AWS endpoint); empty applies the compatible default with an
	// endpoint, as AWS_REGION does.
	Region string
	// Credential is how the store authenticates: CredentialKubernetesSecrets
	// (S3 keys read through Keys), CredentialAmbient (the Pod's workload
	// identity), or for Azure and GCS the mode of the Secret value below.
	Credential CredentialMode
	// Keys reads the static S3 key pair from its Secrets when needed.
	Keys KeySource
	// S3SSECustomerKey is the configuration's sseCustomerKey value as the
	// Secret holds it (base64), read once at start; empty without SSE-C.
	S3SSECustomerKey []byte
	// AzureAccount is the storage account (identity, and the account the
	// key or SAS token is for).
	AzureAccount string
	// The Azure and GCS Secret values, read once at start (a change
	// restarts the agent).
	AzureConnectionString []byte
	AzureAccountKey       []byte
	AzureSASToken         []byte
	GoogleCredentialsJSON []byte
	// Format is FormatPGBackRest for a Cluster backed up by a pgBackRest
	// plugin; empty is barman-cloud. ServerName is then unused: Stanza is
	// the backup stanza, ArchiveStanza the WAL's when it differs,
	// Repository the repository number and CipherPass the repository
	// passphrase, read once at start (a change restarts the agent).
	Format        RepositoryFormat
	Stanza        string
	ArchiveStanza string
	Repository    int
	CipherPass    []byte
	// Retention is the repository's retention as the Stanza or Archive
	// sets it; nil when the resolver gave none.
	Retention *PGBackRestRetention
}

// ApplyResolved completes a self-resolving agent's configuration. epoch is
// the producer epoch the agent derived (at least 1). A non-nil failure (a
// resolution problem, a Secret not allowed or not readable) becomes the
// repository error, served as unknown evidence that names it, exactly like
// a bad repository variable in the operator profile; so does a resolved
// value the environment parsers would refuse.
func ApplyResolved(cfg Config, resolved ResolvedRepository, epoch uint64, failure error) Config {
	cfg.ProducerEpoch = epoch
	cfg.RepositoryFormat, cfg.Provider = FormatBarmanCloud, ProviderS3
	if resolved.Provider != "" {
		cfg.Provider = resolved.Provider
	}
	cfg.BarmanServerNames = []string{cfg.CNPGClusterName}
	cfg.CNPGClusterUID = resolved.ClusterUID
	fail := func(err error) Config {
		cfg.RepositoryError = err
		cfg.Credentials, cfg.PGBackRestCipher = Credentials{}, Secret{}
		cfg.Destination, cfg.DestinationBucket, cfg.DestinationPrefix = nil, "", ""
		cfg.Endpoint, cfg.EndpointCABundle = nil, nil
		if invalidIdentity(cfg.CNPGClusterUID, 128, true) {
			cfg.CNPGClusterUID = unresolvedClusterUID
		}
		return cfg
	}
	if failure != nil {
		return fail(failure)
	}
	if invalidIdentity(resolved.ClusterUID, 128, true) {
		return fail(configError("CNPG_CLUSTER_UID", "the resolved Cluster has no usable UID"))
	}
	if resolved.Format == FormatPGBackRest {
		if err := applyResolvedPgBackRest(&cfg, resolved); err != nil {
			return fail(err)
		}
	} else {
		servers, err := parseNames("BARMAN_SERVER_NAMES", resolved.ServerName)
		if err != nil || len(servers) != 1 {
			return fail(configError("BARMAN_SERVER_NAMES", "the resolved server name is not a valid scope name"))
		}
		cfg.BarmanServerNames = servers
	}
	if _, err := parseProvider(string(cfg.Provider)); err != nil {
		// The unknown evidence still needs a provider the contract knows.
		cfg.Provider = ProviderS3
		return fail(err)
	}
	destination, err := parseDestination(cfg.Provider, resolved.DestinationPath)
	if err != nil {
		return fail(err)
	}
	cfg.Destination, cfg.DestinationBucket, cfg.DestinationPrefix = destination, destination.Host, strings.Trim(destination.Path, "/")
	if resolved.EndpointURL != "" {
		if cfg.Provider == ProviderGCS {
			return fail(configError("ENDPOINT_URL", "supported only for s3 and azure"))
		}
		if cfg.Endpoint, err = parseEndpoint(resolved.EndpointURL); err != nil {
			return fail(err)
		}
	}
	if len(resolved.EndpointCABundle) > 0 {
		if cfg.Provider == ProviderGCS {
			return fail(configError("ENDPOINT_CA_FILE", "supported only for s3 and azure"))
		}
		if !x509.NewCertPool().AppendCertsFromPEM(resolved.EndpointCABundle) {
			return fail(configError("ENDPOINT_CA_FILE", "the configuration's endpointCA must contain at least one PEM certificate"))
		}
		cfg.EndpointCABundle = bytes.Clone(resolved.EndpointCABundle)
	}
	if len(resolved.S3SSECustomerKey) > 0 && cfg.Provider != ProviderS3 {
		return fail(configError("S3_SSE_CUSTOMER_KEY_FILE", "supported only for s3"))
	}
	switch cfg.Provider {
	case ProviderS3:
		if err := applyResolvedS3(&cfg, resolved); err != nil {
			return fail(err)
		}
	case ProviderAzure:
		if err := applyResolvedAzure(&cfg, resolved); err != nil {
			return fail(err)
		}
	default:
		switch resolved.Credential {
		case CredentialAmbient:
		case CredentialJSONFile:
			if err := checkServiceAccountKey("GOOGLE_APPLICATION_CREDENTIALS", resolved.GoogleCredentialsJSON); err != nil {
				return fail(err)
			}
			cfg.Credentials.GoogleCredentialsJSON = Secret{value: bytes.Clone(resolved.GoogleCredentialsJSON)}
		default:
			return fail(configError("STORE_CREDENTIAL_MODE", "the resolved GCS repository has no credential"))
		}
		cfg.Credentials.Mode = resolved.Credential
	}
	return cfg
}

// applyResolvedPgBackRest sets a self-resolved pgBackRest repository's
// scope: one backup stanza, the WAL stanza (the backup
// stanza unless Opera's configuration names another), the repository
// number and the cipher pass, the same fields PGBACKREST_* set.
func applyResolvedPgBackRest(cfg *Config, resolved ResolvedRepository) error {
	stanzas, err := parseNames("PGBACKREST_STANZA", resolved.Stanza)
	if err != nil || len(stanzas) != 1 {
		return configError("PGBACKREST_STANZA", "the resolved stanza is not a valid scope name")
	}
	if resolved.ArchiveStanza != "" {
		archive, err := parseNames("PGBACKREST_ARCHIVE_STANZA", resolved.ArchiveStanza)
		if err != nil || len(archive) != 1 {
			return configError("PGBACKREST_ARCHIVE_STANZA", "the resolved WAL stanza is not a valid scope name")
		}
	}
	if resolved.Repository < 1 || resolved.Repository > 256 {
		return configError("PGBACKREST_REPOSITORY", "the resolved repository number is out of range")
	}
	archive := resolved.ArchiveStanza
	if archive == "" {
		archive = stanzas[0]
	}
	cfg.RepositoryFormat, cfg.BarmanServerNames = FormatPGBackRest, nil
	cfg.PGBackRestStanza, cfg.PGBackRestStanzas = stanzas[0], nil
	cfg.PGBackRestArchiveStanza, cfg.PGBackRestRepository = archive, resolved.Repository
	if len(resolved.CipherPass) > 0 {
		cfg.PGBackRestCipher = Secret{value: bytes.Clone(resolved.CipherPass)}
	}
	cfg.PGBackRestRetention = nil
	if resolved.Retention != nil {
		if err := resolved.Retention.validate(); err != nil {
			return err
		}
		retention := *resolved.Retention
		cfg.PGBackRestRetention = &retention
	}
	return nil
}

func applyResolvedS3(cfg *Config, resolved ResolvedRepository) error {
	region := resolved.Region
	if region == "" && resolved.Credential == CredentialAmbient && resolved.EndpointURL == "" {
		region = cfg.podAWSRegion
	}
	if len(region) > 128 || containsControl(region) {
		return configError("AWS_REGION", "the resolved region must be at most 128 characters without control characters")
	}
	if err := resolveS3Region(cfg, region); err != nil {
		return err
	}
	if len(resolved.S3SSECustomerKey) > 0 {
		key, err := parseSSECustomerKey("S3_SSE_CUSTOMER_KEY_FILE", resolved.S3SSECustomerKey)
		if err != nil {
			return configError("S3_SSE_CUSTOMER_KEY_FILE", "the configuration's sseCustomerKey must be a base64-encoded 256-bit (32-byte) key")
		}
		cfg.Credentials.S3SSECustomerKey = Secret{value: key}
	}
	switch resolved.Credential {
	case "", CredentialKubernetesSecrets:
		if resolved.Keys == nil {
			return configError("STORE_CREDENTIAL_MODE", "the resolved repository has no key source")
		}
		cfg.Credentials.Mode = CredentialKubernetesSecrets
		cfg.Credentials.AWSKeySource = resolved.Keys
	case CredentialAmbient:
		if err := applyAWSWorkloadIdentity(cfg, cfg.podAWSIdentity); err != nil {
			return err
		}
	default:
		return configError("STORE_CREDENTIAL_MODE", "the resolved S3 repository has no usable credential")
	}
	return nil
}

func applyResolvedAzure(cfg *Config, resolved ResolvedRepository) error {
	if !validAzureAccountName(resolved.AzureAccount) {
		return configError("AZURE_STORAGE_ACCOUNT", "the resolved storage account is not a valid account name")
	}
	cfg.Credentials.AzureAccount = resolved.AzureAccount
	value := map[CredentialMode][]byte{
		CredentialConnectionFile: resolved.AzureConnectionString,
		CredentialAccountKeyFile: resolved.AzureAccountKey,
		CredentialSASTokenFile:   resolved.AzureSASToken,
	}
	switch resolved.Credential {
	case CredentialAmbient:
	case CredentialConnectionFile, CredentialAccountKeyFile, CredentialSASTokenFile:
		if len(value[resolved.Credential]) == 0 {
			return configError("STORE_CREDENTIAL_MODE", "the resolved Azure credential is empty")
		}
		secret := Secret{value: bytes.Clone(value[resolved.Credential])}
		switch resolved.Credential {
		case CredentialConnectionFile:
			if err := checkConnectionString("AZURE_STORAGE_CONNECTION_STRING_FILE", secret.value, resolved.AzureAccount, cfg.Endpoint); err != nil {
				return err
			}
			cfg.Credentials.AzureConnectionString = secret
		case CredentialAccountKeyFile:
			cfg.Credentials.AzureAccountKey = secret
		default:
			cfg.Credentials.AzureSASToken = secret
		}
	default:
		return configError("STORE_CREDENTIAL_MODE", "the resolved Azure repository has no credential")
	}
	cfg.Credentials.Mode = resolved.Credential
	return nil
}

// validAzureAccountName: 3 to 24 lower-case letters and digits.
func validAzureAccountName(value string) bool {
	if len(value) < 3 || len(value) > 24 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

// validDNSLabel accepts a lower-case RFC 1123 label of at most maximum bytes.
func validDNSLabel(value string, maximum int) bool {
	if value == "" || len(value) > maximum || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

// LoadOS loads the current process environment and preserves explicitly empty
// values such as TRUSTED_USER_HEADER="".
func LoadOS() (Config, error) { return load(os.Getenv, os.LookupEnv) }

func loadFormatConfig(cfg *Config, getenv func(string) string) error {
	barman, err := parseNames("BARMAN_SERVER_NAMES", getenv("BARMAN_SERVER_NAMES"))
	if err != nil {
		return err
	}
	stanzas, err := parseNames("PGBACKREST_STANZAS", getenv("PGBACKREST_STANZAS"))
	if err != nil {
		return err
	}
	cipherPath := getenv("PGBACKREST_CIPHER_PASS_FILE")
	single := append([]string{"PGBACKREST_STANZA", "PGBACKREST_ARCHIVE_STANZA", "PGBACKREST_REPOSITORY"}, pgbackrestRetentionVariables...)

	switch cfg.RepositoryFormat {
	case FormatBarmanCloud:
		if len(stanzas) > 0 {
			return configError("PGBACKREST_STANZAS", "does not apply to barman-cloud")
		}
		if cipherPath != "" {
			return configError("PGBACKREST_CIPHER_PASS_FILE", "does not apply to barman-cloud")
		}
		for _, variable := range single {
			if getenv(variable) != "" {
				return configError(variable, "does not apply to barman-cloud")
			}
		}
		cfg.BarmanServerNames = barman
	case FormatPGBackRest:
		if len(barman) > 0 {
			return configError("BARMAN_SERVER_NAMES", "does not apply to pgbackrest")
		}
		if err := loadPGBackRestScope(cfg, stanzas, getenv); err != nil {
			return err
		}
		if cipherPath != "" {
			value, readErr := readBoundedFile("PGBACKREST_CIPHER_PASS_FILE", cipherPath, maxSecretFileBytes, true)
			if readErr != nil {
				return readErr
			}
			cfg.PGBackRestCipher = Secret{value: value}
		}
	}
	return nil
}

// loadPGBackRestScope: the viewer reads the stanzas PGBACKREST_STANZAS
// lists (all when empty); a producing mode reads exactly one,
// PGBACKREST_STANZA, with its WAL under PGBACKREST_ARCHIVE_STANZA when set.
func loadPGBackRestScope(cfg *Config, stanzas []string, getenv func(string) string) error {
	if !cfg.RuntimeMode.Produces() {
		for _, variable := range append([]string{"PGBACKREST_STANZA", "PGBACKREST_ARCHIVE_STANZA", "PGBACKREST_REPOSITORY"}, pgbackrestRetentionVariables...) {
			if getenv(variable) != "" {
				return configError(variable, "requires agent; the viewer reads PGBACKREST_STANZAS")
			}
		}
		cfg.PGBackRestStanzas = stanzas
		return nil
	}
	if len(stanzas) > 0 {
		return configError("PGBACKREST_STANZAS", "applies to the viewer only; an agent reads one PGBACKREST_STANZA")
	}
	stanza := getenv("PGBACKREST_STANZA")
	if stanza == "" {
		return configError("PGBACKREST_STANZA", "required in "+string(cfg.RuntimeMode))
	}
	if !evidencev1alpha1.ValidPGBackRestStanza(stanza) {
		return configError("PGBACKREST_STANZA", "must be a stanza name: "+evidencev1alpha1.PGBackRestStanzaRule)
	}
	archive := getenv("PGBACKREST_ARCHIVE_STANZA")
	if archive == "" {
		archive = stanza
	} else if !evidencev1alpha1.ValidPGBackRestStanza(archive) {
		return configError("PGBACKREST_ARCHIVE_STANZA", "must be a stanza name: "+evidencev1alpha1.PGBackRestStanzaRule)
	}
	repository, err := parseInt(getenv("PGBACKREST_REPOSITORY"), 1, 1, evidencev1alpha1.MaxPGBackRestRepository, "PGBACKREST_REPOSITORY")
	if err != nil {
		return err
	}
	cfg.PGBackRestStanza, cfg.PGBackRestArchiveStanza, cfg.PGBackRestRepository = stanza, archive, repository
	retention, err := loadPGBackRestRetention(getenv)
	if err != nil {
		return err
	}
	cfg.PGBackRestRetention = retention
	return nil
}

// PGBackRestRetention is a repository's repo-retention-* options, as its
// plugin's CRD sets them, for the agent to compare the stanza with. A zero
// count is an unset option; the types carry pgBackRest's defaults when the
// resolver renders them (count, full).
type PGBackRestRetention struct {
	Full        int
	FullType    string
	Diff        int
	Archive     int
	ArchiveType string
}

// pgbackrestRetentionVariables carry the retention: the operator renders
// them from the Stanza or Archive, and always renders both types, so that
// "repo-retention-full is not set" is told apart from "no expectation".
var pgbackrestRetentionVariables = []string{
	"PGBACKREST_RETENTION_FULL", "PGBACKREST_RETENTION_FULL_TYPE", "PGBACKREST_RETENTION_DIFF",
	"PGBACKREST_RETENTION_ARCHIVE", "PGBACKREST_RETENTION_ARCHIVE_TYPE",
}

// loadPGBackRestRetention reads PGBACKREST_RETENTION_*: nil when none is
// set; otherwise the expectation, whose unset counts stay zero.
func loadPGBackRestRetention(getenv func(string) string) (*PGBackRestRetention, error) {
	set := false
	for _, variable := range pgbackrestRetentionVariables {
		set = set || getenv(variable) != ""
	}
	if !set {
		return nil, nil
	}
	retention := &PGBackRestRetention{FullType: getenv("PGBACKREST_RETENTION_FULL_TYPE"), ArchiveType: getenv("PGBACKREST_RETENTION_ARCHIVE_TYPE")}
	for _, field := range []struct {
		variable string
		target   *int
	}{
		{"PGBACKREST_RETENTION_FULL", &retention.Full},
		{"PGBACKREST_RETENTION_DIFF", &retention.Diff},
		{"PGBACKREST_RETENTION_ARCHIVE", &retention.Archive},
	} {
		value, err := parseInt(getenv(field.variable), 0, 1, evidencev1alpha1.MaxPGBackRestRetention, field.variable)
		if err != nil {
			return nil, err
		}
		*field.target = value
	}
	if err := retention.validate(); err != nil {
		return nil, err
	}
	return retention, nil
}

// validate checks the types and the counts (0 is unset) with the
// contract's one check, naming the variable of the refused option.
func (r PGBackRestRetention) validate() error {
	err := evidencev1alpha1.ValidatePGBackRestRetention(r.Full, r.FullType, r.Diff, r.Archive, r.ArchiveType)
	var refused *evidencev1alpha1.PGBackRestRetentionError
	if errors.As(err, &refused) {
		return configError("PGBACKREST_RETENTION_"+strings.ToUpper(strings.ReplaceAll(refused.Option, "-", "_")), refused.Problem)
	}
	return err
}

// ScopeKind and ScopeName are the one scope a producing mode reads, as the
// evidence contract names it: a Barman server, or a pgBackRest stanza (the
// backup stanza; its WAL may sit under PGBackRestArchiveStanza).
func (c Config) ScopeKind() string {
	if c.RepositoryFormat == FormatPGBackRest {
		return evidencev1alpha1.ScopeKindStanza
	}
	return evidencev1alpha1.ScopeKindBarmanServer
}

// ScopeName is the name of that scope (see ScopeKind); empty when the
// configuration names no single scope.
func (c Config) ScopeName() string {
	if c.RepositoryFormat == FormatPGBackRest {
		stanza, _ := c.PGBackRestScope()
		return stanza
	}
	if len(c.BarmanServerNames) == 1 {
		return c.BarmanServerNames[0]
	}
	return ""
}

// PGBackRestScope is the stanza a producing mode reads and its archive
// stanza: PGBackRestStanza (PGBACKREST_STANZA), or the single entry of
// PGBackRestStanzas a self-resolved configuration sets; the archive stanza
// is PGBackRestArchiveStanza, the stanza when that is empty. Both are empty
// when the configuration names no single stanza (the viewer's list).
func (c Config) PGBackRestScope() (stanza, archive string) {
	stanza = c.PGBackRestStanza
	if stanza == "" && c.RuntimeMode.Produces() && len(c.PGBackRestStanzas) == 1 {
		stanza = c.PGBackRestStanzas[0]
	}
	if stanza == "" {
		return "", ""
	}
	archive = c.PGBackRestArchiveStanza
	if archive == "" {
		archive = stanza
	}
	return stanza, archive
}

// ScopePrefixes are the key prefixes, relative to the repository root, a
// producing mode may read: "<server>/" for Barman; "backup/<stanza>/" and
// "archive/<archive stanza>/" for pgBackRest.
func (c Config) ScopePrefixes() []string {
	if c.RepositoryFormat == FormatPGBackRest {
		stanza, archive := c.PGBackRestScope()
		if stanza == "" {
			return nil
		}
		return []string{"archive/" + archive + "/", "backup/" + stanza + "/"}
	}
	if len(c.BarmanServerNames) != 1 {
		return nil
	}
	return []string{c.BarmanServerNames[0] + "/"}
}

func loadCredentials(cfg *Config, getenv func(string) string) error {
	if getenv("AWS_ACCESS_KEY_ID") != "" || getenv("AWS_SECRET_ACCESS_KEY") != "" || getenv("AWS_SESSION_TOKEN") != "" {
		return configError("AWS_ACCESS_KEY_ID", "direct static credentials are prohibited; use credential files or workload identity")
	}

	providerFiles := map[Provider][]string{
		// AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE are not listed: the
		// IRSA webhook injects them into every Pod of an annotated
		// ServiceAccount, and only the S3 web-identity and ambient modes
		// read them.
		ProviderS3:    {"AWS_ACCESS_KEY_ID_FILE", "AWS_SECRET_ACCESS_KEY_FILE", "AWS_SESSION_TOKEN_FILE", "S3_SSE_CUSTOMER_KEY_FILE"},
		ProviderAzure: {"AZURE_STORAGE_CONNECTION_STRING_FILE", "AZURE_STORAGE_ACCOUNT_KEY_FILE", "AZURE_STORAGE_SAS_TOKEN_FILE"},
		ProviderGCS:   {"GOOGLE_APPLICATION_CREDENTIALS"},
	}
	for provider, variables := range providerFiles {
		if provider == cfg.Provider {
			continue
		}
		for _, variable := range variables {
			if getenv(variable) != "" {
				return configError(variable, "does not apply to selected provider")
			}
		}
	}

	switch cfg.Provider {
	case ProviderS3:
		if err := loadSSECustomerKey(cfg, getenv); err != nil {
			return err
		}
		return loadS3Credentials(cfg, getenv)
	case ProviderAzure:
		return loadAzureCredentials(cfg, getenv)
	case ProviderGCS:
		return loadGCSCredentials(cfg, getenv)
	default:
		return configError("PROVIDER", "unsupported")
	}
}

// loadSSECustomerKey reads S3_SSE_CUSTOMER_KEY_FILE: the SSE-C key, in
// the form barman-cloud reads from its --sse-customer-key file.
func loadSSECustomerKey(cfg *Config, getenv func(string) string) error {
	path := getenv("S3_SSE_CUSTOMER_KEY_FILE")
	if path == "" {
		return nil
	}
	value, err := readBoundedFile("S3_SSE_CUSTOMER_KEY_FILE", path, maxSecretFileBytes, true)
	if err != nil {
		return err
	}
	key, err := parseSSECustomerKey("S3_SSE_CUSTOMER_KEY_FILE", value)
	if err != nil {
		return err
	}
	cfg.Credentials.S3SSECustomerKey = Secret{value: key}
	return nil
}

// sseCustomerKeyBytes is the AES-256 key length of S3 SSE-C.
const sseCustomerKeyBytes = 32

// parseSSECustomerKey decodes an SSE-C key as barman-cloud does
// (barman.cloud_providers.aws_s3: the content stripped of surrounding
// white space, strict base64, exactly 32 bytes once decoded). The error
// names no value.
func parseSSECustomerKey(variable string, value []byte) ([]byte, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(string(bytes.TrimSpace(value)))
	if err != nil || len(key) != sseCustomerKeyBytes {
		return nil, configError(variable, "must be a base64-encoded 256-bit (32-byte) SSE-C key")
	}
	return key, nil
}

func loadS3Credentials(cfg *Config, getenv func(string) string) error {
	accessPath := getenv("AWS_ACCESS_KEY_ID_FILE")
	secretPath := getenv("AWS_SECRET_ACCESS_KEY_FILE")
	tokenPath := getenv("AWS_SESSION_TOKEN_FILE")
	if (accessPath == "") != (secretPath == "") {
		return configError("AWS_ACCESS_KEY_ID_FILE", "access-key and secret-key files must be configured together")
	}
	if tokenPath != "" && accessPath == "" {
		return configError("AWS_SESSION_TOKEN_FILE", "requires access-key and secret-key files")
	}
	explicitRegion := getenv("AWS_REGION")
	if len(explicitRegion) > 128 || containsControl(explicitRegion) {
		return configError("AWS_REGION", "must be at most 128 characters without control characters")
	}
	if err := resolveS3Region(cfg, explicitRegion); err != nil {
		return err
	}
	if cfg.RuntimeMode.Produces() {
		mode := CredentialMode(getenv("STORE_CREDENTIAL_MODE"))
		switch mode {
		case CredentialStaticFiles:
			// AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE, which the IRSA
			// webhook may inject, are ignored: the files are the credential.
			if accessPath == "" {
				return configError("STORE_CREDENTIAL_MODE", "static-files requires the static credential files")
			}
		case CredentialAWSWebIdentity:
			if accessPath != "" || secretPath != "" || tokenPath != "" {
				return configError("STORE_CREDENTIAL_MODE", "aws-web-identity cannot use static credential files")
			}
			webIdentityPath := getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
			roleARN := getenv("AWS_ROLE_ARN")
			if !validAbsolutePath(webIdentityPath) {
				return configError("AWS_WEB_IDENTITY_TOKEN_FILE", "must be an absolute clean path")
			}
			if !validAWSRoleARN(roleARN) {
				return configError("AWS_ROLE_ARN", "must be a bounded AWS role ARN")
			}
			if explicitRegion == "" {
				return configError("AWS_REGION", "required with aws-web-identity")
			}
			cfg.Credentials.Mode = CredentialAWSWebIdentity
			cfg.Credentials.AWSWebIdentityTokenFile = webIdentityPath
			cfg.Credentials.AWSRoleARN = roleARN
			return nil
		case CredentialAmbient:
			// The agent's workload identity (s3Credentials.inheritFromIAMRole):
			// what the identity webhook injects, IRSA or EKS Pod Identity,
			// and nothing else (no instance metadata). Only an agent may use
			// it.
			if cfg.RuntimeMode != RuntimeAgent {
				return configError("STORE_CREDENTIAL_MODE", "must be static-files or aws-web-identity")
			}
			if accessPath != "" || secretPath != "" || tokenPath != "" {
				return configError("STORE_CREDENTIAL_MODE", "ambient cannot use static credential files")
			}
			return applyAWSWorkloadIdentity(cfg, awsWorkloadIdentity{
				roleARN: getenv("AWS_ROLE_ARN"), tokenFile: getenv("AWS_WEB_IDENTITY_TOKEN_FILE"),
				containerURI: getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"), containerTokenFile: getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"),
			})
		default:
			return configError("STORE_CREDENTIAL_MODE", "must be static-files, aws-web-identity or (agent) ambient")
		}
	}
	if accessPath == "" {
		cfg.Credentials.Mode = CredentialAmbient
		return nil
	}
	access, err := readBoundedFile("AWS_ACCESS_KEY_ID_FILE", accessPath, maxSecretFileBytes, true)
	if err != nil {
		return err
	}
	secret, err := readBoundedFile("AWS_SECRET_ACCESS_KEY_FILE", secretPath, maxSecretFileBytes, true)
	if err != nil {
		return err
	}
	cfg.Credentials.Mode = CredentialStaticFiles
	cfg.Credentials.AWSAccessKeyID = Secret{value: access}
	cfg.Credentials.AWSSecretKey = Secret{value: secret}
	if cfg.Credentials.AWSAccessKeyIDFile, err = absoluteFilePath("AWS_ACCESS_KEY_ID_FILE", accessPath); err != nil {
		return err
	}
	if cfg.Credentials.AWSSecretKeyFile, err = absoluteFilePath("AWS_SECRET_ACCESS_KEY_FILE", secretPath); err != nil {
		return err
	}
	if tokenPath != "" {
		token, readErr := readBoundedFile("AWS_SESSION_TOKEN_FILE", tokenPath, maxSecretFileBytes, true)
		if readErr != nil {
			return readErr
		}
		cfg.Credentials.AWSSessionToken = Secret{value: token}
		if cfg.Credentials.AWSSessionTokenFile, err = absoluteFilePath("AWS_SESSION_TOKEN_FILE", tokenPath); err != nil {
			return err
		}
	}
	return nil
}

// defaultCompatibleS3Region signs requests to an S3-compatible ENDPOINT_URL
// when no region is configured. Compatible stores (MinIO, Ceph RGW, Garage)
// accept it unless configured otherwise; AWS itself never reaches this
// default because an AWS deployment without ENDPOINT_URL must name a region.
const defaultCompatibleS3Region = "us-east-1"

// resolveS3Region gives the S3 client a signing region on every path. An
// explicit AWS_REGION wins. With ENDPOINT_URL the compatible default applies.
// Without either, AWS S3 cannot be addressed and every request would fail as
// an opaque "unavailable" at scan time, so configuration fails loudly instead.
// The region is a signing coordinate only: it is not part of the destination
// fingerprint.
func resolveS3Region(cfg *Config, explicit string) error {
	switch {
	case explicit != "":
		cfg.Credentials.AWSRegion = explicit
	case cfg.Endpoint != nil:
		cfg.Credentials.AWSRegion = defaultCompatibleS3Region
	default:
		return configError("AWS_REGION", "required for AWS S3 when ENDPOINT_URL is not set")
	}
	return nil
}

func parseRuntimeMode(raw string) (RuntimeMode, error) {
	switch RuntimeMode(raw) {
	case RuntimeStandalone, RuntimeAgent:
		return RuntimeMode(raw), nil
	default:
		return "", configError("RUNTIME_MODE", "must be agent or empty")
	}
}

func loadAzureCredentials(cfg *Config, getenv func(string) string) error {
	connectionPath := getenv("AZURE_STORAGE_CONNECTION_STRING_FILE")
	account := getenv("AZURE_STORAGE_ACCOUNT")
	keyPath := getenv("AZURE_STORAGE_ACCOUNT_KEY_FILE")
	sasPath := getenv("AZURE_STORAGE_SAS_TOKEN_FILE")
	if len(account) > 256 || containsControl(account) {
		return configError("AZURE_STORAGE_ACCOUNT", "must be at most 256 characters without control characters")
	}
	if cfg.RuntimeMode.Produces() && account == "" {
		// The account is part of the repository identity the agent publishes.
		return configError("AZURE_STORAGE_ACCOUNT", "required in "+string(cfg.RuntimeMode))
	}
	if err := checkCredentialMode(cfg, getenv, map[CredentialMode]bool{
		CredentialConnectionFile: connectionPath != "", CredentialAccountKeyFile: keyPath != "",
		CredentialSASTokenFile: sasPath != "", CredentialAmbient: connectionPath == "" && keyPath == "" && sasPath == "",
	}); err != nil {
		return err
	}
	if connectionPath != "" {
		// AZURE_STORAGE_ACCOUNT names the account beside a connection
		// string; when it does, the connection string must address that
		// account at the destination's endpoint.
		if keyPath != "" || sasPath != "" {
			return configError("AZURE_STORAGE_CONNECTION_STRING_FILE", "cannot be combined with account credentials")
		}
		value, err := readBoundedFile("AZURE_STORAGE_CONNECTION_STRING_FILE", connectionPath, maxSecretFileBytes, true)
		if err != nil {
			return err
		}
		if err := checkConnectionString("AZURE_STORAGE_CONNECTION_STRING_FILE", value, account, cfg.Endpoint); err != nil {
			return err
		}
		cfg.Credentials.Mode = CredentialConnectionFile
		cfg.Credentials.AzureAccount = account
		cfg.Credentials.AzureConnectionString = Secret{value: value}
		return nil
	}
	if keyPath != "" && sasPath != "" {
		return configError("AZURE_STORAGE_ACCOUNT_KEY_FILE", "account-key and SAS-token files are mutually exclusive")
	}
	if (keyPath != "" || sasPath != "") && account == "" {
		return configError("AZURE_STORAGE_ACCOUNT", "required with account-key or SAS-token file")
	}
	cfg.Credentials.AzureAccount = account
	if keyPath == "" && sasPath == "" {
		cfg.Credentials.Mode = CredentialAmbient
		return nil
	}
	if keyPath != "" {
		value, err := readBoundedFile("AZURE_STORAGE_ACCOUNT_KEY_FILE", keyPath, maxSecretFileBytes, true)
		if err != nil {
			return err
		}
		cfg.Credentials.Mode = CredentialAccountKeyFile
		cfg.Credentials.AzureAccountKey = Secret{value: value}
		return nil
	}
	value, err := readBoundedFile("AZURE_STORAGE_SAS_TOKEN_FILE", sasPath, maxSecretFileBytes, true)
	if err != nil {
		return err
	}
	cfg.Credentials.Mode = CredentialSASTokenFile
	cfg.Credentials.AzureSASToken = Secret{value: value}
	return nil
}

// checkCredentialMode: a producer names its credential mode explicitly
// (STORE_CREDENTIAL_MODE), and it must be the one the configured files
// select (selected maps each mode of the provider to whether the variables
// select it), so an omitted file never silently falls back to ambient
// credentials. The standalone viewer infers the mode.
func checkCredentialMode(cfg *Config, getenv func(string) string, selected map[CredentialMode]bool) error {
	if !cfg.RuntimeMode.Produces() {
		return nil
	}
	mode := CredentialMode(getenv("STORE_CREDENTIAL_MODE"))
	if _, known := selected[mode]; !known {
		names := make([]string, 0, len(selected))
		for name := range selected {
			names = append(names, string(name))
		}
		sort.Strings(names)
		return configError("STORE_CREDENTIAL_MODE", "must be one of "+strings.Join(names, ", "))
	}
	if !selected[mode] {
		return configError("STORE_CREDENTIAL_MODE", "does not match the configured credential files")
	}
	return nil
}

func loadGCSCredentials(cfg *Config, getenv func(string) string) error {
	path := getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if err := checkCredentialMode(cfg, getenv, map[CredentialMode]bool{CredentialJSONFile: path != "", CredentialAmbient: path == ""}); err != nil {
		return err
	}
	if path == "" {
		cfg.Credentials.Mode = CredentialAmbient
		return nil
	}
	value, err := readBoundedFile("GOOGLE_APPLICATION_CREDENTIALS", path, maxConfigFileBytes, true)
	if err != nil {
		return err
	}
	if err := checkServiceAccountKey("GOOGLE_APPLICATION_CREDENTIALS", value); err != nil {
		return err
	}
	cfg.Credentials.Mode = CredentialJSONFile
	cfg.Credentials.GoogleCredentialsJSON = Secret{value: value}
	return nil
}

// checkServiceAccountKey admits a Google service account key only. Other
// credential files make the client act on their content: an
// external_account file names a token URL and a local file or command whose
// output it sends there, so a Secret value could make the agent post files
// of its Pod to any host.
func checkServiceAccountKey(variable string, value []byte) error {
	var key struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(value, &key); err != nil {
		return configError(variable, "must be a Google service account JSON key")
	}
	if key.Type != "service_account" {
		return configError(variable, `must be a Google service account JSON key ("type": "service_account"); other credential types are refused`)
	}
	return nil
}

func parseFormat(raw string) (RepositoryFormat, error) {
	switch RepositoryFormat(raw) {
	case FormatBarmanCloud, FormatPGBackRest:
		return RepositoryFormat(raw), nil
	default:
		return "", configError("REPOSITORY_FORMAT", "must be barman-cloud or pgbackrest")
	}
}

func parseProvider(raw string) (Provider, error) {
	switch Provider(raw) {
	case ProviderS3, ProviderAzure, ProviderGCS:
		return Provider(raw), nil
	default:
		return "", configError("PROVIDER", "must be s3, azure, or gcs")
	}
}

func parseDestination(provider Provider, raw string) (*url.URL, error) {
	if raw == "" {
		return nil, configError("DESTINATION_PATH", "required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, configError("DESTINATION_PATH", "must be a credential-free object-store URI")
	}
	expectedScheme := map[Provider]string{ProviderS3: "s3", ProviderAzure: "azure", ProviderGCS: "gs"}[provider]
	if parsed.Scheme != expectedScheme {
		return nil, configError("DESTINATION_PATH", "scheme does not match PROVIDER")
	}
	if strings.Contains(parsed.EscapedPath(), "//") || containsControl(raw) {
		return nil, configError("DESTINATION_PATH", "contains an invalid prefix")
	}
	if provider == ProviderS3 {
		bucket, prefix, err := evidencev1alpha1.ParseS3Destination(raw)
		if err != nil || bucket != parsed.Host || prefix != strings.Trim(parsed.Path, "/") {
			return nil, configError("DESTINATION_PATH", "must be an s3://bucket/prefix URI without query, fragment, port, or empty segments")
		}
	}
	return parsed, nil
}

func parseEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" || containsControl(raw) ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, configError("ENDPOINT_URL", "must be a credential-free http or https origin")
	}
	return parsed, nil
}

// absoluteFilePath anchors a credential file path once at startup, so a later
// re-read cannot depend on the working directory.
func absoluteFilePath(variable, path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", configError(variable, "file path cannot be resolved")
	}
	return absolute, nil
}

func validAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func invalidIdentity(value string, maximum int, required bool) bool {
	return required && value == "" || len(value) > maximum || containsControl(value)
}

// awsWorkloadIdentity is what an AWS identity webhook injects into the
// Pod: IRSA (a role and a projected web-identity token) or EKS Pod
// Identity (the agent's credential endpoint and its token).
type awsWorkloadIdentity struct {
	roleARN, tokenFile               string
	containerURI, containerTokenFile string
}

// eksPodIdentityHosts are the EKS Pod Identity Agent's link-local
// addresses: the only container credential endpoints the agent calls.
var eksPodIdentityHosts = []string{"169.254.170.23", "[fd00:ec2::23]"}

// applyAWSWorkloadIdentity selects IRSA, else EKS Pod Identity, as the
// ambient S3 credential; with neither injected it names what is missing
// rather than fall back to the node's identity.
func applyAWSWorkloadIdentity(cfg *Config, identity awsWorkloadIdentity) error {
	switch {
	case identity.roleARN != "" || identity.tokenFile != "":
		if !validAbsolutePath(identity.tokenFile) {
			return configError("AWS_WEB_IDENTITY_TOKEN_FILE", "must be an absolute clean path (IRSA)")
		}
		if !validAWSRoleARN(identity.roleARN) {
			return configError("AWS_ROLE_ARN", "must be a bounded AWS role ARN (IRSA)")
		}
		cfg.Credentials.AWSRoleARN, cfg.Credentials.AWSWebIdentityTokenFile = identity.roleARN, identity.tokenFile
	case identity.containerURI != "":
		endpoint, err := url.Parse(identity.containerURI)
		if err != nil || endpoint.Scheme != "http" || !slices.Contains(eksPodIdentityHosts, endpoint.Host) || endpoint.User != nil {
			return configError("AWS_CONTAINER_CREDENTIALS_FULL_URI", "must be the EKS Pod Identity Agent's link-local endpoint")
		}
		if !validAbsolutePath(identity.containerTokenFile) {
			return configError("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "must be an absolute clean path (EKS Pod Identity)")
		}
		cfg.Credentials.AWSContainerCredentialsURI = identity.containerURI
		cfg.Credentials.AWSContainerAuthorizationTokenFile = identity.containerTokenFile
	default:
		return configError("STORE_CREDENTIAL_MODE",
			"ambient S3 credentials need IRSA (AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE) or EKS Pod Identity "+
				"(AWS_CONTAINER_CREDENTIALS_FULL_URI) injected into the Pod; the node's instance metadata is never used")
	}
	cfg.Credentials.Mode = CredentialAmbient
	return nil
}

func validAWSRoleARN(value string) bool {
	if invalidIdentity(value, 2048, true) {
		return false
	}
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] == "" || parts[2] != "iam" || parts[3] != "" || len(parts[4]) != 12 || !strings.HasPrefix(parts[5], "role/") || len(parts[5]) == len("role/") {
		return false
	}
	for _, character := range parts[4] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validateListenAddr(raw string) error {
	_, port, err := net.SplitHostPort(raw)
	if err != nil || port == "" {
		return configError("LISTEN_ADDR", "must be host:port")
	}
	value, err := strconv.Atoi(port)
	if err != nil || value < 1 || value > 65535 {
		return configError("LISTEN_ADDR", "port must be between 1 and 65535")
	}
	return nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

func parseNames(variable, raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		name := strings.TrimSpace(item)
		if name == "" || len(name) > 128 || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || containsControl(name) {
			return nil, configError(variable, "contains an invalid scope name")
		}
		seen[name] = struct{}{}
		if len(seen) > maxConfiguredScopes {
			return nil, configError(variable, "contains too many scope names")
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

func parseBool(raw string, fallback bool, variable string) (bool, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, configError(variable, "must be true or false")
	}
	return value, nil
}

func parseDuration(raw string, fallback, minimum, maximum time.Duration, variable string) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, configError(variable, fmt.Sprintf("must be between %s and %s", minimum, maximum))
	}
	return value, nil
}

func parseInt(raw string, fallback, minimum, maximum int, variable string) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, configError(variable, fmt.Sprintf("must be between %d and %d", minimum, maximum))
	}
	return value, nil
}

func readBoundedFile(variable, path string, maximum int64, trimNewline bool) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, configError(variable, "file cannot be opened")
	}
	defer func() { _ = file.Close() }()
	value, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, configError(variable, "file cannot be read")
	}
	if int64(len(value)) > maximum {
		return nil, configError(variable, "file exceeds the size limit")
	}
	if trimNewline {
		value = bytes.TrimSuffix(value, []byte("\n"))
		value = bytes.TrimSuffix(value, []byte("\r"))
	}
	if len(value) == 0 {
		return nil, configError(variable, "file is empty")
	}
	return value, nil
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func configError(variable, code string) error {
	return &Error{Variable: variable, Code: code}
}

// ParseLogLevel reads LOG_LEVEL: empty selects info; otherwise exactly one of
// debug, info, warn, or error (case-insensitive).
func ParseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, configError("LOG_LEVEL", "must be debug, info, warn, or error")
	}
}

// IsError reports whether err is a redacted configuration error.
func IsError(err error) bool {
	var target *Error
	return errors.As(err, &target)
}
