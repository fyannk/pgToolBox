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

package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// minSecretLength is the minimum accepted length of a cookie secret.
const minSecretLength = 16

// Validate checks the whole configuration and returns all problems as one
// joined error. Messages reference fields and values that are safe to
// print; they never include cookie secrets or password hashes.
func (c *Config) Validate() error {
	var errs []error
	errs = append(errs, c.validateSession()...)
	errs = append(errs, c.validateServer()...)
	errs = append(errs, c.validateProvider()...)
	errs = append(errs, c.validateUsers()...)
	errs = append(errs, c.validateRoutes()...)
	errs = append(errs, c.validateAccessRequest()...)
	return errors.Join(errs...)
}

func (c *Config) validateSession() []error {
	var errs []error
	if c.Session.CookieName == "" {
		errs = append(errs, fmt.Errorf("session.cookieName is required"))
	} else if !isCookieNameValid(c.Session.CookieName) {
		errs = append(errs, fmt.Errorf("session.cookieName %q is not a valid cookie name", c.Session.CookieName))
	}
	if len(c.Session.CookieSecrets) == 0 {
		errs = append(errs, fmt.Errorf("session.cookieSecrets must list at least one secret"))
	}
	for i, s := range c.Session.CookieSecrets {
		if len(s) < minSecretLength {
			errs = append(errs, fmt.Errorf("session.cookieSecrets[%d] is too short (minimum %d characters)", i, minSecretLength))
		}
	}
	if c.Session.MaxAge <= 0 {
		errs = append(errs, fmt.Errorf("session.maxAge must be positive"))
	}
	return errs
}

func (c *Config) validateServer() []error {
	var errs []error
	if c.Server.Listen == "" {
		errs = append(errs, fmt.Errorf("server.listen is required"))
	}
	if c.Server.UpstreamDialTimeout <= 0 {
		errs = append(errs, fmt.Errorf("server.upstreamDialTimeout must be positive"))
	}
	if c.Server.UpstreamResponseHeaderTimeout <= 0 {
		errs = append(errs, fmt.Errorf("server.upstreamResponseHeaderTimeout must be positive"))
	}
	return errs
}

func (c *Config) validateProvider() []error {
	var errs []error
	if len(c.Provider.Modes) == 0 {
		return append(errs, fmt.Errorf("provider.modes must enable at least one provider"))
	}

	seen := map[string]bool{}
	for _, mode := range c.Provider.Modes {
		switch mode {
		case ModeOIDC, ModeLocal, ModeOpenShift:
		default:
			errs = append(errs, fmt.Errorf(
				"provider.modes %q is invalid (want %q, %q or %q)", mode, ModeOIDC, ModeLocal, ModeOpenShift))
			continue
		}
		if seen[mode] {
			errs = append(errs, fmt.Errorf("provider.modes lists %q twice", mode))
		}
		seen[mode] = true
	}

	// A provider's settings block belongs to that provider: present when it
	// is enabled, absent when it is not, so a configuration cannot carry
	// credentials for something it never consults.
	if seen[ModeOIDC] {
		if c.Provider.OIDC == nil {
			errs = append(errs, fmt.Errorf("provider.oidc is required when provider.modes includes oidc"))
		} else {
			errs = append(errs, c.Provider.OIDC.validate()...)
		}
	} else if c.Provider.OIDC != nil {
		errs = append(errs, fmt.Errorf("provider.oidc may only be set when provider.modes includes oidc"))
	}

	if seen[ModeOpenShift] {
		if c.Provider.OpenShift == nil {
			errs = append(errs, fmt.Errorf("provider.openshift is required when provider.modes includes openshift"))
		} else {
			errs = append(errs, c.Provider.OpenShift.validate()...)
		}
	} else if c.Provider.OpenShift != nil {
		errs = append(errs, fmt.Errorf("provider.openshift may only be set when provider.modes includes openshift"))
	}
	return errs
}

// LocalEnabled reports whether local accounts are one of the ways in.
func (c *Config) LocalEnabled() bool {
	for _, mode := range c.Provider.Modes {
		if mode == ModeLocal {
			return true
		}
	}
	return false
}

func (o *OIDCConfig) validate() []error {
	var errs []error
	if o.IssuerURL == "" {
		errs = append(errs, fmt.Errorf("provider.oidc.issuerURL is required"))
	} else if u, err := url.Parse(o.IssuerURL); err != nil || u.Scheme != "https" || u.Host == "" {
		errs = append(errs, fmt.Errorf("provider.oidc.issuerURL must be an absolute https URL"))
	}
	if o.ClientID == "" {
		errs = append(errs, fmt.Errorf("provider.oidc.clientID is required"))
	}
	if o.ClientSecretFile == "" {
		errs = append(errs, fmt.Errorf("provider.oidc.clientSecretFile is required"))
	}
	// An empty redirect URL is not a gap. It means the proxy resolves one
	// against the origin each request arrived on, which is the only correct
	// answer when the deployment has no external hostname to state — the
	// provider sends the browser there, so the only address that can work
	// is the one the browser used.
	if o.RedirectURL != "" {
		if u, err := url.Parse(o.RedirectURL); err != nil ||
			(u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("provider.oidc.redirectURL must be an absolute URL"))
		}
	}
	if len(o.Scopes) == 0 {
		errs = append(errs, fmt.Errorf("provider.oidc.scopes must not be empty"))
	}
	if o.SubjectClaim == "" {
		errs = append(errs, fmt.Errorf("provider.oidc.subjectClaim must not be empty"))
	}
	return errs
}

func (o *OpenShiftConfig) validate() []error {
	var errs []error
	if o.ClientID == "" {
		errs = append(errs, fmt.Errorf("provider.openshift.clientID is required"))
	}
	if o.ClientSecretFile == "" {
		errs = append(errs, fmt.Errorf("provider.openshift.clientSecretFile is required"))
	}
	if o.RedirectURL == "" {
		errs = append(errs, fmt.Errorf("provider.openshift.redirectURL is required"))
	} else if u, err := url.Parse(o.RedirectURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		errs = append(errs, fmt.Errorf("provider.openshift.redirectURL must be an absolute URL"))
	}
	if o.DiscoveryURL != "" {
		if u, err := url.Parse(o.DiscoveryURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("provider.openshift.discoveryURL must be an absolute URL"))
		}
	}
	if o.APIURL != "" {
		if u, err := url.Parse(o.APIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("provider.openshift.apiURL must be an absolute URL"))
		}
	}
	if len(o.Scopes) == 0 {
		errs = append(errs, fmt.Errorf("provider.openshift.scopes must not be empty"))
	}
	if o.UserInfoPath != "" && !strings.HasPrefix(o.UserInfoPath, "/") {
		errs = append(errs, fmt.Errorf("provider.openshift.userInfoPath must start with /"))
	}
	return errs
}

func (c *Config) validateUsers() []error {
	var errs []error
	seen := map[string]struct{}{}
	for i, u := range c.Users {
		field := fmt.Sprintf("users[%d]", i)
		if u.Subject == "" {
			errs = append(errs, fmt.Errorf("%s.subject is required", field))
		} else {
			key := strings.ToLower(u.Subject)
			if _, dup := seen[key]; dup {
				errs = append(errs, fmt.Errorf("%s.subject %q is a duplicate", field, u.Subject))
			}
			seen[key] = struct{}{}
		}
		if !u.Level.Valid() {
			errs = append(errs, fmt.Errorf("%s.level %q is invalid (want view, poweruser or dba)", field, u.Level))
		}
		// A hash is only meaningful where local accounts are offered, and
		// only required for the users that have one: with an identity
		// provider alongside, most users authenticate there and carry none.
		if u.LocalPasswordBcrypt != "" {
			if !c.LocalEnabled() {
				errs = append(errs, fmt.Errorf("%s.localPasswordBcrypt is set but local is not enabled", field))
			} else if _, err := bcrypt.Cost([]byte(u.LocalPasswordBcrypt)); err != nil {
				errs = append(errs, fmt.Errorf("%s.localPasswordBcrypt is not a valid bcrypt hash", field))
			}
		}
	}
	return errs
}

func (c *Config) validateRoutes() []error {
	var errs []error
	if len(c.Routes) == 0 {
		errs = append(errs, fmt.Errorf("routes must list at least one route"))
		return errs
	}
	seen := map[string]struct{}{}
	for i, r := range c.Routes {
		field := fmt.Sprintf("routes[%d]", i)
		if r.PathPrefix == "" || !strings.HasPrefix(r.PathPrefix, "/") {
			errs = append(errs, fmt.Errorf("%s.pathPrefix %q must start with /", field, r.PathPrefix))
		} else {
			if strings.Contains(r.PathPrefix, "..") {
				errs = append(errs, fmt.Errorf("%s.pathPrefix %q must not contain \"..\"", field, r.PathPrefix))
			}
			if _, dup := seen[r.PathPrefix]; dup {
				errs = append(errs, fmt.Errorf("%s.pathPrefix %q is a duplicate", field, r.PathPrefix))
			}
			seen[r.PathPrefix] = struct{}{}
			for _, res := range ReservedPrefixes {
				if r.PathPrefix == res || strings.HasPrefix(r.PathPrefix, res+"/") {
					errs = append(errs, fmt.Errorf("%s.pathPrefix %q collides with the reserved prefix %q", field, r.PathPrefix, res))
				}
			}
		}
		if r.Upstream == "" {
			errs = append(errs, fmt.Errorf("%s.upstream is required", field))
		} else if u, err := url.Parse(r.Upstream); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s.upstream must be an absolute http or https URL", field))
		}
		if !r.MinLevel.Valid() {
			errs = append(errs, fmt.Errorf("%s.minLevel %q is invalid (want view, poweruser or dba)", field, r.MinLevel))
		}
	}
	return errs
}

func (c *Config) validateAccessRequest() []error {
	if !c.AccessRequest.Enabled {
		return nil
	}
	var errs []error
	if c.AccessRequest.ConsoleName == "" {
		errs = append(errs, fmt.Errorf("accessRequest.consoleName is required when accessRequest.enabled is true"))
	}
	if c.AccessRequest.Namespace == "" {
		errs = append(errs, fmt.Errorf("accessRequest.namespace is required when accessRequest.enabled is true"))
	}
	return errs
}

// warnings returns non-fatal remarks about an otherwise valid config.
func (c *Config) warnings() []string {
	var warns []string
	if len(c.Users) == 0 {
		warns = append(warns, "no users configured: nobody can log in until the operator renders users")
	}
	if c.Provider.OIDC != nil && strings.HasPrefix(c.Provider.OIDC.RedirectURL, "http://") {
		warns = append(warns, "provider.oidc.redirectURL is not https")
	}
	for _, r := range c.Routes {
		if u, err := url.Parse(r.Upstream); err == nil && !isLoopbackHost(u.Hostname()) {
			warns = append(warns, fmt.Sprintf("route %q upstream host %q is not a loopback address; identity headers are sent over the network", r.PathPrefix, u.Hostname()))
		}
	}
	return warns
}

func isCookieNameValid(name string) bool {
	for _, r := range name {
		ok := r == '!' || (r >= '#' && r <= '+') || (r >= '-' && r <= '.') ||
			(r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= '^' && r <= 'z') ||
			r == '|' || r == '~'
		if !ok {
			return false
		}
	}
	return true
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
