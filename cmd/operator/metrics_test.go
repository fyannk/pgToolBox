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

package main

import (
	"crypto/tls"
	"testing"
)

// TestMetricsOptionsAreSecure: the operator's /metrics is never plain
// HTTP and never unauthenticated, whatever the flags say.
func TestMetricsOptionsAreSecure(t *testing.T) {
	for _, certDir := range []string{"", "/var/run/metrics-cert"} {
		options := metricsOptions(":8080", certDir)
		if !options.SecureServing {
			t.Fatal("metrics are served over plain HTTP")
		}
		if options.FilterProvider == nil {
			t.Fatal("metrics are served without authentication and authorization")
		}
		if options.CertDir != certDir || options.BindAddress != ":8080" {
			t.Fatalf("options = %+v", options)
		}
		config := &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
		for _, apply := range options.TLSOpts {
			apply(config)
		}
		if config.MinVersion < tls.VersionTLS12 || len(config.NextProtos) != 1 || config.NextProtos[0] != "http/1.1" {
			t.Fatalf("TLS config: min %x, protocols %v", config.MinVersion, config.NextProtos)
		}
	}
}
