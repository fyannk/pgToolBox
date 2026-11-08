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

// Package config owns the environment configuration contract: parsing,
// defaulting, and total validation before the application listens. It is
// the only package that reads the process environment. Validation errors
// name the offending variable and the constraint, and never echo the
// supplied value.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Variable names of the configuration contract.
const (
	// EnvAllowClusterCatalogs lets the console read the one
	// cluster-scoped ClusterImageCatalog its Cluster references. It is
	// the only capability that needs authority outside the namespace, so
	// it is opt-in, needs its own ClusterRole, and degrades to unknown
	// when the flag is set but the binding is absent.
	EnvAllowClusterCatalogs = "ALLOW_CLUSTER_CATALOGS"
	// EnvLogStreamEnabled follows the member containers' logs
	// continuously so the diagnostics matcher can analyse each line as
	// it arrives. The matcher keeps only what matched, so this retains
	// no log text on its own.
	EnvLogStreamEnabled = "LOG_STREAM_ENABLED"
	// EnvLogMatchMaxAge expires a diagnostics log observation this long
	// after its last matching line.
	EnvLogMatchMaxAge = "LOG_MATCH_MAX_AGE"
	// EnvAllowLogs enables the bounded instance log tail.
	EnvAllowLogs = "ALLOW_LOGS"
	// EnvEventsMaxAge is the age window for the rendered event list.
	EnvEventsMaxAge = "EVENTS_MAX_AGE"
	// EnvAPIRequestTimeout bounds an individual Kubernetes API request.
	EnvAPIRequestTimeout = "API_REQUEST_TIMEOUT"
	// EnvHistoryEnabled enables the bounded in-memory revision history of
	// the watched object definitions.
	EnvHistoryEnabled = "HISTORY_ENABLED"
	// EnvHistoryMaxRevisions bounds the retained revisions across all
	// objects.
	EnvHistoryMaxRevisions = "HISTORY_MAX_REVISIONS"
	// EnvHistoryMaxBytes bounds the summed stored manifest bytes.
	EnvHistoryMaxBytes = "HISTORY_MAX_BYTES"
	// EnvHistoryPath is the journal file the history is mirrored into so
	// it survives restarts. Empty keeps history in memory only — the
	// default, which preserves the stateless deployment. Setting it
	// implies a writable volume and a single replica, and requires
	// history to be enabled.
	EnvHistoryPath = "HISTORY_PATH"
	// EnvMetricsEnabled enables the bounded in-memory metrics window
	// scraped from the instance pods' metrics endpoints.
	EnvMetricsEnabled = "METRICS_ENABLED"
	// EnvInstanceStatusEnabled reads each instance manager's own status
	// report from the instance pods' status port on a sweep, the way the
	// metrics exporter is read.
	EnvInstanceStatusEnabled = "INSTANCE_STATUS_ENABLED"
	// EnvInstanceStatusInterval is the sweep cadence.
	EnvInstanceStatusInterval = "INSTANCE_STATUS_INTERVAL"
	// EnvInstanceStatusCAFile is the PEM file holding the cluster CA the
	// status port's certificate is verified against — the <cluster>-ca
	// Secret's ca.crt, mounted by the deployer as a volume. Required
	// while the instance status sweep is enabled, unless
	// INSTANCE_STATUS_INSECURE=true is set explicitly.
	EnvInstanceStatusCAFile = "INSTANCE_STATUS_CA_FILE"
	// EnvInstanceStatusInsecure accepts, without a CA, any certificate
	// that names this cluster's read-write Service: no chain is
	// verified, so any endpoint presenting a self-signed certificate for
	// that name is believed. For labs only; it must be set explicitly,
	// and conflicts with INSTANCE_STATUS_CA_FILE.
	EnvInstanceStatusInsecure = "INSTANCE_STATUS_INSECURE"
	// EnvMonitoringURL is the link-out base URL to the operator's
	// dashboard for the cluster.
	EnvMonitoringURL = "MONITORING_URL"
	// EnvPgAdminURL is a fixed pgAdmin link shown on every Cluster, for a
	// pgAdmin installed without the operator (the pgtools chart's
	// pgadmin section). Unset, the console links only the
	// PgAdmin objects that register a Cluster.
	EnvPgAdminURL = "PGADMIN_URL"
	// EnvAllowInsecureLinks permits http link-out URLs, for lab use only.
	EnvAllowInsecureLinks = "ALLOW_INSECURE_LINKS"
)

// Lookup reports the value of one environment variable and whether it was
// set, mirroring os.LookupEnv so tests can substitute a map.
type Lookup func(name string) (value string, ok bool)

// VarError reports that one variable violated one constraint. Its message
// contains the variable name and the constraint, never the supplied
// value.
type VarError struct {
	// Name is the environment variable name.
	Name string
	// Constraint states the violated rule without echoing the value.
	Constraint string
}

// Error returns "NAME: constraint".
func (e *VarError) Error() string {
	return e.Name + ": " + e.Constraint
}

// dns1123Label matches a lowercase RFC 1123 label.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// boolVar reads an optional strict boolean ("true" or "false").
func boolVar(lookup Lookup, name string, def bool, fail func(name, constraint string)) bool {
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return def
	}
	switch raw {
	case "true":
		return true
	case "false":
		return false
	default:
		fail(name, `must be "true" or "false"`)
		return def
	}
}

// intVar reads an optional bounded integer.
func intVar(lookup Lookup, name string, def, minimum, maximum int, fail func(name, constraint string)) int {
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < minimum || v > maximum {
		fail(name, fmt.Sprintf("must be an integer between %d and %d", minimum, maximum))
		return def
	}
	return v
}

// durationVar reads an optional bounded Go duration.
func durationVar(lookup Lookup, name string, def, minimum, maximum time.Duration, fail func(name, constraint string)) time.Duration {
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v < minimum || v > maximum {
		fail(name, fmt.Sprintf("must be a duration between %s and %s", minimum, maximum))
		return def
	}
	return v
}

// linkVar reads an optional link-out target. Two shapes are accepted.
//
// An absolute URL must be https — or http only when insecure links are
// explicitly allowed — and never carries user information.
//
// A root-relative path such as "/pgadmin" names a sibling application
// served from the console's own origin, which is how a single Route or
// Ingress usually exposes the family. It is accepted without the scheme
// argument applying at all: a relative reference inherits whatever scheme
// the reader already has, so it cannot downgrade the way an http:// link
// can, and ALLOW_INSECURE_LINKS has nothing to say about it. Behind a
// proxy that terminates TLS and rewrites Host, it is also the more honest
// value, because the console cannot know its own external URL.
func linkVar(lookup Lookup, name string, allowInsecure bool, fail func(name, constraint string)) string {
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "/") {
		if err := validateRelativeLink(raw); err != nil {
			fail(name, err.Error())
			return ""
		}
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		fail(name, "must be an absolute URL or a root-relative path")
		return ""
	}
	if u.User != nil {
		fail(name, "must not contain user information")
		return ""
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowInsecure {
			fail(name, "http scheme requires "+EnvAllowInsecureLinks+"=true")
			return ""
		}
	default:
		fail(name, "must use the https scheme")
		return ""
	}
	return raw
}

// validateRelativeLink accepts a root-relative path on the console's own
// origin and rejects everything that only looks like one.
//
// The trap it exists for is the protocol-relative form. Go parses
// "//evil.com/x" as a relative reference carrying an authority — Host is
// set, Scheme is empty — so a check that asks only "does it start with a
// slash?" waves an entirely different origin straight through. Browsers
// also normalise a backslash in that position, which makes "/\evil.com"
// the same attack spelled differently. Both are refused on the second
// byte, before any parsing, because a parser's opinion is not the thing a
// browser will act on.
func validateRelativeLink(raw string) error {
	constraint := errors.New(`must be a root-relative path such as "/pgadmin", not protocol-relative`)
	if len(raw) > 1 && (raw[1] == '/' || raw[1] == '\\') {
		return constraint
	}
	for _, character := range raw {
		if character < 0x20 || character == 0x7f || character == ' ' {
			return errors.New("must not contain spaces or control characters; percent-encode them")
		}
	}
	// Belt and braces: a relative reference must carry no authority, no
	// scheme, and no user information once parsed either.
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return constraint
	}
	return nil
}
