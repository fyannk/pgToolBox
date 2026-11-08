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

package mcpauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
)

func post(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, publicURL+PathMCP, strings.NewReader(body))
	r.RemoteAddr = "10.0.0.7:5555"
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// echoBody answers with what reached it, so a test sees the body the
// guard handed on.
func echoGuard(o Options) http.Handler {
	return NewGuard(o).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write(b)
	}))
}

func TestGuardOneMessagePerRequest(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, body string
		code       int
		calls      int // API server reviews
	}{
		{"one message is handed on whole", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 200, 1},
		{"a batch is refused before the token is checked", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, 400, 0},
		{"leading blanks do not hide a batch", " \n\t [ {} ]", 400, 0},
		{"a body over the limit", `{"x":"` + strings.Repeat("a", 2<<10) + `"}`, 413, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ch := &checker{on: true}
			h := echoGuard(Options{Authenticator: RawToken{Checker: ch}, PublicURL: publicURL, MaxBodyBytes: 1 << 10})
			w := post(t, h, "Bearer good-dba", c.body)
			if w.Code != c.code || ch.calls != c.calls {
				t.Fatalf("code %d, reviews %d; want %d, %d: %s", w.Code, ch.calls, c.code, c.calls, w.Body)
			}
			if c.code == 200 && w.Body.String() != c.body {
				t.Errorf("the body handed on = %q", w.Body)
			}
			if c.code == 400 && !strings.Contains(w.Body.String(), `"code":-32600`) {
				t.Errorf("batch refusal = %s", w.Body)
			}
		})
	}
}

// Every header the SDK would refuse, the guard refuses first, before any
// budget is spent or the API server is asked.
func TestGuardParsesTheBearerAsTheSDKDoes(t *testing.T) {
	t.Parallel()
	for _, hdr := range []string{"Bearer Bearer good-dba", "Bearer good-dba extra", "Bearer", "Token good-dba", "Bearergood-dba"} {
		ch := &checker{on: true}
		h := echoGuard(Options{Authenticator: RawToken{Checker: ch}, PublicURL: publicURL})
		if w := post(t, h, hdr, `{}`); w.Code != http.StatusUnauthorized || ch.calls != 0 {
			t.Errorf("%q: %d, %d reviews", hdr, w.Code, ch.calls)
		}
	}
	ch := &checker{on: true}
	h := echoGuard(Options{Authenticator: RawToken{Checker: ch}, PublicURL: publicURL})
	if w := post(t, h, "  bEaReR   good-dba  ", `{}`); w.Code != http.StatusOK {
		t.Errorf("spacing and case the SDK accepts = %d", w.Code)
	}
}

func TestGuardConcurrentCallsPerUser(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	auth1 := AuthenticatorFunc(func(_ context.Context, tok string) (*auth.Principal, error) {
		return &auth.Principal{User: authz.User{Name: strings.TrimPrefix(tok, "good-")}}, nil
	})
	h := NewGuard(Options{Authenticator: auth1, PublicURL: publicURL, MaxConcurrent: 2}).Wrap(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			entered <- struct{}{}
			<-release
		}))
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { post(t, h, "Bearer good-dba", `{}`) })
	}
	<-entered
	<-entered
	if w := post(t, h, "Bearer good-dba", `{}`); w.Code != http.StatusTooManyRequests {
		t.Errorf("a third call in flight = %d", w.Code)
	}
	// Another user is not held by dba's calls.
	done := make(chan int, 1)
	go func() { done <- post(t, h, "Bearer good-ops", `{}`).Code }()
	<-entered
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Errorf("another user = %d", code)
	}
	wg.Wait()
	// The slots are given back.
	if w := post(t, h, "Bearer good-dba", `{}`); w.Code != http.StatusOK {
		t.Errorf("after the calls ended = %d", w.Code)
	}
}

func TestGuardExpiredTokenIsInvalid(t *testing.T) {
	t.Parallel()
	h := echoGuard(Options{
		Authenticator: AuthenticatorFunc(func(context.Context, string) (*auth.Principal, error) { return nil, auth.ErrBearerExpired }),
		PublicURL:     publicURL,
	})
	w := post(t, h, "Bearer old", `{}`)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), `error_description="the token is expired"`) {
		t.Errorf("expired = %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
}
