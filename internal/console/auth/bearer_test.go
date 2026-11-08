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
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// unsignedJWT is a JWT-shaped token the fake API server accepts ("eyJ*");
// the console reads iat/exp only after the API server said yes.
func unsignedJWT(iat, exp int64) string {
	return b64url([]byte(`{"alg":"RS256"}`)) + "." +
		b64url([]byte(`{"iat":`+strconv.FormatInt(iat, 10)+`,"exp":`+strconv.FormatInt(exp, 10)+`}`)) + ".c2ln"
}

func revoke(env *testEnv, data map[string]string) {
	rev := NewRevocations(nil, "x", nil)
	rev.log = env.h.log
	rev.apply(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x"}, Data: data})
	env.h.revocations = rev
}

func TestBearerPrincipal(t *testing.T) {
	t.Parallel()
	iat := time.Now().Add(-time.Hour).Truncate(time.Second).UTC()
	exp := iat.Add(2 * time.Hour)
	before, after := iat.Add(-24*time.Hour).Format(time.RFC3339), iat.Add(30*time.Minute).Format(time.RFC3339)
	jwt := unsignedJWT(iat.Unix(), exp.Unix())

	t.Run("token sign-in off refuses every bearer", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, ProviderOIDC)
		if env.h.TokenLoginEnabled() {
			t.Fatal("token sign-in reported on")
		}
		if _, err := env.h.BearerPrincipal(t.Context(), "sa-token"); !errors.Is(err, ErrTokenLoginDisabled) {
			t.Fatalf("err = %v", err)
		}
		if env.rev.calls != 0 {
			t.Error("the API server was asked about a token while token sign-in is off")
		}
	})

	t.Run("an accepted ServiceAccount token", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, ProviderToken)
		p, err := env.h.BearerPrincipal(t.Context(), "sa-token")
		if err != nil {
			t.Fatal(err)
		}
		if p.User.Name != "system:serviceaccount:team-a:pgtoolbox-console-breakglass" || p.Provider != BearerProvider {
			t.Errorf("principal = %+v", p)
		}
		if p.SessionID != BearerReference("sa-token") || strings.Contains(p.SessionID, "sa-token") {
			t.Errorf("session reference = %q", p.SessionID)
		}
		if !p.IssuedAt.IsZero() || !p.Expiry.After(time.Now()) {
			t.Errorf("an opaque token's times: issued %v, expiry %v", p.IssuedAt, p.Expiry)
		}
	})

	t.Run("a JWT's iat and exp are read", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, ProviderToken)
		p, err := env.h.BearerPrincipal(t.Context(), "Bearer "+jwt)
		if err != nil {
			t.Fatal(err)
		}
		if !p.IssuedAt.Equal(iat) || !p.Expiry.Equal(exp) || !p.TokenExpiry.Equal(exp) {
			t.Errorf("issued %v expiry %v", p.IssuedAt, p.Expiry)
		}
	})

	t.Run("an expired JWT is refused even while its review is cached", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, ProviderToken)
		past := unsignedJWT(time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-time.Second).Unix())
		if _, err := env.h.BearerPrincipal(t.Context(), past); !errors.Is(err, ErrBearerExpired) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("signing out of a session refuses its token as a bearer", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, ProviderToken)
		b := env.browser(t)
		signIn(t, b)
		if _, err := env.h.BearerPrincipal(t.Context(), "sa-token"); err != nil {
			t.Fatalf("before the sign-out: %v", err)
		}
		if w := b.do(http.MethodPost, "/logout", url.Values{}, sameOriginForm); w.Code != http.StatusOK {
			t.Fatal(w.Code)
		}
		if _, err := env.h.BearerPrincipal(t.Context(), "sa-token"); !errors.Is(err, ErrBearerRevoked) {
			t.Errorf("after the sign-out: %v", err)
		}
	})

	t.Run("rejected and malformed tokens", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, ProviderToken)
		if _, err := env.h.BearerPrincipal(t.Context(), "nope"); !errors.Is(err, ErrTokenRejected) {
			t.Errorf("unknown token: %v", err)
		}
		if _, err := env.h.BearerPrincipal(t.Context(), "a b"); !errors.Is(err, ErrMalformedBearer) {
			t.Errorf("two words: %v", err)
		}
	})

	t.Run("an API server outage is not a refusal", func(t *testing.T) {
		t.Parallel()
		env := newTestEnv(t, ProviderToken)
		env.rev.down = true
		_, err := env.h.BearerPrincipal(t.Context(), "sa-token")
		if err == nil || errors.Is(err, ErrTokenRejected) {
			t.Errorf("err = %v", err)
		}
	})

	revocations := []struct {
		name    string
		token   string
		data    map[string]string
		revoked bool
	}{
		{"listed by reference", "sa-token", map[string]string{"sessions": BearerReference("sa-token")}, true},
		{"another reference", "sa-token", map[string]string{"sessions": BearerReference("other")}, false},
		{
			"user revoked, opaque token", "sa-token",
			map[string]string{"users": "system:serviceaccount:team-a:pgtoolbox-console-breakglass 2020-01-01T00:00:00Z"},
			true,
		},
		{"notBefore, opaque token", "sa-token", map[string]string{"notBefore": "2020-01-01T00:00:00Z"}, true},
		{"notBefore before the JWT's iat", jwt, map[string]string{"notBefore": before}, false},
		{"notBefore after the JWT's iat", jwt, map[string]string{"notBefore": after}, true},
		{"user revoked after the JWT's iat", jwt, map[string]string{"users": "oidc:jane@corp.example " + after}, true},
	}
	for _, c := range revocations {
		t.Run("revocation: "+c.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv(t, ProviderToken)
			revoke(env, c.data)
			_, err := env.h.BearerPrincipal(t.Context(), c.token)
			if got := errors.Is(err, ErrBearerRevoked); got != c.revoked {
				t.Errorf("revoked = %v (err %v), want %v", got, err, c.revoked)
			}
		})
	}
}

func TestBearerTimes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		token    string
		iat, exp int64
	}{
		{"jwt", unsignedJWT(1700000000, 1700003600), 1700000000, 1700003600},
		{"opaque", "sha256~abcdef", 0, 0},
		{"bad payload", "eyJhbGciOiJub25lIn0.!!!.sig", 0, 0},
		{"not json", "a." + b64url([]byte("nope")) + ".c", 0, 0},
		{"negative", "a." + b64url([]byte(`{"iat":-5,"exp":"x"}`)) + ".c", 0, 0},
	} {
		issued, expiry := bearerTimes(c.token)
		if (c.iat == 0) != issued.IsZero() || (c.iat != 0 && issued.Unix() != c.iat) {
			t.Errorf("%s: issued = %v", c.name, issued)
		}
		if (c.exp == 0) != expiry.IsZero() || (c.exp != 0 && expiry.Unix() != c.exp) {
			t.Errorf("%s: expiry = %v", c.name, expiry)
		}
	}
}
