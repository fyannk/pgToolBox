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
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCLI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args []string
		run  error
		code int
		out  string
	}{
		{[]string{"--version"}, nil, 0, "dev\n"},
		{[]string{"--verbose"}, nil, 2, "configured by environment"},
		{nil, nil, 0, ""},
		{nil, errors.New("boom"), 1, "boom"},
	}
	for _, tc := range cases {
		var out, errOut bytes.Buffer
		code := cli(tc.args, &out, &errOut, func() error { return tc.run })
		if code != tc.code || !strings.Contains(out.String()+errOut.String(), tc.out) {
			t.Errorf("cli(%v) = %d %q %q", tc.args, code, out.String(), errOut.String())
		}
	}
}

// The chart's environment (charts/pgtools, token provider) loads.
var chartEnv = map[string]string{
	"POD_NAMESPACE":        "team-a",
	"WATCH_NAMESPACES":     "team-a",
	"LISTEN_ADDR":          ":3000",
	"PROBE_LISTEN_ADDR":    ":3001",
	"AUTH_PROVIDER":        "token",
	"SESSION_KEY_FILE":     "/var/run/console/session/session-key",
	"SESSION_IDLE_TIMEOUT": "8h",
	"REVOCATION_CONFIGMAP": "pgtoolbox-console-team-a-revocations",
}

func lookupOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoad(t *testing.T) {
	t.Parallel()
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range chartEnv {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			if kv[i+1] == "-" {
				delete(m, kv[i])
				continue
			}
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"chart environment", with(), ""},
		{"no watched namespace", with("WATCH_NAMESPACES", "-", "POD_NAMESPACE", "-", "REVOCATION_CONFIGMAP", "-"), "WATCH_NAMESPACES"},
		{"no provider", with("AUTH_PROVIDER", "-"), "AUTH_PROVIDER"},
		{"same listener twice", with("PROBE_LISTEN_ADDR", ":3000"), "must differ"},
		{"half a TLS pair", with("TLS_CERT_FILE", "/tls/tls.crt"), "together"},
		{"shutdown delay not a duration", with("SHUTDOWN_DELAY", "five"), "SHUTDOWN_DELAY"},
		{"negative shutdown delay", with("SHUTDOWN_DELAY", "-1s"), "SHUTDOWN_DELAY"},
		{"shutdown delay past the grace period", with("SHUTDOWN_DELAY", "30s"), "SHUTDOWN_DELAY"},
		{"no shutdown delay", with("SHUTDOWN_DELAY", "0s"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := load(lookupOf(tc.env))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error = %v, want it to mention %s", err, tc.want)
			case tc.want == "" && (s.ListenAddr != ":3000" || s.ProbeAddr != ":3001" || s.Auth.IdleTimeout != 8*time.Hour):
				t.Fatalf("settings = %+v", s)
			case tc.want == "" && tc.env["SHUTDOWN_DELAY"] == "" && s.ShutdownDelay != defaultShutdownDelay:
				t.Fatalf("settings = %+v", s)
			}
		})
	}
}
