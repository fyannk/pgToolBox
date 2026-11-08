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

package api

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/sse"
)

// rejectedTokenAuthorizer answers every question as the SelfReviewer does
// once the session's access token has expired and was not refreshed: the
// API server refuses the token (401).
type rejectedTokenAuthorizer struct{ FixtureAuthorizer }

func (rejectedTokenAuthorizer) Allowed(_ context.Context, _ authz.User, at authz.Attributes) authz.Decision {
	return authz.Decision{Err: fmt.Errorf("SubjectAccessReview for %s: %w", at, auth.ErrTokenRejected)}
}

// readStream opens /api/v1/stream as p, publishes one invalidation for
// team-a/orders once subscribed, and returns everything written until the
// server closes the stream.
func readStream(t *testing.T, deps Deps, p *auth.Principal) string {
	t.Helper()
	broker := sse.NewBroker()
	deps.Source, deps.Broker = FixtureSource{}, broker
	h, _ := New(deps)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	u, _ := url.JoinPath(srv.URL, "/api/v1/stream")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	go func() {
		for broker.Clients() == 0 && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		broker.Publish(sse.Invalidate{Topic: sse.TopicCluster, Namespace: "team-a", Name: "orders"})
	}()
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if ctx.Err() != nil {
		t.Fatalf("the stream did not close by itself:\n%s", strings.Join(lines, "\n"))
	}
	return strings.Join(lines, "\n")
}

// lastEvent is the name and data of the stream's last event.
func lastEvent(stream string) (name, data string) {
	for line := range strings.SplitSeq(stream, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name, data = strings.TrimPrefix(line, "event: "), ""
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	return name, data
}

// The stream never hides an event it could not check, and never ends
// without a last event the UI acts on: a reviewer that
// fails or times out yields a resync in place of the invalidation; a token
// the API server no longer accepts (expired, not refreshed) and a session
// revoked meanwhile end the stream with a resync, whose refetch meets the
// 401 that leads to sign-in.
func TestStreamUndecidedAndSessionEnd(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		deps   Deps
		expiry time.Duration
		// want is in the stream; hidden: the invalidation must not be;
		// wantLast is the last event's reason.
		hidden         bool
		want, wantLast string
	}{
		"reviewer error": {
			deps:   Deps{Authz: FixtureAuthorizer{Unchecked: map[authz.ObjectRef]bool{{Namespace: "team-a", Name: "orders"}: true}}},
			expiry: 2 * time.Second,
			want:   `"reason":"` + sse.ReasonUndecided + `"`, hidden: true, wantLast: sse.ReasonSessionExpired,
		},
		"token expired without a refresh": {
			deps:   Deps{Authz: rejectedTokenAuthorizer{}},
			expiry: time.Hour, hidden: true, wantLast: sse.ReasonSessionExpired,
		},
		"session revoked": {
			deps:   Deps{Authz: FixtureAuthorizer{}, Sessions: revokedSessions{}, Heartbeat: 50 * time.Millisecond},
			expiry: time.Hour, wantLast: sse.ReasonSessionEnded,
		},
		"session reaches its end": {
			deps:   Deps{Authz: FixtureAuthorizer{}},
			expiry: 2 * time.Second,
			want:   `"name":"orders"`, wantLast: sse.ReasonSessionExpired,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := FixturePrincipal()
			p.Expiry = time.Now().Add(tc.expiry)
			if tc.deps.Heartbeat == 0 {
				tc.deps.Heartbeat = time.Hour
			}
			stream := readStream(t, tc.deps, p)
			if tc.want != "" && !strings.Contains(stream, tc.want) {
				t.Errorf("stream lacks %s:\n%s", tc.want, stream)
			}
			if tc.hidden && strings.Contains(stream, `"name":"orders"`) {
				t.Errorf("an invalidation whose check did not allow it was sent:\n%s", stream)
			}
			event, data := lastEvent(stream)
			if event != "resync" || data != `{"reason":"`+tc.wantLast+`"}` {
				t.Errorf("last event = %s %s, want resync with reason %q:\n%s", event, data, tc.wantLast, stream)
			}
		})
	}
}
