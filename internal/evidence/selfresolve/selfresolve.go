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

// Package selfresolve is the agent's AGENT_RESOLVE=cluster mode: without
// the operator, the agent reads its Cluster, the barman-cloud ObjectStore
// it names (or the pgBackRest plugin's Stanza or Archive) and the
// credential Secrets itself, with a
// namespaced Role the pgtools chart renders (get/list/watch on that Cluster
// and that object, get on the Secrets named in values).
//
// The resolution is internal/backupresolve, the operator's own, so both
// install paths support the same shapes. This package adds what the
// operator's reconciler does around it:
//
//   - the allow-list: every Secret the ObjectStore references must be in
//     AGENT_ALLOWED_SECRETS (the chart's Role names the same list). One
//     that is not is reported by name with the value to add, and nothing
//     is read; the agent never lists Secrets.
//   - change handling: the Cluster and the ObjectStore are watched by name.
//     When what they resolve to changes, the agent exits 0 and the kubelet
//     starts it again with a fresh resolution. Restarting is simpler and
//     more robust than re-plumbing a running scanner, store client and
//     evidence engine: the evidence identity (fingerprint, server name,
//     Cluster UID) is fixed for the life of a process, as it is for an
//     operator-rendered Pod.
//
// Secrets are followed too, without widening the Role (get on the named
// Secrets only, no list, no watch, never cached):
//
//   - the S3 key pair is read at run time (config.KeySource) and re-read at
//     the S3 adapter's credential interval, so a rotated key needs no
//     restart;
//   - every other value read at start (the endpoint CA, the region, the
//     pgBackRest cipher pass, the S3 SSE-C key, the Azure connection string, account key or SAS token,
//     the GCS JSON key)
//     is configuration the running scanner was built with. The Watcher
//     re-reads them every SecretRecheck and exits, like a change of the
//     Cluster, when one changed or disappeared. A get every two minutes is
//     the price of the Role granting no watch on Secrets; the comparison is
//     a keyed digest whose key never leaves the process, so not even a hash
//     of a credential is kept in a form that could be compared elsewhere.
package selfresolve

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/backupresolve"
	"github.com/fyannk/pgToolBox/internal/evidence/config"
)

const (
	// maximumSecretValueBytes bounds one credential value, like the
	// configuration loader's credential files.
	maximumSecretValueBytes = 64 * 1024
	// readTimeout bounds each API read at start.
	readTimeout = 10 * time.Second
	// retryAfter is how long a start that failed for a reason the watch
	// cannot see (a Secret missing or not readable yet, the API server
	// unreachable) serves its unknown evidence before the agent exits to
	// try again. The Secrets are deliberately not watched: the Role grants
	// only get on them.
	retryAfter = 2 * time.Minute
	// SecretRecheck is how often the Watcher re-reads the Secret values
	// read at start (the S3 key pair aside, which follows rotation by
	// itself): the S3 adapter's own credential interval.
	SecretRecheck = 2 * time.Minute
)

// Options configure a Resolver.
type Options struct {
	// Reader reads the Cluster, the ObjectStore and the Secrets: a
	// namespaced client without a cache (Secrets are never cached).
	Reader client.Reader
	// Namespace is the agent's own; Cluster names the Cluster in it.
	Namespace string
	Cluster   string
	// AllowedSecrets are the Secrets the agent may read (sorted).
	AllowedSecrets []string
	// AllowInsecureEndpoint and WorkloadIdentity are the chart entry's
	// opt-ins (AGENT_ALLOW_INSECURE_ENDPOINT, AGENT_WORKLOAD_IDENTITY).
	AllowInsecureEndpoint bool
	WorkloadIdentity      bool
	// PgBackRest are the chart entry's pgbackrest settings (repository,
	// endpointCA) for a Cluster backed up by a pgBackRest plugin; nil
	// selects repo1 and the CA the plugin's object names.
	PgBackRest *backupresolve.PgBackRestSettings
	// PgBackRestEndpoint is the chart entry's pgbackrest.endpoint
	// (AGENT_PGBACKREST_ENDPOINT), the endpoint pin of Policy.
	PgBackRestEndpoint string
	Logger             *slog.Logger
}

// Resolver resolves the agent's repository from its Cluster.
type Resolver struct {
	reader     client.Reader
	namespace  string
	cluster    string
	allowed    []string
	policy     backupresolve.Policy
	pgBackRest *backupresolve.PgBackRestSettings
	logger     *slog.Logger
	// digestKey keys the digest of the Secret values read at start; it is
	// random per process and never leaves it.
	digestKey []byte
}

// New validates the options.
func New(options Options) (*Resolver, error) {
	// An empty allow-list is valid: a workload-identity repository reads
	// no Secret, and any reference is refused by name.
	if options.Reader == nil || options.Namespace == "" || options.Cluster == "" {
		return nil, errors.New("self-resolution configuration is invalid")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	allowed := slices.Clone(options.AllowedSecrets)
	slices.Sort(allowed)
	digestKey := make([]byte, sha256.Size)
	if _, err := rand.Read(digestKey); err != nil {
		return nil, fmt.Errorf("self-resolution digest key: %w", err)
	}
	return &Resolver{
		reader: options.Reader, namespace: options.Namespace, cluster: options.Cluster, allowed: allowed, logger: logger,
		digestKey: digestKey, pgBackRest: options.PgBackRest,
		policy: backupresolve.Policy{
			AllowedSecrets: allowed, AllowInsecureEndpoint: options.AllowInsecureEndpoint, WorkloadIdentity: options.WorkloadIdentity,
			PgBackRestEndpoint: options.PgBackRestEndpoint,
		},
	}, nil
}

// Outcome is one resolution.
type Outcome struct {
	// Repository is what config.ApplyResolved needs; meaningful only
	// when Failure is nil.
	Repository config.ResolvedRepository
	// Failure is the repository error to serve, nil when resolved. It
	// names objects and the value to change, never a credential.
	Failure error
	// Hash identifies what the watched objects resolve to; the watch
	// compares it with what they resolve to later.
	Hash string
	// ObjectStoreName is the ObjectStore to watch, empty when the
	// Cluster names none (yet).
	ObjectStoreName string
	// PgBackRest is the pgBackRest plugin's Stanza or Archive to watch
	// instead, nil when the Cluster names none.
	PgBackRest *backupresolve.PgBackRestLocation
	// RetryAfter is set when the failure can heal without a change of a
	// watched object: exit and start again after it.
	RetryAfter time.Duration
	// secrets are the Secret keys read at start whose values the running
	// process was built with (every one but the S3 key pair), and
	// secretDigest their keyed digest; the Watcher re-reads and compares.
	secrets      []backupresolve.SecretKeyRef
	secretDigest []byte
}

// Epoch is the producer epoch of a self-resolving agent: its start time in
// Unix seconds. It is at least 1 and grows across restarts (each restart
// is at least a second later), which is all the contract asks of an epoch;
// the console compares no status with it in this profile.
func Epoch(started time.Time) uint64 {
	if seconds := started.Unix(); seconds > 0 {
		return uint64(seconds)
	}
	return 1
}

// Resolve walks Cluster → ObjectStore → Secrets once.
func (r *Resolver) Resolve(ctx context.Context) Outcome {
	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	located, err := r.locate(readCtx, r.reader)
	if err != nil {
		return Outcome{Failure: err, RetryAfter: retryAfter, Hash: hashOf(watched{Error: "unreadable"})}
	}
	outcome := Outcome{Hash: located.hash(), ObjectStoreName: located.objectStoreName, PgBackRest: located.pgBackRest}
	if located.failure != nil {
		outcome.Failure = located.failure
		return outcome
	}
	composition := located.composition
	repository := config.ResolvedRepository{
		ClusterUID: composition.ClusterUID, ServerName: composition.ServerName,
		Provider: config.Provider(composition.Provider), DestinationPath: composition.AgentDestination,
		EndpointURL: composition.EndpointURL, Region: composition.Region.Value, AzureAccount: composition.AzureAccount,
	}
	if composition.Format == backupresolve.FormatPgBackRest {
		repository.Format = config.FormatPGBackRest
		repository.Stanza, repository.ArchiveStanza, repository.Repository = composition.Stanza, composition.ArchiveStanza, composition.Repository
		if retention := composition.Retention; retention != nil {
			repository.Retention = &config.PGBackRestRetention{
				Full: retention.Full, FullType: retention.FullType, Diff: retention.Diff, Archive: retention.Archive, ArchiveType: retention.ArchiveType,
			}
		}
	}
	secretFailure := func(err error) Outcome {
		outcome.Failure, outcome.RetryAfter = err, retryAfter
		return outcome
	}
	// fixed reads a value the process is built with and records it for
	// the Watcher's re-read.
	var fixedValues [][]byte
	fixed := func(reference backupresolve.SecretKeyRef) ([]byte, error) {
		value, err := r.secretValue(readCtx, reference)
		if err == nil {
			outcome.secrets = append(outcome.secrets, reference)
			fixedValues = append(fixedValues, value)
		}
		return value, err
	}
	if composition.EndpointCA != nil {
		if repository.EndpointCABundle, err = fixed(*composition.EndpointCA); err != nil {
			return secretFailure(err)
		}
	}
	if composition.SSECustomerKey != nil {
		// The SSE-C key: fixed for the process (a changed key restarts it;
		// barman-cloud cannot read the old objects with a new key either).
		if repository.S3SSECustomerKey, err = fixed(*composition.SSECustomerKey); err != nil {
			return secretFailure(err)
		}
	}
	if composition.CipherPass != nil {
		// The repository passphrase: fixed for the process, like the CA.
		if repository.CipherPass, err = fixed(*composition.CipherPass); err != nil {
			return secretFailure(err)
		}
	}
	if composition.Region.SecretRef != nil {
		region, err := fixed(*composition.Region.SecretRef)
		if err != nil {
			return secretFailure(err)
		}
		repository.Region = string(region)
	}
	switch composition.Credential {
	case backupresolve.CredentialStaticKeys:
		keys := &secretKeys{resolver: r, access: *composition.AccessKeyID, secret: *composition.SecretAccessKey, token: composition.SessionToken}
		// One read at start validates the references, so a missing Secret is
		// reported now instead of as an opaque authentication failure later.
		if _, _, _, err := keys.read(readCtx); err != nil {
			return secretFailure(err)
		}
		repository.Credential, repository.Keys = config.CredentialKubernetesSecrets, keys
	case backupresolve.CredentialConnectionString, backupresolve.CredentialStorageKey, backupresolve.CredentialSASToken:
		value, err := fixed(*composition.AzureSecret)
		if err != nil {
			return secretFailure(err)
		}
		switch composition.Credential {
		case backupresolve.CredentialConnectionString:
			repository.Credential, repository.AzureConnectionString = config.CredentialConnectionFile, value
		case backupresolve.CredentialStorageKey:
			repository.Credential, repository.AzureAccountKey = config.CredentialAccountKeyFile, value
		default:
			repository.Credential, repository.AzureSASToken = config.CredentialSASTokenFile, value
		}
	case backupresolve.CredentialJSONKey:
		value, err := fixed(*composition.GoogleCredentials)
		if err != nil {
			return secretFailure(err)
		}
		repository.Credential, repository.GoogleCredentialsJSON = config.CredentialJSONFile, value
	case backupresolve.CredentialAmbient:
		// Workload identity: the SDK's default chain, no Secret.
		repository.Credential = config.CredentialAmbient
	default:
		// Compose yields no other credential; refuse rather than guess.
		outcome.Failure = fmt.Errorf("%s resolves to a credential the agent does not read", composition.Origin)
		return outcome
	}
	outcome.Repository = repository
	outcome.secretDigest = r.digest(outcome.secrets, fixedValues)
	return outcome
}

// digest is the keyed digest of the Secret values read at start, each
// length-prefixed with its reference so two layouts never collide.
func (r *Resolver) digest(references []backupresolve.SecretKeyRef, values [][]byte) []byte {
	mac := hmac.New(sha256.New, r.digestKey)
	field := func(value []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		mac.Write(length[:])
		mac.Write(value)
	}
	for index, reference := range references {
		field([]byte(reference.Name))
		field([]byte(reference.Key))
		field(values[index])
	}
	return mac.Sum(nil)
}

// secretsChanged re-reads the Secret values the process was built with
// and says whether one changed or became unreadable. An API that does not
// answer is not a change: the next recheck tries again.
func (r *Resolver) secretsChanged(ctx context.Context, outcome Outcome) (bool, error) {
	if len(outcome.secrets) == 0 {
		return false, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	values := make([][]byte, 0, len(outcome.secrets))
	for _, reference := range outcome.secrets {
		value, err := r.secretValue(readCtx, reference)
		if errors.As(err, new(*unavailableError)) {
			return false, err
		}
		if err != nil {
			return true, nil
		}
		values = append(values, value)
	}
	return !hmac.Equal(r.digest(outcome.secrets, values), outcome.secretDigest), nil
}

// unavailableError is a Secret read the API server did not answer: it
// says nothing about the Secret.
type unavailableError struct{ name string }

func (e *unavailableError) Error() string {
	return fmt.Sprintf("the Secret %s could not be read: the Kubernetes API did not answer", e.name)
}

// located is the part of a resolution the watch can see: the Cluster and
// the ObjectStore (or the pgBackRest plugin's Stanza or Archive), and the
// allow-list check on the references.
type located struct {
	objectStoreName string
	pgBackRest      *backupresolve.PgBackRestLocation
	composition     *backupresolve.Composition
	failure         error
}

// watched is the hashed form of a located: the composition (Secret names,
// never values) or the failure's text.
type watched struct {
	Composition *backupresolve.Composition `json:"composition,omitempty"`
	Error       string                     `json:"error,omitempty"`
}

func (l located) hash() string {
	value := watched{Composition: l.composition}
	if l.failure != nil {
		value = watched{Error: l.failure.Error()}
	}
	return hashOf(value)
}

func hashOf(value watched) string {
	// A struct of strings always marshals.
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// locate resolves the Cluster and the ObjectStore through reader (the API
// at start, the watch's stores later) and applies the allow-list. An error
// is an API failure; an expected outcome is located.failure.
func (r *Resolver) locate(ctx context.Context, reader client.Reader) (located, error) {
	result := located{}
	// refuse records an expected outcome (a *backupresolve.Problem): it is
	// served as the repository error, not returned as an API failure.
	refuse := func(failure error) (located, error) {
		result.failure = failure
		return result, nil
	}
	request := backupresolve.Request{Namespace: r.namespace, ClusterName: r.cluster, PgBackRest: r.pgBackRest}
	where, whyNot, err := backupresolve.Locate(ctx, reader, request)
	if err != nil {
		return located{}, readFailure(err)
	}
	if where != nil {
		result.objectStoreName, result.pgBackRest = where.ObjectStoreName, where.PgBackRest
	}
	if whyNot != nil {
		return refuse(whyNot)
	}
	composition, whyNot, err := r.composeLocated(ctx, reader, where)
	if err != nil {
		return result, readFailure(err)
	}
	if whyNot != nil {
		return refuse(whyNot)
	}
	composition.ClusterName, composition.ClusterUID = where.Cluster.Name, string(where.Cluster.UID)
	result.composition = composition
	refused, whyNot := composition.Admit(r.policy)
	if whyNot != nil {
		return refuse(whyNot)
	}
	if len(refused) > 0 {
		return refuse(&NotAllowedError{Origin: composition.Origin.String(), Secrets: refused})
	}
	return result, nil
}

// composeLocated reads what the Cluster names (its Barman configuration, or a
// pgBackRest plugin's Stanza or Archive) and composes it.
func (r *Resolver) composeLocated(ctx context.Context, reader client.Reader, where *backupresolve.Located) (*backupresolve.Composition, *backupresolve.Problem, error) {
	if where.PgBackRest != nil {
		object, whyNot, err := backupresolve.ReadPgBackRest(ctx, reader, where.Cluster.Namespace, where.PgBackRest)
		if err != nil || whyNot != nil {
			return nil, whyNot, err
		}
		composition, whyNot := backupresolve.ComposePgBackRest(where, object, r.pgBackRest)
		return composition, whyNot, nil
	}
	configuration, whyNot, err := backupresolve.Configuration(ctx, reader, where)
	if err != nil || whyNot != nil {
		return nil, whyNot, err
	}
	composition, whyNot := backupresolve.Compose(where.Origin(), where.ServerName, configuration)
	return composition, whyNot, nil
}

// NotAllowedError is the refusal of a Secret the agent may not read: it
// names the Secret and the value to change, and nothing was read.
type NotAllowedError struct {
	// Origin names where the reference was read (an ObjectStore, the
	// Cluster's in-tree spec.backup.barmanObjectStore, a Stanza or an
	// Archive).
	Origin  string
	Secrets []string
}

func (e *NotAllowedError) Error() string {
	return fmt.Sprintf("%s references Secret %s, which this agent may not read: add it to evidence.agents[].secrets "+
		"(AGENT_ALLOWED_SECRETS) and run helm upgrade", e.Origin, strings.Join(e.Secrets, ", "))
}

// readFailure turns an API error into the repository error: a Forbidden
// says which value to check, anything else that the API could not be read.
func readFailure(err error) error {
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("the agent's Role does not allow this read (check evidence.agents[].cluster, objectStore, stanza and archive in the chart values): %w", err)
	}
	return fmt.Errorf("the Kubernetes API could not be read: %w", err)
}

// secretValue reads one key of one allowed Secret through the API. The
// error names the Secret and the key, never a value.
func (r *Resolver) secretValue(ctx context.Context, reference backupresolve.SecretKeyRef) ([]byte, error) {
	if _, found := slices.BinarySearch(r.allowed, reference.Name); !found {
		// Unreachable after the allow-list check; kept so no path reads
		// a Secret outside the list.
		return nil, fmt.Errorf("the Secret %s is not in AGENT_ALLOWED_SECRETS", reference.Name)
	}
	var secret corev1.Secret
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: reference.Name}, &secret); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("the Secret %s was not found in namespace %s", reference.Name, r.namespace)
		case apierrors.IsForbidden(err):
			return nil, fmt.Errorf("the Secret %s is not readable by the agent: the chart's Role must list it (evidence.agents[].secrets)", reference.Name)
		default:
			return nil, &unavailableError{name: reference.Name}
		}
	}
	value, ok := secret.Data[reference.Key]
	if !ok {
		return nil, fmt.Errorf("the Secret %s has no key %s", reference.Name, reference.Key)
	}
	value = bytes.TrimSuffix(bytes.TrimSuffix(value, []byte("\n")), []byte("\r"))
	if len(value) == 0 || len(value) > maximumSecretValueBytes {
		return nil, fmt.Errorf("the Secret %s key %s is empty or larger than %d bytes", reference.Name, reference.Key, maximumSecretValueBytes)
	}
	return bytes.Clone(value), nil
}

// secretKeys is the config.KeySource of a self-resolved repository: the
// key pair is read from its Secrets each time the S3 adapter asks (every
// two minutes at most), so a rotated Secret is followed without a restart.
type secretKeys struct {
	resolver       *Resolver
	access, secret backupresolve.SecretKeyRef
	token          *backupresolve.SecretKeyRef

	mu   sync.Mutex
	last []byte // digest of the last key set read, to report a rotation
}

// Keys implements config.KeySource.
func (k *secretKeys) Keys(ctx context.Context) (accessKeyID, secretAccessKey, sessionToken []byte, err error) {
	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	accessKeyID, secretAccessKey, sessionToken, err = k.read(readCtx)
	if err != nil {
		// The S3 adapter keeps the last good keys; say why they were not
		// renewed, or a rotation that never lands is invisible. The error
		// names the Secret and the key, never a value.
		k.resolver.logger.Warn("the S3 keys could not be read again; the last ones stay in use", slog.String("reason", err.Error()))
		return accessKeyID, secretAccessKey, sessionToken, err
	}
	refs := []backupresolve.SecretKeyRef{k.access, k.secret}
	values := [][]byte{accessKeyID, secretAccessKey}
	if k.token != nil {
		refs, values = append(refs, *k.token), append(values, sessionToken)
	}
	digest := k.resolver.digest(refs, values)
	k.mu.Lock()
	rotated := k.last != nil && !hmac.Equal(k.last, digest)
	k.last = digest
	k.mu.Unlock()
	if rotated {
		k.resolver.logger.Info("the S3 keys changed in their Secrets; the new ones are in use")
	}
	return accessKeyID, secretAccessKey, sessionToken, err
}

func (k *secretKeys) read(ctx context.Context) (accessKeyID, secretAccessKey, sessionToken []byte, err error) {
	if accessKeyID, err = k.resolver.secretValue(ctx, k.access); err != nil {
		return nil, nil, nil, err
	}
	if secretAccessKey, err = k.resolver.secretValue(ctx, k.secret); err != nil {
		return nil, nil, nil, err
	}
	if k.token != nil {
		if sessionToken, err = k.resolver.secretValue(ctx, *k.token); err != nil {
			return nil, nil, nil, err
		}
	}
	return accessKeyID, secretAccessKey, sessionToken, nil
}

var _ config.KeySource = (*secretKeys)(nil)
