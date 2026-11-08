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

// Package otlp builds the console's OpenTelemetry SDK when tracing is on:
// a batch span processor exporting over OTLP/HTTP, the
// resource and sampler from the standard OTEL_* variables, and the
// exporter headers of TRACING_HEADERS_FILE (a mounted Secret key) on top
// of OTEL_EXPORTER_OTLP_HEADERS. Tracing off builds nothing: no
// exporter, no goroutine, no connection.
package otlp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/tracing"
)

// maxHeadersFile bounds TRACING_HEADERS_FILE: a few headers.
const maxHeadersFile = 16 << 10

// Options complete the configuration.
type Options struct {
	// Version is service.version.
	Version string
	// Lookup reads the environment (os.LookupEnv); the SDK reads the rest
	// of OTEL_* itself.
	Lookup func(string) (string, bool)
	Log    *slog.Logger
	// Exporter replaces the OTLP exporter (tests).
	Exporter sdktrace.SpanExporter
}

// Shutdown flushes and stops the exporter.
type Shutdown func(context.Context) error

// New builds tracing from cfg: nil and a no-op Shutdown when it is off.
func New(ctx context.Context, cfg config.Tracing, o Options) (*tracing.Tracing, Shutdown, error) {
	if !cfg.Enabled {
		return nil, func(context.Context) error { return nil }, nil
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Lookup == nil {
		o.Lookup = os.LookupEnv
	}
	exp := o.Exporter
	if exp == nil {
		var opts []otlptracehttp.Option
		if cfg.HeadersFile != "" {
			headers, err := mergedHeaders(cfg.HeadersFile, o.Lookup)
			if err != nil {
				return nil, nil, err
			}
			opts = append(opts, otlptracehttp.WithHeaders(headers))
		}
		var err error
		if exp, err = otlptracehttp.New(ctx, opts...); err != nil {
			return nil, nil, fmt.Errorf("building the OTLP trace exporter: %w", err)
		}
	}
	// service.name and service.version first, so OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES override them.
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "pgtoolbox-console"), attribute.String("service.version", version(o.Version))),
		resource.WithFromEnv(), resource.WithTelemetrySDK(), resource.WithHost(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("building the trace resource (%s): %w", config.EnvOTelResourceAttributes, err)
	}
	// The SDK's own diagnostics may quote what they parse; its export
	// errors go to the console's log, at most one a minute.
	otel.SetLogger(logr.Discard())
	otel.SetErrorHandler(&errorLog{log: o.Log, every: time.Minute})
	// The sampler is the SDK's default: OTEL_TRACES_SAMPLER and _ARG,
	// else parentbased_always_on.
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	o.Log.Info("tracing on", "endpoint", cfg.Endpoint, "sampler", cfg.Sampler, "payloads", cfg.IncludePayloads,
		"headersFile", cfg.HeadersFile)
	return tracing.New(tp, cfg.IncludePayloads), tp.Shutdown, nil
}

func version(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

// mergedHeaders are OTEL_EXPORTER_OTLP_HEADERS, then _TRACES_HEADERS, then
// the file's, each overriding the previous by key: an explicit header
// list replaces the SDK's own reading of the variables.
func mergedHeaders(file string, lookup func(string) (string, bool)) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range []string{config.EnvOTLPHeaders, config.EnvOTLPTracesHeaders} {
		raw, _ := lookup(name)
		h, err := config.ParseOTLPHeaders(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		maps.Copy(out, h)
	}
	h, err := readHeadersFile(file)
	if err != nil {
		return nil, err
	}
	maps.Copy(out, h)
	return out, nil
}

func readHeadersFile(path string) (map[string]string, error) {
	f, err := os.Open(path) // #nosec G304 -- TRACING_HEADERS_FILE, an absolute path the operator configured
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvTracingHeadersFile, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxHeadersFile+1))
	if err != nil {
		return nil, fmt.Errorf("%s: reading %s: %w", config.EnvTracingHeadersFile, path, err)
	}
	if len(raw) > maxHeadersFile {
		return nil, fmt.Errorf("%s: %s is larger than %d bytes", config.EnvTracingHeadersFile, path, maxHeadersFile)
	}
	// A Secret key often ends with a newline; entries may sit on lines.
	h, err := config.ParseOTLPHeaders(strings.ReplaceAll(strings.TrimSpace(string(raw)), "\n", ","))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.EnvTracingHeadersFile, err)
	}
	if len(h) == 0 {
		return nil, fmt.Errorf("%s: %s holds no header", config.EnvTracingHeadersFile, path)
	}
	return h, nil
}

// errorLog reports the SDK's errors (an unreachable collector, a refused
// batch) to the console's log, at most once per interval.
type errorLog struct {
	log   *slog.Logger
	every time.Duration
	mu    sync.Mutex
	last  time.Time
	quiet int
}

func (e *errorLog) Handle(err error) {
	e.mu.Lock()
	now := time.Now()
	if !e.last.IsZero() && now.Sub(e.last) < e.every {
		e.quiet++
		e.mu.Unlock()
		return
	}
	suppressed := e.quiet
	e.last, e.quiet = now, 0
	e.mu.Unlock()
	e.log.Warn("tracing export failed", "err", err, "suppressed", suppressed)
}
