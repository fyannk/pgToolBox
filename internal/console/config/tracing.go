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
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// Tracing: OpenTelemetry spans exported over OTLP/HTTP, off
// unless an OTLP endpoint is set. The OTEL_* variables are the
// OpenTelemetry SDK's own (it reads them itself); the console validates
// the ones it can get wrong before any listener opens, and adds two of
// its own.
const (
	// EnvOTLPEndpoint is the OTLP/HTTP base URL; the SDK appends
	// /v1/traces. Setting it (or EnvOTLPTracesEndpoint) turns tracing on.
	EnvOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	// EnvOTLPTracesEndpoint is the full traces URL, used as is.
	EnvOTLPTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	// EnvOTLPHeaders are extra request headers, `key=value,...` with
	// percent-encoded values (an API key, a project name).
	EnvOTLPHeaders = "OTEL_EXPORTER_OTLP_HEADERS"
	// EnvOTLPTracesHeaders are the traces-only headers.
	EnvOTLPTracesHeaders = "OTEL_EXPORTER_OTLP_TRACES_HEADERS"
	// EnvOTLPProtocol and EnvOTLPTracesProtocol may only name
	// http/protobuf: the console carries no gRPC exporter.
	EnvOTLPProtocol       = "OTEL_EXPORTER_OTLP_PROTOCOL"
	EnvOTLPTracesProtocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	// EnvOTelSDKDisabled=true turns tracing off whatever else is set.
	EnvOTelSDKDisabled = "OTEL_SDK_DISABLED"
	// EnvOTelTracesExporter may be otlp (the default) or none (off).
	EnvOTelTracesExporter = "OTEL_TRACES_EXPORTER"
	// EnvOTelServiceName names the service (default pgtoolbox-console).
	EnvOTelServiceName = "OTEL_SERVICE_NAME"
	// EnvOTelResourceAttributes adds resource attributes, `key=value,...`.
	EnvOTelResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"
	// EnvOTelTracesSampler and EnvOTelTracesSamplerArg choose the sampler
	// (default parentbased_always_on).
	EnvOTelTracesSampler    = "OTEL_TRACES_SAMPLER"
	EnvOTelTracesSamplerArg = "OTEL_TRACES_SAMPLER_ARG"

	// EnvTracingIncludePayloads adds MCP tool arguments and results to the
	// tool spans, redacted and capped (default false). Tokens never.
	EnvTracingIncludePayloads = "TRACING_INCLUDE_PAYLOADS"
	// EnvTracingHeadersFile is a file holding exporter headers in the
	// EnvOTLPHeaders format, read once at start: a mounted Secret key, so
	// an API key never sits in the Deployment's environment.
	EnvTracingHeadersFile = "TRACING_HEADERS_FILE"
)

// samplers are the OTEL_TRACES_SAMPLER values the Go SDK implements.
var samplers = map[string]bool{
	"always_on": true, "always_off": true, "traceidratio": true,
	"parentbased_always_on": true, "parentbased_always_off": true, "parentbased_traceidratio": true,
}

// Tracing configures the OpenTelemetry exporter.
type Tracing struct {
	// Enabled: an OTLP endpoint is set and neither OTEL_SDK_DISABLED nor
	// OTEL_TRACES_EXPORTER=none turns it off.
	Enabled bool
	// Endpoint is the configured endpoint without user information or
	// query, for pgtoolbox_console_config_info.
	Endpoint string
	// Sampler is OTEL_TRACES_SAMPLER as set (empty: the SDK's default).
	Sampler string
	// IncludePayloads is TRACING_INCLUDE_PAYLOADS.
	IncludePayloads bool
	// HeadersFile is TRACING_HEADERS_FILE.
	HeadersFile string
}

func tracingConfig(lookup Lookup, fail func(name, constraint string)) Tracing {
	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	var t Tracing
	for _, name := range []string{EnvOTLPTracesEndpoint, EnvOTLPEndpoint} {
		raw := get(name)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fail(name, "must be an http or https URL")
			continue
		}
		if t.Endpoint == "" {
			t.Endpoint = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
		}
	}
	for _, name := range []string{EnvOTLPProtocol, EnvOTLPTracesProtocol} {
		if v := get(name); v != "" && v != "http/protobuf" {
			fail(name, `must be "http/protobuf": the console exports OTLP over HTTP only`)
		}
	}
	exporter := get(EnvOTelTracesExporter)
	if exporter != "" && exporter != "otlp" && exporter != "none" {
		fail(EnvOTelTracesExporter, `must be "otlp" or "none"`)
	}
	disabled := boolVar(lookup, EnvOTelSDKDisabled, false, fail)
	if s := get(EnvOTelTracesSampler); s != "" {
		if !samplers[s] {
			fail(EnvOTelTracesSampler, "must be one of always_on, always_off, traceidratio, parentbased_always_on, "+
				"parentbased_always_off, parentbased_traceidratio")
		}
		t.Sampler = s
	}
	if arg := get(EnvOTelTracesSamplerArg); arg != "" && strings.HasSuffix(t.Sampler, "traceidratio") {
		if r, err := strconv.ParseFloat(arg, 64); err != nil || r < 0 || r > 1 {
			fail(EnvOTelTracesSamplerArg, "must be a ratio between 0 and 1")
		}
	}
	// The SDK parses these itself and would report a malformed entry with
	// its value, an API key perhaps: refuse them here, naming the variable
	// and never the value.
	for _, name := range []string{EnvOTLPHeaders, EnvOTLPTracesHeaders} {
		if _, err := ParseOTLPHeaders(get(name)); err != nil {
			fail(name, err.Error())
		}
	}
	t.IncludePayloads = boolVar(lookup, EnvTracingIncludePayloads, false, fail)
	if raw := get(EnvTracingHeadersFile); raw != "" {
		if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
			fail(EnvTracingHeadersFile, "must be an absolute, clean path")
		} else {
			t.HeadersFile = raw
		}
	}
	t.Enabled = t.Endpoint != "" && !disabled && exporter != "none"
	if t.Endpoint == "" {
		if t.IncludePayloads {
			fail(EnvTracingIncludePayloads, "requires "+EnvOTLPEndpoint+" or "+EnvOTLPTracesEndpoint)
		}
		if t.HeadersFile != "" {
			fail(EnvTracingHeadersFile, "requires "+EnvOTLPEndpoint+" or "+EnvOTLPTracesEndpoint)
		}
	}
	return t
}

// ParseOTLPHeaders parses the OTEL_EXPORTER_OTLP_HEADERS format:
// comma-separated key=value entries, keys HTTP header names, values
// percent-encoded; blank entries are ignored. Errors name the entry's
// position, never its text: the values are credentials.
func ParseOTLPHeaders(s string) (map[string]string, error) {
	out := map[string]string{}
	for i, entry := range strings.Split(s, ",") {
		if strings.TrimSpace(entry) == "" {
			continue
		}
		k, v, ok := strings.Cut(entry, "=")
		k = strings.TrimSpace(k)
		if !ok || !headerToken(k) {
			return nil, fmt.Errorf("entry %d is not key=value with an HTTP header name as key", i+1)
		}
		value, err := url.PathUnescape(v)
		if err != nil {
			return nil, fmt.Errorf("entry %d (%s) has a malformed percent-encoding", i+1, k)
		}
		value = strings.TrimSpace(value)
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("entry %d (%s) holds a control character", i+1, k)
		}
		out[k] = value
	}
	return out, nil
}

// headerToken reports an RFC 9110 token.
func headerToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

func (t Tracing) info(into map[string]string) {
	into["TRACING"] = strconv.FormatBool(t.Enabled)
	if !t.Enabled {
		return
	}
	into[EnvOTLPEndpoint] = t.Endpoint
	into[EnvTracingIncludePayloads] = strconv.FormatBool(t.IncludePayloads)
	if t.Sampler != "" {
		into[EnvOTelTracesSampler] = t.Sampler
	}
	if t.HeadersFile != "" {
		into[EnvTracingHeadersFile] = t.HeadersFile
	}
}
