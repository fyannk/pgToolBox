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
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/timeline"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

var t0 = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

type fakeSource struct {
	rows       []viewmodel.ClusterRow
	namespaces []viewmodel.NamespaceState
	entries    map[string][]viewmodel.TimelineEntry
	sources    []timeline.SourceFacts
}

func (f fakeSource) ClusterList(context.Context) (viewmodel.ClusterList, error) {
	return viewmodel.ClusterList{Items: f.rows, Namespaces: f.namespaces}, nil
}

func (f fakeSource) DigestEntries(_ context.Context, from, to time.Time) map[viewmodel.ClusterRef][]viewmodel.TimelineEntry {
	out := map[viewmodel.ClusterRef][]viewmodel.TimelineEntry{}
	for key, entries := range f.entries {
		ns, name, _ := strings.Cut(key, "/")
		for _, e := range entries {
			if !e.At.Before(from) && e.At.Before(to) {
				ref := viewmodel.ClusterRef{Namespace: ns, Name: name}
				out[ref] = append(out[ref], e)
			}
		}
	}
	return out
}

func (f fakeSource) SourceFacts() []timeline.SourceFacts { return f.sources }

func revision(kind string, at time.Time) viewmodel.TimelineEntry {
	e := entry("revision", "spec-changed", at)
	e.Subject = &viewmodel.ObjectRef{Kind: kind, Name: "x"}
	return e
}

func row(ns, name string, h viewmodel.Health, reasons ...string) viewmodel.ClusterRow {
	r := viewmodel.ClusterRow{Ref: viewmodel.ClusterRef{Namespace: ns, Name: name}}
	r.Verdict.Health = h
	for _, label := range reasons {
		r.Verdict.Contributors = append(r.Verdict.Contributors, viewmodel.Contributor{Label: label, Health: h})
	}
	return r
}

func entry(kind, action string, at time.Time) viewmodel.TimelineEntry {
	return viewmodel.TimelineEntry{ID: kind + action + at.String(), Kind: kind, Action: action, At: at}
}

type staticClock struct{ t time.Time }

func (c staticClock) Now() time.Time { return c.t }

func source() fakeSource {
	return fakeSource{
		rows: []viewmodel.ClusterRow{
			row("team-a", "quiet", viewmodel.HealthOK),
			row("team-a", "busy", viewmodel.HealthWarning, "Backups failing"),
			row("team-b", "lost", viewmodel.HealthUnknown, "No status"),
		},
		entries: map[string][]viewmodel.TimelineEntry{
			"team-a/busy": {
				revision("Cluster", t0.Add(-3*time.Hour)),
				revision("Pooler", t0.Add(-3*time.Hour)), // not the Cluster's own spec: not counted
				entry("finding", timeline.ActionAppeared, t0.Add(-2*time.Hour)),
				entry("finding", timeline.ActionFirstObserved, t0.Add(-2*time.Hour)),
				entry("primary", timeline.ActionFailover, t0.Add(-time.Hour)),
				entry("backup", timeline.ActionFailed, t0.Add(-30*time.Minute)),
				entry("backup", timeline.ActionCompleted, t0.Add(-20*time.Minute)),
				entry("event", "Warning", t0.Add(-10*time.Minute)),
				revision("Cluster", t0.Add(-25*time.Hour)), // before the window
			},
			"team-a/quiet": {entry("finding", timeline.ActionCleared, t0.Add(-time.Hour))},
		},
	}
}

func TestGenerate(t *testing.T) {
	j := timeline.New(timeline.Limits{}, staticClock{t0})
	j.Record(timeline.Event{Key: "console/start", Kind: timeline.KindConsole, Action: timeline.ActionStarted, At: t0.Add(-30 * time.Hour)})
	d, err := Generate(context.Background(), source(), j, t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "20261103T1000Z" || !d.From.Equal(t0.Add(-Window)) || d.Partial {
		t.Fatalf("headline: id=%s from=%s partial=%v", d.ID, d.From, d.Partial)
	}
	want := viewmodel.DigestCounts{Clusters: 3, Attention: 2, Revisions: 1, NewFindings: 1, ClearedFindings: 1, Failovers: 1, FailedBackups: 1}
	if d.Counts != want {
		t.Fatalf("counts = %+v, want %+v", d.Counts, want)
	}
	if len(d.Attention) != 2 || d.Clusters[0].Ref.Name != "busy" || d.Clusters[1].Ref.Name != "lost" || d.Clusters[2].Attention {
		t.Fatalf("order: %+v", d.Clusters)
	}
	if got := d.Clusters[0].Reasons; len(got) != 1 || got[0] != "Backups failing" {
		t.Fatalf("reasons: %v", got)
	}
	if n := len(d.Clusters[0].Entries); n != 4 {
		t.Fatalf("busy lists %d entries, want 4 (first-observed, a completed backup and Events are not digest news)", n)
	}
	if q := d.Clusters[2]; q.Counts.ClearedFindings != 1 || len(q.Entries) != 0 {
		t.Fatalf("a Cluster needing no attention carries counts only: %+v", q)
	}
}

// TestGenerateCapsEntries: a fleet's digest lists a bounded number of
// entries, the worst Clusters first; the counts stay whole.
func TestGenerateCapsEntries(t *testing.T) {
	src := fakeSource{entries: map[string][]viewmodel.TimelineEntry{}}
	for i := range 40 {
		name := fmt.Sprintf("c%02d", i)
		src.rows = append(src.rows, row("a", name, viewmodel.HealthCritical))
		for k := range 60 {
			src.entries["a/"+name] = append(src.entries["a/"+name], entry("backup", timeline.ActionFailed, t0.Add(-time.Duration(k+1)*time.Minute)))
		}
	}
	d, err := Generate(context.Background(), src, nil, t0, t0)
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, c := range d.Clusters {
		listed += len(c.Entries)
	}
	if listed != maxEntries || d.Counts.FailedBackups != 40*60 || !d.Clusters[0].Truncated || len(d.Clusters[39].Entries) != 0 {
		t.Fatalf("listed %d (want %d), counted %d, first truncated %v", listed, maxEntries, d.Counts.FailedBackups, d.Clusters[0].Truncated)
	}
}

func journalWith(events ...timeline.Event) *timeline.Journal {
	j := timeline.New(timeline.Limits{}, staticClock{t0.Add(time.Hour)})
	j.Record(timeline.Event{Key: "console/old", Kind: timeline.KindConsole, Action: timeline.ActionStarted, At: t0.Add(-48 * time.Hour)})
	for _, e := range events {
		j.Record(e)
	}
	return j
}

// TestGeneratePartialReasons: every way the console can miss part of the
// window makes the digest partial, and says which.
func TestGeneratePartialReasons(t *testing.T) {
	gap := func(action string, at time.Time, since time.Time) timeline.Event {
		return timeline.Event{Kind: timeline.KindGap, Action: action, Namespace: "team-a", Ref: "pods", At: at, Since: since, Timing: timeline.TimingObserved}
	}
	cases := []struct {
		name   string
		j      *timeline.Journal
		facts  []timeline.SourceFacts
		reason string
	}{
		{
			"restart after the window whose previous life ended inside it",
			journalWith(timeline.Event{Kind: timeline.KindConsole, Action: timeline.ActionStarted, At: t0.Add(5 * time.Minute), Since: t0.Add(-time.Hour)}), nil, PartialConsoleRestarted,
		},
		{
			"gap opened before the window and still open",
			journalWith(gap(timeline.ActionBegan, t0.Add(-30*time.Hour), time.Time{})), nil, PartialSourceGap,
		},
		{
			"gap opened before the window, closed inside it",
			journalWith(gap(timeline.ActionBegan, t0.Add(-30*time.Hour), time.Time{}), gap(timeline.ActionEnded, t0.Add(-20*time.Hour), t0.Add(-30*time.Hour))), nil, PartialSourceGap,
		},
		{
			"gap whose beginning was evicted: the end's since says it",
			journalWith(gap(timeline.ActionEnded, t0.Add(-20*time.Hour), t0.Add(-30*time.Hour))), nil, PartialSourceGap,
		},
		{
			"a namespace's Clusters unreadable",
			journalWith(),
			[]timeline.SourceFacts{{Namespace: "team-b", Source: "clusters", State: "denied"}},
			PartialClustersUnreadable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := source()
			src.sources = tc.facts
			d, err := Generate(context.Background(), src, tc.j, t0, t0.Add(10*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if !d.Partial || !slices.Contains(d.PartialReasons, tc.reason) {
				t.Fatalf("want partial for %s, got %v %v", tc.reason, d.Partial, d.PartialReasons)
			}
		})
	}
	// A Cluster list that failed on a live informer: the list says so,
	// and the digest cannot claim it saw every Cluster.
	src := source()
	src.namespaces = []viewmodel.NamespaceState{{Name: "team-b", Sources: []viewmodel.SourceState{{Source: "clusters", State: viewmodel.SourceUnavailable}}}}
	d, err := Generate(context.Background(), src, journalWith(), t0, t0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(d.PartialReasons, PartialClustersUnreadable) {
		t.Fatalf("a failed Cluster list read as a full digest: %v", d.PartialReasons)
	}
	// A gap opened and closed before the window, and a restart whose
	// previous life outlived the window, cost it nothing.
	j := journalWith(gap(timeline.ActionBegan, t0.Add(-40*time.Hour), time.Time{}), gap(timeline.ActionEnded, t0.Add(-30*time.Hour), t0.Add(-40*time.Hour)),
		timeline.Event{Kind: timeline.KindConsole, Action: timeline.ActionStarted, At: t0.Add(20 * time.Minute), Since: t0.Add(15 * time.Minute)})
	d, err = Generate(context.Background(), source(), j, t0, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if d.Partial {
		t.Fatalf("nothing missed, yet partial: %v", d.PartialReasons)
	}
}

// TestPartialIsNeverAnAllClear: a partial digest with nobody needing
// attention says it did not see everything, first.
func TestPartialIsNeverAnAllClear(t *testing.T) {
	m := message()
	m.Summary.Counts.Attention, m.Summary.Attention = 0, nil
	m.Summary.Partial, m.Summary.PartialReasons = true, []string{PartialConsoleRestarted}
	text := m.Text()
	if strings.Contains(text, "none of 3 Clusters needs attention") || !strings.Contains(text, "PARTIAL (the console was down during the window)") ||
		!strings.Contains(text, "did not see everything") {
		t.Fatalf("a partial digest read as an all-clear: %s", text)
	}
	if !strings.Contains(m.Subject(), "(partial)") {
		t.Fatalf("subject: %s", m.Subject())
	}
	m.Summary.GeneratedAt = m.Summary.To.Add(3 * time.Hour)
	if !strings.Contains(m.Text(), "Health and attention as of") {
		t.Fatal("a catch-up digest dates its health")
	}
}

func TestGeneratePartial(t *testing.T) {
	j := timeline.New(timeline.Limits{}, staticClock{t0})
	j.Record(timeline.Event{Key: "console/start", Kind: timeline.KindConsole, Action: timeline.ActionStarted, At: t0.Add(-2 * time.Hour)})
	d, err := Generate(context.Background(), source(), j, t0, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Partial || d.ObservedSince == nil || len(d.ConsoleGaps) != 1 {
		t.Fatalf("a console that started in the window is partial: %+v", d.DigestSummary)
	}
	d, _ = Generate(context.Background(), source(), nil, t0, t0)
	if !d.Partial {
		t.Fatal("without a journal the digest cannot claim a full window")
	}
}

type memPersister struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *memPersister) LoadDigests() ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, 0, len(m.data))
	for _, v := range m.data {
		out = append(out, v)
	}
	return out, nil
}

func (m *memPersister) PutDigest(id string, raw []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[id] = raw
}

func (m *memPersister) DeleteDigest(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, id)
}

func TestStoreRetentionAndPersistence(t *testing.T) {
	p := &memPersister{data: map[string][]byte{"broken": []byte("{")}}
	s, err := NewStore(72*time.Hour, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for day := 5; day >= 0; day-- {
		to := t0.AddDate(0, 0, -day)
		s.Add(viewmodel.Digest{DigestSummary: viewmodel.DigestSummary{ID: to.Format(IDLayout), To: to}}, t0)
	}
	all := s.All()
	if len(all) != 4 || all[0].ID != "20261103T1000Z" || all[3].ID != "20261031T1000Z" {
		t.Fatalf("kept %d digests: %v", len(all), all)
	}
	if !s.Has(t0) || s.Has(t0.AddDate(0, 0, -5)) {
		t.Fatal("Has")
	}
	reloaded, err := NewStore(72*time.Hour, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.All(); len(got) != 4 || got[0].ID != all[0].ID {
		t.Fatalf("reloaded %d digests", len(got))
	}
}

// stepClock moves time forward on every Wait; the scheduler's long waits
// are counted so the test can end the run.
type stepClock struct {
	mu     sync.Mutex
	now    time.Time
	long   int
	stopAt int
	// gate holds the first long wait until the test opens it.
	gate chan struct{}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) Wait(ctx context.Context, d time.Duration) error {
	if d > time.Hour && c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	if d > time.Hour {
		c.long++
		if c.long >= c.stopAt {
			c.mu.Unlock()
			<-ctx.Done() // the test cancels once deliveries settled
			return ctx.Err()
		}
	}
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

type fakeChannel struct {
	name  string
	mu    sync.Mutex
	fails int
	sent  []Message
}

func (f *fakeChannel) Name() string { return f.name }

func (f *fakeChannel) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return errors.New("HTTP 502")
	}
	f.sent = append(f.sent, m)
	return nil
}

func TestServiceSchedulesAndRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &stepClock{now: t0, stopAt: 2, gate: make(chan struct{})}
	store, _ := NewStore(30*24*time.Hour, &memPersister{data: map[string][]byte{}}, nil)
	chat := &fakeChannel{name: "chat", fails: 2}
	mail := &fakeChannel{name: "mail", fails: 10}
	s := NewService(Options{
		Hour: 7, Retention: 30 * 24 * time.Hour, Persistent: true, Console: "team-a", PublicURL: "https://console.example.com/",
		Backoff: []time.Duration{0, time.Second, time.Second}, Clock: clock, Source: source(), Store: store,
		Channels: []Channel{chat, mail},
	})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	settle := func(id string) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			settled := true
			for _, c := range s.Status().Channels {
				if c.Configured && (c.Pending || c.DigestID != id) {
					settled = false
				}
			}
			if settled || time.Now().After(deadline) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	settle("20261103T0700Z")
	close(clock.gate)
	settle("20261104T0700Z")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	all := store.All()
	if len(all) != 2 || all[1].ID != "20261103T0700Z" || all[0].ID != "20261104T0700Z" {
		t.Fatalf("digests: %v", all)
	}
	if len(chat.sent) != 2 || chat.sent[0].URL != "https://console.example.com/digests/20261103T0700Z" {
		t.Fatalf("chat got %d messages: %+v", len(chat.sent), chat.sent)
	}
	st := s.Status()
	byName := map[string]viewmodel.DeliveryState{}
	for _, c := range st.Channels {
		byName[c.Channel] = c
	}
	if c := byName["chat"]; !c.Configured || c.LastSuccess == nil || c.LastError != "" || c.Pending {
		t.Fatalf("chat state: %+v", c)
	}
	if c := byName["mail"]; c.LastSuccess != nil || c.Attempts != 3 || c.LastError != "HTTP 502" || c.Pending {
		t.Fatalf("mail state: %+v", c)
	}
	if byName["webhook"].Configured {
		t.Fatal("an unset channel is not configured")
	}
	if st.Last != "20261104T0700Z" || st.Schedule.Hour != 7 || st.Schedule.Next == nil {
		t.Fatalf("status: %+v", st)
	}
}

func TestSlot(t *testing.T) {
	if got := Slot(t0, 7); !got.Equal(time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
	if got := Slot(t0, 23); !got.Equal(time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
	if Link("", "x") != "" {
		t.Fatal("no public URL, no link")
	}
}

func message() Message {
	return Message{Console: "team-a", URL: "https://console.example.com/digests/20261103T0700Z", Summary: viewmodel.DigestSummary{
		ID: "20261103T0700Z", To: t0, Counts: viewmodel.DigestCounts{Clusters: 3, Attention: 1, Failovers: 1},
		Attention: []viewmodel.ClusterRef{{Namespace: "team-a", Name: "busy"}},
	}}
}

func TestMessageText(t *testing.T) {
	text := message().Text()
	for _, want := range []string{"pgToolBox console team-a", "1 of 3 Clusters need attention (team-a/busy)", "1 failovers", "https://console.example.com/digests/20261103T0700Z"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q: %s", want, text)
		}
	}
}

func urlFile(t *testing.T, url string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "url")
	if err := os.WriteFile(p, []byte(url+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHTTPChannels(t *testing.T) {
	var got []map[string]any
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/fail") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/redirect") {
			http.Redirect(w, r, "https://elsewhere.example.com/", http.StatusFound)
			return
		}
	}))
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = NewHTTPClient().CheckRedirect
	ctx := context.Background()

	if err := NewChatChannel(urlFile(t, srv.URL+"/hooks/T0K3N"), client).Send(ctx, message()); err != nil {
		t.Fatal(err)
	}
	if err := NewWebhookChannel(urlFile(t, srv.URL+"/hooks/T0K3N"), client).Send(ctx, message()); err != nil {
		t.Fatal(err)
	}
	if text, _ := got[0]["text"].(string); len(got[0]) != 1 || !strings.Contains(text, "team-a/busy") {
		t.Fatalf("chat payload: %v", got[0])
	}
	if got[1]["type"] != "pgtoolbox-console.digest" || got[1]["id"] != "20261103T0700Z" {
		t.Fatalf("webhook payload: %v", got[1])
	}
	for _, path := range []string{"/hooks/T0K3N/fail", "/hooks/T0K3N/redirect"} {
		err := NewChatChannel(urlFile(t, srv.URL+path), client).Send(ctx, message())
		if err == nil || strings.Contains(err.Error(), "T0K3N") {
			t.Fatalf("%s: err = %v", path, err)
		}
	}
	if err := NewChatChannel(urlFile(t, "http://hooks.example.com/T0K3N"), client).Send(ctx, message()); !errors.Is(err, errURL) {
		t.Fatalf("plain http must be refused: %v", err)
	}
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	closed.Close()
	err := NewChatChannel(urlFile(t, closed.URL+"/hooks/T0K3N"), client).Send(ctx, message())
	if err == nil || strings.Contains(err.Error(), "T0K3N") || strings.Contains(err.Error(), closed.URL) {
		t.Fatalf("a transport error must not carry the URL: %v", err)
	}
}

// fakeSMTP serves one session: it offers STARTTLS only when told, and
// accepts AUTH PLAIN and one message.
func fakeSMTP(t *testing.T, ln net.Listener, offerStartTLS bool, data chan<- string, rcptReply ...string) {
	t.Helper()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		write := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
		write("220 fake ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				if offerStartTLS {
					write("250-fake")
					write("250 STARTTLS")
				} else {
					write("250-fake")
					write("250 AUTH PLAIN")
				}
			case strings.HasPrefix(cmd, "AUTH"):
				write("235 ok")
			case strings.HasPrefix(cmd, "RCPT") && len(rcptReply) > 0:
				for _, l := range rcptReply {
					write(l)
				}
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				write("250 ok")
			case cmd == "DATA":
				write("354 go")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				data <- b.String()
				write("250 queued")
			case cmd == "QUIT":
				write("221 bye")
				return
			default:
				write("502 no")
			}
		}
	}()
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestMailRefusesPlaintext(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	fakeSMTP(t, ln, false, make(chan string, 1))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = NewMailChannel(MailOptions{Address: ln.Addr().String(), From: "pgtoolbox-console@example.com", To: []string{"dba@example.com"}}).Send(ctx, message())
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("a server without STARTTLS must be refused: %v", err)
	}
}

func TestMailImplicitTLS(t *testing.T) {
	cert, pool := selfSigned(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	data := make(chan string, 1)
	fakeSMTP(t, ln, false, data)
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = NewMailChannel(MailOptions{
		Address: ln.Addr().String(), ImplicitTLS: true, Username: "pgtoolbox-console", PasswordFile: password,
		From: "pgtoolbox-console@example.com", To: []string{"dba@example.com"}, RootCAs: pool,
	}).Send(ctx, message())
	if err != nil {
		t.Fatal(err)
	}
	msg := <-data
	for _, want := range []string{"Subject: ", "Content-Transfer-Encoding: quoted-printable", "team-a/busy"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

// TestMailReplyKeepsNoAddress: a refusal is reported as its code and
// first line, without the addresses servers like to echo.
func TestMailReplyKeepsNoAddress(t *testing.T) {
	cert, pool := selfSigned(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	fakeSMTP(t, ln, false, make(chan string, 1), "550-5.1.1 <dba@example.com>: Recipient address rejected; user dba@example.com unknown", "550 5.1.1 see https://help.example.com/dba@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = NewMailChannel(MailOptions{
		Address: ln.Addr().String(), ImplicitTLS: true, From: "pgtoolbox-console@example.com", To: []string{"dba@example.com"}, RootCAs: pool,
	}).Send(ctx, message())
	if err == nil {
		t.Fatal("a refused recipient must fail")
	}
	got := err.Error()
	if strings.Contains(got, "@") || strings.Contains(got, "help.example.com") || !strings.Contains(got, "SMTP 550") {
		t.Fatalf("the reply must be its code and first line, without addresses: %q", got)
	}
}

// TestMailHeadersAreRebuilt: the From and To headers are quoted from the
// bare addresses, so an address needing quotes cannot break the header.
func TestMailHeadersAreRebuilt(t *testing.T) {
	c := &mailChannel{o: MailOptions{From: "pg console@example.com", To: []string{"dba@example.com", "on call@example.com"}}}
	msg := string(c.message(message()))
	for _, want := range []string{"From: <\"pg console\"@example.com>\r\n", "To: <dba@example.com>, <\"on call\"@example.com>\r\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("headers lack %q:\n%s", want, msg)
		}
	}
}

// scriptClock advances by each wait (plus a scripted jump: a suspend)
// and ends the run at the stopAt-th wait.
type scriptClock struct {
	mu     sync.Mutex
	now    time.Time
	waits  []time.Duration
	jumps  map[int]time.Duration
	stopAt int
	cancel context.CancelFunc
}

func (c *scriptClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *scriptClock) Wait(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	if len(c.waits) >= c.stopAt {
		c.cancel()
		return context.Canceled
	}
	c.now = c.now.Add(d + c.jumps[len(c.waits)-1])
	return ctx.Err()
}

func runScript(t *testing.T, clock *scriptClock, o Options) *Store {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock.cancel = cancel
	if o.Store == nil {
		o.Store, _ = NewStore(30*24*time.Hour, &memPersister{data: map[string][]byte{}}, nil)
	}
	o.Hour, o.Clock, o.Backoff = 7, clock, []time.Duration{0}
	if o.Source == nil {
		o.Source = source()
	}
	if err := NewService(o).Run(ctx); err != nil {
		t.Fatal(err)
	}
	return o.Store
}

func ids(ds []viewmodel.Digest) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

// TestServiceWaitsForTheCache: before the first sync the cluster list is
// empty, and a digest made then would be an all-clear.
func TestServiceWaitsForTheCache(t *testing.T) {
	polls := 0
	clock := &scriptClock{now: t0, stopAt: 3}
	store := runScript(t, clock, Options{Ready: func() bool { polls++; return polls > 2 }})
	if clock.waits[0] != readyPoll || clock.waits[1] != readyPoll {
		t.Fatalf("waits: %v", clock.waits)
	}
	all := store.All()
	if len(all) != 1 || all[0].Counts.Clusters != 3 || !all[0].GeneratedAt.Equal(t0.Add(2*readyPoll)) {
		t.Fatalf("the digest must be made after the sync: %+v", all)
	}
}

// TestServiceGraceAndCatchUp: a slot's digest is made Grace after it, and
// after a long suspend only the latest missed slot is made.
func TestServiceGraceAndCatchUp(t *testing.T) {
	start := time.Date(2026, 11, 3, 7, 5, 0, 0, time.UTC) // inside the 07:00 slot's grace
	clock := &scriptClock{now: start, stopAt: 3, jumps: map[int]time.Duration{1: 5 * 24 * time.Hour}}
	store := runScript(t, clock, Options{})
	if clock.waits[0] != 5*time.Minute {
		t.Fatalf("the 07:00 digest is made at 07:10, waited %v", clock.waits[0])
	}
	got := ids(store.All())
	want := []string{"20261109T0700Z", "20261103T0700Z", "20261102T0700Z"}
	if !slices.Equal(got, want) {
		t.Fatalf("digests %v, want %v (one catch-up after the suspend, none per missed day)", got, want)
	}
}

// TestServiceResumesUnsentDelivery: after a restart, a channel that had
// delivered the current digest is not sent it again; one that had not is.
func TestServiceResumesUnsentDelivery(t *testing.T) {
	store, _ := NewStore(30*24*time.Hour, &memPersister{data: map[string][]byte{}}, nil)
	due := time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)
	d, err := Generate(context.Background(), source(), nil, due, due.Add(DefaultGrace))
	if err != nil {
		t.Fatal(err)
	}
	store.Add(d, due)
	store.MarkSent(d.ID, "chat", due.Add(DefaultGrace))
	for range 2 { // two restarts
		chat, mail := &fakeChannel{name: "chat"}, &fakeChannel{name: "mail"}
		runScript(t, &scriptClock{now: t0, stopAt: 1}, Options{Persistent: true, Store: store, Channels: []Channel{chat, mail}})
		if len(chat.sent) != 0 {
			t.Fatal("a delivered digest was sent again after a restart")
		}
		if _, sent := store.Sent(d.ID, "mail"); !sent {
			t.Fatal("the undelivered channel must be resumed and marked")
		}
		if len(mail.sent) > 1 {
			t.Fatalf("mail got %d", len(mail.sent))
		}
	}
	// The marks survive the file.
	p := store.persist.(*memPersister)
	reloaded, _ := NewStore(30*24*time.Hour, p, nil)
	if _, sent := reloaded.Sent(d.ID, "chat"); !sent {
		t.Fatal("delivery marks must be persisted with the digest")
	}
}
