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

package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
)

const (
	// credentialRecheckInterval bounds how long a rotated Secret can go
	// unnoticed: the credentials cache asks the provider again after it, and
	// the provider re-reads the files only when one of them changed.
	credentialRecheckInterval = 2 * time.Minute
	maxCredentialFileBytes    = 64 * 1024
)

var errCredentialFiles = &Error{kind: fault.Authentication, operation: "load credentials"}

// fileCredentialsProvider serves static S3 keys from mounted files and
// re-reads them when a file's identity, size, or modification time changes.
// Kubernetes updates projected Secret volumes in place (an atomic symlink
// swap), so a rotated key reaches the running process without a restart. A
// failed re-read keeps serving the last good keys and tries again at the next
// interval, so a transient read never turns a working store into a failing
// one. Errors never contain a path or a value.
type fileCredentialsProvider struct {
	accessPath, secretPath, tokenPath string
	interval                          time.Duration
	now                               func() time.Time
	// loaded, when set, receives each newly loaded key set (for redaction).
	loaded func(values ...[]byte)
	// afterRead is a test seam called between reading and re-stamping.
	afterRead func()

	mu      sync.Mutex
	stamp   []os.FileInfo
	current aws.Credentials
}

func newFileCredentialsProvider(accessPath, secretPath, tokenPath string, now func() time.Time) (*fileCredentialsProvider, error) {
	for _, path := range []string{accessPath, secretPath, tokenPath} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path) {
			return nil, &Error{kind: fault.InvalidConfig, operation: "configure"}
		}
	}
	if accessPath == "" || secretPath == "" {
		return nil, &Error{kind: fault.InvalidConfig, operation: "configure"}
	}
	if now == nil {
		now = time.Now
	}
	return &fileCredentialsProvider{accessPath: accessPath, secretPath: secretPath, tokenPath: tokenPath, interval: credentialRecheckInterval, now: now}, nil
}

// Retrieve implements aws.CredentialsProvider. It is called by
// aws.CredentialsCache only when the cached value has expired.
func (p *fileCredentialsProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	if err := ctx.Err(); err != nil {
		return aws.Credentials{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	expires := p.now().Add(p.interval)
	stamp, err := p.fileStamp()
	if err == nil && sameStamp(stamp, p.stamp) && p.current.HasKeys() {
		return p.refreshed(expires), nil
	}
	if err == nil {
		if loaded, loadedStamp, readErr := p.readConsistent(stamp); readErr == nil {
			p.stamp, p.current = loadedStamp, loaded
			if p.loaded != nil {
				p.loaded([]byte(loaded.AccessKeyID), []byte(loaded.SecretAccessKey), []byte(loaded.SessionToken))
			}
			return p.refreshed(expires), nil
		}
	}
	if p.current.HasKeys() {
		return p.refreshed(expires), nil
	}
	return aws.Credentials{}, errCredentialFiles
}

// readConsistent reads every file and accepts the result only when the file
// stamps taken before and after the read agree. A Secret update swaps all
// keys at once (..data), so a read that straddles the swap could pair an old
// access key with a new secret; it is retried once, then refused.
func (p *fileCredentialsProvider) readConsistent(before []os.FileInfo) (aws.Credentials, []os.FileInfo, error) {
	for attempt := 0; attempt < 2; attempt++ {
		credentials, err := p.read()
		if err != nil {
			return aws.Credentials{}, nil, err
		}
		if p.afterRead != nil {
			p.afterRead()
		}
		after, err := p.fileStamp()
		if err != nil {
			return aws.Credentials{}, nil, err
		}
		if sameStamp(before, after) {
			return credentials, after, nil
		}
		before = after
	}
	return aws.Credentials{}, nil, errCredentialFiles
}

func (p *fileCredentialsProvider) refreshed(expires time.Time) aws.Credentials {
	credentials := p.current
	credentials.CanExpire, credentials.Expires = true, expires
	return credentials
}

func (p *fileCredentialsProvider) fileStamp() ([]os.FileInfo, error) {
	stamp := make([]os.FileInfo, 0, 3)
	for _, path := range []string{p.accessPath, p.secretPath, p.tokenPath} {
		if path == "" {
			continue
		}
		// Stat follows the projected-volume symlinks, so a Secret update
		// shows up as a different file, size, or modification time.
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errCredentialFiles
		}
		stamp = append(stamp, info)
	}
	return stamp, nil
}

func sameStamp(current, previous []os.FileInfo) bool {
	if len(current) != len(previous) {
		return false
	}
	for index := range current {
		if !os.SameFile(current[index], previous[index]) || current[index].Size() != previous[index].Size() || !current[index].ModTime().Equal(previous[index].ModTime()) {
			return false
		}
	}
	return true
}

func (p *fileCredentialsProvider) read() (aws.Credentials, error) {
	access, err := readCredentialFile(p.accessPath)
	if err != nil {
		return aws.Credentials{}, err
	}
	secret, err := readCredentialFile(p.secretPath)
	if err != nil {
		return aws.Credentials{}, err
	}
	credentials := aws.Credentials{AccessKeyID: string(access), SecretAccessKey: string(secret), Source: "evidence-agent-files"}
	if p.tokenPath != "" {
		token, err := readCredentialFile(p.tokenPath)
		if err != nil {
			return aws.Credentials{}, err
		}
		credentials.SessionToken = string(token)
	}
	return credentials, nil
}

// readCredentialFile mirrors the configuration loader: bounded, one trailing
// line ending removed, never empty.
func readCredentialFile(path string) ([]byte, error) {
	// #nosec G304 -- the path is operator configuration validated as absolute
	// and clean; it names a mounted Secret key, never request data.
	file, err := os.Open(path)
	if err != nil {
		return nil, errCredentialFiles
	}
	defer func() { _ = file.Close() }()
	value, err := io.ReadAll(io.LimitReader(file, maxCredentialFileBytes+1))
	if err != nil || len(value) > maxCredentialFileBytes {
		return nil, errCredentialFiles
	}
	value = bytes.TrimSuffix(value, []byte("\n"))
	value = bytes.TrimSuffix(value, []byte("\r"))
	if len(value) == 0 {
		return nil, errCredentialFiles
	}
	return value, nil
}

var _ aws.CredentialsProvider = (*fileCredentialsProvider)(nil)

// KeySource supplies the static key pair at run time; it is the
// consumer-owned view of config.KeySource.
type KeySource interface {
	Keys(ctx context.Context) (accessKeyID, secretAccessKey, sessionToken []byte, err error)
}

var errKeySource = &Error{kind: fault.Authentication, operation: "load credentials"}

// keySourceProvider serves the keys a KeySource reads, with the file
// provider's rules: asked again after credentialRecheckInterval, a failed
// read keeps the last good keys, and errors carry no value.
type keySourceProvider struct {
	source   KeySource
	interval time.Duration
	now      func() time.Time
	loaded   func(values ...[]byte)

	mu      sync.Mutex
	current aws.Credentials
}

func newKeySourceProvider(source KeySource, now func() time.Time) *keySourceProvider {
	return &keySourceProvider{source: source, interval: credentialRecheckInterval, now: now}
}

// Retrieve implements aws.CredentialsProvider.
func (p *keySourceProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	if err := ctx.Err(); err != nil {
		return aws.Credentials{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	expires := p.now().Add(p.interval)
	access, secret, token, err := p.source.Keys(ctx)
	if err == nil && len(access) > 0 && len(secret) > 0 {
		next := aws.Credentials{AccessKeyID: string(access), SecretAccessKey: string(secret), SessionToken: string(token), Source: "evidence-agent-secrets"}
		if next != p.current && p.loaded != nil {
			p.loaded(access, secret, token)
		}
		p.current = next
	}
	if !p.current.HasKeys() {
		return aws.Credentials{}, errKeySource
	}
	credentials := p.current
	credentials.CanExpire, credentials.Expires = true, expires
	return credentials, nil
}

var _ aws.CredentialsProvider = (*keySourceProvider)(nil)

// tokenFile is EKS Pod Identity's authorization token: a projected
// ServiceAccount token the kubelet rotates, read again on every refresh.
type tokenFile string

// GetToken implements endpointcreds.AuthTokenProvider.
func (path tokenFile) GetToken() (string, error) {
	if path == "" {
		return "", errCredentialFiles
	}
	value, err := readCredentialFile(string(path))
	if err != nil {
		return "", err
	}
	return string(value), nil
}

// podIdentityProvider asks the EKS Pod Identity Agent at endpoint, with
// the token read from tokenPath at every refresh.
func podIdentityProvider(endpoint, tokenPath string) aws.CredentialsProvider {
	return endpointcreds.New(endpoint, func(providerOptions *endpointcreds.Options) {
		providerOptions.AuthorizationTokenProvider = tokenFile(tokenPath)
	})
}

// rotationSigner signs like the S3 client's default signer, but starts a
// new one when the secret key changes. The SDK signer caches its derived
// signing key by access key ID and UTC day only, so a secret rotated under
// the same access key ID would keep being signed with the old secret until
// midnight UTC: the providers above would read the new key and every
// request would still be refused (or still accepted after a revocation).
type rotationSigner struct {
	mu     sync.Mutex
	secret [sha256.Size]byte
	signer *v4.Signer
}

func newRotationSigner() *rotationSigner { return &rotationSigner{} }

// SignHTTP implements awss3.HTTPSignerV4.
func (s *rotationSigner) SignHTTP(ctx context.Context, credentials aws.Credentials, r *http.Request, payloadHash, service, region string, signingTime time.Time, optFns ...func(*v4.SignerOptions)) error {
	digest := sha256.Sum256([]byte(credentials.SecretAccessKey))
	s.mu.Lock()
	if s.signer == nil || digest != s.secret {
		// The S3 client's own signer options (api_client.go
		// newDefaultV4Signer): S3 object keys are signed unescaped.
		s.signer = v4.NewSigner(func(options *v4.SignerOptions) { options.DisableURIPathEscaping = true })
		s.secret = digest
	}
	signer := s.signer
	s.mu.Unlock()
	return signer.SignHTTP(ctx, credentials, r, payloadHash, service, region, signingTime, optFns...)
}
