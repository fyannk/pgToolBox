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
	"errors"
	"maps"
	"strings"
	"testing"
)

func TestTracing(t *testing.T) {
	t.Parallel()
	const ep = "https://api.smith.langchain.com/otel"
	cases := []struct {
		name    string
		env     map[string]string
		want    Tracing
		wantVar string
	}{
		{"off by default", nil, Tracing{}, ""},
		{"on with the base endpoint", map[string]string{EnvOTLPEndpoint: ep}, Tracing{Enabled: true, Endpoint: ep}, ""},
		{
			"on with the traces endpoint, user information and query dropped from the display",
			map[string]string{EnvOTLPTracesEndpoint: "http://u:p@tempo.monitoring:4318/v1/traces?x=1"},
			Tracing{Enabled: true, Endpoint: "http://tempo.monitoring:4318/v1/traces"},
			"",
		},
		{
			"everything",
			map[string]string{
				EnvOTLPEndpoint: ep, EnvOTelTracesSampler: "parentbased_traceidratio", EnvOTelTracesSamplerArg: "0.25",
				EnvTracingIncludePayloads: "true", EnvTracingHeadersFile: "/var/run/console/tracing/headers",
				EnvOTLPProtocol: "http/protobuf", EnvOTelTracesExporter: "otlp", EnvOTLPHeaders: "Langsmith-Project=pg",
			},
			Tracing{Enabled: true, Endpoint: ep, Sampler: "parentbased_traceidratio", IncludePayloads: true, HeadersFile: "/var/run/console/tracing/headers"},
			"",
		},
		{"OTEL_SDK_DISABLED wins", map[string]string{EnvOTLPEndpoint: ep, EnvOTelSDKDisabled: "true"}, Tracing{Endpoint: ep}, ""},
		{"exporter none", map[string]string{EnvOTLPEndpoint: ep, EnvOTelTracesExporter: "none"}, Tracing{Endpoint: ep}, ""},
		{"not a URL", map[string]string{EnvOTLPEndpoint: "tempo:4318"}, Tracing{}, EnvOTLPEndpoint},
		{"gRPC", map[string]string{EnvOTLPEndpoint: ep, EnvOTLPProtocol: "grpc"}, Tracing{}, EnvOTLPProtocol},
		{"traces gRPC", map[string]string{EnvOTLPEndpoint: ep, EnvOTLPTracesProtocol: "grpc"}, Tracing{}, EnvOTLPTracesProtocol},
		{"another exporter", map[string]string{EnvOTLPEndpoint: ep, EnvOTelTracesExporter: "zipkin"}, Tracing{}, EnvOTelTracesExporter},
		{"unknown sampler", map[string]string{EnvOTLPEndpoint: ep, EnvOTelTracesSampler: "jaeger_remote"}, Tracing{}, EnvOTelTracesSampler},
		{"ratio out of range", map[string]string{EnvOTLPEndpoint: ep, EnvOTelTracesSampler: "traceidratio", EnvOTelTracesSamplerArg: "2"}, Tracing{}, EnvOTelTracesSamplerArg},
		{"payloads without an endpoint", map[string]string{EnvTracingIncludePayloads: "true"}, Tracing{}, EnvTracingIncludePayloads},
		{"headers file without an endpoint", map[string]string{EnvTracingHeadersFile: "/h"}, Tracing{}, EnvTracingHeadersFile},
		{"relative headers file", map[string]string{EnvOTLPEndpoint: ep, EnvTracingHeadersFile: "h"}, Tracing{}, EnvTracingHeadersFile},
		{"malformed headers", map[string]string{EnvOTLPEndpoint: ep, EnvOTLPHeaders: "s3cr3t"}, Tracing{}, EnvOTLPHeaders},
		{"malformed traces headers", map[string]string{EnvOTLPEndpoint: ep, EnvOTLPTracesHeaders: "k=%zz"}, Tracing{}, EnvOTLPTracesHeaders},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{EnvWatchNamespaces: "team-a"}
			maps.Copy(env, c.env)
			cfg, err := LoadConsole(lookupOf(env))
			if c.wantVar == "" {
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Tracing != c.want {
					t.Errorf("Tracing = %+v, want %+v", cfg.Tracing, c.want)
				}
				return
			}
			if err != nil && strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("the error quotes a header value: %v", err)
			}
			var varErr *VarError
			for _, e := range unwrapAll(err) {
				if errors.As(e, &varErr) && varErr.Name == c.wantVar {
					return
				}
			}
			t.Errorf("no VarError for %s in %v", c.wantVar, err)
		})
	}
}

func TestParseOTLPHeaders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    map[string]string
		wantErr bool
	}{
		{"", map[string]string{}, false},
		{"x-api-key=abc, Langsmith-Project = pg ,", map[string]string{"x-api-key": "abc", "Langsmith-Project": "pg"}, false},
		{"Authorization=Basic%20dXNlcjpwYXNz", map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, false},
		{"k=a=b", map[string]string{"k": "a=b"}, false},
		{"k=a+b", map[string]string{"k": "a+b"}, false},
		{"novalue", nil, true},
		{"bad key=v", nil, true},
		{"=v", nil, true},
		{"k=%zz", nil, true},
		{"k=a%0Db", nil, true},
	}
	for _, c := range cases {
		got, err := ParseOTLPHeaders(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseOTLPHeaders(%q) error %v", c.in, err)
			continue
		}
		if !c.wantErr && !maps.Equal(got, c.want) {
			t.Errorf("ParseOTLPHeaders(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
