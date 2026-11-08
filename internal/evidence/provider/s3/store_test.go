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
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

var testCursorKey = []byte("0123456789abcdef0123456789abcdef")

type fakeAPI struct {
	list func(context.Context, *awss3.ListObjectsV2Input) (*awss3.ListObjectsV2Output, error)
	get  func(context.Context, *awss3.GetObjectInput) (*awss3.GetObjectOutput, error)
	head func(context.Context, *awss3.HeadObjectInput) (*awss3.HeadObjectOutput, error)
}

func (f *fakeAPI) ListObjectsV2(ctx context.Context, input *awss3.ListObjectsV2Input, _ ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error) {
	if f.list == nil {
		return &awss3.ListObjectsV2Output{}, nil
	}
	return f.list(ctx, input)
}

func (f *fakeAPI) GetObject(ctx context.Context, input *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	if f.get == nil {
		return &awss3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	return f.get(ctx, input)
}

func (f *fakeAPI) HeadObject(ctx context.Context, input *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	if f.head == nil {
		return &awss3.HeadObjectOutput{ContentLength: aws.Int64(0)}, nil
	}
	return f.head(ctx, input)
}

func TestS3ContractListPaginationAndPrefixConfinement(t *testing.T) {
	t.Parallel()
	modified := time.Date(2026, 7, 27, 12, 0, 0, 123, time.FixedZone("offset", 3600))
	var mu sync.Mutex
	var inputs []*awss3.ListObjectsV2Input
	api := &fakeAPI{list: func(_ context.Context, input *awss3.ListObjectsV2Input) (*awss3.ListObjectsV2Output, error) {
		mu.Lock()
		copyInput := *input
		inputs = append(inputs, &copyInput)
		call := len(inputs)
		mu.Unlock()
		if call == 1 {
			return &awss3.ListObjectsV2Output{
				Contents:    []types.Object{{Key: aws.String("repository/alpha/one"), Size: aws.Int64(7), LastModified: &modified, ETag: aws.String("etag-one")}},
				IsTruncated: aws.Bool(true), NextContinuationToken: aws.String("native-secret-token"),
			}, nil
		}
		return &awss3.ListObjectsV2Output{
			Contents: []types.Object{{Key: aws.String("repository/alpha/two"), Size: aws.Int64(9)}},
		}, nil
	}}
	adapter := newTestStore(t, api)
	first, err := adapter.List(context.Background(), store.ListRequest{Prefix: "alpha/", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Objects) != 1 || first.Objects[0].Key != "alpha/one" || first.Objects[0].Size != 7 || first.Objects[0].LastModified.Location() != time.UTC {
		t.Fatalf("first page = %#v", first)
	}
	if first.NextCursor == "" || strings.Contains(first.NextCursor, "native-secret-token") {
		t.Fatalf("cursor was not opaque: %q", first.NextCursor)
	}
	second, err := adapter.List(context.Background(), store.ListRequest{Prefix: "alpha/", Cursor: first.NextCursor, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Objects) != 1 || second.Objects[0].Key != "alpha/two" || second.NextCursor != "" {
		t.Fatalf("second page = %#v", second)
	}
	if len(inputs) != 2 || aws.ToString(inputs[0].Bucket) != "synthetic-bucket" || aws.ToString(inputs[0].Prefix) != "repository/alpha/" || inputs[0].ContinuationToken != nil || aws.ToString(inputs[1].ContinuationToken) != "native-secret-token" {
		t.Fatalf("SDK inputs = %#v", inputs)
	}
}

func TestS3ContractRejectsTamperedOrCrossScopeCursors(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{list: func(context.Context, *awss3.ListObjectsV2Input) (*awss3.ListObjectsV2Output, error) {
		return &awss3.ListObjectsV2Output{IsTruncated: aws.Bool(true), NextContinuationToken: aws.String("native")}, nil
	}}
	firstStore := newTestStore(t, api)
	page, err := firstStore.List(context.Background(), store.ListRequest{Prefix: "alpha/", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	tampered := page.NextCursor[:len(page.NextCursor)-1] + "A"
	secondStore, err := newWithAPI(api, testOptions("repository"), []byte("abcdef0123456789abcdef0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		adapter *Store
		prefix  string
		cursor  string
	}{
		"tampered":     {firstStore, "alpha/", tampered},
		"cross-prefix": {firstStore, "beta/", page.NextCursor},
		"cross-store":  {secondStore, "alpha/", page.NextCursor},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := test.adapter.List(context.Background(), store.ListRequest{Prefix: test.prefix, Cursor: test.cursor, Limit: 1}); !errors.Is(err, store.ErrInvalidRequest) {
				t.Fatalf("List() error = %v", err)
			}
		})
	}
}

func TestS3ContractRejectsAdapterBoundaryViolations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output *awss3.ListObjectsV2Output
		kind   fault.Category
	}{
		{name: "object escapes root", output: &awss3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String("other/value"), Size: aws.Int64(1)}}}, kind: fault.SafetyLimit},
		{name: "negative size", output: &awss3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String("repository/value"), Size: aws.Int64(-1)}}}, kind: fault.Unavailable},
		{name: "too many rows", output: &awss3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String("repository/one"), Size: aws.Int64(1)}, {Key: aws.String("repository/two"), Size: aws.Int64(1)}}}, kind: fault.Unavailable},
		{name: "truncation without token", output: &awss3.ListObjectsV2Output{IsTruncated: aws.Bool(true)}, kind: fault.Unavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			adapter := newTestStore(t, &fakeAPI{list: func(context.Context, *awss3.ListObjectsV2Input) (*awss3.ListObjectsV2Output, error) {
				return test.output, nil
			}})
			_, err := adapter.List(context.Background(), store.ListRequest{Limit: 1})
			if fault.Categorize(err) != test.kind {
				t.Fatalf("List() error = %v, category %s", err, fault.Categorize(err))
			}
		})
	}
}

func TestS3ContractMapsErrorsWithoutDisclosingSDKDetails(t *testing.T) {
	t.Parallel()
	canary := "secret-endpoint-and-token-canary"
	tests := []struct {
		code string
		want fault.Category
	}{
		{code: "AccessDenied", want: fault.Authorization},
		{code: "ExpiredToken", want: fault.Authentication},
		{code: "NoSuchBucket", want: fault.NotFound},
		{code: "SlowDown", want: fault.Throttled},
		{code: "InternalError", want: fault.Unavailable},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			t.Parallel()
			adapter := newTestStore(t, &fakeAPI{list: func(context.Context, *awss3.ListObjectsV2Input) (*awss3.ListObjectsV2Output, error) {
				return nil, &smithy.GenericAPIError{Code: test.code, Message: canary}
			}})
			_, err := adapter.List(context.Background(), store.ListRequest{Limit: 1})
			if fault.Categorize(err) != test.want || strings.Contains(err.Error(), canary) || errors.Unwrap(err) != nil {
				t.Fatalf("safe error = %#v (%v)", err, err)
			}
			// The cause stays available, unrendered, for redacted logging.
			var carrier interface{ RootCause() string }
			if !errors.As(err, &carrier) || !strings.Contains(carrier.RootCause(), canary) {
				t.Fatalf("root cause was not retained: %v", err)
			}
			var keyed interface{ CauseKey() string }
			if !errors.As(err, &keyed) || keyed.CauseKey() != "list/"+test.code {
				t.Fatalf("cause key = %v", err)
			}
		})
	}
}

func TestS3ContractHonorsTimeoutAndCancellation(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{list: func(ctx context.Context, _ *awss3.ListObjectsV2Input) (*awss3.ListObjectsV2Output, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	options := testOptions("")
	options.RequestTimeout = 10 * time.Millisecond
	adapter, err := newWithAPI(api, options, testCursorKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.List(context.Background(), store.ListRequest{Limit: 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.List(ctx, store.ListRequest{Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestS3ContractOpenIsBoundedAndCloseCancelsRequest(t *testing.T) {
	t.Parallel()
	body := &trackingBody{Reader: strings.NewReader("0123456789")}
	api := &fakeAPI{get: func(_ context.Context, input *awss3.GetObjectInput) (*awss3.GetObjectOutput, error) {
		if aws.ToString(input.Bucket) != "synthetic-bucket" || aws.ToString(input.Key) != "repository/alpha/value" {
			t.Fatalf("GetObject input = %#v", input)
		}
		return &awss3.GetObjectOutput{Body: body}, nil
	}}
	adapter := newTestStore(t, api)
	reader, err := adapter.Open(context.Background(), store.OpenRequest{Key: "alpha/value", MaxBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "0123" {
		t.Fatalf("bounded content = %q", content)
	}
	if err := reader.Close(); err != nil || !body.closed {
		t.Fatalf("Close() error = %v, body closed = %v", err, body.closed)
	}
}

func TestS3ContractStatReturnsOnlyAllowlistedMetadata(t *testing.T) {
	t.Parallel()
	modified := time.Date(2026, 7, 27, 12, 0, 0, 0, time.FixedZone("offset", 7200))
	api := &fakeAPI{head: func(_ context.Context, input *awss3.HeadObjectInput) (*awss3.HeadObjectOutput, error) {
		if aws.ToString(input.Key) != "repository/alpha/value" {
			t.Fatalf("HeadObject key = %q", aws.ToString(input.Key))
		}
		return &awss3.HeadObjectOutput{
			ContentLength: aws.Int64(99), LastModified: &modified, ETag: aws.String("etag"),
			Metadata: map[string]string{"credential-canary": "must-not-cross-boundary"},
		}, nil
	}}
	adapter := newTestStore(t, api)
	object, err := adapter.Stat(context.Background(), store.StatRequest{Key: "alpha/value"})
	if err != nil {
		t.Fatal(err)
	}
	if object.Key != "alpha/value" || object.Size != 99 || object.ETag != "etag" || object.LastModified.Location() != time.UTC {
		t.Fatalf("Stat() = %#v", object)
	}
	if strings.Contains(object.Key+object.ETag, "canary") {
		t.Fatalf("custom metadata crossed adapter: %#v", object)
	}
}

func TestS3ContractRejectsUnboundedOrAbsoluteRequests(t *testing.T) {
	t.Parallel()
	adapter := newTestStore(t, &fakeAPI{})
	if _, err := adapter.List(context.Background(), store.ListRequest{Limit: 0}); !errors.Is(err, store.ErrInvalidRequest) {
		t.Fatalf("unbounded List() error = %v", err)
	}
	if _, err := adapter.List(context.Background(), store.ListRequest{Prefix: "/escape", Limit: 1}); !errors.Is(err, store.ErrInvalidRequest) {
		t.Fatalf("absolute-prefix List() error = %v", err)
	}
	if _, err := adapter.Open(context.Background(), store.OpenRequest{Key: "/escape", MaxBytes: 1}); !errors.Is(err, store.ErrInvalidRequest) {
		t.Fatalf("absolute-key Open() error = %v", err)
	}
}

func TestS3ContractRejectsInvalidAdapterConfiguration(t *testing.T) {
	t.Parallel()
	endpointWithPath, err := url.Parse("https://endpoint-canary.invalid/api")
	if err != nil {
		t.Fatal(err)
	}
	tests := []Options{
		{Bucket: "synthetic-bucket", RequestTimeout: 6 * time.Minute},
		{Bucket: "synthetic-bucket", RequestTimeout: time.Second, AccessKeyID: []byte("unpaired-canary")},
		{Bucket: "synthetic-bucket", RequestTimeout: time.Second, SessionToken: []byte("orphan-token-canary")},
		{Bucket: "synthetic-bucket", RequestTimeout: time.Second, WebIdentityTokenFile: "/token-canary"},
		{Bucket: "synthetic-bucket", RequestTimeout: time.Second, RoleARN: "arn:aws:iam::123:role/canary"},
		{Bucket: "synthetic-bucket", RequestTimeout: time.Second, AccessKeyID: []byte("access"), SecretAccessKey: []byte("secret"), WebIdentityTokenFile: "/token-canary", RoleARN: "arn:aws:iam::123:role/canary"},
		{Bucket: "synthetic-bucket", RequestTimeout: time.Second, Endpoint: endpointWithPath},
	}
	for _, options := range tests {
		_, err := newWithAPI(&fakeAPI{}, options, testCursorKey)
		if fault.Categorize(err) != fault.InvalidConfig || strings.Contains(err.Error(), "canary") {
			t.Fatalf("newWithAPI() error = %v", err)
		}
	}
}

func TestNewAcceptsExplicitWebIdentityWithoutDefaultCredentialChain(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "web-identity-token")
	if err := os.WriteFile(path, []byte("synthetic-jwt"), 0o400); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(context.Background(), Options{
		Bucket: "synthetic-bucket", Region: "eu-west-1", RequestTimeout: time.Second,
		WebIdentityTokenFile: path, RoleARN: "arn:aws:iam::123456789012:role/evidence-agent",
	})
	if err != nil || adapter == nil {
		t.Fatalf("New() = %#v, %v", adapter, err)
	}
}

func TestNewRequiresASigningRegion(t *testing.T) {
	t.Parallel()
	endpoint, err := url.Parse("https://minio.example.test")
	if err != nil {
		t.Fatal(err)
	}
	// Configuration resolves the region; the adapter refuses to guess, even
	// for an S3-compatible endpoint, so no ambient profile file can decide it.
	for _, options := range []Options{
		{Bucket: "synthetic-bucket", RequestTimeout: time.Second},
		{Bucket: "synthetic-bucket", RequestTimeout: time.Second, Endpoint: endpoint},
	} {
		if adapter, err := New(context.Background(), options); adapter != nil || fault.Categorize(err) != fault.InvalidConfig {
			t.Fatalf("New() without region = %#v, %v", adapter, err)
		}
	}
}

func newTestStore(t *testing.T, api *fakeAPI) *Store {
	t.Helper()
	adapter, err := newWithAPI(api, testOptions("repository"), testCursorKey)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func testOptions(prefix string) Options {
	return Options{Bucket: "synthetic-bucket", Prefix: prefix, RequestTimeout: time.Second}
}

type trackingBody struct {
	io.Reader
	closed bool
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}
