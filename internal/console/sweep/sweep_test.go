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

package sweep

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// pki is a throwaway CA and a server certificate it signed.
type pki struct {
	caPEM  []byte
	server tls.Certificate
}

func newPKI(t *testing.T, dnsNames ...string) pki {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "server"}, DNSNames: dnsNames,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pki{
		caPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		server: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
	}
}

const report = `{"isPrimary":%t,"currentLsn":"0/3000000","replayLsn":"%s","readyWalFiles":%d,"instanceManagerVersion":"1.30.0","timeLineID":1}`

// statusServer serves a /pg/status body over TLS 1.3 with the PKI's
// certificate, counting requests.
func statusServer(t *testing.T, p pki, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{p.server}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &hits
}

type staticRoster []Target

func (r staticRoster) Targets(context.Context) []Target { return r }

func bundleFile(t *testing.T, pems ...[]byte) *CABundle {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	var all []byte
	for _, p := range pems {
		all = append(all, p...)
	}
	if err := os.WriteFile(path, all, 0o600); err != nil {
		t.Fatal(err)
	}
	return NewCABundle(path, nil)
}

func port(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sweeper(roster Roster, bundle *CABundle, statusPort string, now func() time.Time) *Sweeper {
	return New(roster, Options{
		Interval: 5 * time.Second, Timeout: 2 * time.Second, Concurrency: 4, Retention: time.Hour,
		Bundle: bundle, StatusPort: statusPort, Now: now,
	})
}

var key = Key{Namespace: "team-a", Name: "orders"}

func TestSweepOutcomes(t *testing.T) {
	good := newPKI(t, "orders-rw.team-a.svc")
	other := newPKI(t, "orders-rw.team-a.svc")
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintf(w, report, true, "", 0) }
	srv, hits := statusServer(t, good, ok)

	tests := []struct {
		name       string
		bundle     *CABundle
		serverName string
		pod        PodTarget
		handler    http.HandlerFunc
		wantState  viewmodel.StatusReadState
		wantReason string
		wantHits   int32
	}{
		{name: "verified read", bundle: bundleFile(t, good.caPEM), serverName: "orders-rw.team-a.svc", wantState: viewmodel.StatusReadOK, wantHits: 1},
		{name: "CA not in the bundle", bundle: bundleFile(t, other.caPEM), serverName: "orders-rw.team-a.svc", wantState: viewmodel.StatusReadNotVerifiable, wantReason: ReasonCANotInBundle, wantHits: 0},
		{name: "bundle of several CAs", bundle: bundleFile(t, other.caPEM, good.caPEM), serverName: "orders-rw.team-a.svc", wantState: viewmodel.StatusReadOK, wantHits: 1},
		{name: "certificate names another cluster", bundle: bundleFile(t, good.caPEM), serverName: "billing-rw.team-a.svc", wantState: viewmodel.StatusReadTLSError, wantReason: "does not name billing-rw.team-a.svc"},
		{name: "pod without --status-port-tls is never contacted", bundle: bundleFile(t, good.caPEM), serverName: "orders-rw.team-a.svc", pod: PodTarget{StatusPortTLS: false, IP: "127.0.0.1"}, wantState: viewmodel.StatusReadNoTLS, wantReason: ReasonNoTLS},
		{name: "no bundle configured is never contacted", bundle: NewCABundle("", nil), serverName: "orders-rw.team-a.svc", wantState: viewmodel.StatusReadNotVerifiable, wantReason: "no CA bundle configured"},
		{name: "no server name is never contacted", bundle: bundleFile(t, good.caPEM), serverName: "", wantState: viewmodel.StatusReadNotVerifiable, wantReason: ReasonNoServerName},
		{name: "pod without IP", bundle: bundleFile(t, good.caPEM), serverName: "x", pod: PodTarget{StatusPortTLS: true}, wantState: viewmodel.StatusReadNotRunning, wantReason: ReasonNoIP},
		{name: "deleting pod", bundle: bundleFile(t, good.caPEM), serverName: "x", pod: PodTarget{StatusPortTLS: true, IP: "127.0.0.1", Deleting: true}, wantState: viewmodel.StatusReadNotRunning, wantReason: ReasonDeleting},
		{name: "server error", bundle: bundleFile(t, good.caPEM), serverName: "orders-rw.team-a.svc", handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, wantState: viewmodel.StatusReadBadResponse, wantReason: "HTTP status 500"},
		{name: "oversized report", bundle: bundleFile(t, good.caPEM), serverName: "orders-rw.team-a.svc", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{"isPrimary":true,"node":"%s"}`, strings.Repeat("x", maxBody))
		}, wantState: viewmodel.StatusReadBadResponse, wantReason: "too large"},
		{name: "valid report padded past the bound", bundle: bundleFile(t, good.caPEM), serverName: "orders-rw.team-a.svc", handler: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, report+"%s", true, "", 0, strings.Repeat(" ", maxBody))
		}, wantState: viewmodel.StatusReadBadResponse, wantReason: "too large"},
		{name: "report of exactly the bound", bundle: bundleFile(t, good.caPEM), serverName: "orders-rw.team-a.svc", handler: func(w http.ResponseWriter, _ *http.Request) {
			body := fmt.Sprintf(report, true, "", 0)
			_, _ = w.Write([]byte(body + strings.Repeat(" ", maxBody-len(body))))
		}, wantState: viewmodel.StatusReadOK, wantHits: 1},
		{name: "garbage body", bundle: bundleFile(t, good.caPEM), serverName: "orders-rw.team-a.svc", handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }, wantState: viewmodel.StatusReadBadResponse, wantReason: "does not parse"},
	}
	var handler atomic.Pointer[http.HandlerFunc]
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		(*handler.Load())(w, r)
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.HandlerFunc(ok)
			if tt.handler != nil {
				h = tt.handler
			}
			handler.Store(&h)
			hits.Store(0)
			pod := tt.pod
			if pod == (PodTarget{}) {
				pod = PodTarget{IP: "127.0.0.1", StatusPortTLS: true}
			}
			pod.Name, pod.UID = "orders-1", "uid-1"
			s := sweeper(staticRoster{{Key: key, ServerName: tt.serverName, Pods: []PodTarget{pod}}}, tt.bundle, port(t, srv), time.Now)
			s.SweepOnce(context.Background())
			snap, found := s.Snapshot(key)
			if !found {
				t.Fatal("no snapshot")
			}
			got := snap.Instances["orders-1"]
			if got.State != tt.wantState {
				t.Fatalf("state = %s (%s), want %s", got.State, got.Reason, tt.wantState)
			}
			if !strings.Contains(got.Reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", got.Reason, tt.wantReason)
			}
			if tt.wantState == viewmodel.StatusReadOK && (got.Reading == nil || got.Reading.InstanceManagerVersion != "1.30.0") {
				t.Errorf("reading = %+v", got.Reading)
			}
			if tt.wantHits >= 0 && tt.wantState != viewmodel.StatusReadTLSError && tt.wantState != viewmodel.StatusReadBadResponse && hits.Load() != tt.wantHits {
				t.Errorf("server hits = %d, want %d", hits.Load(), tt.wantHits)
			}
		})
	}
}

// TestPlainHTTPIsNeverRead: a status port answering plaintext (a
// downgrade attempt, or a pre-1.24 instance whose command was edited) is
// a TLS error, not a read.
func TestPlainHTTPIsNeverRead(t *testing.T) {
	good := newPKI(t, "orders-rw.team-a.svc")
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = fmt.Fprintf(w, report, true, "", 0)
	}))
	defer plain.Close()
	s := sweeper(staticRoster{{Key: key, ServerName: "orders-rw.team-a.svc", Pods: []PodTarget{{Name: "orders-1", UID: "u", IP: "127.0.0.1", StatusPortTLS: true}}}}, bundleFile(t, good.caPEM), port(t, plain), time.Now)
	s.SweepOnce(context.Background())
	snap, _ := s.Snapshot(key)
	if got := snap.Instances["orders-1"]; got.State != viewmodel.StatusReadTLSError || got.Reading != nil {
		t.Errorf("plaintext port: %+v", got)
	}
	if hits.Load() != 0 {
		t.Errorf("the plaintext handler served %d requests", hits.Load())
	}
}

func TestUnreachable(t *testing.T) {
	good := newPKI(t, "orders-rw.team-a.svc")
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closedPort, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	s := sweeper(staticRoster{{Key: key, ServerName: "orders-rw.team-a.svc", Pods: []PodTarget{{Name: "orders-1", UID: "u", IP: "127.0.0.1", StatusPortTLS: true}}}}, bundleFile(t, good.caPEM), closedPort, time.Now)
	s.SweepOnce(context.Background())
	snap, _ := s.Snapshot(key)
	if got := snap.Instances["orders-1"]; got.State != viewmodel.StatusReadUnreachable || !strings.HasPrefix(got.Reason, ReasonNotReachable) {
		t.Errorf("closed port: %+v", got)
	}
}

// TestSinceAndLastGoodReading checks the "since when" clock, the
// retained last good reading (same pod UID only), lag computation and
// the ring gaps.
func TestSinceAndLastGoodReading(t *testing.T) {
	good := newPKI(t, "orders-rw.team-a.svc")
	var fail atomic.Bool
	srv, _ := statusServer(t, good, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() && strings.HasPrefix(r.Host, "127.0.0.1") && r.Header.Get("X-Replica") == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintf(w, report, false, "0/2000000", 3)
	})
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	pods := []PodTarget{{Name: "orders-2", UID: "uid-2", IP: "127.0.0.1", StatusPortTLS: true}}
	roster := &mutableRoster{targets: []Target{{Key: key, ServerName: "orders-rw.team-a.svc", Pods: pods}}}
	s := sweeper(roster, bundleFile(t, good.caPEM), port(t, srv), clock)

	s.SweepOnce(context.Background())
	first, _ := s.Snapshot(key)
	if first.Instances["orders-2"].State != viewmodel.StatusReadOK {
		t.Fatalf("first sweep: %+v", first.Instances["orders-2"])
	}

	now = now.Add(30 * time.Second)
	fail.Store(true)
	s.SweepOnce(context.Background())
	failed, _ := s.Snapshot(key)
	inst := failed.Instances["orders-2"]
	if inst.State != viewmodel.StatusReadBadResponse || inst.Reading == nil || !inst.Reading.ObservedAt.Equal(first.SweptAt) {
		t.Fatalf("after failure: %+v", inst)
	}
	since := inst.Since

	now = now.Add(30 * time.Second)
	s.SweepOnce(context.Background())
	again, _ := s.Snapshot(key)
	if !again.Instances["orders-2"].Since.Equal(since) {
		t.Error("Since moved while the state held")
	}

	series := s.Series(key, "orders-2")
	if len(series) != 4 {
		t.Fatalf("series = %+v", series)
	}
	for _, sr := range series {
		switch sr.Key {
		case SeriesReadyWALFiles:
			// A replica reports no readyWalFiles (CloudNativePG counts
			// them only on a primary): its 3 is no reading, a gap like
			// the failed reads (round 7, CNPG P1).
			if len(sr.Points) != 3 {
				t.Errorf("%s points = %+v, want three samples", sr.Key, sr.Points)
			}
			for _, p := range sr.Points {
				if p.Value != nil {
					t.Errorf("readyWalFiles of a replica: %+v", sr.Points)
				}
			}
		case SeriesCurrentLSN:
			// A replica's position is not the WAL its archiver has.
			for _, p := range sr.Points {
				if p.Value != nil {
					t.Errorf("WAL position of a replica: %+v", sr.Points)
				}
			}
		case SeriesLastArchivedAt:
			// Only a primary's archiver is the cluster's.
			for _, p := range sr.Points {
				if p.Value != nil {
					t.Errorf("last archived time of a replica: %+v", sr.Points)
				}
			}
		case SeriesLagBytes:
			// No primary was read, so the replica's lag is unknown, never 0.
			for _, p := range sr.Points {
				if p.Value != nil {
					t.Errorf("lag without a primary reading: %+v", sr.Points)
				}
			}
		}
	}

	roster.targets[0].Pods[0].UID = "uid-2-recreated"
	s.SweepOnce(context.Background())
	recreated, _ := s.Snapshot(key)
	if recreated.Instances["orders-2"].Reading != nil {
		t.Error("a recreated pod inherited the previous pod's reading")
	}

	roster.targets = nil
	s.SweepOnce(context.Background())
	if _, ok := s.Snapshot(key); ok {
		t.Error("a Cluster gone from the roster is still published")
	}
}

type mutableRoster struct{ targets []Target }

func (r *mutableRoster) Targets(context.Context) []Target { return r.targets }

func TestStatusSourceAdapter(t *testing.T) {
	s := New(staticRoster{{Key: key, Pods: []PodTarget{{Name: "orders-1", UID: "u", StatusPortTLS: false, IP: "1.2.3.4"}}}}, Options{Interval: time.Second, Timeout: 500 * time.Millisecond, Retention: time.Minute})
	if _, ok := s.InstanceStatus(key).CurrentInstanceStatus(); ok {
		t.Error("swept before any sweep")
	}
	s.SweepOnce(context.Background())
	snap, ok := s.InstanceStatus(key).CurrentInstanceStatus()
	if !ok || snap.Failing["orders-1"] != ReasonNoTLS || len(snap.Readings) != 0 {
		t.Errorf("adapter = %+v", snap)
	}
	if st := s.State(); st.State != viewmodel.SourceUnavailable || !strings.Contains(st.Reason, "no CA bundle") {
		t.Errorf("state = %+v", st)
	}

	disabled := New(staticRoster{}, Options{Interval: time.Second, Disabled: "INSTANCE_STATUS_ENABLED=false"})
	if snap, ok := disabled.InstanceStatus(key).CurrentInstanceStatus(); !ok || snap.Disabled == "" {
		t.Errorf("disabled adapter = %+v", snap)
	}
	if disabled.State().State != viewmodel.SourceDisabled {
		t.Error("disabled sweep not reported disabled")
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	good := newPKI(t, "orders-rw.team-a.svc")
	var inFlight, peak atomic.Int32
	srv, _ := statusServer(t, good, func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = fmt.Fprintf(w, report, true, "", 0)
	})
	pods := make([]PodTarget, 0, 10)
	for i := range 10 {
		pods = append(pods, PodTarget{Name: fmt.Sprintf("orders-%d", i), UID: "u", IP: "127.0.0.1", StatusPortTLS: true})
	}
	s := New(staticRoster{{Key: key, ServerName: "orders-rw.team-a.svc", Pods: pods}}, Options{
		Interval: 10 * time.Second, Timeout: 5 * time.Second, Concurrency: 3, Retention: time.Hour,
		Bundle: bundleFile(t, good.caPEM), StatusPort: port(t, srv),
	})
	s.SweepOnce(context.Background())
	if peak.Load() > 3 {
		t.Errorf("peak concurrency %d > 3", peak.Load())
	}
	snap, _ := s.Snapshot(key)
	if len(snap.Instances) != 10 {
		t.Errorf("instances = %d", len(snap.Instances))
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want viewmodel.StatusReadState
	}{
		{"nil", nil, viewmodel.StatusReadOK},
		{"unknown authority", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, viewmodel.StatusReadNotVerifiable},
		{"hostname", x509.HostnameError{Host: "x", Certificate: &x509.Certificate{}}, viewmodel.StatusReadTLSError},
		{"expired", x509.CertificateInvalidError{Reason: x509.Expired}, viewmodel.StatusReadTLSError},
		{"record header", tls.RecordHeaderError{}, viewmodel.StatusReadTLSError},
		{"bad status", errBadStatus{404}, viewmodel.StatusReadBadResponse},
		{"decode", errDecode{errors.New("x")}, viewmodel.StatusReadBadResponse},
		{"too large", errTooLarge{}, viewmodel.StatusReadBadResponse},
		{"deadline", context.DeadlineExceeded, viewmodel.StatusReadUnreachable},
		{"other", errors.New("no route to host"), viewmodel.StatusReadUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := classify(tt.err)
			if got != tt.want {
				t.Errorf("classify = %s, want %s", got, tt.want)
			}
			if (got == viewmodel.StatusReadOK) != (reason == "") {
				t.Errorf("reason %q for %s", reason, got)
			}
		})
	}
}

func TestParseLSNAndLag(t *testing.T) {
	tests := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"0/0", 0, true},
		{"0/3000060", 0x3000060, true},
		{"16/B374D848", 0x16_B374D848, true},
		{"FFFFFFFF/FFFFFFFF", 0xFFFFFFFF_FFFFFFFF, true},
		{"", 0, false},
		{"0", 0, false},
		{"/1", 0, false},
		{"1/", 0, false},
		{"G/1", 0, false},
		{"100000000/0", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseLSN(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseLSN(%q) = %x, %v", tt.in, got, ok)
		}
	}
	if lag, ok := LagBytes("0/3000000", "0/2000000"); !ok || lag != 0x1000000 {
		t.Errorf("lag = %d %v", lag, ok)
	}
	if lag, ok := LagBytes("0/1", "0/2"); !ok || lag != 0 {
		t.Errorf("replica ahead: %d %v", lag, ok)
	}
	if _, ok := LagBytes("", "0/2"); ok {
		t.Error("unknown primary position gave a lag")
	}
}

func TestParseBundle(t *testing.T) {
	good := newPKI(t, "x")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("k")})
	tests := []struct {
		name    string
		raw     []byte
		certs   int
		problem string
	}{
		{"one CA", good.caPEM, 1, ""},
		{"two CAs", append(append([]byte{}, good.caPEM...), newPKI(t, "y").caPEM...), 2, ""},
		{"empty", nil, 0, "holds no certificate yet"},
		{"not PEM", []byte("hello"), 0, "holds no certificate yet"},
		{"a private key is refused", append(append([]byte{}, good.caPEM...), keyPEM...), 0, "contains a EC PRIVATE KEY block"},
		{"broken certificate", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("nope")}), 0, "does not parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBundle("/b/ca.crt", tt.raw)
			if got.Certificates != tt.certs || !strings.Contains(got.Problem, tt.problem) || (tt.problem == "") != (got.Pool != nil) {
				t.Errorf("parseBundle = %+v", got)
			}
		})
	}
	if got := loadBundle("/nonexistent/ca.crt"); got.Pool != nil || !strings.Contains(got.Problem, "does not exist") {
		t.Errorf("missing file: %+v", got)
	}
}

// TestBundleReloadsOnChange: the kubelet swaps a ConfigMap mount
// atomically; the watcher must pick the new content up without restart.
func TestBundleReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := NewCABundle(path, nil)
	if b.Current().Pool != nil {
		t.Fatal("empty bundle has a pool")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Watch(ctx) }()
	time.Sleep(100 * time.Millisecond)
	tmp := filepath.Join(dir, "ca.tmp")
	if err := os.WriteFile(tmp, newPKI(t, "x").caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for b.Current().Pool == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if b.Current().Certificates != 1 {
		t.Errorf("bundle after change: %+v", b.Current())
	}
}
