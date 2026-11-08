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

// Package azure implements the read-only Blob Storage boundary. Azure SDK
// types do not leave this package.
package azure

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/provider/cursor"
	"github.com/fyannk/pgToolBox/internal/evidence/provider/nofollow"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Options configures New. Credentials are tried in this order: a connection
// string, Account with AccountKey, Account with SASToken, then a keyless
// credential for Account.
type Options struct {
	Container, Prefix, Account, ConnectionString string
	// Endpoint is the destination's (evidencev1alpha1.AzureDestination):
	// empty for the public cloud, the account's origin in a national cloud,
	// or an emulator's origin (Azurite: http://host:10000), under which the
	// account is the first path segment, as Barman addresses it. A
	// connection string carries its own endpoint, which the configuration
	// checks against this one.
	Endpoint             string
	AccountKey, SASToken []byte
	// CABundle is the PEM of the configuration's endpointCA (Azurite over
	// TLS, Azure Stack): trusted for the Blob endpoint in addition to the
	// system roots, as barman-cloud trusts it through REQUESTS_CA_BUNDLE.
	// Workload identity's token requests keep the system roots.
	CABundle []byte
	// WorkloadIdentityOnly authenticates without a key with Azure workload
	// identity alone (the federated token the webhook projects), never
	// DefaultAzureCredential's managed identity of the node (the evidence
	// agent).
	WorkloadIdentityOnly bool
	RequestTimeout       time.Duration
}

// serviceURL is the Blob service URL of the account (public cloud,
// national cloud or emulator), as the evidence contract spells it.
func (o Options) serviceURL() string {
	return evidencev1alpha1.AzureServiceURL(o.Endpoint, o.Account)
}

// Store is a read-only Blob Storage container, rooted at the configured prefix.
type Store struct {
	client            *azblob.Client
	container, prefix string
	timeout           time.Duration
	cursor            cursor.Codec
}

// New validates options and builds the Blob Storage client; it makes no
// request.
func New(ctx context.Context, options Options) (*Store, error) {
	if options.Container == "" || options.RequestTimeout <= 0 || options.RequestTimeout > 5*time.Minute {
		return nil, &Error{kind: fault.InvalidConfig}
	}
	if len(options.CABundle) > 1024*1024 {
		return nil, &Error{kind: fault.InvalidConfig}
	}
	// Always the agent's own client, which follows no redirect (azcore's
	// default follows any).
	transport, err := httpClient(options.CABundle)
	if err != nil {
		return nil, &Error{kind: fault.InvalidConfig}
	}
	clientOptions := &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Transport: transport}}
	var client *azblob.Client
	switch {
	case options.ConnectionString != "":
		client, err = azblob.NewClientFromConnectionString(options.ConnectionString, clientOptions)
	case options.Account != "" && len(options.AccountKey) > 0:
		credential, credentialErr := azblob.NewSharedKeyCredential(options.Account, string(options.AccountKey))
		if credentialErr != nil {
			return nil, &Error{kind: fault.InvalidConfig}
		}
		client, err = azblob.NewClientWithSharedKeyCredential(options.serviceURL(), credential, clientOptions)
	case options.Account != "" && len(options.SASToken) > 0:
		client, err = azblob.NewClientWithNoCredential(options.serviceURL()+"?"+strings.TrimPrefix(string(options.SASToken), "?"), clientOptions)
	default:
		credential, credentialErr := ambientCredential(options.WorkloadIdentityOnly)
		if credentialErr != nil {
			return nil, &Error{kind: fault.InvalidConfig}
		}
		if options.Account == "" {
			return nil, &Error{kind: fault.InvalidConfig}
		}
		client, err = azblob.NewClient(options.serviceURL(), credential, clientOptions)
	}
	if err != nil {
		return nil, &Error{kind: fault.InvalidConfig}
	}
	codec, err := cursor.New()
	if err != nil {
		return nil, &Error{kind: fault.Unavailable}
	}
	return &Store{client: client, container: options.Container, prefix: rooted(options.Prefix), timeout: options.RequestTimeout, cursor: codec}, nil
}

// httpClient trusts bundle in addition to the system roots when one is
// given, TLS 1.2 at least, the rest of the transport Go's default, in a
// client that follows no redirect (nofollow).
func httpClient(bundle []byte) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if len(bundle) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(bundle) {
			return nil, errors.New("invalid CA bundle")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	return nofollow.Client(transport), nil
}

// ambientCredential is the keyless credential: workload identity only for
// the agent, the SDK's default chain otherwise.
func ambientCredential(workloadIdentityOnly bool) (azcore.TokenCredential, error) {
	if workloadIdentityOnly {
		// AZURE_CLIENT_ID, AZURE_TENANT_ID and AZURE_FEDERATED_TOKEN_FILE,
		// as the workload identity webhook injects them.
		return azidentity.NewWorkloadIdentityCredential(nil)
	}
	return azidentity.NewDefaultAzureCredential(nil)
}

// List implements store.Reader with one Blob Storage list call under the configured
// prefix; the returned cursor is signed and bound to the requested prefix.
func (s *Store) List(ctx context.Context, request store.ListRequest) (store.Page, error) {
	if err := request.Validate(); err != nil || strings.HasPrefix(request.Prefix, "/") {
		return store.Page{}, store.ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	prefix := s.prefix + request.Prefix
	// #nosec G115 -- ListRequest.Validate bounds Limit to 1..store.MaxPageObjects (1000).
	options := &azblob.ListBlobsFlatOptions{Prefix: &prefix, MaxResults: new(int32(request.Limit))}
	if request.Cursor != "" {
		marker, err := s.cursor.Decode(request.Prefix, request.Cursor)
		if err != nil {
			return store.Page{}, store.ErrInvalidRequest
		}
		options.Marker = &marker
	}
	pager := s.client.NewListBlobsFlatPager(s.container, options)
	response, err := pager.NextPage(ctx)
	if err != nil {
		return store.Page{}, safeError(ctx, err)
	}
	if response.Segment == nil || len(response.Segment.BlobItems) > request.Limit {
		return store.Page{}, &Error{kind: fault.Unavailable}
	}
	page := store.Page{Objects: make([]store.Object, 0, len(response.Segment.BlobItems))}
	for _, item := range response.Segment.BlobItems {
		if item.Name == nil || item.Properties.ContentLength == nil || *item.Properties.ContentLength < 0 {
			return store.Page{}, &Error{kind: fault.Unavailable}
		}
		key, ok := s.relative(*item.Name, request.Prefix)
		if !ok || key == "" || len(key) > store.MaxKeyBytes {
			return store.Page{}, &Error{kind: fault.SafetyLimit}
		}
		object := store.Object{Key: key, Size: *item.Properties.ContentLength}
		if item.Properties.LastModified != nil {
			object.LastModified = item.Properties.LastModified.UTC()
		}
		if item.Properties.ETag != nil && len(string(*item.Properties.ETag)) <= 512 {
			object.ETag = string(*item.Properties.ETag)
		}
		page.Objects = append(page.Objects, object)
	}
	if response.NextMarker != nil && *response.NextMarker != "" {
		encoded, err := s.cursor.Encode(request.Prefix, *response.NextMarker)
		if err != nil {
			return store.Page{}, &Error{kind: fault.SafetyLimit}
		}
		page.NextCursor = encoded
	}
	return page, nil
}

// Open implements store.Reader. The body yields at most request.MaxBytes,
// and closing it ends the request.
func (s *Store) Open(ctx context.Context, request store.OpenRequest) (io.ReadCloser, error) {
	if err := request.Validate(); err != nil || strings.HasPrefix(request.Key, "/") {
		return nil, store.ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	response, err := s.client.DownloadStream(ctx, s.container, s.prefix+request.Key, nil)
	if err != nil {
		// Classified before cancel: a cancelled context would make every
		// failure read as the caller's cancellation.
		classified := safeError(ctx, err)
		cancel()
		return nil, classified
	}
	return &body{Reader: io.LimitReader(response.Body, request.MaxBytes), body: response.Body, cancel: cancel}, nil
}

// Stat implements store.Reader by reading the object's metadata only.
func (s *Store) Stat(ctx context.Context, request store.StatRequest) (store.Object, error) {
	if err := request.Validate(); err != nil || strings.HasPrefix(request.Key, "/") {
		return store.Object{}, store.ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	response, err := s.client.ServiceClient().NewContainerClient(s.container).NewBlobClient(s.prefix+request.Key).GetProperties(ctx, nil)
	if err != nil {
		return store.Object{}, safeError(ctx, err)
	}
	if response.ContentLength == nil || *response.ContentLength < 0 {
		return store.Object{}, &Error{kind: fault.Unavailable}
	}
	result := store.Object{Key: request.Key, Size: *response.ContentLength}
	if response.LastModified != nil {
		result.LastModified = response.LastModified.UTC()
	}
	if response.ETag != nil && len(string(*response.ETag)) <= 512 {
		result.ETag = string(*response.ETag)
	}
	return result, nil
}

func rooted(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return prefix
}

func (s *Store) relative(full, requested string) (string, bool) {
	if !strings.HasPrefix(full, s.prefix) {
		return "", false
	}
	key := strings.TrimPrefix(full, s.prefix)
	return key, strings.HasPrefix(key, requested)
}

type body struct {
	io.Reader
	body   io.ReadCloser
	cancel context.CancelFunc
}

func (b *body) Close() error { b.cancel(); return b.body.Close() }

// Error exposes only a stable category. The provider cause stays inside the
// adapter: Error never renders it and it is not unwrappable, so no SDK type
// crosses the boundary. RootCause hands its text to the one caller that
// redacts and logs it.
type Error struct {
	kind  fault.Category
	cause error
}

func (e *Error) Error() string { return "azure request failed: " + string(e.kind) }

// Category returns the failure category.
func (e *Error) Category() fault.Category { return e.kind }

// RootCause returns the unredacted provider message for redacted logging.
func (e *Error) RootCause() string {
	if e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

// CauseKey identifies a service error by status and error code for log
// deduplication, without its request ID; "" for transport errors.
func (e *Error) CauseKey() string {
	var responseError *azcore.ResponseError
	if e.cause != nil && errors.As(e.cause, &responseError) {
		return strconv.Itoa(responseError.StatusCode) + "/" + responseError.ErrorCode
	}
	return ""
}

func safeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, nofollow.ErrRedirect) {
		// The endpoint is not the store's: a configuration problem, said
		// without the Location (which may carry a SAS).
		return &Error{kind: fault.InvalidConfig, cause: nofollow.ErrRedirect}
	}
	var responseError *azcore.ResponseError
	if errors.As(err, &responseError) {
		switch responseError.StatusCode {
		case http.StatusUnauthorized:
			return &Error{kind: fault.Authentication, cause: err}
		case http.StatusForbidden:
			return &Error{kind: fault.Authorization, cause: err}
		case http.StatusNotFound:
			return &Error{kind: fault.NotFound, cause: err}
		case http.StatusTooManyRequests:
			return &Error{kind: fault.Throttled, cause: err}
		}
	}
	return &Error{kind: fault.Unavailable, cause: err}
}

var _ store.Reader = (*Store)(nil)
