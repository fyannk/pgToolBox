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

package mcpoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// Client registration. Only public clients are admitted (no secret: a
// native program cannot keep one, RFC 8252 §8.5) with loopback redirect
// URIs, which a code can reach only on the person's own computer.
//
// What the clients send (2026-11, their documentation): Claude Code
// registers http://localhost:<port>/callback by dynamic registration or
// a client ID metadata document
// (https://code.claude.com/docs/en/mcp, "OAuth"); Codex registers
// http://127.0.0.1:<port>/callback, with a per-server suffix
// (/callback/<id>) unless the server sends iss, by CIMD or DCR
// (https://learn.chatgpt.com/docs/extend/mcp?surface=cli); OpenCode
// registers http://127.0.0.1:19876/mcp/oauth/callback by DCR
// (https://opencode.ai/docs/mcp-servers). None needs a private-use URI
// scheme, so none is admitted: a custom scheme can be claimed by any
// installed application, a loopback port only by a process of the
// person's own machine.

const (
	clientIDPrefix   = "pgc_cl_"
	purposeClient    = "mcp-client"
	maxRedirectURIs  = 8
	maxRedirectURI   = 512
	defaultName      = "an unnamed application"
	maxClientIDBytes = 2048
)

// client is a resolved client.
type client struct {
	ID   string
	Name string
	// URL is the client ID metadata document's URL ("" for a dynamic
	// registration).
	URL          string
	RedirectURIs []string
}

// registration is the sealed payload of a dynamically registered client ID.
type registration struct {
	Name         string   `json:"n"`
	RedirectURIs []string `json:"r"`
	IssuedAt     int64    `json:"t"`
}

// registerRequest is the RFC 7591 §2 metadata this server reads.
type registerRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod *string  `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	Scope                   string   `json:"scope"`
	ApplicationType         string   `json:"application_type"`
}

// registerResponse is the RFC 7591 §3.2.1 answer: no secret.
type registerResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	ApplicationType         string   `json:"application_type"`
}

// metaError is a refused registration: the RFC 7591 §3.2.2 error code
// (invalid_client_metadata or invalid_redirect_uri) and a description.
type metaError struct{ code, desc string }

func (e *metaError) Error() string { return e.code + ": " + e.desc }

func metadataError(format string, args ...any) error {
	return &metaError{code: "invalid_client_metadata", desc: fmt.Sprintf(format, args...)}
}

func redirectError(format string, args ...any) error {
	return &metaError{code: "invalid_redirect_uri", desc: fmt.Sprintf(format, args...)}
}

// checkMetadata validates what a client declares about itself, from a
// registration or a client ID metadata document, and returns its name
// and redirect URIs.
func checkMetadata(req registerRequest) (string, []string, error) {
	if m := req.TokenEndpointAuthMethod; m != nil && *m != "none" {
		return "", nil, metadataError("token_endpoint_auth_method %q: only public clients (none) are accepted; this console issues no client secret", *m)
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return "", nil, metadataError("grant type %q is not supported (authorization_code and refresh_token only)", g)
		}
	}
	if len(req.GrantTypes) > 0 && !slices.Contains(req.GrantTypes, "authorization_code") {
		return "", nil, metadataError("grant_types must include authorization_code")
	}
	for _, t := range req.ResponseTypes {
		if t != "code" {
			return "", nil, metadataError("response type %q is not supported (code only)", t)
		}
	}
	if req.ApplicationType != "" && req.ApplicationType != "native" {
		return "", nil, metadataError("application_type %q: only native applications with loopback redirect URIs are accepted", req.ApplicationType)
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > maxRedirectURIs {
		return "", nil, redirectError("between 1 and %d redirect URIs are required", maxRedirectURIs)
	}
	// A client that also registers URIs this console does not admit (VS
	// Code adds https://vscode.dev/redirect beside its loopback one) is
	// registered with its loopback URIs only: the response lists what was
	// kept (RFC 7591 §3.2.1 lets the server replace metadata values). A
	// registration with no loopback URI at all is refused.
	uris := make([]string, 0, len(req.RedirectURIs))
	var first error
	for _, u := range req.RedirectURIs {
		if _, err := parseLoopback(u); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if !slices.Contains(uris, u) {
			uris = append(uris, u)
		}
	}
	if len(uris) == 0 {
		return "", nil, first
	}
	return cleanName(req.ClientName), uris, nil
}

// cleanName keeps a printable, bounded display name: no control and no
// format characters (bidirectional overrides, zero-width spaces and
// joiners: a name that displays other than it reads).
func cleanName(n string) string {
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.TrimSpace(n))
	n = strings.Join(strings.Fields(n), " ")
	if n == "" {
		return defaultName
	}
	return sharedstate.TruncateClientName(n)
}

// parseLoopback admits a loopback redirect URI (RFC 8252 §7.3): http, the
// host 127.0.0.1, [::1] or localhost, any port, no user information or
// fragment, no empty query ("?" alone, which would never match itself).
func parseLoopback(raw string) (*url.URL, error) {
	if len(raw) > maxRedirectURI {
		return nil, redirectError("a redirect URI is longer than %d characters", maxRedirectURI)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") ||
		u.ForceQuery || strings.HasPrefix(u.EscapedPath(), "//") {
		// A path starting with // reads as another host to a careless
		// client or log reader; no client registers one.
		return nil, redirectError("%q is not an http loopback URI (http://127.0.0.1:<port>/..., http://localhost:<port>/... or http://[::1]:<port>/...)", raw)
	}
	switch strings.ToLower(u.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
	default:
		return nil, redirectError("%q does not point at this computer: only loopback redirect URIs are accepted", raw)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return nil, redirectError("%q has an invalid port", raw)
		}
	}
	return u, nil
}

// matchRedirect finds the registered URI given matches: identical but for
// the port, which a native client picks when it starts listening (RFC
// 8252 §7.3).
func matchRedirect(registered []string, given string) (string, bool) {
	g, err := parseLoopback(given)
	if err != nil {
		return "", false
	}
	for _, r := range registered {
		u, err := parseLoopback(r)
		if err != nil {
			continue
		}
		if strings.EqualFold(u.Hostname(), g.Hostname()) && u.EscapedPath() == g.EscapedPath() && u.RawQuery == g.RawQuery {
			return given, true
		}
	}
	return "", false
}

// handleRegister is dynamic client registration (RFC 7591).
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	client := auth.ClientOf(r, s.o.TrustedProxyHops)
	if !s.registry.Allow(auth.ClientKey(r, s.o.TrustedProxyHops)) {
		requests.WithLabelValues("register", "throttled").Inc()
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "slow_down", "too many registrations from this address; wait a minute")
		return
	}
	var req registerRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRegisterBytes))
	if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		requests.WithLabelValues("register", "invalid").Inc()
		writeError(w, http.StatusBadRequest, "invalid_client_metadata", "the body is not one JSON object of client metadata")
		return
	}
	name, uris, err := checkMetadata(req)
	if err != nil {
		var me *metaError
		_ = errors.As(err, &me)
		requests.WithLabelValues("register", "refused").Inc()
		s.audit.Record(r.Context(), client, auth.AuditMCPRegister, "outcome", "refused", "reason", me.desc)
		writeError(w, http.StatusBadRequest, me.code, me.desc)
		return
	}
	now := s.o.Now().UTC()
	sealed, err := s.codec.SealPurpose(purposeClient, registration{Name: name, RedirectURIs: uris, IssuedAt: now.Unix()})
	if err != nil {
		s.o.Log.Error("sealing a client registration failed", "error", err)
		writeError(w, http.StatusInternalServerError, "server_error", "the registration could not be sealed")
		return
	}
	requests.WithLabelValues("register", "ok").Inc()
	s.audit.Record(r.Context(), client, auth.AuditMCPRegister, "outcome", "registered", "client", name, "redirect_hosts", redirectHosts(uris))
	writeJSON(w, http.StatusCreated, registerResponse{
		ClientID: clientIDPrefix + sealed, ClientIDIssuedAt: now.Unix(), ClientName: name, RedirectURIs: uris,
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		TokenEndpointAuthMethod: "none", ApplicationType: "native",
	})
}

// redirectHosts lists the host:port of each URI, for the audit line.
func redirectHosts(uris []string) []string {
	out := make([]string, 0, len(uris))
	for _, u := range uris {
		if p, err := url.Parse(u); err == nil {
			out = append(out, p.Host)
		}
	}
	return out
}

// errUnknownClient: the client ID is neither a registration of this
// console nor an admitted metadata document.
var errUnknownClient = errors.New("unknown client")

// resolveClient reads a client ID: a sealed registration, or (when
// enabled) the URL of a client ID metadata document.
func (s *Server) resolveClient(ctx context.Context, id string) (client, error) {
	switch {
	case id == "" || len(id) > maxClientIDBytes:
		return client{}, errUnknownClient
	case strings.HasPrefix(id, clientIDPrefix):
		var reg registration
		if err := s.codec.OpenPurpose(purposeClient, strings.TrimPrefix(id, clientIDPrefix), &reg); err != nil {
			return client{}, fmt.Errorf("%w: not a registration of this console (or of a session key it no longer holds); register again", errUnknownClient)
		}
		if len(reg.RedirectURIs) == 0 {
			return client{}, errUnknownClient
		}
		return client{ID: id, Name: cleanName(reg.Name), RedirectURIs: reg.RedirectURIs}, nil
	case strings.HasPrefix(id, "https://"):
		if !s.o.ClientMetadataDocuments || s.o.Fetcher == nil {
			return client{}, fmt.Errorf("%w: this console does not accept client ID metadata documents; register dynamically", errUnknownClient)
		}
		doc, err := s.o.Fetcher.Fetch(ctx, id)
		if err != nil {
			return client{}, fmt.Errorf("%w: %w", errUnknownClient, err)
		}
		return client{ID: id, Name: doc.Name, URL: id, RedirectURIs: doc.RedirectURIs}, nil
	}
	return client{}, errUnknownClient
}
