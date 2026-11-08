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
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDigestDefaults(t *testing.T) {
	cfg, err := LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a"}))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Digest
	if d.Enabled {
		t.Fatal("with in-memory history the digest is off unless asked for: each replica would make its own")
	}
	if d.Hour != 7 || d.Retention != 30*24*time.Hour || d.Timeout != 10*time.Second {
		t.Fatalf("defaults: %+v", d)
	}
	if d.ChatWebhookFile != "" || d.WebhookFile != "" || d.SMTP.Enabled() {
		t.Fatalf("delivery must be off by default: %+v", d)
	}
	if got := cfg.History.TimelinePath(); got != "" {
		t.Fatalf("in-memory history has no timeline file, got %q", got)
	}
}

func TestDigestDefaultFollowsPersistentHistory(t *testing.T) {
	cfg, err := LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a", EnvHistoryPath: "/var/lib/console/history.db"}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Digest.Enabled {
		t.Fatal("persistent history turns the digest on by default")
	}
	cfg, err = LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a", EnvDigestEnabled: "true"}))
	if err != nil || !cfg.Digest.Enabled {
		t.Fatalf("an explicit digest with in-memory history is allowed (the page only): %v", err)
	}
}

func TestDigestFollowsHistory(t *testing.T) {
	cfg, err := LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a", EnvHistoryEnabled: "false"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.Enabled {
		t.Fatal("the digest must be off when history is off")
	}
	_, err = LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a", EnvHistoryEnabled: "false", EnvDigestEnabled: "true"}))
	if err == nil {
		t.Fatal("an explicit digest without history must be refused")
	}
}

func TestTimelinePathBesideHistory(t *testing.T) {
	for path, want := range map[string]string{
		"/var/lib/console/history.db":  "/var/lib/console/timeline.db",
		"/var/lib/console/timeline.db": "/var/lib/console/timeline-journal.db",
	} {
		if got := (History{Path: path}).TimelinePath(); got != want {
			t.Errorf("TimelinePath(%s) = %s, want %s", path, got, want)
		}
	}
}

func TestDigestSMTP(t *testing.T) {
	base := map[string]string{
		EnvWatchNamespaces:        "team-a",
		EnvHistoryPath:            "/var/lib/console/history.db",
		EnvDigestSMTPAddress:      "smtp.example.com:587",
		EnvDigestSMTPFrom:         "pgtoolbox-console@example.com",
		EnvDigestSMTPTo:           "dba@example.com, oncall@example.com",
		EnvDigestSMTPUsername:     "pgtoolbox-console",
		EnvDigestSMTPPasswordFile: "/var/run/secrets/digest/password",
	}
	cfg, err := LoadConsole(lookupOf(base))
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Digest.SMTP; s.ImplicitTLS || s.Address != "smtp.example.com:587" || !slices.Equal(s.To, []string{"dba@example.com", "oncall@example.com"}) {
		t.Fatalf("smtp: %+v", s)
	}
	for _, tt := range []struct {
		name, key, value, wantVar string
	}{
		{"no plaintext", EnvDigestSMTPTLS, "none", EnvDigestSMTPTLS},
		{"bad address", EnvDigestSMTPAddress, "smtp.example.com", EnvDigestSMTPAddress},
		{"display name refused", EnvDigestSMTPFrom, "Console <pgtoolbox-console@example.com>", EnvDigestSMTPFrom},
		{"bad recipient", EnvDigestSMTPTo, "dba@example.com, not an address", EnvDigestSMTPTo},
		{"relative password file", EnvDigestSMTPPasswordFile, "password", EnvDigestSMTPPasswordFile},
		{"user without password", EnvDigestSMTPPasswordFile, "", EnvDigestSMTPUsername},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			env[tt.key] = tt.value
			_, err := LoadConsole(lookupOf(env))
			var ve *VarError
			if err == nil || !errors.As(err, &ve) || !strings.Contains(err.Error(), tt.wantVar) {
				t.Fatalf("err = %v, want a VarError naming %s", err, tt.wantVar)
			}
			if strings.Contains(err.Error(), tt.value) && tt.value != "" && tt.value != "none" && tt.value != "password" {
				t.Fatalf("the error echoes the value: %v", err)
			}
		})
	}
}

func TestDigestChannelsNeedTheDigest(t *testing.T) {
	_, err := LoadConsole(lookupOf(map[string]string{
		EnvWatchNamespaces: "team-a", EnvDigestEnabled: "false", EnvDigestChatWebhookFile: "/var/run/secrets/chat/url",
		EnvHistoryPath: "/var/lib/console/history.db",
	}))
	if err == nil || !strings.Contains(err.Error(), EnvDigestEnabled) {
		t.Fatalf("a channel with the digest off must be refused: %v", err)
	}
	_, err = LoadConsole(lookupOf(map[string]string{
		EnvWatchNamespaces: "team-a", EnvDigestChatWebhookFile: "/var/run/secrets/chat/url",
	}))
	if err == nil || !strings.Contains(err.Error(), EnvHistoryPath) {
		t.Fatalf("delivery with in-memory history must be refused (the digest's page would not survive a restart): %v", err)
	}
	cfg, err := LoadConsole(lookupOf(map[string]string{
		EnvWatchNamespaces: "team-a", EnvDigestChatWebhookFile: "/var/run/secrets/chat/url", EnvDigestHour: "23",
		EnvHistoryPath: "/var/lib/console/history.db",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest.ChatWebhookFile != "/var/run/secrets/chat/url" || cfg.Digest.Hour != 23 {
		t.Fatalf("%+v", cfg.Digest)
	}
	if info := cfg.Info(); info[EnvDigestChatWebhookFile] != "/var/run/secrets/chat/url" {
		t.Fatalf("info names the file: %v", info)
	}
	if _, err := LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a", EnvDigestHour: "24"})); err == nil {
		t.Fatal("hour 24 must be refused")
	}
}

func TestDigestLease(t *testing.T) {
	base := map[string]string{
		EnvWatchNamespaces: "team-a", EnvDigestChatWebhookFile: "/var/run/secrets/chat/url",
		EnvHistoryPath: "/var/lib/console/history.db",
	}
	cfg, err := LoadConsole(lookupOf(base))
	if err != nil || cfg.Digest.Lease != "" {
		t.Fatalf("without DIGEST_LEASE the console delivers alone: %+v %v", cfg.Digest, err)
	}
	base[EnvDigestLease] = "pgtoolbox-console-digest"
	cfg, err = LoadConsole(lookupOf(base))
	if err != nil || cfg.Digest.Lease != "pgtoolbox-console-digest" || cfg.Info()[EnvDigestLease] != "pgtoolbox-console-digest" {
		t.Fatalf("%+v %v", cfg.Digest, err)
	}
	base[EnvDigestLease] = "Not/A/Name"
	var ve *VarError
	if _, err := LoadConsole(lookupOf(base)); err == nil || !errors.As(err, &ve) || !strings.Contains(err.Error(), EnvDigestLease) {
		t.Fatalf("a bad Lease name must be refused: %v", err)
	}
}
