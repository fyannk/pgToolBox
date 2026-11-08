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
	"log/slog"
	"net"
	"net/http"
	"strings"
	"unicode"
)

// Audit event names: one structured line per security-relevant event,
// always with the attribute audit=<event> so the trail can be filtered
// out of the rest of the log. No line ever carries a token, a cookie value
// or a client secret; the session ID is a reference, not a credential.
const (
	AuditLoginSuccess = "login.success"
	AuditLoginFailure = "login.failure"
	AuditAccessDenied = "access.denied"
	AuditLogout       = "logout"
	AuditRevoked      = "session.revoked"
	// AuditMCPCall is one MCP tool call: user, tool, the
	// arguments' names, outcome and size.
	AuditMCPCall = "mcp.call"
	// AuditMCPDenied is a request to /mcp refused before any tool ran:
	// no or a rejected bearer, a revoked one, or a spent budget.
	AuditMCPDenied = "mcp.denied"
	// AuditMCPRegister is an MCP client's registration (RFC 7591), or
	// its refusal.
	AuditMCPRegister = "mcp.register"
	// AuditMCPGrant is a consent given (a console grant minted for an MCP
	// client) or refused, with the grant ID, the client and the scope.
	AuditMCPGrant = "mcp.grant"
	// AuditMCPToken is a grant's refresh (refresh_token grant), or its
	// refusal.
	AuditMCPToken = "mcp.token"
	// AuditMCPRevoke is a grant revoked by its owner.
	AuditMCPRevoke = "mcp.revoke"
	// AuditMCPConfirm is a person's answer on an MCP write tool's
	// confirmation page (approved or declined), with the confirmation ID
	// the tool's mcp.call and operation lines carry.
	AuditMCPConfirm = "mcp.confirm"
)

// maxUserAgent bounds the client-controlled user agent in a line.
const maxUserAgent = 256

// Auditor writes the audit trail.
type Auditor struct {
	Logger *slog.Logger
	// TrustedProxyHops is how many proxies append to X-Forwarded-For.
	TrustedProxyHops int
}

// Log writes one audit line for r. attrs are slog key/value pairs and must
// never hold secret material.
func (a *Auditor) Log(ctx context.Context, r *http.Request, event string, attrs ...any) {
	if a == nil || a.Logger == nil {
		return
	}
	a.Record(ctx, ClientOf(r, a.TrustedProxyHops), event, attrs...)
}

// Client is who sent a request, as the audit trail names them.
type Client struct {
	IP        string
	UserAgent string
}

// ClientOf is r's client, honouring the trusted proxy hops; the user
// agent is bounded.
func ClientOf(r *http.Request, trustedProxyHops int) Client {
	ua := r.UserAgent()
	if len(ua) > maxUserAgent {
		ua = ua[:maxUserAgent]
	}
	return Client{IP: clientIP(r, trustedProxyHops), UserAgent: ua}
}

// Record writes one audit line for a request that is no longer at hand
// (an MCP tool call runs after its HTTP request was decoded). attrs are
// slog key/value pairs and must never hold secret material.
func (a *Auditor) Record(ctx context.Context, c Client, event string, attrs ...any) {
	if a == nil || a.Logger == nil {
		return
	}
	line := []any{"audit", event, "client_ip", LogSafe(c.IP), "user_agent", LogSafe(c.UserAgent)}
	for _, v := range attrs {
		line = append(line, auditValue(v))
	}
	level := slog.LevelInfo
	if event == AuditLoginFailure || event == AuditAccessDenied || event == AuditRevoked || event == AuditMCPDenied {
		level = slog.LevelWarn
	}
	a.Logger.Log(ctx, level, "audit "+event, line...)
}

// auditValue is v with CR and LF escaped in every string it carries: an
// audit line names values a client chose (a user agent, a redirect host,
// a name read back from a sealed cookie), and none of them may start a
// forged line whatever handler writes the trail. Numbers, booleans,
// durations and times keep their kind; anything else is logged as its
// escaped text.
func auditValue(v any) any {
	switch x := v.(type) {
	case string:
		return LogSafe(x)
	case []string:
		out := make([]string, len(x))
		for i := range x {
			out[i] = LogSafe(x[i])
		}
		return out
	case slog.Attr:
		return slog.Attr{Key: LogSafe(x.Key), Value: auditSlogValue(x.Value)}
	}
	return auditSlogValue(slog.AnyValue(v))
}

// auditSlogValue rebuilds v from its kind: scalars are copied, and every
// other kind (a string, an error, a group, a struct) becomes its escaped
// text.
func auditSlogValue(v slog.Value) slog.Value {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindInt64:
		return slog.Int64Value(v.Int64())
	case slog.KindUint64:
		return slog.Uint64Value(v.Uint64())
	case slog.KindFloat64:
		return slog.Float64Value(v.Float64())
	case slog.KindBool:
		return slog.BoolValue(v.Bool())
	case slog.KindDuration:
		return slog.DurationValue(v.Duration())
	case slog.KindTime:
		return slog.TimeValue(v.Time())
	case slog.KindString, slog.KindAny, slog.KindGroup, slog.KindLogValuer:
	}
	return slog.StringValue(LogSafe(v.String()))
}

// LogSafe escapes CR and LF (and replaces other control characters) in a
// value a client could have chosen before it is logged, so that a text
// handler cannot be made to write a forged line. The JSON handler the
// console runs escapes them too; this holds whatever the handler is.
func LogSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		if r != '\r' && r != '\n' && unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", `\r`), "\n", `\n`)
}

// clientIP returns the client's address: the
// direct peer, or behind N trusted reverse proxies the entry N positions
// from the right of X-Forwarded-For. Entries further left were written by
// the client and are never read; a chain shorter than N, or an entry that
// is not an address, yields the peer.
func clientIP(r *http.Request, hops int) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	if hops <= 0 {
		return peer
	}
	var chain []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for part := range strings.SplitSeq(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				chain = append(chain, part)
			}
		}
	}
	if len(chain) < hops {
		return peer
	}
	candidate := chain[len(chain)-hops]
	if host, _, err := net.SplitHostPort(candidate); err == nil {
		candidate = host
	}
	ip := net.ParseIP(strings.Trim(candidate, "[]"))
	if ip == nil {
		return peer
	}
	return ip.String()
}
