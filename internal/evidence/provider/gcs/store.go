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

// Package gcs implements the read-only Google Cloud Storage boundary.
package gcs

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/api/option/internaloption"
	htransport "google.golang.org/api/transport/http"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/provider/cursor"
	"github.com/fyannk/pgToolBox/internal/evidence/provider/nofollow"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// Options configures New. Endpoint overrides the API endpoint and disables
// authentication (an emulator); CredentialsJSON is a service account key;
// with neither, the SDK's default credentials apply.
type Options struct {
	Bucket, Prefix  string
	Endpoint        string
	CredentialsJSON []byte
	RequestTimeout  time.Duration
}

// Store is a read-only Cloud Storage bucket, rooted at the configured prefix.
type Store struct {
	client         *storage.Client
	bucket, prefix string
	timeout        time.Duration
	cursor         cursor.Codec
}

// New validates options and builds the Cloud Storage client; it makes no
// request.
func New(ctx context.Context, options Options) (*Store, error) {
	if options.Bucket == "" || options.RequestTimeout <= 0 || options.RequestTimeout > 5*time.Minute {
		return nil, &Error{kind: fault.InvalidConfig}
	}
	httpClient, err := newHTTPClient(ctx, options)
	if err != nil {
		return nil, &Error{kind: fault.InvalidConfig}
	}
	// Keep list, stat, and bounded object reads on one JSON API endpoint. This
	// is explicitly supported by the SDK and avoids the separate XML read host.
	// The credential options are given again so the SDK's own view of
	// the credentials (it detects them beside the HTTP client) is the
	// same as the transport's.
	clientOptions := append([]option.ClientOption{storage.WithJSONReads(), option.WithHTTPClient(httpClient)}, credentialOptions(options)...)
	client, err := storage.NewClient(ctx, clientOptions...)
	if err != nil {
		return nil, &Error{kind: fault.InvalidConfig}
	}
	prefix := strings.Trim(options.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	codec, err := cursor.New()
	if err != nil {
		return nil, &Error{kind: fault.Unavailable}
	}
	return &Store{client: client, bucket: options.Bucket, prefix: prefix, timeout: options.RequestTimeout, cursor: codec}, nil
}

// newHTTPClient is the Cloud Storage client's HTTP client: the SDK's
// authentication (the bearer token of the service account key or of the
// default credentials, read-only scope) over a transport that refuses a
// redirect (nofollow). Without it the oauth2 transport would set the
// bearer token again on a redirected request, whatever its host: Go's
// client drops Authorization on a cross-host redirect, but the transport
// below it adds it back. option.WithHTTPClient replaces the SDK's own
// authenticated client, so the authentication is built here with the
// defaults storage.NewClient applies.
func newHTTPClient(ctx context.Context, options Options) (*http.Client, error) {
	credentials := credentialOptions(options)
	authOptions := make([]option.ClientOption, 0, 5+len(credentials))
	authOptions = append(authOptions,
		option.WithScopes(storage.ScopeReadOnly),
		internaloption.WithDefaultEndpointTemplate("https://storage.UNIVERSE_DOMAIN/storage/v1/"),
		internaloption.WithDefaultMTLSEndpoint("https://storage.mtls.googleapis.com/storage/v1/"),
		internaloption.WithDefaultUniverseDomain("googleapis.com"),
		internaloption.EnableNewAuthLibrary(),
	)
	authOptions = append(authOptions, credentials...)
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("the default HTTP transport is not an *http.Transport")
	}
	transport, err := htransport.NewTransport(ctx, nofollow.Transport(base.Clone()), authOptions...)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// credentialOptions are the endpoint and credential options: an emulator
// endpoint without authentication, or a service account key, or nothing
// (the default credentials).
func credentialOptions(options Options) []option.ClientOption {
	var out []option.ClientOption
	if options.Endpoint != "" {
		out = append(out, option.WithEndpoint(options.Endpoint), option.WithoutAuthentication())
	}
	if len(options.CredentialsJSON) > 0 {
		// A service account key only (the configuration checks the type
		// too): an external_account or impersonation file would make the
		// client call the URLs it names.
		out = append(out, option.WithAuthCredentialsJSON(option.ServiceAccount, options.CredentialsJSON))
	}
	return out
}

// List implements store.Reader with one Cloud Storage objects list call under the configured
// prefix; the returned cursor is signed and bound to the requested prefix.
func (s *Store) List(ctx context.Context, request store.ListRequest) (store.Page, error) {
	if err := request.Validate(); err != nil || strings.HasPrefix(request.Prefix, "/") {
		return store.Page{}, store.ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	it := s.client.Bucket(s.bucket).Objects(ctx, &storage.Query{Prefix: s.prefix + request.Prefix, Projection: storage.ProjectionNoACL})
	nativeCursor := ""
	if request.Cursor != "" {
		token, err := s.cursor.Decode(request.Prefix, request.Cursor)
		if err != nil {
			return store.Page{}, store.ErrInvalidRequest
		}
		nativeCursor = token
	}
	pager := iterator.NewPager(it, request.Limit, nativeCursor)
	var objects []*storage.ObjectAttrs
	nextCursor, err := pager.NextPage(&objects)
	if err != nil {
		return store.Page{}, safeError(ctx, err)
	}
	if len(objects) > request.Limit {
		return store.Page{}, &Error{kind: fault.Unavailable}
	}
	result := store.Page{Objects: make([]store.Object, 0, len(objects))}
	for _, attrs := range objects {
		key, ok := s.relative(attrs.Name, request.Prefix)
		if !ok || key == "" || len(key) > store.MaxKeyBytes || attrs.Size < 0 {
			return store.Page{}, &Error{kind: fault.SafetyLimit}
		}
		etag := attrs.Etag
		if len(etag) > 512 {
			etag = ""
		}
		result.Objects = append(result.Objects, store.Object{Key: key, Size: attrs.Size, LastModified: attrs.Updated.UTC(), ETag: etag})
	}
	if nextCursor != "" {
		encoded, err := s.cursor.Encode(request.Prefix, nextCursor)
		if err != nil {
			return store.Page{}, &Error{kind: fault.SafetyLimit}
		}
		result.NextCursor = encoded
	}
	return result, nil
}

// Open implements store.Reader. The body yields at most request.MaxBytes,
// and closing it ends the request.
func (s *Store) Open(ctx context.Context, request store.OpenRequest) (io.ReadCloser, error) {
	if err := request.Validate(); err != nil || strings.HasPrefix(request.Key, "/") {
		return nil, store.ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	reader, err := s.client.Bucket(s.bucket).Object(s.prefix + request.Key).NewReader(ctx)
	if err != nil {
		// Categorized before cancel: after it, ctx.Err() would turn every
		// failure into a cancellation.
		err = safeError(ctx, err)
		cancel()
		return nil, err
	}
	return &body{Reader: io.LimitReader(reader, request.MaxBytes), body: reader, cancel: cancel}, nil
}

// Stat implements store.Reader by reading the object's metadata only.
func (s *Store) Stat(ctx context.Context, request store.StatRequest) (store.Object, error) {
	if err := request.Validate(); err != nil || strings.HasPrefix(request.Key, "/") {
		return store.Object{}, store.ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	attrs, err := s.client.Bucket(s.bucket).Object(s.prefix + request.Key).Attrs(ctx)
	if err != nil {
		return store.Object{}, safeError(ctx, err)
	}
	if attrs.Size < 0 {
		return store.Object{}, &Error{kind: fault.Unavailable}
	}
	etag := attrs.Etag
	if len(etag) > 512 {
		etag = ""
	}
	return store.Object{Key: request.Key, Size: attrs.Size, LastModified: attrs.Updated.UTC(), ETag: etag}, nil
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

func (e *Error) Error() string { return "gcs request failed: " + string(e.kind) }

// Category returns the failure category.
func (e *Error) Category() fault.Category { return e.kind }

// RootCause returns the unredacted provider message for redacted logging.
func (e *Error) RootCause() string {
	if e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

// CauseKey identifies a service error by status and first reason for log
// deduplication; "" for transport errors.
func (e *Error) CauseKey() string {
	var apiError *googleapi.Error
	if e.cause != nil && errors.As(e.cause, &apiError) {
		reason := ""
		if len(apiError.Errors) > 0 {
			reason = apiError.Errors[0].Reason
		}
		return strconv.Itoa(apiError.Code) + "/" + reason
	}
	return ""
}

func safeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, nofollow.ErrRedirect) {
		// The cause is the bare sentinel: the wrapping url.Error names the
		// URL, and a Location may carry a signed query.
		return &Error{kind: fault.InvalidConfig, cause: nofollow.ErrRedirect}
	}
	var apiError *googleapi.Error
	if errors.As(err, &apiError) {
		switch apiError.Code {
		case 401:
			return &Error{kind: fault.Authentication, cause: err}
		case 403:
			return &Error{kind: fault.Authorization, cause: err}
		case 404:
			return &Error{kind: fault.NotFound, cause: err}
		case 429:
			return &Error{kind: fault.Throttled, cause: err}
		}
	}
	return &Error{kind: fault.Unavailable, cause: err}
}

var _ store.Reader = (*Store)(nil)
