/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

// Package config defines the pgtoolbox-proxy configuration file schema,
// its defaults, and its validation. The file is rendered by the operator
// into a Secret-mounted volume; the proxy validates it totally before
// opening its listener and on every hot reload.
package config

import (
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

// Level is a coarse authorization level attached to a user and required
// by a route.
type Level string

const (
	// LevelNone is the implicit level of an authenticated identity that is
	// not present in the user list. It grants access to nothing.
	LevelNone Level = ""
	// LevelView grants read-only access.
	LevelView Level = "view"
	// LevelPowerUser grants operational access.
	LevelPowerUser Level = "poweruser"
	// LevelDBA grants full access.
	LevelDBA Level = "dba"
)

// Rank orders levels: none < view < poweruser < dba. Unknown levels
// rank below everything.
func Rank(l Level) int {
	switch l {
	case LevelView:
		return 1
	case LevelPowerUser:
		return 2
	case LevelDBA:
		return 3
	case LevelNone:
		return 0
	default:
		return -1
	}
}

// Valid reports whether l is a level an operator may configure.
func (l Level) Valid() bool {
	return Rank(l) > 0
}

// Provider modes.
const (
	ModeOIDC  = "oidc"
	ModeLocal = "local"
	// ModeOpenShift authenticates against OpenShift's integrated OAuth
	// server using the workload service account as the OAuth client.
	ModeOpenShift = "openshift"
)

// Duration is a time.Duration that unmarshals from a YAML string such as
// "8h" or "30s".
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("must be a duration string such as \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Config is the root of the proxy configuration file.
type Config struct {
	Session       SessionConfig       `json:"session"`
	Server        ServerConfig        `json:"server"`
	Provider      ProviderConfig      `json:"provider"`
	Users         []User              `json:"users"`
	Routes        []RouteConfig       `json:"routes"`
	AccessRequest AccessRequestConfig `json:"accessRequest"`
}

// SessionConfig configures the sealed session cookie.
type SessionConfig struct {
	// CookieName is the name of the session cookie. Default:
	// "pgtoolbox_session".
	CookieName string `json:"cookieName,omitempty"`
	// CookieSecrets is the key list for session sealing. The first secret
	// encrypts; all are tried on decrypt so keys can be rotated by
	// prepending a new secret. Values are never logged.
	CookieSecrets []string `json:"cookieSecrets"`
	// MaxAge is the session lifetime. Default: 8h.
	MaxAge Duration `json:"maxAge,omitempty"`
	// Secure sets the Secure flag on cookies. Default: true.
	Secure *bool `json:"secure,omitempty"`
}

// CookieSecure reports whether cookies carry the Secure flag.
func (s SessionConfig) CookieSecure() bool {
	return s.Secure == nil || *s.Secure
}

// ServerConfig configures the HTTP listener and upstream behavior.
type ServerConfig struct {
	// Listen is the listen address. Default: ":8080".
	Listen string `json:"listen,omitempty"`
	// UpstreamDialTimeout bounds connecting to an upstream. Default: 10s.
	UpstreamDialTimeout Duration `json:"upstreamDialTimeout,omitempty"`
	// UpstreamResponseHeaderTimeout bounds waiting for upstream response
	// headers. Default: 30s.
	UpstreamResponseHeaderTimeout Duration `json:"upstreamResponseHeaderTimeout,omitempty"`
}

// ProviderConfig selects and configures the authentication provider.
type ProviderConfig struct {
	// Mode is "oidc", "local" or "openshift".
	// Modes are the authentication providers to enable, in the order the
	// login page offers them. More than one is a supported deployment: a
	// local account is how the first administrator gets in, and how anyone
	// gets in when the identity provider is the thing that is down.
	Modes []string `json:"modes"`
	// OIDC configures the oidc mode; required exactly when mode is oidc.
	OIDC *OIDCConfig `json:"oidc,omitempty"`
	// OpenShift configures the openshift mode; required exactly when mode is
	// openshift.
	OpenShift *OpenShiftConfig `json:"openshift,omitempty"`
}

// OIDCConfig configures the OIDC authorization-code flow.
type OIDCConfig struct {
	// IssuerURL is the OIDC issuer URL. Must use https.
	IssuerURL string `json:"issuerURL"`
	// ClientID is the OAuth2 client ID.
	ClientID string `json:"clientID"`
	// ClientSecretFile is the path to a file holding the client secret.
	ClientSecretFile string `json:"clientSecretFile"`
	// RedirectURL is the absolute callback URL of the proxy.
	RedirectURL string `json:"redirectURL"`
	// Scopes are the requested scopes. Default: ["openid", "email"].
	Scopes []string `json:"scopes,omitempty"`
	// SubjectClaim is the ID-token claim used as the subject. Default:
	// "email".
	SubjectClaim string `json:"subjectClaim,omitempty"`
}

// OpenShiftConfig configures the OpenShift OAuth2 authorization-code flow.
// The service-account token doubles as the OAuth client secret; it is read
// from ClientSecretFile at redemption time so projected-token rotation works.
type OpenShiftConfig struct {
	// ClientID is the OAuth client ID. For the service-account flow the
	// operator renders system:serviceaccount:<namespace>:<service-account>.
	ClientID string `json:"clientID"`
	// ClientSecretFile is the path to the service-account token file.
	ClientSecretFile string `json:"clientSecretFile"`
	// RedirectURL is the absolute callback URL of the proxy.
	RedirectURL string `json:"redirectURL"`
	// DiscoveryURL is the OAuth authorization-server discovery endpoint.
	// Default: https://openshift.default.svc/.well-known/oauth-authorization-server.
	DiscoveryURL string `json:"discoveryURL,omitempty"`
	// APIURL is the OpenShift API base used for the current-user lookup.
	// Default: https://kubernetes.default.svc.
	APIURL string `json:"apiURL,omitempty"`
	// CAFile is the CA bundle for the discovery/API endpoints. Empty uses the
	// system pool. Default: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt.
	CAFile string `json:"caFile,omitempty"`
	// Scopes are the requested scopes. Default: ["user:info"].
	Scopes []string `json:"scopes,omitempty"`
	// UserInfoPath is the current-user endpoint path.
	// Default: /apis/user.openshift.io/v1/users/~.
	UserInfoPath string `json:"userInfoPath,omitempty"`
}

// User is one known identity and its level.
type User struct {
	// Subject is the provider identity (compared case-insensitively).
	Subject string `json:"subject"`
	// Level is the user's authorization level.
	Level Level `json:"level"`
	// LocalPasswordBcrypt holds the bcrypt password hash; required in
	// local mode, unused otherwise. Never logged.
	LocalPasswordBcrypt string `json:"localPasswordBcrypt,omitempty"`
}

// RouteConfig maps a path prefix to an upstream behind a minimum level.
type RouteConfig struct {
	// PathPrefix is the request path prefix this route matches.
	PathPrefix string `json:"pathPrefix"`
	// Upstream is the base URL of the upstream service.
	Upstream string `json:"upstream"`
	// MinLevel is the minimum level allowed on this route. Default: view.
	MinLevel Level `json:"minLevel,omitempty"`
}

// AccessRequestConfig configures the PgToolBoxAccessRequest flow.
type AccessRequestConfig struct {
	// Enabled turns on the request-access form on the denied page.
	Enabled bool `json:"enabled,omitempty"`
	// ConsoleName is the PgConsole name referenced by created requests.
	ConsoleName string `json:"consoleName,omitempty"`
	// Namespace is the namespace requests are created in.
	Namespace string `json:"namespace,omitempty"`
}

// ReservedPrefixes are URL prefixes owned by the proxy itself. Routes may
// never claim them.
var ReservedPrefixes = []string{"/auth", "/logout", "/healthz"}

// Load reads, parses, defaults, and validates the configuration file at
// path. The returned warnings are non-fatal remarks (for example a
// non-loopback upstream); an error means the configuration must not be
// used. Error messages never contain secret values.
func Load(path string) (*Config, []string, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path from process flag/env
	if err != nil {
		return nil, nil, fmt.Errorf("reading config file: %w", err)
	}
	return Parse(raw)
}

// Parse is Load for in-memory bytes.
func Parse(raw []byte) (*Config, []string, error) {
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return nil, nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.setDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	return &cfg, cfg.warnings(), nil
}

func (c *Config) setDefaults() {
	if c.Session.CookieName == "" {
		c.Session.CookieName = "pgtoolbox_session"
	}
	if c.Session.MaxAge == 0 {
		c.Session.MaxAge = Duration(8 * time.Hour)
	}
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.UpstreamDialTimeout == 0 {
		c.Server.UpstreamDialTimeout = Duration(10 * time.Second)
	}
	if c.Server.UpstreamResponseHeaderTimeout == 0 {
		c.Server.UpstreamResponseHeaderTimeout = Duration(30 * time.Second)
	}
	if c.Provider.OIDC != nil {
		if len(c.Provider.OIDC.Scopes) == 0 {
			c.Provider.OIDC.Scopes = []string{"openid", "email"}
		}
		if c.Provider.OIDC.SubjectClaim == "" {
			c.Provider.OIDC.SubjectClaim = "email"
		}
	}
	if c.Provider.OpenShift != nil {
		if len(c.Provider.OpenShift.Scopes) == 0 {
			c.Provider.OpenShift.Scopes = []string{"user:info"}
		}
		if c.Provider.OpenShift.UserInfoPath == "" {
			c.Provider.OpenShift.UserInfoPath = "/apis/user.openshift.io/v1/users/~"
		}
	}
	for i := range c.Routes {
		if c.Routes[i].MinLevel == "" {
			c.Routes[i].MinLevel = LevelView
		}
	}
}
