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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Provider names, as configured in AUTH_PROVIDER and recorded in a
// session.
const (
	ProviderOIDC      = "oidc"
	ProviderOpenShift = "openshift"
	ProviderToken     = "token"
	// ProviderOIDCGrant is never configured or a session's: it names the
	// IdP tokens of a grant signed in with the OIDC grant client
	// (OIDC_GRANT_CLIENT_ID), which RefreshUpstream renews with that
	// client.
	ProviderOIDCGrant = "oidc-grant"
)

// Defaults. The session lifetime is bounded three ways: the absolute
// MaxAge, the IdleTimeout, and the expiry of the token it carries,
// whichever comes first.
const (
	DefaultMaxAge      = 12 * time.Hour
	DefaultIdleTimeout = time.Hour
	DefaultIdPTimeout  = 10 * time.Second
	// DefaultServiceAccountTokenFile is where the kubelet projects the
	// console's own token; on OpenShift it is also the OAuth client
	// secret of a ServiceAccount used as an OAuth client.
	DefaultServiceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- a well-known path, not a credential
	// DefaultServiceCAFile is the OpenShift service CA the kubelet mounts
	// next to the token.
	DefaultServiceCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"
	// maxSecretFileBytes bounds every secret file read at start.
	maxSecretFileBytes = 64 << 10
	// minSecretBytes is the shortest session secret accepted: HKDF
	// stretches, it does not add entropy.
	minSecretBytes = 32
)

// Config is the sign-in configuration, read from the environment the
// chart renders (charts/pgtools, "The console binary contract":
// AUTH_PROVIDER, SESSION_KEY_FILE, SESSION_IDLE_TIMEOUT, OIDC_*,
// REVOCATION_CONFIGMAP, POD_NAMESPACE) plus optional variables with safe
// defaults (AUTH_TOKEN_LOGIN, PUBLIC_URL, SESSION_MAX_AGE, COOKIE_SECURE,
// TRUSTED_PROXY_HOPS, IDP_TIMEOUT, OIDC_CA_FILE,
// OIDC_REQUIRE_EMAIL_VERIFIED, OIDC_GRANT_CLIENT_*, OPENSHIFT_*). OIDC_GROUPS_CLAIM is not
// read: groups come from the API server's SelfSubjectReview. It holds file paths, never
// secret values: secrets are read by LoadSecrets at start.
type Config struct {
	// Providers lists the enabled providers in the order the login page
	// offers them. At least one is required.
	Providers []string
	// SessionSecretFile holds one session secret per line, at least 32
	// bytes each. The first line seals; every line opens, so a rotation
	// prepends a new line and drops the old one a session lifetime later.
	SessionSecretFile string
	// CookieSecure marks cookies Secure and names them with the __Host-
	// prefix. It is false only for plain-HTTP local development.
	CookieSecure bool
	// MaxAge bounds a session from sign-in, IdleTimeout from the last
	// request.
	MaxAge      time.Duration
	IdleTimeout time.Duration
	// PublicURL is the origin the browser uses (https://console.example).
	// When set, OAuth redirect URIs and the Origin check use it; when
	// empty they are derived from the request.
	PublicURL string
	// TrustedProxyHops is how many reverse proxies in front of the
	// console append to X-Forwarded-For; the audit log's client address
	// is read that many entries from the right.
	TrustedProxyHops int
	// IdPTimeout bounds every call to an identity provider.
	IdPTimeout time.Duration
	// RevocationConfigMap names the ConfigMap in Namespace that lists
	// revoked sessions; empty disables the list.
	RevocationConfigMap string
	// Namespace is the console's own (release) namespace.
	Namespace string

	OIDC      OIDCConfig
	OpenShift OpenShiftConfig
}

// OIDCConfig configures the OIDC code flow. The API server must trust the
// same issuer and client (its --oidc-* flags or structured
// AuthenticationConfiguration): the ID token is what the console presents
// to it.
type OIDCConfig struct {
	IssuerURL        string
	ClientID         string
	ClientSecretFile string
	// RedirectURL is the registered redirect URI; empty derives it from
	// the public URL or the request.
	RedirectURL string
	Scopes      []string
	// CAFile adds a CA to the system pool for the issuer.
	CAFile string
	// RequireEmailVerified refuses a token whose email claim is not
	// vouched for by email_verified=true. Default true.
	RequireEmailVerified bool
	// GrantClientID and GrantClientSecretFile (OIDC_GRANT_CLIENT_ID,
	// OIDC_GRANT_CLIENT_SECRET_FILE), optional: a second client of the
	// same issuer for the IdP sign-ins of console grants (MCP clients),
	// registered with the same redirect URI. An identity provider that
	// keeps one refresh token per person and client (dex) then keeps the
	// browser's and the grants' apart. The API server must accept this
	// client's ID tokens too (its audience in the JWT authenticator).
	// Empty: grants sign in with the console's client.
	GrantClientID         string
	GrantClientSecretFile string
}

// OpenShiftConfig configures the OpenShift OAuth code flow, with the
// console's ServiceAccount as the OAuth client.
type OpenShiftConfig struct {
	// ClientID is system:serviceaccount:<namespace>:<name>.
	ClientID         string
	ClientSecretFile string
	DiscoveryURL     string
	Scopes           []string
	CAFile           string
}

// Getenv reads one variable; os.LookupEnv in production, a map in tests.
type Getenv func(string) (string, bool)

// LoadConfig reads the variables listed on Config. It reports every problem at once
// so a misconfigured chart is fixed in one rollout.
func LoadConfig(getenv Getenv) (Config, error) {
	var errs []error
	str := func(key, def string) string {
		if v, ok := getenv(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := str(key, "")
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s=%q is not a positive duration", key, v))
			return def
		}
		return d
	}
	boolean := func(key string, def bool) bool {
		v := str(key, "")
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q is not a boolean", key, v))
			return def
		}
		return b
	}
	list := func(key string, def []string) []string {
		v := str(key, "")
		if v == "" {
			return def
		}
		var out []string
		for f := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			out = append(out, f)
		}
		return out
	}

	providers := list("AUTH_PROVIDER", nil)
	// The pasted-token form is the break-glass path: it stays
	// offered beside an SSO provider unless AUTH_TOKEN_LOGIN=false.
	if boolean("AUTH_TOKEN_LOGIN", true) && len(providers) > 0 && !slices.Contains(providers, ProviderToken) {
		providers = append(providers, ProviderToken)
	}
	publicURL := strings.TrimSuffix(str("PUBLIC_URL", ""), "/")
	redirect := str("OIDC_REDIRECT_URL", "")
	if publicURL == "" && redirect != "" {
		// The registered redirect URI names the origin the browser uses.
		if u, err := url.Parse(redirect); err == nil && u.Host != "" {
			publicURL = u.Scheme + "://" + u.Host
		}
	}
	cfg := Config{
		Providers:           providers,
		SessionSecretFile:   str("SESSION_KEY_FILE", ""),
		CookieSecure:        boolean("COOKIE_SECURE", true),
		MaxAge:              dur("SESSION_MAX_AGE", DefaultMaxAge),
		IdleTimeout:         dur("SESSION_IDLE_TIMEOUT", DefaultIdleTimeout),
		PublicURL:           publicURL,
		IdPTimeout:          dur("IDP_TIMEOUT", DefaultIdPTimeout),
		RevocationConfigMap: str("REVOCATION_CONFIGMAP", ""),
		Namespace:           str("POD_NAMESPACE", ""),
		OIDC: OIDCConfig{
			IssuerURL:             str("OIDC_ISSUER_URL", ""),
			ClientID:              str("OIDC_CLIENT_ID", ""),
			ClientSecretFile:      str("OIDC_CLIENT_SECRET_FILE", ""),
			RedirectURL:           redirect,
			Scopes:                list("OIDC_SCOPES", []string{"openid", "email", "profile", "offline_access"}),
			CAFile:                str("OIDC_CA_FILE", ""),
			RequireEmailVerified:  boolean("OIDC_REQUIRE_EMAIL_VERIFIED", true),
			GrantClientID:         str("OIDC_GRANT_CLIENT_ID", ""),
			GrantClientSecretFile: str("OIDC_GRANT_CLIENT_SECRET_FILE", ""),
		},
		OpenShift: OpenShiftConfig{
			ClientID:         str("OPENSHIFT_CLIENT_ID", ""),
			ClientSecretFile: str("OPENSHIFT_CLIENT_SECRET_FILE", DefaultServiceAccountTokenFile),
			DiscoveryURL:     str("OPENSHIFT_DISCOVERY_URL", ""),
			Scopes:           list("OPENSHIFT_SCOPES", []string{"user:info", "user:check-access", "user:full"}),
			CAFile:           str("OPENSHIFT_CA_FILE", ""),
		},
	}
	if slices.Contains(cfg.Providers, ProviderOpenShift) && cfg.OpenShift.ClientID == "" {
		// The console's ServiceAccount is the OAuth client; its name is
		// read from the console's own projected token.
		cfg.OpenShift.ClientID = serviceAccountFromTokenFile(cfg.OpenShift.ClientSecretFile)
	}
	if v := str("TRUSTED_PROXY_HOPS", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 8 {
			errs = append(errs, fmt.Errorf("TRUSTED_PROXY_HOPS=%q must be an integer between 0 and 8", v))
		} else {
			cfg.TrustedProxyHops = n
		}
	}
	if err := cfg.Validate(); err != nil {
		errs = append(errs, err)
	}
	return cfg, errors.Join(errs...)
}

// Validate checks the configuration without touching the filesystem or
// the network.
func (c *Config) Validate() error {
	var errs []error
	if len(c.Providers) == 0 {
		errs = append(errs, errors.New("AUTH_PROVIDER is empty: set one of token, oidc, openshift"))
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		switch p {
		case ProviderOIDC, ProviderOpenShift, ProviderToken:
		default:
			errs = append(errs, fmt.Errorf("AUTH_PROVIDER: unknown provider %q (token, oidc, openshift)", p))
		}
		if seen[p] {
			errs = append(errs, fmt.Errorf("AUTH_PROVIDER: %q listed twice", p))
		}
		seen[p] = true
	}
	if c.SessionSecretFile == "" {
		errs = append(errs, errors.New("SESSION_KEY_FILE is required"))
	}
	if c.IdleTimeout > c.MaxAge {
		errs = append(errs, fmt.Errorf("SESSION_IDLE_TIMEOUT (%s) exceeds SESSION_MAX_AGE (%s)", c.IdleTimeout, c.MaxAge))
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			errs = append(errs, fmt.Errorf("PUBLIC_URL=%q must be an origin such as https://console.example", c.PublicURL))
		} else if c.CookieSecure && u.Scheme != "https" {
			errs = append(errs, errors.New("PUBLIC_URL is http:// while COOKIE_SECURE is true: the browser would drop every cookie"))
		}
	}
	if c.RevocationConfigMap != "" && c.Namespace == "" {
		errs = append(errs, errors.New("REVOCATION_CONFIGMAP needs POD_NAMESPACE (the console's own namespace)"))
	}
	if slices.Contains(c.Providers, ProviderOIDC) {
		u, err := url.Parse(c.OIDC.IssuerURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			errs = append(errs, fmt.Errorf("OIDC_ISSUER_URL=%q must be an https URL", c.OIDC.IssuerURL))
		}
		if c.OIDC.ClientID == "" {
			errs = append(errs, errors.New("OIDC_CLIENT_ID is required with the oidc provider"))
		}
		if c.OIDC.ClientSecretFile == "" {
			errs = append(errs, errors.New("OIDC_CLIENT_SECRET_FILE is required with the oidc provider"))
		}
		if !slices.Contains(c.OIDC.Scopes, "openid") {
			errs = append(errs, errors.New("OIDC_SCOPES must include openid"))
		}
		if (c.OIDC.GrantClientID == "") != (c.OIDC.GrantClientSecretFile == "") {
			errs = append(errs, errors.New("OIDC_GRANT_CLIENT_ID and OIDC_GRANT_CLIENT_SECRET_FILE go together"))
		}
		if c.OIDC.GrantClientID != "" && c.OIDC.GrantClientID == c.OIDC.ClientID {
			errs = append(errs, errors.New("OIDC_GRANT_CLIENT_ID must differ from OIDC_CLIENT_ID (or be left empty)"))
		}
	} else if c.OIDC.GrantClientID != "" {
		errs = append(errs, errors.New("OIDC_GRANT_CLIENT_ID needs the oidc provider"))
	}
	if slices.Contains(c.Providers, ProviderOpenShift) {
		if !strings.HasPrefix(c.OpenShift.ClientID, "system:serviceaccount:") {
			errs = append(errs, fmt.Errorf("OPENSHIFT_CLIENT_ID=%q must be system:serviceaccount:<namespace>:<name> (or the token at OPENSHIFT_CLIENT_SECRET_FILE must name it)", c.OpenShift.ClientID))
		}
		if c.OpenShift.DiscoveryURL != "" {
			if u, err := url.Parse(c.OpenShift.DiscoveryURL); err != nil || u.Scheme != "https" || u.Host == "" {
				errs = append(errs, fmt.Errorf("OPENSHIFT_DISCOVERY_URL=%q must be an https URL", c.OpenShift.DiscoveryURL))
			}
		}
	}
	return errors.Join(errs...)
}

// LoadSecrets reads the session secrets: one per non-empty line, each at
// least 32 bytes. The values are returned to the caller and never logged.
func LoadSecrets(path string) ([][]byte, error) {
	raw, err := readBoundedFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading session secrets: %w", err)
	}
	var out [][]byte
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) < minSecretBytes {
			return nil, fmt.Errorf("session secret on line %d is shorter than %d bytes", i+1, minSecretBytes)
		}
		out = append(out, []byte(line))
	}
	if len(out) == 0 {
		return nil, errors.New("the session secret file holds no secret")
	}
	return out, nil
}

// readBoundedFile reads a mounted file the chart names, bounded so a
// wrong mount cannot exhaust memory at start.
func readBoundedFile(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 G703 -- path from the chart-rendered environment
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(raw) > maxSecretFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxSecretFileBytes)
	}
	return raw, nil
}

// readSecretFile reads a one-value secret file (client secret, token).
func readSecretFile(path string) (string, error) {
	raw, err := readBoundedFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

// serviceAccountFromTokenFile reads the subject of the console's own
// projected ServiceAccount token (system:serviceaccount:<ns>:<name>)
// without verifying it: it is the console's own file, and the value only
// names the OAuth client the OpenShift OAuth server then authenticates.
func serviceAccountFromTokenFile(path string) string {
	raw, err := readSecretFile(path)
	if err != nil {
		return ""
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Sub string `json:"sub"`
	}
	if json.Unmarshal(payload, &c) != nil || !strings.HasPrefix(c.Sub, "system:serviceaccount:") {
		return ""
	}
	return c.Sub
}

// Info lists the effective sign-in settings for pgtoolbox_console_config_info:
// names and values an operator may read, never a secret (files are named
// by path, never read here).
func (c *Config) Info() map[string]string {
	return map[string]string{
		"auth_providers":          strings.Join(c.Providers, ","),
		"session_key_file":        c.SessionSecretFile,
		"cookie_secure":           strconv.FormatBool(c.CookieSecure),
		"session_max_age":         c.MaxAge.String(),
		"session_idle_timeout":    c.IdleTimeout.String(),
		"public_url":              c.PublicURL,
		"trusted_proxy_hops":      strconv.Itoa(c.TrustedProxyHops),
		"idp_timeout":             c.IdPTimeout.String(),
		"revocation_configmap":    c.RevocationConfigMap,
		"oidc_issuer_url":         c.OIDC.IssuerURL,
		"oidc_client_id":          c.OIDC.ClientID,
		"oidc_redirect_url":       c.OIDC.RedirectURL,
		"oidc_scopes":             strings.Join(c.OIDC.Scopes, ","),
		"oidc_email_verified":     strconv.FormatBool(c.OIDC.RequireEmailVerified),
		"oidc_grant_client_id":    c.OIDC.GrantClientID,
		"openshift_client_id":     c.OpenShift.ClientID,
		"openshift_discovery_url": c.OpenShift.DiscoveryURL,
	}
}
