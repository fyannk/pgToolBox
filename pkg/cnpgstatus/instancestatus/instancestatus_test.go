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

package instancestatus

import (
	"bytes"
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
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

const sampleReport = `{
  "currentLsn": "0/6000060", "receivedLsn": "", "replayLsn": "", "systemID": "7411",
  "isPrimary": true, "replayPaused": false, "pendingRestart": true, "pendingRestartForDecrease": false,
  "isWalReceiverActive": false, "isPgRewindRunning": false, "mightBeUnavailable": false,
  "isArchivingWAL": true, "node": "worker-1",
  "lastArchivedWAL": "000000010000000000000005", "lastArchivedWALTime": "2026-09-17T12:00:00.123456Z",
  "lastFailedWAL": "000000010000000000000006", "lastFailedWALTime": "2026-09-17 12:01:00.5+00",
  "currentWAL": "000000010000000000000006", "readyWalFiles": 3, "timeLineID": 1,
  "instanceManagerVersion": "1.30.0", "instanceArch": "amd64",
  "replicationInfo": [{"applicationName": "orders-2", "state": "streaming", "syncState": "async", "syncPriority": "0", "replayLag": "00:00:00.5"}],
  "replicationSlotsInfo": [{"slotName": "_cnpg_orders_2", "slotType": "physical", "active": true, "walStatus": "reserved"},
                           {"slotName": "debezium", "slotType": "logical", "plugin": "pgoutput", "database": "app", "active": false, "walStatus": "reserved"}]
}`

// TestParseReadsTheReportVerbatim proves the subset the console keeps,
// both timestamp forms, and the bounds.
func TestParseReadsTheReportVerbatim(t *testing.T) {
	t.Parallel()
	reading, err := Parse(strings.NewReader(sampleReport))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reading.IsPrimary || !reading.PendingRestart || reading.ReadyWALFiles != 3 || reading.TimelineID != 1 ||
		reading.InstanceManagerVersion != "1.30.0" || reading.CurrentLSN != "0/6000060" || reading.Node != "worker-1" {
		t.Errorf("scalars wrong: %+v", reading)
	}
	if reading.LastArchivedWALTime == nil || !reading.LastArchivedWALTime.Equal(time.Date(2026, 9, 17, 12, 0, 0, 123456000, time.UTC)) {
		t.Errorf("RFC3339 archive time = %v", reading.LastArchivedWALTime)
	}
	if reading.LastFailedWALTime == nil || !reading.LastFailedWALTime.Equal(time.Date(2026, 9, 17, 12, 1, 0, 500000000, time.UTC)) {
		t.Errorf("PostgreSQL-form failure time = %v", reading.LastFailedWALTime)
	}
	if len(reading.Standbys) != 1 || reading.Standbys[0].ApplicationName != "orders-2" || reading.Standbys[0].ReplayLag != "00:00:00.5" {
		t.Errorf("standbys = %+v", reading.Standbys)
	}
	if len(reading.Slots) != 2 || reading.Slots[1].Name != "debezium" || reading.Slots[1].Active || reading.Slots[1].Plugin != "pgoutput" {
		t.Errorf("slots = %+v", reading.Slots)
	}
	long := strings.Repeat("x", 1000)
	bounded, err := Parse(strings.NewReader(`{"node": "` + long + `", "lastArchivedWALTime": "yesterday"}`))
	if err != nil || len(bounded.Node) != maxText || bounded.LastArchivedWALTime != nil {
		t.Errorf("bounds: node %d, time %v, err %v", len(bounded.Node), bounded.LastArchivedWALTime, err)
	}
	if _, err := Parse(strings.NewReader("not json")); err == nil {
		t.Error("a malformed report parsed")
	}
}

// TestParseNamesTheSentLSNAndFencing pins the two CloudNativePG JSON
// quirks: replicationInfo[].receivedLsn is pg_stat_replication.sent_lsn
// and surfaces as SentLSN, while the top-level receivedLsn stays the
// instance's own received position; isFenced is decoded only when
// present.
func TestParseNamesTheSentLSNAndFencing(t *testing.T) {
	t.Parallel()
	reading, err := Parse(strings.NewReader(`{
	  "receivedLsn": "0/5000000", "isFenced": true,
	  "replicationInfo": [{"applicationName": "orders-2", "receivedLsn": "0/6000060",
	    "writeLsn": "0/6000050", "flushLsn": "0/6000040", "replayLsn": "0/6000030"}]
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if reading.ReceivedLSN != "0/5000000" {
		t.Errorf("top-level ReceivedLSN = %q", reading.ReceivedLSN)
	}
	if reading.IsFenced == nil || !*reading.IsFenced {
		t.Errorf("IsFenced = %v, want true", reading.IsFenced)
	}
	want := Standby{ApplicationName: "orders-2", SentLSN: "0/6000060", WriteLSN: "0/6000050", FlushLSN: "0/6000040", ReplayLSN: "0/6000030"}
	if len(reading.Standbys) != 1 || reading.Standbys[0] != want {
		t.Errorf("standbys = %+v, want %+v", reading.Standbys, want)
	}
	absent, err := Parse(strings.NewReader(sampleReport))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if absent.IsFenced != nil {
		t.Errorf("IsFenced = %v for a report without the key, want nil", *absent.IsFenced)
	}
}

type staticPods struct{ snap observe.PodsSnapshot }

func (s staticPods) CurrentPods() (observe.PodsSnapshot, bool) { return s.snap, true }

type sweepClock struct {
	mu     sync.Mutex
	sweeps int
	now    time.Time
}

func (c *sweepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(10 * time.Second)
	return c.now
}

func (c *sweepClock) Wait(ctx context.Context, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sweeps == 0 {
		return context.Canceled
	}
	c.sweeps--
	return ctx.Err()
}

// TestCollectorKeepsTheLastReportBesideAFailure proves a sweep reads
// every pod with an IP, records a failure per unreachable instance while
// keeping that instance's previous report, forgets an instance that
// left the roster, and logs reachability changes once.
func TestCollectorKeepsTheLastReportBesideAFailure(t *testing.T) {
	t.Parallel()
	leaf, ca := clusterCertificate(t, "orders")
	tlsConfig, err := TLSConfig("orders", ca)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(sampleReport))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))

	source := &staticPods{snap: observe.PodsSnapshot{Pods: []observe.PodFacts{
		{Name: "orders-1", UID: "u1", IP: host},
		{Name: "orders-2", UID: "u2", IP: "203.0.113.1"},
		{Name: "orders-3", UID: "u3"},
	}}}
	store := NewStore(10 * time.Second)
	var logs bytes.Buffer
	c := New(source, store, port, tlsConfig, &sweepClock{sweeps: 2, now: time.Unix(1700000000, 0)}, slog.New(slog.NewJSONHandler(&logs, nil)))
	c.client.Timeout = 500 * time.Millisecond
	if err := c.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	snap, ok := store.CurrentInstanceStatus()
	if !ok || snap.Generation != 2 || len(snap.Readings) != 1 || snap.Readings["orders-1"].Instance != "orders-1" {
		t.Fatalf("snapshot = %+v, %v", snap, ok)
	}
	if snap.Failing["orders-2"] == "" {
		t.Errorf("the unreachable instance carries no failure: %+v", snap.Failing)
	}
	if snap.Failing["orders-3"] != "not started" {
		t.Errorf("a pod without an IP is not recorded as unread: %+v", snap.Failing)
	}
	if got := strings.Count(logs.String(), "instance status read failed"); got != 1 {
		t.Errorf("failure logged %d times across two sweeps, want once (a pod without an IP is not a failed read)", got)
	}

	// The reachable pod stops answering: its last report stays, with
	// the failure beside it, for as long as it is the same pod; a
	// recreated pod of the same name gets no report of another's; a pod
	// gone from the roster is forgotten.
	srv.Close()
	store.publish(time.Unix(1700000100, 0), []member{{"orders-1", "u1"}}, nil, map[string]string{"orders-1": "unavailable"})
	snap, _ = store.CurrentInstanceStatus()
	if snap.Readings["orders-1"].Instance != "orders-1" || snap.Failing["orders-1"] != "unavailable" {
		t.Errorf("last report not kept beside the failure: %+v", snap)
	}
	if _, kept := snap.Readings["orders-2"]; kept {
		t.Error("an instance that left the roster is still listed")
	}
	store.publish(time.Unix(1700000200, 0), []member{{"orders-1", "u1-recreated"}}, nil, map[string]string{"orders-1": "unavailable"})
	snap, _ = store.CurrentInstanceStatus()
	if _, kept := snap.Readings["orders-1"]; kept {
		t.Error("a recreated pod inherited the previous pod's report")
	}

	// A stale roster is not swept at all.
	stale := &staticPods{snap: observe.PodsSnapshot{Stale: true, Pods: []observe.PodFacts{{Name: "orders-1", UID: "u1", IP: host}}}}
	before, _ := store.CurrentInstanceStatus()
	New(stale, store, port, tlsConfig, &sweepClock{sweeps: 1, now: time.Unix(1700000300, 0)}, slog.New(slog.NewJSONHandler(&logs, nil))).sweep(context.Background())
	after, _ := store.CurrentInstanceStatus()
	if after.Generation != before.Generation {
		t.Error("a stale roster was swept")
	}
}

// clusterCertificate issues a certificate the way the operator does —
// naming the cluster's Services — signed by a fresh CA, and returns the
// leaf as a TLS certificate with the CA's PEM.
func clusterCertificate(t *testing.T, clusterName string) (tls.Certificate, []byte) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: clusterName + " CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: clusterName + "-rw"},
		DNSNames:  []string{clusterName + "-rw", clusterName + "-r", clusterName + "-ro"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)
	leaf, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	return leaf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

// TestTLSConfigJudgesTheClustersCertificate proves the scheme follows
// the pod spec with no fallback, and the verification
// matrix against real TLS servers: with the CA, the chain is verified
// and another CA's certificate is refused; the explicit insecure
// configuration accepts this cluster's name and refuses another's;
// plain HTTP is never read (there is no fallback) and a server capped
// below TLS 1.3 is refused.
func TestTLSConfigJudgesTheClustersCertificate(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(sampleReport)) })
	ordersLeaf, ordersCA := clusterCertificate(t, "orders")
	billingLeaf, _ := clusterCertificate(t, "billing")
	serve := func(leaf tls.Certificate, maxVersion uint16) *httptest.Server {
		srv := httptest.NewUnstartedServer(handler)
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12, MaxVersion: maxVersion}
		srv.StartTLS()
		return srv
	}
	verified, err := TLSConfig("orders", ordersCA)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	insecure := InsecureTLSConfig("orders")
	for name, tc := range map[string]struct {
		srv    *httptest.Server
		config *tls.Config
		want   bool
		scheme string
	}{
		"own cluster, CA verified":           {serve(ordersLeaf, 0), verified, true, ""},
		"own cluster, explicit insecure":     {serve(ordersLeaf, 0), insecure, true, ""},
		"other cluster's name, insecure":     {serve(billingLeaf, 0), insecure, false, ""},
		"other CA's chain":                   {serve(billingLeaf, 0), verified, false, ""},
		"own cluster capped at TLS 1.2":      {serve(ordersLeaf, tls.VersionTLS12), verified, false, ""},
		"plain HTTP is never read":           {httptest.NewServer(handler), verified, false, ""},
		"plain HTTP is never read, insecure": {httptest.NewServer(handler), insecure, false, ""},
		"pre-TLS pod declared http":          {httptest.NewServer(handler), verified, true, "http"},
		"https pod never falls back":         {httptest.NewServer(handler), verified, false, "https"},
		"http pod is not read over TLS":      {serve(ordersLeaf, 0), verified, false, "http"},
	} {
		host, port, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(tc.srv.URL, "https://"), "http://"))
		store := NewStore(10 * time.Second)
		c := New(&staticPods{snap: observe.PodsSnapshot{Pods: []observe.PodFacts{{Name: "orders-1", IP: host, StatusScheme: tc.scheme}}}},
			store, port, tc.config, &sweepClock{sweeps: 1, now: time.Unix(1700000000, 0)}, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
		c.client.Timeout = 2 * time.Second
		_ = c.Run(context.Background())
		snap, _ := store.CurrentInstanceStatus()
		_, read := snap.Readings["orders-1"]
		if read != tc.want {
			t.Errorf("%s: read=%v, want %v (failing: %v)", name, read, tc.want, snap.Failing)
		}
		tc.srv.Close()
	}
	if verified.MinVersion != tls.VersionTLS13 || insecure.MinVersion != tls.VersionTLS13 {
		t.Error("a status-port TLS configuration offers less than TLS 1.3")
	}
	if verified.InsecureSkipVerify {
		t.Error("the CA configuration skips verification")
	}
	for name, ca := range map[string][]byte{"empty": nil, "not a certificate": []byte("not a certificate")} {
		if _, err := TLSConfig("orders", ca); err == nil {
			t.Errorf("TLSConfig accepted a %s CA", name)
		}
	}
}

// TestDisabledStoreStatesWhy proves a disabled sweep publishes one
// snapshot whose reason is the visible fact, with nothing read.
func TestDisabledStoreStatesWhy(t *testing.T) {
	t.Parallel()
	store := NewDisabledStore(15*time.Second, "instance status sweep disabled: no CA configured")
	snap, ok := store.CurrentInstanceStatus()
	if !ok || snap.Disabled != "instance status sweep disabled: no CA configured" || len(snap.Readings) != 0 {
		t.Fatalf("snapshot = %+v, %v", snap, ok)
	}
	if store.Disabled() == "" || NewStore(time.Second).Disabled() != "" {
		t.Error("Disabled() does not distinguish the two stores")
	}
}

// TestParseVolumeUsages pins the fork's volumeUsages[] (origin/dev/64):
// an absent key is "not reported", a malformed one costs only itself,
// and entries that are misnamed or contradict themselves are dropped.
func TestParseVolumeUsages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		body     string
		reported bool
		volumes  []VolumeUsage
	}{
		{"upstream report", `{"isPrimary": true}`, false, nil},
		{"null", `{"volumeUsages": null}`, false, nil},
		{"malformed keeps the report", `{"isPrimary": true, "volumeUsages": {"pgdata": 1}}`, true, nil},
		{
			"fork report",
			`{"volumeUsages": [
			  {"volume": "pgdata", "totalBytes": 1000, "availableBytes": 400, "usedBytes": 550, "inodes": 100, "inodesFree": 60},
			  {"volume": "wal", "totalBytes": 500, "availableBytes": 450, "usedBytes": 40},
			  {"volume": "tbs-hist", "totalBytes": 10, "availableBytes": 1, "usedBytes": 9, "inodes": 4, "inodesFree": 0}
			]}`,
			true,
			[]VolumeUsage{
				{Volume: "pgdata", TotalBytes: 1000, AvailableBytes: 400, UsedBytes: 550, Inodes: 100, InodesFree: 60, HasInodes: true},
				{Volume: "wal", TotalBytes: 500, AvailableBytes: 450, UsedBytes: 40},
				{Volume: "tbs-hist", TotalBytes: 10, AvailableBytes: 1, UsedBytes: 9, Inodes: 4, HasInodes: true},
			},
		},
		{
			"contradictions and strangers dropped",
			`{"volumeUsages": [
			  {"volume": "pgdata", "totalBytes": 10, "availableBytes": 11, "usedBytes": 1},
			  {"volume": "wal", "totalBytes": 10, "availableBytes": 1, "usedBytes": 1, "inodes": 1, "inodesFree": 2},
			  {"volume": "../etc", "totalBytes": 10, "availableBytes": 1, "usedBytes": 1},
			  {"volume": "tbs-", "totalBytes": 10, "availableBytes": 1, "usedBytes": 1},
			  {"volume": "tbs-a", "availableBytes": 1, "usedBytes": 1},
			  {"volume": "tbs-b", "totalBytes": 10, "availableBytes": 1, "usedBytes": 1},
			  {"volume": "tbs-b", "totalBytes": 20, "availableBytes": 1, "usedBytes": 1}
			]}`,
			true,
			[]VolumeUsage{{Volume: "tbs-b", TotalBytes: 10, AvailableBytes: 1, UsedBytes: 1}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reading, err := Parse(strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if reading.VolumeUsagesReported != tc.reported {
				t.Errorf("reported = %v, want %v", reading.VolumeUsagesReported, tc.reported)
			}
			if len(reading.VolumeUsages) != len(tc.volumes) {
				t.Fatalf("volumes = %+v, want %+v", reading.VolumeUsages, tc.volumes)
			}
			for i := range tc.volumes {
				if reading.VolumeUsages[i] != tc.volumes[i] {
					t.Errorf("volume %d = %+v, want %+v", i, reading.VolumeUsages[i], tc.volumes[i])
				}
			}
		})
	}
	var many strings.Builder
	many.WriteString(`{"volumeUsages": [`)
	for i := range 100 {
		if i > 0 {
			many.WriteString(",")
		}
		fmt.Fprintf(&many, `{"volume": "tbs-t%d", "totalBytes": 1, "availableBytes": 1, "usedBytes": 0}`, i)
	}
	many.WriteString(`]}`)
	reading, err := Parse(strings.NewReader(many.String()))
	if err != nil || len(reading.VolumeUsages) != maxVolumes {
		t.Errorf("bound: %d volumes, err %v", len(reading.VolumeUsages), err)
	}
}
