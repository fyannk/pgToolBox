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
	"crypto/md5" // #nosec G501 -- the SSE-C key digest is MD5 by protocol.
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// sseCServer is an S3-compatible endpoint whose objects are SSE-C
// encrypted: a GET or HEAD without the right three headers is refused, as
// S3 refuses it; a list must carry none (barman-cloud lists without the
// key, and S3 rejects SSE-C headers on a list).
type sseCServer struct {
	key []byte

	mu      sync.Mutex
	seen    map[string]int
	refused []string
}

func (s *sseCServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	algorithm := r.Header.Get("X-Amz-Server-Side-Encryption-Customer-Algorithm")
	key := r.Header.Get("X-Amz-Server-Side-Encryption-Customer-Key")
	digest := r.Header.Get("X-Amz-Server-Side-Encryption-Customer-Key-Md5")
	sum := md5.Sum(s.key) // #nosec G401 -- protocol digest.
	matches := algorithm == "AES256" && key == base64.StdEncoding.EncodeToString(s.key) && digest == base64.StdEncoding.EncodeToString(sum[:])
	present := algorithm != "" || key != "" || digest != ""
	operation := r.Method
	if r.URL.Query().Get("list-type") == "2" {
		operation = "LIST"
	}
	s.mu.Lock()
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	s.seen[operation]++
	refuse := (operation == "LIST" && present) || (operation != "LIST" && !matches)
	if refuse {
		s.refused = append(s.refused, operation)
	}
	s.mu.Unlock()
	if refuse {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>InvalidRequest</Code><Message>The object was stored using a form of Server Side Encryption.</Message></Error>`)
		}
		return
	}
	switch operation {
	case "LIST":
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>synthetic-bucket</Name><KeyCount>1</KeyCount><IsTruncated>false</IsTruncated>`+
			`<Contents><Key>repository/orders/base/backup.info</Key><Size>5</Size><ETag>"e"</ETag></Contents></ListBucketResult>`)
	case http.MethodHead:
		w.Header().Set("Content-Length", "5")
		w.Header().Set("ETag", `"e"`)
	default:
		w.Header().Set("Content-Length", "5")
		_, _ = io.WriteString(w, "plain")
	}
}

// TestS3ContractSendsTheSSECustomerKeyOnEveryRead drives the real SDK
// client: the headers are what the wire carries, not what an input struct
// says. The endpoint is TLS with a private CA (SSE-C is refused over plain
// HTTP by S3 and MinIO alike), through the CA bundle option.
func TestS3ContractSendsTheSSECustomerKeyOnEveryRead(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x5a}, sseCustomerKeyBytes)
	handler := &sseCServer{key: key}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	caBundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	open := func(t *testing.T, options Options) *Store {
		t.Helper()
		options.Bucket, options.Prefix, options.Region, options.RequestTimeout = "synthetic-bucket", "repository", "us-east-1", 5*time.Second
		options.Endpoint, options.CABundle = endpoint, caBundle
		options.AccessKeyID, options.SecretAccessKey = []byte("synthetic-access"), []byte("synthetic-secret-canary")
		adapter, err := New(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	}
	ctx := context.Background()

	adapter := open(t, Options{SSECustomerKey: key})
	page, err := adapter.List(ctx, store.ListRequest{Prefix: "orders/", Limit: 10})
	if err != nil || len(page.Objects) != 1 {
		t.Fatalf("List = %#v, %v (a list carries no SSE-C header)", page, err)
	}
	object, err := adapter.Stat(ctx, store.StatRequest{Key: "orders/base/backup.info"})
	if err != nil || object.Size != 5 {
		t.Fatalf("Stat = %#v, %v", object, err)
	}
	body, err := adapter.Open(ctx, store.OpenRequest{Key: "orders/base/backup.info", MaxBytes: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	content, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(content) != "plain" {
		t.Fatalf("Open read %q, %v", content, err)
	}
	handler.mu.Lock()
	seen, refused := handler.seen, handler.refused
	handler.mu.Unlock()
	if seen["LIST"] != 1 || seen[http.MethodHead] != 1 || seen[http.MethodGet] != 1 || len(refused) != 0 {
		t.Fatalf("requests = %v, refused = %v", seen, refused)
	}

	// Without the key (the shape before SSE-C was supported) every object
	// read is refused by the store.
	unkeyed := open(t, Options{})
	if _, err := unkeyed.Stat(ctx, store.StatRequest{Key: "orders/base/backup.info"}); err == nil {
		t.Fatal("a HEAD without the SSE-C key was answered")
	}
	if _, err := unkeyed.Open(ctx, store.OpenRequest{Key: "orders/base/backup.info", MaxBytes: 64}); err == nil {
		t.Fatal("a GET without the SSE-C key was answered")
	} else if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(key)) {
		t.Fatal("the error discloses the key")
	}
}

func TestS3SSECustomerKeyMustBe256Bits(t *testing.T) {
	t.Parallel()
	for _, length := range []int{1, 16, 31, 33, 64} {
		options := testOptions("repository")
		options.SSECustomerKey = bytes.Repeat([]byte{1}, length)
		if _, err := newWithAPI(&fakeAPI{}, options, testCursorKey); fault.Categorize(err) != fault.InvalidConfig {
			t.Fatalf("a %d-byte SSE-C key was accepted: %v", length, err)
		}
	}
}
