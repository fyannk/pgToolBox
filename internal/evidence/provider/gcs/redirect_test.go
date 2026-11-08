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

package gcs

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/provider/nofollow"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// redirecting is an endpoint that redirects every request to elsewhere,
// counting the requests and whether they carried a bearer token.
type redirecting struct {
	asked, bearer, stolen atomic.Int32
	server, elsewhere     *httptest.Server
}

func newRedirecting(t *testing.T) *redirecting {
	t.Helper()
	r := &redirecting{}
	r.elsewhere = httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { r.stolen.Add(1) }))
	t.Cleanup(r.elsewhere.Close)
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		r.asked.Add(1)
		if request.Header.Get("Authorization") != "" {
			r.bearer.Add(1)
		}
		http.Redirect(w, request, r.elsewhere.URL+request.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(r.server.Close)
	return r
}

// TestGCSFollowsNoRedirect (as the S3 and Azure ones): an endpoint that
// redirects elsewhere gets nothing sent there, and the refusal is a
// configuration error naming no URL, not retried.
func TestGCSFollowsNoRedirect(t *testing.T) {
	t.Parallel()
	r := newRedirecting(t)
	adapter, err := New(context.Background(), Options{Bucket: "backups", Prefix: "team-a", Endpoint: r.server.URL + "/storage/v1/", RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	before := r.asked.Load()
	if _, err := adapter.Stat(context.Background(), store.StatRequest{Key: "orders/base/backup.info"}); !refusedRedirect(err) {
		t.Errorf("Stat = %v, want the redirect refused", err)
	}
	if got := r.asked.Load() - before; got != 1 {
		t.Errorf("the endpoint was asked %d times, want once (a refused redirect is not retried)", got)
	}
	if _, err := adapter.Open(context.Background(), store.OpenRequest{Key: "orders/base/backup.info", MaxBytes: 64}); !refusedRedirect(err) {
		t.Errorf("Open = %v, want the redirect refused", err)
	}
	if _, err := adapter.List(context.Background(), store.ListRequest{Prefix: "orders/", Limit: 10}); !refusedRedirect(err) {
		t.Errorf("List = %v, want the redirect refused", err)
	}
	if r.stolen.Load() != 0 {
		t.Fatalf("the redirect target was reached %d times", r.stolen.Load())
	}
}

// TestGCSBearerTokenIsNotFollowedElsewhere: with a service account key, the
// oauth2 transport sets the bearer token on every request it sends; the
// redirect is refused below it, so the token never reaches another host.
func TestGCSBearerTokenIsNotFollowedElsewhere(t *testing.T) {
	t.Parallel()
	r := newRedirecting(t)
	var tokens atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokens.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"synthetic-bearer-canary","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, key)})
	credentials, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "synthetic", "private_key_id": "synthetic",
		"private_key": string(keyPEM), "client_email": "agent@synthetic.iam.gserviceaccount.com",
		"client_id": "1", "token_uri": tokenServer.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := newHTTPClient(context.Background(), Options{CredentialsJSON: credentials})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, r.server.URL+"/storage/v1/b/backups/o", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err == nil {
		_ = response.Body.Close()
	}
	if !errors.Is(err, nofollow.ErrRedirect) {
		t.Fatalf("request = %v, want the redirect refused", err)
	}
	if r.bearer.Load() != 1 || tokens.Load() == 0 {
		t.Fatalf("the endpoint saw %d authenticated requests (%d tokens issued), want 1: the test does not exercise the oauth2 transport", r.bearer.Load(), tokens.Load())
	}
	if r.stolen.Load() != 0 {
		t.Fatalf("the redirect target was reached %d times", r.stolen.Load())
	}
}

func mustPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// refusedRedirect: the adapter's error says the redirect was refused, as a
// configuration problem, with no URL in its cause.
func refusedRedirect(err error) bool {
	var adapterError *Error
	return errors.As(err, &adapterError) && adapterError.Category() == fault.InvalidConfig && adapterError.RootCause() == nofollow.ErrRedirect.Error()
}
