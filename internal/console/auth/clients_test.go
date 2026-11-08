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

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/fyannk/pgToolBox/internal/console/authz"
)

func TestParseRevocationList(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	sid := strings.Repeat("ab", 16)
	l, err := ParseRevocationList(map[string]string{
		"notBefore": "2026-10-29T10:00:00Z",
		"users":     "# comment\nalice 2026-10-29T12:00:00Z\n\nalice 2026-10-29T11:00:00Z\n",
		"sessions":  sid + "\n" + BearerReference("sa-token") + "\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		sid, user string
		issued    time.Time
		want      bool
	}{
		{"before global notBefore", "x", "bob", t0.Add(-3 * time.Hour), true},
		{"after global notBefore", "x", "bob", t0.Add(-time.Hour), false},
		{"user before her cut-off (the later of two lines wins)", "x", "alice", t0.Add(-30 * time.Minute), true},
		{"user after her cut-off", "x", "alice", t0.Add(time.Minute), false},
		{"listed session", sid, "bob", t0, true},
		{"listed bearer reference", BearerReference("sa-token"), "bob", t0, true},
		{"another bearer reference", BearerReference("other"), "bob", t0, false},
	}
	for _, tc := range cases {
		if got := l.Revoked(tc.sid, tc.user, tc.issued); got != tc.want {
			t.Errorf("%s: Revoked = %v, want %v", tc.name, got, tc.want)
		}
	}
	for name, data := range map[string]map[string]string{
		"bad notBefore": {"notBefore": "yesterday"},
		"bad user line": {"users": "alice"},
		"bad user time": {"users": "alice tomorrow"},
		"bad session":   {"sessions": "not-hex"},
		"bad bearer":    {"sessions": "bearer-" + sid},
	} {
		if _, err := ParseRevocationList(data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if l, err := ParseRevocationList(nil); err != nil || l.Revoked("x", "y", t0) {
		t.Fatalf("an absent list revokes nothing: %v", err)
	}
}

func TestRevocationsWatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cs := fake.NewClientset()
	cms := cs.CoreV1().ConfigMaps("team-a")
	r := NewRevocations(cms, "pgtoolbox-console-revoked", slog.New(slog.DiscardHandler))
	r.backoff = func(int) time.Duration { return 10 * time.Millisecond }
	if r.Synced() {
		t.Fatal("synced before reading")
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	select {
	case <-r.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("an absent ConfigMap must count as an empty, synced list")
	}
	sid := strings.Repeat("cd", 16)
	if _, err := cms.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-console-revoked", Namespace: "team-a"},
		Data:       map[string]string{"sessions": sid},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return r.Revoked(sid, "u", time.Now()) })

	// A broken edit keeps the previous list in force.
	if _, err := cms.Update(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-console-revoked", Namespace: "team-a"},
		Data:       map[string]string{"sessions": "garbage"},
	}, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !r.Revoked(sid, "u", time.Now()) {
		t.Fatal("a refused list replaced the good one")
	}
	if err := cms.Delete(ctx, "pgtoolbox-console-revoked", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !r.Revoked(sid, "u", time.Now()) })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRevocationsDisabled(t *testing.T) {
	t.Parallel()
	r := NewRevocations(nil, "", nil)
	if !r.Synced() || r.Enabled() || r.Revoked("x", "y", time.Now()) {
		t.Fatal("a disabled list must be synced and revoke nothing")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeAPIServer answers SelfSubjectReviews for one token.
func fakeAPIServer(t *testing.T, token string, user authenticationv1.UserInfo) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/apis/authentication.k8s.io/v1/selfsubjectreviews" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Impersonate-User") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`))
			return
		}
		_ = json.NewEncoder(w).Encode(authenticationv1.SelfSubjectReview{
			TypeMeta: metav1.TypeMeta{Kind: "SelfSubjectReview", APIVersion: "authentication.k8s.io/v1"},
			Status:   authenticationv1.SelfSubjectReviewStatus{UserInfo: user},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestSelfSubjectReviewerAsUser(t *testing.T) {
	t.Parallel()
	srv, _ := fakeAPIServer(t, "user-token", authenticationv1.UserInfo{
		Username: "oidc:jane", UID: "u1", Groups: []string{"dba"},
		Extra: map[string]authenticationv1.ExtraValue{"scopes": {"a"}},
	})
	// The console's own configuration carries its ServiceAccount token
	// and an impersonation setting; neither may reach the user's calls.
	base := &rest.Config{
		Host: srv.URL, BearerToken: "console-sa-token",
		Impersonate:     rest.ImpersonationConfig{UserName: "system:admin"},
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
	}
	f, err := NewClientFactory(base)
	if err != nil {
		t.Fatal(err)
	}
	u, err := NewSelfSubjectReviewer(f).Review(t.Context(), "user-token")
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "oidc:jane" || u.UID != "u1" || u.Groups[0] != "dba" || u.Extra["scopes"][0] != "a" {
		t.Fatalf("user = %+v", u)
	}
	if _, err := NewSelfSubjectReviewer(f).Review(t.Context(), "other"); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("an unknown token = %v, want ErrTokenRejected", err)
	}
	// Without a user token in the context the client refuses to send.
	_, err = f.Client().AuthenticationV1().SelfSubjectReviews().Create(t.Context(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if !errors.Is(err, ErrNoUserToken) {
		t.Fatalf("a call without a user token = %v, want ErrNoUserToken", err)
	}
}

type countingReviewer struct {
	calls int
	err   error
}

func (c *countingReviewer) Review(context.Context, string) (authz.User, error) {
	c.calls++
	if c.err != nil {
		return authz.User{}, c.err
	}
	return authz.User{Name: "jane"}, nil
}

func TestCachedReviewer(t *testing.T) {
	t.Parallel()
	next := &countingReviewer{}
	c := NewCachedReviewer(next, time.Second, 2)
	now := time.Now()
	c.now = func() time.Time { return now }
	for range 3 {
		if _, err := c.Review(t.Context(), "t1"); err != nil {
			t.Fatal(err)
		}
	}
	if next.calls != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", next.calls)
	}
	now = now.Add(time.Second)
	_, _ = c.Review(t.Context(), "t1")
	if next.calls != 2 {
		t.Fatalf("calls = %d after TTL, want 2", next.calls)
	}
	c.Forget("t1")
	_, _ = c.Review(t.Context(), "t1")
	if next.calls != 3 {
		t.Fatalf("calls = %d after Forget, want 3", next.calls)
	}
	next.err = ErrTokenRejected
	now = now.Add(2 * time.Second)
	for range 2 {
		if _, err := c.Review(t.Context(), "t1"); !errors.Is(err, ErrTokenRejected) {
			t.Fatal(err)
		}
	}
	if next.calls != 5 {
		t.Fatalf("calls = %d: a rejection must never be cached", next.calls)
	}
	for k := range c.entries {
		if strings.Contains(string(k[:]), "t1") {
			t.Fatal("the token is a cache key")
		}
	}
}
