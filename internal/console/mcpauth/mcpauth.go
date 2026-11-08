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

// Package mcpauth authenticates the console's MCP endpoint:
// `Authorization: Bearer <token>` on every request, never the session
// cookie, resolved by an Authenticator to the person it acts for, with
// that person's rights.
//
// Two kinds of bearer. A console grant (the console's own OAuth flow,
// internal/console/mcpoauth) is the first Authenticator of
// the Chain. A raw API-server token (RawToken) is accepted only where the
// break-glass token sign-in is on and is checked exactly like a pasted
// token: the API server says whose it is (SelfSubjectReview) and the
// revocation list applies. The tools never see which kind authenticated
// the call.
//
// Guard wraps the endpoint: it refuses a missing or rejected bearer with
// 401 and a WWW-Authenticate challenge naming the RFC 9728 protected
// resource metadata (MetadataHandler), spends the per-address and
// per-user budgets, and hands the request on with the auth.Principal in
// its context.
package mcpauth

import (
	"context"
	"errors"
	"slices"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// Authenticator resolves a bearer token to the person it acts for.
//
// It returns ErrNotMine for a token it does not handle (so a Chain asks
// the next one), an error wrapping ErrUnauthenticated (or one of the
// auth package's token errors) for a token it handles and refuses, and
// any other error when it could not decide (the API server could not be
// asked): the request then fails with 503, never as anonymous.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*auth.Principal, error)
}

// ErrNotMine means the token is not of this authenticator's kind.
var ErrNotMine = errors.New("not a token this authenticator handles")

// ErrUnauthenticated means the token is of this authenticator's kind and is
// refused (expired, revoked, unknown).
var ErrUnauthenticated = errors.New("the bearer token is not accepted")

// AuthenticatorFunc adapts a function.
type AuthenticatorFunc func(ctx context.Context, token string) (*auth.Principal, error)

// Authenticate implements Authenticator.
func (f AuthenticatorFunc) Authenticate(ctx context.Context, token string) (*auth.Principal, error) {
	return f(ctx, token)
}

// Chain asks each Authenticator in turn; the first that does not answer
// ErrNotMine decides. A token nobody claims is unauthenticated.
type Chain []Authenticator

// Authenticate implements Authenticator.
func (c Chain) Authenticate(ctx context.Context, token string) (*auth.Principal, error) {
	for _, a := range c {
		if a == nil {
			continue
		}
		p, err := a.Authenticate(ctx, token)
		if errors.Is(err, ErrNotMine) {
			continue
		}
		return p, err
	}
	return nil, ErrNotMine
}

// BearerChecker is the auth.Handler slice RawToken uses.
type BearerChecker interface {
	TokenLoginEnabled() bool
	BearerPrincipal(ctx context.Context, token string) (*auth.Principal, error)
}

// RawToken accepts raw API-server tokens (Kubernetes ServiceAccount or
// OIDC tokens the API server trusts) while token sign-in is on: the
// break-glass path. With token sign-in off every token is
// ErrNotMine, so a Chain can still accept its own kind.
type RawToken struct{ Checker BearerChecker }

// Authenticate implements Authenticator.
func (r RawToken) Authenticate(ctx context.Context, token string) (*auth.Principal, error) {
	if r.Checker == nil || !r.Checker.TokenLoginEnabled() {
		return nil, ErrNotMine
	}
	return r.Checker.BearerPrincipal(ctx, token)
}

// refused reports whether err is a refusal of the token (401) rather
// than a failure to decide (503).
func refused(err error) bool {
	return errors.Is(err, ErrNotMine) || errors.Is(err, ErrUnauthenticated) ||
		errors.Is(err, auth.ErrTokenRejected) || errors.Is(err, auth.ErrBearerRevoked) ||
		errors.Is(err, auth.ErrTokenLoginDisabled) || errors.Is(err, auth.ErrMalformedBearer) ||
		errors.Is(err, auth.ErrBearerExpired)
}

// reason is the closed, display-safe description of a refusal, for the
// challenge's error_description and the audit line.
func reason(err error) string {
	switch {
	case errors.Is(err, auth.ErrBearerRevoked):
		return "revoked"
	case errors.Is(err, auth.ErrBearerExpired):
		return "expired"
	case errors.Is(err, auth.ErrTokenRejected):
		return "rejected by the API server"
	case errors.Is(err, auth.ErrMalformedBearer):
		return "malformed"
	case errors.Is(err, ErrNotMine), errors.Is(err, auth.ErrTokenLoginDisabled):
		return "not a token this console accepts"
	default:
		return "not accepted"
	}
}

// The scopes of a console grant.
const (
	// ScopeRead is every read tool.
	ScopeRead = "mcp:read"
	// ScopeOperate is the write tools, on top of the person's own RBAC.
	ScopeOperate = "mcp:operate"
)

// HasScope reports whether the request's principal may use a tool that
// needs scope. A console grant carries what the person consented to on
// the consent page; any other principal (a raw API-server token, the
// break-glass path) is bounded by the person's RBAC alone. A nil principal
// has no scope.
func HasScope(p *auth.Principal, scope string) bool {
	if p == nil {
		return false
	}
	if p.Provider != auth.GrantProvider {
		return true
	}
	return slices.Contains(p.Scopes, scope)
}
