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

package config

import (
	"net/url"
	"path/filepath"
	"strconv"
	"time"
)

// The PromQL source of the metrics charts ("with a Prometheus
// URL the console queries PromQL for charts"). MONITORING_URL stays the
// link-out a person follows; PROMETHEUS_URL is the API the console reads.
const (
	// EnvPrometheusURL is the base of the Prometheus HTTP API the console
	// queries, such as https://thanos-querier.openshift-monitoring.svc:9092.
	// Unset, the charts come from the console's own ring.
	EnvPrometheusURL = "PROMETHEUS_URL"
	// EnvPrometheusTokenFile is a bearer token file sent on every query
	// (a projected ServiceAccount token, re-read as it rotates). It
	// requires an https PROMETHEUS_URL.
	EnvPrometheusTokenFile = "PROMETHEUS_TOKEN_FILE"
	// EnvPrometheusCAFile verifies the Prometheus server; unset uses the
	// system roots.
	EnvPrometheusCAFile = "PROMETHEUS_CA_FILE"
	// EnvPrometheusNamespaced adds namespace=<ns> to each query, as
	// OpenShift's thanos-querier tenancy port requires.
	EnvPrometheusNamespaced = "PROMETHEUS_NAMESPACED"
	// EnvPrometheusRange is the charts' window (default 1h).
	EnvPrometheusRange = "PROMETHEUS_RANGE"
)

// Prometheus configures the PromQL source; URL empty disables it.
type Prometheus struct {
	URL        string
	TokenFile  string
	CAFile     string
	Namespaced bool
	Range      time.Duration
}

// Enabled reports whether a PROMETHEUS_URL is set.
func (p Prometheus) Enabled() bool { return p.URL != "" }

func prometheus(lookup Lookup, fail func(name, constraint string)) Prometheus {
	p := Prometheus{
		Namespaced: boolVar(lookup, EnvPrometheusNamespaced, false, fail),
		Range:      durationVar(lookup, EnvPrometheusRange, time.Hour, 5*time.Minute, 24*time.Hour, fail),
	}
	path := func(name string) string {
		raw, ok := lookup(name)
		if !ok || raw == "" {
			return ""
		}
		if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
			fail(name, "must be an absolute, clean path")
			return ""
		}
		return raw
	}
	p.TokenFile, p.CAFile = path(EnvPrometheusTokenFile), path(EnvPrometheusCAFile)
	raw, ok := lookup(EnvPrometheusURL)
	if !ok || raw == "" {
		if p.TokenFile != "" || p.CAFile != "" {
			fail(EnvPrometheusURL, "is required by "+EnvPrometheusTokenFile+" and "+EnvPrometheusCAFile)
		}
		return p
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		fail(EnvPrometheusURL, "must be an absolute http or https URL")
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		fail(EnvPrometheusURL, "must carry no user information, query or fragment")
	case u.Scheme == "http" && p.TokenFile != "":
		fail(EnvPrometheusURL, "must use https when "+EnvPrometheusTokenFile+" is set: the token is never sent in clear")
	default:
		p.URL = raw
	}
	return p
}

// info is the PromQL source's part of Console.Info (paths, never content).
func (p Prometheus) info(into map[string]string) {
	into[EnvPrometheusURL] = p.URL
	into[EnvPrometheusTokenFile] = p.TokenFile
	into[EnvPrometheusCAFile] = p.CAFile
	into[EnvPrometheusNamespaced] = strconv.FormatBool(p.Namespaced)
	into[EnvPrometheusRange] = p.Range.String()
}
