//go:build e2echart

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

package chart

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// A kind control plane on a loaded single CI host can answer a write with an
// etcd timeout while the cluster is otherwise healthy. Those answers, and only
// those, are retried a few times with a backoff; every retry is logged. Any other
// error (a rejected object, a failed rollout, a policy assertion) is returned at
// once.
const retryAttempts = 4

// transientAPIMarkers are the API server's own wording for "try again".
var transientAPIMarkers = []string{
	"etcdserver: request timed out",
	"etcdserver: leader changed",
	"the server is currently unable to handle the request",
}

func retryBackoff(attempt int) time.Duration { return time.Duration(attempt) * 5 * time.Second }

func transientText(s string, extra ...string) bool {
	for _, m := range append(extra, transientAPIMarkers...) {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// retryTransport retries a REST request when the API server answers 5xx with a
// transient message. Wired in with rest.Config.Wrap, so every client-go Create,
// Update and Get in the harness gets it. Watches (streaming bodies) are not read.
type retryTransport struct{ next http.RoundTripper }

func (r retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		resp, err := r.next.RoundTrip(req)
		if err != nil || resp.StatusCode < 500 || attempt == retryAttempts ||
			(req.Body != nil && req.GetBody == nil) {
			return resp, err
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if rerr != nil || !transientText(string(body), "context deadline exceeded") {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			return resp, nil //nolint:nilerr // a body that cannot be read is not transient: the caller gets the response as it came
		}
		fmt.Fprintf(os.Stderr, "[e2e-chart] transient API error on %s %s (attempt %d/%d), retrying: %s\n",
			req.Method, req.URL.Path, attempt, retryAttempts, strings.TrimSpace(string(body)))
		time.Sleep(retryBackoff(attempt))
		if req.GetBody != nil {
			b, gerr := req.GetBody()
			if gerr != nil {
				return nil, gerr
			}
			req = req.Clone(req.Context())
			req.Body = b
		}
	}
}

// helmTransient reports whether helm's output names a transient API error. A bare
// "context deadline exceeded" is not one here: helm prints it when --wait itself
// times out, which is a real failure.
func helmTransient(out string) bool { return transientText(out) }

// TestBrowserClientsDropIdleConnectionsBeforeTheConsoleDoes: the console closes
// idle connections after 120 s; a client that keeps one longer reuses a dead
// connection after the tests' long sleeps and fails a POST that Go will not retry.
func TestBrowserClientsDropIdleConnectionsBeforeTheConsoleDoes(t *testing.T) {
	for _, noKeepAlive := range []bool{false, true} {
		b := &browser{t: t}
		tr, ok := b.build(noKeepAlive).Transport.(*http.Transport)
		if !ok {
			t.Fatal("the e2e client's transport is not an *http.Transport")
		}
		if tr.IdleConnTimeout <= 0 || tr.IdleConnTimeout >= 120*time.Second {
			t.Fatalf("IdleConnTimeout = %v, want 0 < d < the console's 120 s idle timeout", tr.IdleConnTimeout)
		}
	}
}
