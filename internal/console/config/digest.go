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
	"net"
	"net/mail"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// The daily digest. The page is on with
// persistent history; delivery is off until a channel is configured.
// Every credential (a webhook URL is one) is a file the chart mounts from
// a Secret: the console reads no Secret through the API, and a file is
// re-read on every delivery so a rotation needs no restart.
const (
	// EnvDigestEnabled makes a digest every day. The default follows
	// persistent history (HISTORY_PATH): with in-memory history each
	// replica would make its own, different digest and lose it on
	// restart, so it is on there only when set explicitly.
	EnvDigestEnabled = "DIGEST_ENABLED"
	// EnvDigestHour is the UTC hour (0-23) at which each digest closes
	// its 24 hours (default 7).
	EnvDigestHour = "DIGEST_HOUR"
	// EnvDigestRetention is how long digests are kept (default 720h).
	EnvDigestRetention = "DIGEST_RETENTION"
	// EnvDigestChatWebhookFile is a file holding a Slack or Mattermost
	// incoming-webhook URL (https); both take POST {"text": ...}.
	EnvDigestChatWebhookFile = "DIGEST_CHAT_WEBHOOK_URL_FILE"
	// EnvDigestWebhookFile is a file holding a generic webhook URL
	// (https) that receives the digest's JSON summary.
	EnvDigestWebhookFile = "DIGEST_WEBHOOK_URL_FILE"
	// EnvDigestSMTPAddress is the mail server, host:port.
	EnvDigestSMTPAddress = "DIGEST_SMTP_ADDRESS"
	// EnvDigestSMTPTLS is starttls (default) or tls (implicit TLS, port
	// 465). There is no plaintext mode.
	EnvDigestSMTPTLS = "DIGEST_SMTP_TLS"
	// EnvDigestSMTPUsername and EnvDigestSMTPPasswordFile authenticate
	// (PLAIN, over TLS only); set together or not at all.
	EnvDigestSMTPUsername     = "DIGEST_SMTP_USERNAME"
	EnvDigestSMTPPasswordFile = "DIGEST_SMTP_PASSWORD_FILE" // #nosec G101 -- the name of a variable holding a path, not a credential
	// EnvDigestSMTPFrom and EnvDigestSMTPTo are the sender and the
	// comma-separated recipients.
	EnvDigestSMTPFrom = "DIGEST_SMTP_FROM"
	EnvDigestSMTPTo   = "DIGEST_SMTP_TO"
	// EnvDigestDeliveryTimeout bounds one delivery attempt (default 10s).
	EnvDigestDeliveryTimeout = "DIGEST_DELIVERY_TIMEOUT"
	// EnvDigestLease names a coordination.k8s.io Lease in the console's
	// namespace: with it, every replica makes and keeps the
	// digests, and only the one holding the Lease delivers them; the
	// delivery marks are shared through STATE_CONFIGMAP, so a new holder
	// sends nothing twice. Unset, this console delivers alone: right for
	// one replica (and the local mode), wrong for several.
	EnvDigestLease = "DIGEST_LEASE"
)

// maxDigestRecipients bounds DIGEST_SMTP_TO: a digest is a team's
// handover, not a mailing list.
const maxDigestRecipients = 20

// Digest configures the daily digest.
type Digest struct {
	Enabled   bool
	Hour      int
	Retention time.Duration
	// ChatWebhookFile and WebhookFile are paths, never URLs.
	ChatWebhookFile string
	WebhookFile     string
	SMTP            SMTP
	Timeout         time.Duration
	// Lease is DIGEST_LEASE; empty when this console delivers alone.
	Lease string
}

// SMTP is the mail channel; Address empty disables it.
type SMTP struct {
	Address string
	// ImplicitTLS is DIGEST_SMTP_TLS=tls; otherwise STARTTLS is required.
	ImplicitTLS  bool
	Username     string
	PasswordFile string
	From         string
	To           []string
}

// Enabled reports whether mail is configured.
func (s SMTP) Enabled() bool { return s.Address != "" }

// Delivers reports whether a delivery channel is configured.
func (d Digest) Delivers() bool {
	return d.ChatWebhookFile != "" || d.WebhookFile != "" || d.SMTP.Enabled()
}

func digest(lookup Lookup, history History, fail func(name, constraint string)) Digest {
	historyEnabled := history.Enabled
	_, explicit := lookup(EnvDigestEnabled)
	d := Digest{
		Enabled:   boolVar(lookup, EnvDigestEnabled, history.Persistent(), fail),
		Hour:      intVar(lookup, EnvDigestHour, 7, 0, 23, fail),
		Retention: durationVar(lookup, EnvDigestRetention, 30*24*time.Hour, 24*time.Hour, 366*24*time.Hour, fail),
		Timeout:   durationVar(lookup, EnvDigestDeliveryTimeout, 10*time.Second, time.Second, time.Minute, fail),
	}
	if d.Enabled && !historyEnabled {
		if explicit {
			fail(EnvDigestEnabled, "requires "+EnvHistoryEnabled+"=true: the digest is made from the history")
		}
		d.Enabled = false
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
	d.ChatWebhookFile, d.WebhookFile = path(EnvDigestChatWebhookFile), path(EnvDigestWebhookFile)
	d.SMTP = smtpConfig(lookup, path, fail)
	if raw, _ := lookup(EnvDigestLease); raw != "" {
		if len(validation.IsDNS1123Subdomain(raw)) != 0 {
			fail(EnvDigestLease, "must be a Lease name")
		} else {
			d.Lease = raw
		}
	}
	delivers := d.Delivers()
	switch {
	case delivers && !history.Persistent():
		// A delivered digest links to its page, which an in-memory console
		// loses at its next restart, and a digest made from a memory that
		// began at the last restart is partial. Said first: it is the
		// cause when DIGEST_ENABLED defaulted off.
		fail(EnvHistoryPath, "is required by a digest delivery channel: a delivered digest links to its page, which in-memory history loses at the next restart")
	case delivers && !d.Enabled:
		fail(EnvDigestEnabled, "must be true when a delivery channel is configured")
	}
	return d
}

func smtpConfig(lookup Lookup, path func(string) string, fail func(name, constraint string)) SMTP {
	get := func(name string) string {
		raw, _ := lookup(name)
		return strings.TrimSpace(raw)
	}
	s := SMTP{Username: get(EnvDigestSMTPUsername), PasswordFile: path(EnvDigestSMTPPasswordFile)}
	addr, from, to := get(EnvDigestSMTPAddress), get(EnvDigestSMTPFrom), get(EnvDigestSMTPTo)
	switch mode := get(EnvDigestSMTPTLS); mode {
	case "", "starttls":
	case "tls":
		s.ImplicitTLS = true
	default:
		fail(EnvDigestSMTPTLS, "must be starttls or tls; mail is never sent in clear")
	}
	if addr == "" {
		if from != "" || to != "" || s.Username != "" || s.PasswordFile != "" {
			fail(EnvDigestSMTPAddress, "is required by the other DIGEST_SMTP_* variables")
		}
		return s
	}
	host, port, err := net.SplitHostPort(addr)
	if n, perr := strconv.Atoi(port); err != nil || host == "" || perr != nil || n < 1 || n > 65535 {
		fail(EnvDigestSMTPAddress, "must be host:port")
	} else {
		s.Address = addr
	}
	if (s.Username == "") != (s.PasswordFile == "") {
		fail(EnvDigestSMTPUsername, "and "+EnvDigestSMTPPasswordFile+" are set together or not at all")
	}
	if a, err := mail.ParseAddress(from); err != nil || a.Name != "" {
		fail(EnvDigestSMTPFrom, "must be one bare mail address")
	} else {
		s.From = a.Address
	}
	for part := range strings.SplitSeq(to, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		a, err := mail.ParseAddress(part)
		if err != nil || a.Name != "" {
			fail(EnvDigestSMTPTo, "must be a comma-separated list of bare mail addresses")
			return s
		}
		s.To = append(s.To, a.Address)
	}
	switch {
	case len(s.To) == 0:
		fail(EnvDigestSMTPTo, "must name at least one recipient")
	case len(s.To) > maxDigestRecipients:
		fail(EnvDigestSMTPTo, "must name at most "+strconv.Itoa(maxDigestRecipients)+" recipients")
	}
	return s
}

// info is the digest's part of Console.Info: paths and switches, never
// a URL, an address book or a password.
func (d Digest) info(into map[string]string) {
	into[EnvDigestEnabled] = strconv.FormatBool(d.Enabled)
	into[EnvDigestHour] = strconv.Itoa(d.Hour)
	into[EnvDigestRetention] = d.Retention.String()
	into[EnvDigestChatWebhookFile] = d.ChatWebhookFile
	into[EnvDigestWebhookFile] = d.WebhookFile
	into[EnvDigestSMTPAddress] = d.SMTP.Address
	into[EnvDigestLease] = d.Lease
}
