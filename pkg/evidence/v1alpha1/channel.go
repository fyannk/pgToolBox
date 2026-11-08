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

package v1alpha1

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Agent channel contract shared by the evidence agent, the operator that
// deploys it, and the console that reads it. A party must use these values
// instead of re-declaring them, so one change here moves every side together.
//
// Transport: HTTPS inside the cluster,
// TLS 1.3, the server certificate verified against EvidenceAgent.status.caBundle
// with ServerName(service, namespace); the client presents a bound
// ServiceAccount token projected with audience TokenAudience(namespace) — one
// token per namespace, read from TokenPath(directory, namespace); the agent
// checks it with a TokenReview for its own namespace's audience only and
// authorizes it with a SubjectAccessReview "get evidenceagents/<its own name>"
// in its namespace.
const (
	// MediaType is the Content-Type of every evidence API response.
	MediaType = "application/vnd.pgtoolbox.evidence.v1alpha1+json"

	// HealthPath and ReadinessPath are unauthenticated (kubelet probes cannot
	// present a bound token) and reveal nothing but liveness and readiness.
	HealthPath    = "/healthz"
	ReadinessPath = "/readyz"
	// Route paths of the closed, authenticated, GET-only evidence route set.
	SnapshotPath      = "/api/v1alpha1/snapshot"
	BackupsPath       = "/api/v1alpha1/backups"
	WALRangesPath     = "/api/v1alpha1/wal-ranges"
	WALGapsPath       = "/api/v1alpha1/wal-gaps"
	RecoveryPathsPath = "/api/v1alpha1/recovery-paths"

	// MaximumSnapshotBytes bounds one encoded snapshot response.
	MaximumSnapshotBytes = 256 * 1024
	// MaximumCollectionBytes bounds one encoded collection page.
	MaximumCollectionBytes = 1024 * 1024
	// DefaultPageSize is the page size selected by an absent or zero limit.
	DefaultPageSize = 100
	// MaximumPageSize is the largest accepted limit query value.
	MaximumPageSize = 200
	// MaximumCursorBytes bounds one opaque continuation cursor.
	MaximumCursorBytes = 4096
	// RequestTimeout is the hard deadline of every evidence request,
	// authentication included.
	RequestTimeout = 5 * time.Second

	// ProducerInstanceLength is the exact length of ProducerInstance.
	ProducerInstanceLength = 32

	// TokenAudiencePrefix starts every evidence-agent audience. The agent
	// of namespace N accepts only TokenAudience(N), "pgtoolbox-evidence-agent/N":
	// a token the console presents to the agents of one namespace is
	// useless against the agents of every other namespace, against the API
	// server, and against every other audience.
	// #nosec G101 -- an audience name, not a credential.
	TokenAudiencePrefix = ClusterWideTokenAudience + "/"
	// ClusterWideTokenAudience is the bare, namespace-less audience. No
	// agent accepts it, because one token for it would open every
	// namespace's agents; it is named only so the agent and the console can
	// say why such a token is refused (ErrorAudienceMismatch).
	// #nosec G101 -- an audience name, not a credential.
	ClusterWideTokenAudience = "pgtoolbox-evidence-agent"
	// MinimumTokenExpirationSeconds is the kubelet's floor for a projected
	// token; RecommendedTokenExpirationSeconds is what the console projects.
	MinimumTokenExpirationSeconds     = 600
	RecommendedTokenExpirationSeconds = 3600
	// DefaultTokenDirectory is where the console's packaging projects the
	// per-namespace tokens: TokenPath(DefaultTokenDirectory, N) is
	// "<directory>/N/token", the token of audience TokenAudience(N), one
	// per watched namespace. A consumer re-reads each file
	// (ReadBearerTokenFile) at least on every change: the kubelet rotates
	// them in place.
	// #nosec G101 -- a mount path, not a credential.
	DefaultTokenDirectory = "/var/run/secrets/pgtoolbox.fyannk.dev/evidence-agent"
	// TokenFileName is the file name of a namespace's token inside its
	// directory.
	// #nosec G101 -- a file name, not a credential.
	TokenFileName = "token"
	// MaximumBearerTokenBytes bounds one bound token, in a file or a header.
	MaximumBearerTokenBytes = 16 * 1024

	// AuthorizationGroup, AuthorizationResource and AuthorizationVerb are
	// the SubjectAccessReview resource attributes the agent checks, with its
	// own name and namespace: whoever may get the EvidenceAgent object may
	// read the evidence it publishes.
	AuthorizationGroup    = "pgtoolbox.fyannk.dev"
	AuthorizationResource = "evidenceagents"
	AuthorizationVerb     = "get"

	// AgentPort and AgentPortName are the Service and container port of the
	// HTTPS listener.
	AgentPort     = 8443
	AgentPortName = "https"
	// MinimumTLSVersion is the only protocol version either side offers.
	MinimumTLSVersion = tls.VersionTLS13
)

// Endpoint returns the in-cluster URL of the agent behind Service service
// in namespace namespace, as EvidenceAgent.status.endpoint publishes it.
func Endpoint(service, namespace string) string {
	return "https://" + ServerName(service, namespace) + ":" + strconv.Itoa(AgentPort)
}

// ServerName is the TLS server name a consumer verifies: the Service's
// namespace-qualified cluster DNS name, which the serving certificate
// carries as a DNS SAN.
func ServerName(service, namespace string) string {
	return service + "." + namespace + ".svc"
}

// ValidProducerInstance reports whether value is a well-formed
// ProducerInstance: exactly ProducerInstanceLength lower-case hex digits.
func ValidProducerInstance(value string) bool {
	if len(value) != ProducerInstanceLength {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// ValidProducerEpoch reports whether value is a well-formed ProducerEpoch.
func ValidProducerEpoch(value uint64) bool { return value >= 1 }

// ParseProducerEpoch parses the decimal epoch the operator renders into the
// agent's environment.
func ParseProducerEpoch(raw string) (uint64, error) {
	if raw == "" || len(raw) > 20 || strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, errors.New("producer epoch is invalid")
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || !ValidProducerEpoch(value) {
		return 0, errors.New("producer epoch is invalid")
	}
	return value, nil
}

// ErrInvalidBearerToken is the only error the bearer-token helpers return.
// It never embeds a path, a header, or the token.
var ErrInvalidBearerToken = errors.New("evidence bearer token is invalid")

// BearerToken is a syntactically valid bound ServiceAccount token (a JWS
// compact serialization of three non-empty base64url segments, at most
// MaximumBearerTokenBytes) or, from the shared-token parsers, an opaque
// shared token. Syntax only; the agent's TokenReview or its shared-token
// comparison decides validity. Its formatting, logging, and text encodings never reveal it.
type BearerToken struct {
	value string
}

// ParseBearerToken validates the content of a projected token file: one
// token with at most one trailing line feed.
func ParseBearerToken(content []byte) (BearerToken, error) {
	if len(content) > MaximumBearerTokenBytes+1 {
		return BearerToken{}, ErrInvalidBearerToken
	}
	value := strings.TrimSuffix(string(content), "\n")
	if !validCompactJWS(value) {
		return BearerToken{}, ErrInvalidBearerToken
	}
	return BearerToken{value: value}, nil
}

// ParseAuthorization extracts the bearer token from one Authorization
// header value: the case-insensitive scheme "Bearer", exactly one space, and
// a token ParseBearerToken would accept. Anything else is refused before a
// TokenReview is spent on it.
func ParseAuthorization(header string) (BearerToken, error) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || len(header) > len(scheme)+MaximumBearerTokenBytes ||
		!strings.EqualFold(header[:len(scheme)], scheme) {
		return BearerToken{}, ErrInvalidBearerToken
	}
	value := header[len(scheme):]
	if !validCompactJWS(value) {
		return BearerToken{}, ErrInvalidBearerToken
	}
	return BearerToken{value: value}, nil
}

// ReadBearerTokenFile loads one token from an absolute, clean path, resolving
// the projected volume's ..data symlinks deliberately. The read is bounded;
// errors never include the path or the content.
func ReadBearerTokenFile(path string) (BearerToken, error) {
	return readTokenFile(path, ParseBearerToken)
}

// readTokenFile is the bounded, symlink-resolving read both token kinds
// share; parse decides the syntax.
func readTokenFile(path string, parse func([]byte) (BearerToken, error)) (BearerToken, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return BearerToken{}, ErrInvalidBearerToken
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return BearerToken{}, ErrInvalidBearerToken
	}
	// #nosec G304 -- the path is deployment configuration, validated as
	// absolute and clean; the read is bounded and the content validated.
	file, err := os.Open(resolved)
	if err != nil {
		return BearerToken{}, ErrInvalidBearerToken
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return BearerToken{}, ErrInvalidBearerToken
	}
	content, err := io.ReadAll(io.LimitReader(file, MaximumBearerTokenBytes+2))
	if err != nil {
		return BearerToken{}, ErrInvalidBearerToken
	}
	return parse(content)
}

// MinimumSharedTokenBytes is the shortest shared token accepted.
//
// Shared tokens: the team chart (pgtools) runs the agent
// without the operator and without the cluster-scoped grant a TokenReview
// needs; the console and the agent then share one random token from a
// Secret both mount, and the agent compares what it receives with it. A
// shared token is opaque: any RFC 6750 token68 of MinimumSharedTokenBytes
// to MaximumBearerTokenBytes characters. The minimum refuses a hand-typed
// weak value; the chart generates 48 random alphanumerics.
const MinimumSharedTokenBytes = 32

// ParseSharedToken validates the content of a shared token file: one
// token68 with at most one trailing line ending.
func ParseSharedToken(content []byte) (BearerToken, error) {
	if len(content) > MaximumBearerTokenBytes+2 {
		return BearerToken{}, ErrInvalidBearerToken
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(content), "\n"), "\r")
	if !validToken68(value) {
		return BearerToken{}, ErrInvalidBearerToken
	}
	return BearerToken{value: value}, nil
}

// ParseSharedAuthorization extracts a shared token from one Authorization
// header value: the case-insensitive scheme "Bearer", exactly one space and
// a token ParseSharedToken would accept.
func ParseSharedAuthorization(header string) (BearerToken, error) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || len(header) > len(scheme)+MaximumBearerTokenBytes ||
		!strings.EqualFold(header[:len(scheme)], scheme) {
		return BearerToken{}, ErrInvalidBearerToken
	}
	value := header[len(scheme):]
	if !validToken68(value) {
		return BearerToken{}, ErrInvalidBearerToken
	}
	return BearerToken{value: value}, nil
}

// ReadSharedTokenFile loads a shared token from an absolute, clean path
// (a mounted Secret key), with the same bounds and silence as
// ReadBearerTokenFile.
func ReadSharedTokenFile(path string) (BearerToken, error) {
	return readTokenFile(path, ParseSharedToken)
}

// validToken68 accepts RFC 6750's b64token: ALPHA, DIGIT, "-", ".", "_",
// "~", "+", "/", then any number of trailing "=", within the shared-token
// length bounds.
func validToken68(value string) bool {
	if len(value) < MinimumSharedTokenBytes || len(value) > MaximumBearerTokenBytes {
		return false
	}
	body := strings.TrimRight(value, "=")
	if body == "" {
		return false
	}
	for index := 0; index < len(body); index++ {
		character := body[index]
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9':
		case strings.IndexByte("-._~+/", character) >= 0:
		default:
			return false
		}
	}
	return true
}

// Valid reports whether the token was produced by a parser of this package.
func (t BearerToken) Valid() bool { return t.value != "" }

// Value returns the raw token for a TokenReview. Never log the result.
func (t BearerToken) Value() string { return t.value }

// AuthorizationHeader returns the Authorization request header value. Only a
// consumer building a request may call it; never log the result.
func (t BearerToken) AuthorizationHeader() string {
	if !t.Valid() {
		return ""
	}
	return "Bearer " + t.value
}

// String returns "[REDACTED]", never the token, so fmt verbs such as %v and
// %s cannot print it.
func (BearerToken) String() string { return "[REDACTED]" }

// GoString returns "[REDACTED]", never the token, for the %#v verb.
func (BearerToken) GoString() string { return "[REDACTED]" }

// LogValue makes log/slog record "[REDACTED]" instead of the token.
func (BearerToken) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// MarshalText encodes the token as "[REDACTED]" in JSON and any other
// encoding that uses encoding.TextMarshaler, so a serialized value never
// carries it.
func (BearerToken) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }

// validCompactJWS accepts exactly three non-empty base64url segments joined by
// '.', the shape of every Kubernetes-issued ServiceAccount token.
func validCompactJWS(value string) bool {
	if value == "" || len(value) > MaximumBearerTokenBytes {
		return false
	}
	segments := 0
	length := 0
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character == '.':
			if length == 0 {
				return false
			}
			segments++
			length = 0
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9', character == '-', character == '_':
			length++
		default:
			return false
		}
	}
	return segments == 2 && length > 0
}

// ErrInvalidAudienceNamespace is returned for a namespace that is not a
// DNS-1123 label. It never embeds the input.
var ErrInvalidAudienceNamespace = errors.New("evidence token namespace is invalid")

// TokenAudience is the only audience the agents of namespace accept.
func TokenAudience(namespace string) (string, error) {
	if !validNamespace(namespace) {
		return "", ErrInvalidAudienceNamespace
	}
	return TokenAudiencePrefix + namespace, nil
}

// ParseTokenAudience returns the namespace of a per-namespace evidence-agent
// audience, and false for anything else (the bare cluster-wide audience
// included): exactly TokenAudiencePrefix followed by a DNS-1123 label.
func ParseTokenAudience(audience string) (string, bool) {
	namespace, found := strings.CutPrefix(audience, TokenAudiencePrefix)
	if !found || !validNamespace(namespace) {
		return "", false
	}
	return namespace, true
}

// TokenPath is the file of namespace's token under directory:
// "<directory>/<namespace>/token". directory must be absolute and clean.
func TokenPath(directory, namespace string) (string, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", errors.New("evidence token directory must be absolute and clean")
	}
	if !validNamespace(namespace) {
		return "", ErrInvalidAudienceNamespace
	}
	return filepath.Join(directory, namespace, TokenFileName), nil
}

// AudienceClaim classifies the unverified "aud" claim of a bearer token
// against one agent's namespace. It is a diagnostic: the TokenReview, never
// this function, decides whether a token is accepted.
type AudienceClaim int

const (
	// AudienceUnknown means the claim names no evidence-agent audience, or the
	// token's payload is not readable JSON.
	AudienceUnknown AudienceClaim = iota
	// AudienceOwn means the claim names this namespace's audience.
	AudienceOwn
	// AudienceOtherNamespace means the claim names another namespace's audience.
	AudienceOtherNamespace
	// AudienceClusterWide means the claim names the bare cluster-wide audience.
	AudienceClusterWide
)

// maximumAudienceClaims bounds how many "aud" values are examined.
const maximumAudienceClaims = 16

// ClassifyAudienceClaim reads the token's payload segment without verifying
// its signature and classifies its "aud" claim for namespace. A token that
// names several audiences is classified by the strongest match: own, then
// another namespace, then cluster-wide. Never use the result to accept a token.
func ClassifyAudienceClaim(token BearerToken, namespace string) AudienceClaim {
	if !token.Valid() || !validNamespace(namespace) {
		return AudienceUnknown
	}
	audiences, ok := unverifiedAudiences(token.value)
	if !ok {
		return AudienceUnknown
	}
	result := AudienceUnknown
	for _, audience := range audiences {
		switch other, perNamespace := ParseTokenAudience(audience); {
		case perNamespace && other == namespace:
			return AudienceOwn
		case perNamespace:
			result = AudienceOtherNamespace
		case audience == ClusterWideTokenAudience && result == AudienceUnknown:
			result = AudienceClusterWide
		}
	}
	return result
}

// unverifiedAudiences decodes the "aud" claim (a string or an array of
// strings, RFC 7519 §4.1.3) of a compact JWS already checked by
// validCompactJWS, bounded by its length.
func unverifiedAudiences(value string) ([]string, bool) {
	segments := strings.SplitN(value, ".", 3)
	if len(segments) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return nil, false
	}
	var claims struct {
		Audience json.RawMessage `json:"aud"`
	}
	if json.Unmarshal(payload, &claims) != nil || len(claims.Audience) == 0 {
		return nil, false
	}
	var single string
	if json.Unmarshal(claims.Audience, &single) == nil {
		return []string{single}, true
	}
	var several []string
	if json.Unmarshal(claims.Audience, &several) != nil || len(several) > maximumAudienceClaims {
		return nil, false
	}
	return several, true
}

// validNamespace reports whether value is a DNS-1123 label, the syntax of a
// Kubernetes namespace name: 1 to 63 lower-case alphanumerics or '-',
// starting and ending with an alphanumeric.
func validNamespace(value string) bool {
	if value == "" || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}
