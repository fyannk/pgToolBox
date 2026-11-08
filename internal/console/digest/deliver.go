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

package digest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Message is what every channel sends: a summary and a link, no detail.
type Message struct {
	// Console names the console (its namespace), so a channel fed by
	// several teams' consoles can tell them apart.
	Console string
	Summary viewmodel.DigestSummary
	// URL is the digest's page; empty when PUBLIC_URL is not set.
	URL string
}

// maxNamed bounds the Cluster names a message carries.
const maxNamed = 10

// Text is the one-paragraph summary chat and mail carry.
func (m Message) Text() string {
	s := m.Summary
	var b strings.Builder
	fmt.Fprintf(&b, "pgToolBox console %s, daily digest for the 24 h to %s: ", m.Console, s.To.UTC().Format("2006-01-02 15:04 UTC"))
	if s.Partial {
		// Said first, so a partial digest is never read as an all-clear.
		fmt.Fprintf(&b, "PARTIAL (%s). ", partialWords(s.PartialReasons))
	}
	switch {
	case s.Counts.Attention == 0 && s.Partial:
		fmt.Fprintf(&b, "None of the %d Clusters the console could see needs attention, but it did not see everything.", s.Counts.Clusters)
	case s.Counts.Attention == 0:
		fmt.Fprintf(&b, "none of %d Clusters needs attention.", s.Counts.Clusters)
	default:
		names := make([]string, 0, maxNamed)
		for i, r := range s.Attention {
			if i == maxNamed {
				names = append(names, "and "+strconv.Itoa(len(s.Attention)-maxNamed)+" more")
				break
			}
			names = append(names, r.Namespace+"/"+r.Name)
		}
		fmt.Fprintf(&b, "%d of %d Clusters need attention (%s).", s.Counts.Attention, s.Counts.Clusters, strings.Join(names, ", "))
	}
	c := s.Counts
	fmt.Fprintf(&b, " %d changes, %d new findings, %d worse, %d cleared, %d failovers, %d switchovers, %d failed backups, %d container restarts.",
		c.Revisions, c.NewFindings, c.WorseFindings, c.ClearedFindings, c.Failovers, c.Switchovers, c.FailedBackups, c.Restarts)
	if Late(s) {
		fmt.Fprintf(&b, " Health and attention as of %s.", s.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if m.URL != "" {
		b.WriteString(" " + m.URL)
	} else {
		b.WriteString(" Open Digests in the pgToolBox console for the details.")
	}
	return b.String()
}

// Subject is the mail subject.
func (m Message) Subject() string {
	partial := ""
	if m.Summary.Partial {
		partial = " (partial)"
	}
	return fmt.Sprintf("pgToolBox console %s digest %s%s: %d need attention", m.Console, m.Summary.To.UTC().Format("2006-01-02"), partial, m.Summary.Counts.Attention)
}

// lateAfter is how long after its window a digest is a catch-up whose
// health and attention describe its generation time, not the window's end.
const lateAfter = time.Hour

// Late reports a catch-up digest: made well after its window closed.
func Late(s viewmodel.DigestSummary) bool { return s.GeneratedAt.Sub(s.To) > lateAfter }

func partialWords(reasons []string) string {
	words := map[string]string{
		PartialNotObserved:        "the console's memory begins after the window did",
		PartialConsoleRestarted:   "the console was down during the window",
		PartialSourceGap:          "a source was not live during the window",
		PartialClustersUnreadable: "a namespace's Clusters cannot be read",
		PartialNoTimeline:         "no timeline",
	}
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		if w, ok := words[r]; ok {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return "the console did not observe the whole window"
	}
	return strings.Join(out, "; ")
}

// webhookPayload is the generic webhook's JSON: counts and names only.
type webhookPayload struct {
	Type    string    `json:"type"`
	Version int       `json:"version"`
	Console string    `json:"console"`
	ID      string    `json:"id"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Partial bool      `json:"partial"`
	// PartialReasons are the codes of viewmodel.DigestSummary.
	PartialReasons []string               `json:"partialReasons,omitempty"`
	Counts         viewmodel.DigestCounts `json:"counts"`
	Attention      []string               `json:"attention"`
	URL            string                 `json:"url,omitempty"`
	Text           string                 `json:"text"`
}

// Channel delivers one message.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// httpChannel posts JSON to a URL read from a file at each send.
type httpChannel struct {
	name    string
	file    string
	client  *http.Client
	payload func(Message) any
}

// NewChatChannel posts {"text": ...}, the incoming-webhook payload Slack
// and Mattermost both accept.
func NewChatChannel(file string, client *http.Client) Channel {
	return &httpChannel{name: "chat", file: file, client: client, payload: func(m Message) any {
		return map[string]string{"text": m.Text()}
	}}
}

// NewWebhookChannel posts the digest's JSON summary.
func NewWebhookChannel(file string, client *http.Client) Channel {
	return &httpChannel{name: "webhook", file: file, client: client, payload: func(m Message) any {
		names := make([]string, 0, len(m.Summary.Attention))
		for _, r := range m.Summary.Attention {
			names = append(names, r.Namespace+"/"+r.Name)
		}
		return webhookPayload{
			Type: "pgtoolbox-console.digest", Version: 1, Console: m.Console, ID: m.Summary.ID, From: m.Summary.From, To: m.Summary.To,
			Partial: m.Summary.Partial, PartialReasons: m.Summary.PartialReasons, Counts: m.Summary.Counts, Attention: names, URL: m.URL, Text: m.Text(),
		}
	}}
}

// NewHTTPClient is the delivery client: no redirects (a redirected
// webhook is a misconfiguration, and following it could send the
// payload elsewhere), the environment's proxy, TLS 1.2 at least.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (c *httpChannel) Name() string { return c.name }

// errURL is the only thing said about a URL that cannot be used: the
// URL is the credential and never appears in an error.
var errURL = errors.New("the webhook URL file does not hold one https URL")

func (c *httpChannel) Send(ctx context.Context, m Message) error {
	target, err := readURL(c.file)
	if err != nil {
		return err
	}
	body, err := json.Marshal(c.payload(m))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return errURL
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pgtoolbox-console-digest")
	resp, err := c.client.Do(req)
	if err != nil {
		return scrub(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the webhook answered HTTP %d", resp.StatusCode)
	}
	return nil
}

func readURL(file string) (string, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // the path is configuration (DIGEST_*_URL_FILE), validated absolute and clean
	if err != nil {
		return "", errors.New("the webhook URL file cannot be read")
	}
	target := strings.TrimSpace(string(raw))
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(target) > 4096 {
		return "", errURL
	}
	return target, nil
}

// scrub drops the URL from a transport error: url.Error prints it.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("the webhook did not answer in time")
	}
	return fmt.Errorf("the webhook could not be reached: %s", bounded(err.Error()))
}

// mailAddress matches what may be an address in a server reply.
var mailAddress = regexp.MustCompile(`<[^>]*>|[^\s<>"(),;:]+@[^\s<>"(),;:]+`)

// smtpReply is what the admin page and the logs say of a mail server's
// answer: its status code and first line, addresses removed (a
// recipient's address is not the reader's business).
func smtpReply(err error) string {
	msg := err.Error()
	var te *textproto.Error
	if errors.As(err, &te) {
		msg = fmt.Sprintf("SMTP %d %s", te.Code, te.Msg)
	}
	line, _, _ := strings.Cut(msg, "\n")
	return bounded(strings.TrimSpace(mailAddress.ReplaceAllString(line, "<address>")))
}

func bounded(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// MailOptions configure the mail channel.
type MailOptions struct {
	Address      string
	ImplicitTLS  bool
	Username     string
	PasswordFile string
	From         string
	To           []string
	// RootCAs overrides the system roots (tests).
	RootCAs *x509.CertPool
}

type mailChannel struct{ o MailOptions }

// NewMailChannel sends by SMTP over TLS: implicit TLS, or STARTTLS which
// the server must offer; there is no plaintext fallback.
func NewMailChannel(o MailOptions) Channel { return &mailChannel{o: o} }

func (c *mailChannel) Name() string { return "mail" }

func (c *mailChannel) Send(ctx context.Context, m Message) error {
	host, _, err := net.SplitHostPort(c.o.Address)
	if err != nil {
		return errors.New("the mail server address is not host:port")
	}
	tlsConf := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: c.o.RootCAs}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	dialer := &net.Dialer{Deadline: deadline}
	var conn net.Conn
	if c.o.ImplicitTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConf}).DialContext(ctx, "tcp", c.o.Address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", c.o.Address)
	}
	if err != nil {
		return fmt.Errorf("the mail server could not be reached: %s", bounded(err.Error()))
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("the mail server greeting failed: %s", smtpReply(err))
	}
	defer func() { _ = client.Close() }()
	if !c.o.ImplicitTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("the mail server does not offer STARTTLS; the digest is never sent in clear")
		}
		if err := client.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("STARTTLS failed: %s", smtpReply(err))
		}
	}
	if c.o.Username != "" {
		password, err := os.ReadFile(c.o.PasswordFile) //nolint:gosec // configuration path (DIGEST_SMTP_PASSWORD_FILE)
		if err != nil {
			return errors.New("the SMTP password file cannot be read")
		}
		if err := client.Auth(smtp.PlainAuth("", c.o.Username, strings.TrimRight(string(password), "\r\n"), host)); err != nil {
			return errors.New("the mail server refused the credentials")
		}
	}
	if err := client.Mail(c.o.From); err != nil {
		return fmt.Errorf("the mail server refused the sender: %s", smtpReply(err))
	}
	for _, to := range c.o.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("the mail server refused a recipient: %s", smtpReply(err))
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("the mail server refused the message: %s", smtpReply(err))
	}
	if _, err := w.Write(c.message(m)); err != nil {
		return fmt.Errorf("writing the message failed: %s", smtpReply(err))
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the mail server refused the message: %s", smtpReply(err))
	}
	_ = client.Quit()
	return nil
}

func (c *mailChannel) message(m Message) []byte {
	var b bytes.Buffer
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	// The headers are rebuilt from the bare addresses, quoted as RFC 5322
	// wants, never copied from configuration as written.
	header("From", (&mail.Address{Address: c.o.From}).String())
	to := make([]string, 0, len(c.o.To))
	for _, a := range c.o.To {
		to = append(to, (&mail.Address{Address: a}).String())
	}
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject()))
	header("Date", m.Summary.GeneratedAt.UTC().Format(time.RFC1123Z))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	header("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(m.Text() + "\r\n"))
	_ = qp.Close()
	return b.Bytes()
}
