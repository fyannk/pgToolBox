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

// Package s3 implements the provider-neutral read store using only S3 list,
// get, and head operations. AWS SDK types never leave this package.
package s3

import (
	"context"
	"crypto/hmac"
	"crypto/md5" // #nosec G501 -- S3's SSE-C key digest is MD5 by protocol (RFC 1321), an integrity check, not a security primitive.
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/provider/nofollow"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

const (
	cursorMACBytes    = sha256.Size
	maxRequestTimeout = 5 * time.Minute
)

// Options configures New. Credentials come from the first source set, in this
// order: KeySource, key files, fixed keys, web identity (with RoleARN), Pod
// Identity; with none, the SDK's default chain applies. Endpoint selects an
// S3-compatible service and path-style addressing.
type Options struct {
	Bucket          string
	Prefix          string
	Endpoint        *url.URL
	CABundle        []byte
	Region          string
	AccessKeyID     []byte
	SecretAccessKey []byte
	SessionToken    []byte
	// AccessKeyIDFile, SecretAccessKeyFile, and SessionTokenFile select the
	// reloading file provider instead of the fixed keys above: rotated
	// Secret values are picked up without a restart.
	AccessKeyIDFile     string
	SecretAccessKeyFile string
	SessionTokenFile    string
	// KeySource, when set, reads the static key pair at run time instead
	// (the self-resolving agent reads it from the Secrets the ObjectStore
	// names); it is asked again at the same recheck interval as the files.
	KeySource KeySource
	// CredentialsLoaded, when set, receives every key set the file provider
	// or the key source loads, so log redaction learns rotated values.
	CredentialsLoaded    func(values ...[]byte)
	WebIdentityTokenFile string
	RoleARN              string
	// ContainerCredentialsURI and ContainerAuthorizationTokenFile select
	// EKS Pod Identity (the configuration admits the Pod Identity Agent's
	// endpoint only); the token file is re-read on every refresh.
	ContainerCredentialsURI         string
	ContainerAuthorizationTokenFile string
	// SSECustomerKey is the raw 256-bit key of server-side encryption with
	// customer keys (SSE-C): sent, with its algorithm and MD5, on every
	// GetObject and HeadObject (S3 requires it to read an SSE-C object);
	// ListObjectsV2 needs none. Empty sends no SSE-C header.
	SSECustomerKey []byte
	// DisableInstanceMetadata keeps the SDK off the node's instance
	// metadata (the evidence agent: its AWS identity is the Pod's or none).
	DisableInstanceMetadata bool
	RequestTimeout          time.Duration
}

type api interface {
	ListObjectsV2(context.Context, *awss3.ListObjectsV2Input, ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error)
	GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	HeadObject(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error)
}

// Store is a read-only S3 bucket, rooted at the configured prefix.
type Store struct {
	client         api
	bucket         string
	rootPrefix     string
	requestTimeout time.Duration
	cursorKey      []byte
	// sseC holds the SSE-C request fields, nil without a customer key.
	sseC *sseCustomerKey
}

// sseCustomerKey is the three SSE-C request headers, as S3 defines them:
// the algorithm, the base64 of the key and the base64 of its MD5.
type sseCustomerKey struct {
	algorithm, key, keyMD5 string
}

// sseCustomerKeyBytes is the AES-256 key length S3 requires.
const sseCustomerKeyBytes = 32

func newSSECustomerKey(raw []byte) *sseCustomerKey {
	if len(raw) == 0 {
		return nil
	}
	digest := md5.Sum(raw) // #nosec G401 -- the protocol's key digest, see the import.
	return &sseCustomerKey{
		algorithm: "AES256", key: base64.StdEncoding.EncodeToString(raw), keyMD5: base64.StdEncoding.EncodeToString(digest[:]),
	}
}

// New validates options and builds the S3 client; it makes no request.
func New(ctx context.Context, options Options) (*Store, error) {
	if invalidOptions(options) || options.Region == "" {
		return nil, &Error{kind: fault.InvalidConfig, operation: "configure"}
	}
	// Configuration always resolves a signing region; an empty one would let
	// the SDK fall back to ambient profile files or fail every request opaquely.
	loadOptions := make([]func(*awsconfig.LoadOptions) error, 0, 3)
	loadOptions = append(loadOptions, awsconfig.WithRegion(options.Region))
	switch {
	case options.KeySource != nil:
		provider := newKeySourceProvider(options.KeySource, time.Now)
		provider.loaded = options.CredentialsLoaded
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(aws.NewCredentialsCache(provider)))
	case options.AccessKeyIDFile != "":
		provider, err := newFileCredentialsProvider(options.AccessKeyIDFile, options.SecretAccessKeyFile, options.SessionTokenFile, time.Now)
		if err != nil {
			return nil, err
		}
		provider.loaded = options.CredentialsLoaded
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(aws.NewCredentialsCache(provider)))
	case len(options.AccessKeyID) > 0:
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			string(options.AccessKeyID), string(options.SecretAccessKey), string(options.SessionToken),
		)))
	case options.WebIdentityTokenFile != "":
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(aws.AnonymousCredentials{}))
	case options.ContainerCredentialsURI != "":
		provider := podIdentityProvider(options.ContainerCredentialsURI, options.ContainerAuthorizationTokenFile)
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(aws.NewCredentialsCache(provider)))
	}
	if options.DisableInstanceMetadata {
		loadOptions = append(loadOptions, awsconfig.WithEC2IMDSClientEnableState(imds.ClientDisabled))
	}
	// Always the agent's own client, which follows no redirect (the SDK's
	// default follows 307 and 308 to any host, SSE-C key header included).
	httpClient, err := httpClient(options.CABundle)
	if err != nil {
		return nil, &Error{kind: fault.InvalidConfig, operation: "configure"}
	}
	loadOptions = append(loadOptions, awsconfig.WithHTTPClient(httpClient))
	sdkConfig, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, &Error{kind: fault.InvalidConfig, operation: "configure"}
	}
	if options.WebIdentityTokenFile != "" {
		provider := stscreds.NewWebIdentityRoleProvider(
			sts.NewFromConfig(sdkConfig), options.RoleARN,
			stscreds.IdentityTokenFile(options.WebIdentityTokenFile),
			func(providerOptions *stscreds.WebIdentityRoleOptions) {
				providerOptions.RoleSessionName = "evidence-agent"
			},
		)
		sdkConfig.Credentials = aws.NewCredentialsCache(provider)
	}
	client := awss3.NewFromConfig(sdkConfig, func(optionsValue *awss3.Options) {
		optionsValue.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		optionsValue.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		optionsValue.HTTPSignerV4 = newRotationSigner()
		if options.Endpoint != nil {
			optionsValue.BaseEndpoint = aws.String(options.Endpoint.String())
			optionsValue.UsePathStyle = true
		}
	})
	cursorKey := make([]byte, 32)
	if _, err := rand.Read(cursorKey); err != nil {
		return nil, &Error{kind: fault.Unavailable, operation: "configure"}
	}
	return newWithAPI(client, options, cursorKey)
}

func newWithAPI(client api, options Options, cursorKey []byte) (*Store, error) {
	if client == nil || invalidOptions(options) || len(cursorKey) < 32 {
		return nil, &Error{kind: fault.InvalidConfig, operation: "configure"}
	}
	prefix := strings.Trim(options.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &Store{
		client: client, bucket: options.Bucket, rootPrefix: prefix,
		requestTimeout: options.RequestTimeout, cursorKey: append([]byte(nil), cursorKey...),
		sseC: newSSECustomerKey(options.SSECustomerKey),
	}, nil
}

func invalidOptions(options Options) bool {
	if options.Bucket == "" || len(options.Bucket) > 255 || strings.IndexFunc(options.Bucket, unicode.IsControl) >= 0 ||
		options.RequestTimeout <= 0 || options.RequestTimeout > maxRequestTimeout || len(options.Prefix) > store.MaxKeyBytes ||
		len(options.Region) > 128 || strings.IndexFunc(options.Region, unicode.IsControl) >= 0 || len(options.CABundle) > 1024*1024 ||
		(len(options.SSECustomerKey) != 0 && len(options.SSECustomerKey) != sseCustomerKeyBytes) ||
		len(options.AccessKeyID) > 64*1024 || len(options.SecretAccessKey) > 64*1024 || len(options.SessionToken) > 64*1024 ||
		len(options.WebIdentityTokenFile) > 4096 || len(options.RoleARN) > 2048 || strings.IndexFunc(options.WebIdentityTokenFile, unicode.IsControl) >= 0 || strings.IndexFunc(options.RoleARN, unicode.IsControl) >= 0 ||
		(len(options.AccessKeyID) == 0) != (len(options.SecretAccessKey) == 0) || (len(options.SessionToken) > 0 && len(options.AccessKeyID) == 0) ||
		(options.WebIdentityTokenFile == "") != (options.RoleARN == "") || (len(options.AccessKeyID) > 0 && options.WebIdentityTokenFile != "") ||
		invalidCredentialFiles(options) ||
		(options.KeySource != nil && (len(options.AccessKeyID) > 0 || options.AccessKeyIDFile != "" || options.WebIdentityTokenFile != "")) {
		return true
	}
	if options.Endpoint == nil {
		return false
	}
	endpoint := options.Endpoint
	return endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawPath != "" ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https")
}

// invalidCredentialFiles requires the access and secret files together, a
// session-token file only with them, and no mixing with fixed keys or web
// identity.
func invalidCredentialFiles(options Options) bool {
	files := options.AccessKeyIDFile != "" || options.SecretAccessKeyFile != "" || options.SessionTokenFile != ""
	if !files {
		return false
	}
	return options.AccessKeyIDFile == "" || options.SecretAccessKeyFile == "" || len(options.AccessKeyID) > 0 || options.WebIdentityTokenFile != "" ||
		len(options.AccessKeyIDFile) > 4096 || len(options.SecretAccessKeyFile) > 4096 || len(options.SessionTokenFile) > 4096
}

// List implements store.Reader with one ListObjectsV2 list call under the configured
// prefix; the returned cursor is signed and bound to the requested prefix.
func (s *Store) List(ctx context.Context, request store.ListRequest) (store.Page, error) {
	if err := request.Validate(); err != nil || strings.HasPrefix(request.Prefix, "/") {
		return store.Page{}, store.ErrInvalidRequest
	}
	continuation, err := s.decodeCursor(request.Prefix, request.Cursor)
	if err != nil {
		return store.Page{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()
	input := &awss3.ListObjectsV2Input{
		// #nosec G115 -- ListRequest.Validate bounds Limit to 1..store.MaxPageObjects (1000).
		Bucket: aws.String(s.bucket), Prefix: aws.String(s.rootPrefix + request.Prefix), MaxKeys: aws.Int32(int32(request.Limit)),
	}
	if continuation != "" {
		input.ContinuationToken = aws.String(continuation)
	}
	output, err := s.client.ListObjectsV2(requestCtx, input)
	if err != nil {
		return store.Page{}, safeError(requestCtx, "list", err)
	}
	if output == nil || len(output.Contents) > request.Limit {
		return store.Page{}, &Error{kind: fault.Unavailable, operation: "list"}
	}
	page := store.Page{Objects: make([]store.Object, 0, len(output.Contents))}
	for _, object := range output.Contents {
		if object.Key == nil || object.Size == nil || *object.Size < 0 {
			return store.Page{}, &Error{kind: fault.Unavailable, operation: "list"}
		}
		key, ok := s.relativeKey(*object.Key, request.Prefix)
		if !ok || key == "" || len(key) > store.MaxKeyBytes {
			return store.Page{}, &Error{kind: fault.SafetyLimit, operation: "list"}
		}
		item := store.Object{Key: key, Size: *object.Size}
		if object.LastModified != nil {
			item.LastModified = object.LastModified.UTC()
		}
		if object.ETag != nil && len(*object.ETag) <= 512 {
			item.ETag = *object.ETag
		}
		page.Objects = append(page.Objects, item)
	}
	if aws.ToBool(output.IsTruncated) {
		if output.NextContinuationToken == nil || *output.NextContinuationToken == "" {
			return store.Page{}, &Error{kind: fault.Unavailable, operation: "list"}
		}
		page.NextCursor, err = s.encodeCursor(request.Prefix, *output.NextContinuationToken)
		if err != nil {
			return store.Page{}, err
		}
	}
	return page, nil
}

// Open implements store.Reader. The body yields at most request.MaxBytes,
// and closing it ends the request.
func (s *Store) Open(ctx context.Context, request store.OpenRequest) (io.ReadCloser, error) {
	if err := request.Validate(); err != nil || strings.HasPrefix(request.Key, "/") {
		return nil, store.ErrInvalidRequest
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	input := &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.rootPrefix + request.Key)}
	if s.sseC != nil {
		input.SSECustomerAlgorithm, input.SSECustomerKey, input.SSECustomerKeyMD5 = aws.String(s.sseC.algorithm), aws.String(s.sseC.key), aws.String(s.sseC.keyMD5)
	}
	output, err := s.client.GetObject(requestCtx, input)
	if err != nil {
		// Classified before cancel: a cancelled context would make every
		// failure read as the caller's cancellation.
		classified := safeError(requestCtx, "open", err)
		cancel()
		return nil, classified
	}
	if output == nil || output.Body == nil {
		cancel()
		return nil, &Error{kind: fault.Unavailable, operation: "open"}
	}
	return &limitedBody{Reader: io.LimitReader(output.Body, request.MaxBytes), body: output.Body, cancel: cancel}, nil
}

// Stat implements store.Reader by reading the object's metadata only.
func (s *Store) Stat(ctx context.Context, request store.StatRequest) (store.Object, error) {
	if err := request.Validate(); err != nil || strings.HasPrefix(request.Key, "/") {
		return store.Object{}, store.ErrInvalidRequest
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()
	input := &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.rootPrefix + request.Key)}
	if s.sseC != nil {
		input.SSECustomerAlgorithm, input.SSECustomerKey, input.SSECustomerKeyMD5 = aws.String(s.sseC.algorithm), aws.String(s.sseC.key), aws.String(s.sseC.keyMD5)
	}
	output, err := s.client.HeadObject(requestCtx, input)
	if err != nil {
		return store.Object{}, safeError(requestCtx, "stat", err)
	}
	if output == nil || output.ContentLength == nil || *output.ContentLength < 0 {
		return store.Object{}, &Error{kind: fault.Unavailable, operation: "stat"}
	}
	result := store.Object{Key: request.Key, Size: *output.ContentLength}
	if output.LastModified != nil {
		result.LastModified = output.LastModified.UTC()
	}
	if output.ETag != nil && len(*output.ETag) <= 512 {
		result.ETag = *output.ETag
	}
	return result, nil
}

func (s *Store) relativeKey(fullKey, requestedPrefix string) (string, bool) {
	if !strings.HasPrefix(fullKey, s.rootPrefix) {
		return "", false
	}
	relative := strings.TrimPrefix(fullKey, s.rootPrefix)
	return relative, strings.HasPrefix(relative, requestedPrefix)
}

func (s *Store) encodeCursor(prefix, token string) (string, error) {
	if len(prefix) > store.MaxKeyBytes || len(token) > store.MaxCursorBytes-cursorMACBytes-4 {
		return "", &Error{kind: fault.SafetyLimit, operation: "list"}
	}
	body := make([]byte, 4+len(prefix)+len(token), 4+len(prefix)+len(token)+sha256.Size)
	// #nosec G115 -- the guard above bounds len(prefix) to store.MaxKeyBytes (16 KiB).
	binary.BigEndian.PutUint32(body[:4], uint32(len(prefix)))
	copy(body[4:], prefix)
	copy(body[4+len(prefix):], token)
	mac := hmac.New(sha256.New, s.cursorKey)
	_, _ = mac.Write(body)
	encoded := base64.RawURLEncoding.EncodeToString(append(body, mac.Sum(nil)...))
	if len(encoded) > store.MaxCursorBytes {
		return "", &Error{kind: fault.SafetyLimit, operation: "list"}
	}
	return encoded, nil
}

func (s *Store) decodeCursor(prefix, encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) < 4+cursorMACBytes {
		return "", store.ErrInvalidRequest
	}
	body, signature := decoded[:len(decoded)-cursorMACBytes], decoded[len(decoded)-cursorMACBytes:]
	mac := hmac.New(sha256.New, s.cursorKey)
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return "", store.ErrInvalidRequest
	}
	prefixLength := int(binary.BigEndian.Uint32(body[:4]))
	if prefixLength > len(body)-4 || string(body[4:4+prefixLength]) != prefix {
		return "", store.ErrInvalidRequest
	}
	return string(body[4+prefixLength:]), nil
}

type limitedBody struct {
	io.Reader
	body   io.Closer
	cancel context.CancelFunc
}

func (b *limitedBody) Close() error {
	b.cancel()
	return b.body.Close()
}

// Error exposes only a stable category and operation. The SDK cause stays
// inside the adapter: Error never renders it and it is not unwrappable, so no
// SDK type crosses the boundary. RootCause hands its text to the one caller
// that redacts and logs it.
type Error struct {
	kind      fault.Category
	operation string
	cause     error
}

func (e *Error) Error() string { return fmt.Sprintf("s3 %s failed: %s", e.operation, e.kind) }

// Category returns the failure category.
func (e *Error) Category() fault.Category { return e.kind }

// RootCause returns the unredacted provider message for redacted logging.
func (e *Error) RootCause() string {
	if e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

// CauseKey identifies the cause for log deduplication without request IDs:
// operation and API error code for service errors, "" (compare the text)
// for transport errors.
func (e *Error) CauseKey() string {
	var apiError smithy.APIError
	if e.cause != nil && errors.As(e.cause, &apiError) {
		return e.operation + "/" + apiError.ErrorCode()
	}
	return ""
}

func safeError(ctx context.Context, operation string, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, nofollow.ErrRedirect) {
		// The endpoint is not the store's (a wrong endpoint or region, a
		// proxy): a configuration problem, said without the Location.
		return &Error{kind: fault.InvalidConfig, operation: operation, cause: nofollow.ErrRedirect}
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		return &Error{kind: categoryForCode(apiError.ErrorCode()), operation: operation, cause: err}
	}
	return &Error{kind: fault.Unavailable, operation: operation, cause: err}
}

func categoryForCode(code string) fault.Category {
	switch code {
	case "AccessDenied", "AllAccessDisabled":
		return fault.Authorization
	case "ExpiredToken", "InvalidAccessKeyId", "InvalidToken", "SignatureDoesNotMatch":
		return fault.Authentication
	case "NoSuchBucket", "NoSuchKey", "NotFound":
		return fault.NotFound
	case "SlowDown", "Throttling", "ThrottlingException", "TooManyRequestsException":
		return fault.Throttled
	default:
		return fault.Unavailable
	}
}

// httpClient is the SDK's default transport, trusting bundle in addition
// to the system roots when one is given, in a client that follows no
// redirect (nofollow).
func httpClient(bundle []byte) (*http.Client, error) {
	transport := awshttp.NewBuildableClient().GetTransport()
	if len(bundle) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(bundle) {
			return nil, errors.New("invalid CA bundle")
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	return nofollow.Client(transport), nil
}

var _ store.Reader = (*Store)(nil)
