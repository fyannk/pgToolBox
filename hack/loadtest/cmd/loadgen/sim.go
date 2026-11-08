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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The fake Pods of the harness live on KWOK nodes, where nothing listens on
// their IPs. Left like that, the console's status sweep dials a dead address
// for every Pod and waits out its 5 s timeout, 8 at a time: a 3000-Pod
// population is swept in hours, not in 30 s, and the load it puts on the
// console is the load of a broken network, not of a healthy fleet.
//
// instance-sim fixes that from the kind node. cluster.sh routes the KWOK Pod
// CIDRs to the loopback of the node (ip route add local), so the sweep's
// connection to <pod IP>:8000 lands on this process, which runs with
// hostNetwork. It answers like an instance manager: /pg/status over TLS 1.3
// with a certificate for the name the console verifies (<cluster>-rw.<ns>.svc,
// taken from the TLS server name) signed by the CA the console trusts, and a
// small exporter on :9187. Which Pod a request is for is read from the
// address it was sent to.

type simOpts struct {
	kube          kubeFlags
	listen        string
	metricsListen string
	caCert, caKey string
	refresh       time.Duration
}

func runInstanceSim(ctx context.Context, args []string) error {
	var o simOpts
	fs := flag.NewFlagSet("instance-sim", flag.ContinueOnError)
	o.kube.register(fs, 20, 40)
	fs.StringVar(&o.listen, "listen", ":8000", "address of the status port")
	fs.StringVar(&o.metricsListen, "metrics-listen", ":9187", "address of the exporter")
	fs.StringVar(&o.caCert, "ca-cert", "/ca/tls.crt", "CA certificate the console's instance CA bundle holds")
	fs.StringVar(&o.caKey, "ca-key", "/ca/tls.key", "key of that CA")
	fs.DurationVar(&o.refresh, "refresh", 15*time.Second, "how often the Pod table is re-read")
	if err := fs.Parse(args); err != nil {
		return err
	}
	k, err := o.kube.connect()
	if err != nil {
		return err
	}
	ca, err := tls.LoadX509KeyPair(o.caCert, o.caKey)
	if err != nil {
		return fmt.Errorf("load CA: %w", err)
	}
	ca.Leaf, err = x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse CA: %w", err)
	}
	signer, ok := ca.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return errors.New("the CA key must be an ECDSA key")
	}
	s := &simulator{start: time.Now(), ca: ca.Leaf, caKey: signer}
	s.table.Store(&podTable{})
	if err := s.reload(ctx, k); err != nil {
		return fmt.Errorf("first Pod table: %w", err)
	}
	go s.refreshLoop(ctx, k, o.refresh)

	status := &http.Server{
		Addr: o.listen, Handler: http.HandlerFunc(s.serveStatus), ReadHeaderTimeout: 5 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: s.certificate},
	}
	metrics := &http.Server{Addr: o.metricsListen, Handler: http.HandlerFunc(s.serveMetrics), ReadHeaderTimeout: 5 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- status.ListenAndServeTLS("", "") }()
	go func() { errs <- metrics.ListenAndServe() }()
	slog.Info("instance-sim serving", "status", o.listen, "metrics", o.metricsListen, "pods", len(s.table.Load().byIP))
	select {
	case <-ctx.Done():
	case err := <-errs:
		return err
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = status.Shutdown(shutdown)
	_ = metrics.Shutdown(shutdown)
	return nil
}

type simPod struct {
	namespace, cluster, name, node string
	primary                        bool
	replicas                       []string
}

type podTable struct{ byIP map[string]simPod }

type simulator struct {
	start   time.Time
	ca      *x509.Certificate
	caKey   *ecdsa.PrivateKey
	table   atomic.Pointer[podTable]
	leaves  sync.Map // server name -> *tls.Certificate
	served  atomic.Int64
	unknown atomic.Int64
}

func (s *simulator) refreshLoop(ctx context.Context, k *kube, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.reload(ctx, k); err != nil {
				slog.Warn("pod table refresh failed", "error", err)
			}
		}
	}
}

func (s *simulator) reload(ctx context.Context, k *kube) error {
	var pods corev1.PodList
	// ResourceVersion "0" is answered from the API server's watch cache.
	if err := k.client.List(ctx, &pods, client.HasLabels{clusterLabel},
		&client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: "0"}}); err != nil {
		return err
	}
	replicas := map[string][]string{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Labels["cnpg.io/instanceRole"] != "primary" {
			key := p.Namespace + "/" + p.Labels[clusterLabel]
			replicas[key] = append(replicas[key], p.Name)
		}
	}
	for _, names := range replicas {
		sort.Strings(names)
	}
	t := &podTable{byIP: make(map[string]simPod, len(pods.Items))}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.PodIP == "" {
			continue
		}
		cluster := p.Labels[clusterLabel]
		t.byIP[p.Status.PodIP] = simPod{
			namespace: p.Namespace, cluster: cluster, name: p.Name, node: p.Spec.NodeName,
			primary: p.Labels["cnpg.io/instanceRole"] == "primary", replicas: replicas[p.Namespace+"/"+cluster],
		}
	}
	s.table.Store(t)
	return nil
}

// certificate issues (once per server name) a leaf for the name the client
// asked for; the console verifies <cluster>-rw.<namespace>.svc.
func (s *simulator) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := hello.ServerName
	if name == "" {
		name = "instance.local"
	}
	if c, ok := s.leaves.Load(name); ok {
		cert, _ := c.(*tls.Certificate)
		return cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(7 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.ca, &key.PublicKey, s.caKey)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	s.leaves.Store(name, cert)
	return cert, nil
}

func (s *simulator) podFor(r *http.Request) (simPod, bool) {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return simPod{}, false
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return simPod{}, false
	}
	p, ok := s.table.Load().byIP[host]
	return p, ok
}

// position is the WAL position `lag` bytes behind the primary's, which
// advances 16 KiB a second.
func (s *simulator) position(lag int64) string {
	pos := int64(0x6000060) + int64(time.Since(s.start).Seconds())*16384 - lag
	return fmt.Sprintf("%X/%X", pos>>32, pos&0xFFFFFFFF)
}

func (s *simulator) serveStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/pg/status" {
		http.NotFound(w, r)
		return
	}
	p, ok := s.podFor(r)
	if !ok {
		s.unknown.Add(1)
		http.Error(w, "unknown pod", http.StatusNotFound)
		return
	}
	s.served.Add(1)
	archived := time.Now().UTC().Add(-20 * time.Second)
	report := map[string]any{
		"systemID": systemID, "isPrimary": p.primary, "replayPaused": false, "pendingRestart": false,
		"pendingRestartForDecrease": false, "isPgRewindRunning": false, "mightBeUnavailable": false,
		"isInstanceManagerUpgrading": false, "node": p.node, "timeLineID": 1,
		"instanceManagerVersion": "1.30.0", "instanceArch": "amd64",
		"currentWAL": "000000010000000000000006", "readyWalFiles": 0,
		"lastArchivedWAL": "000000010000000000000005", "lastArchivedWALTime": archived.Format(time.RFC3339Nano),
		"isArchivingWAL": p.primary,
	}
	if p.primary {
		report["currentLsn"] = s.position(0)
		standbys := make([]map[string]string, 0, len(p.replicas))
		slots := make([]map[string]any, 0, len(p.replicas))
		for _, name := range p.replicas {
			standbys = append(standbys, map[string]string{
				"applicationName": name, "state": "streaming", "syncState": "async", "syncPriority": "0",
				"receivedLsn": s.position(0), "writeLsn": s.position(0), "flushLsn": s.position(0), "replayLsn": s.position(2048),
				"writeLag": "00:00:00.0004", "flushLag": "00:00:00.0009", "replayLag": "00:00:00.0021",
			})
			slots = append(slots, map[string]any{"slotName": "_cnpg_" + strings.ReplaceAll(name, "-", "_"), "slotType": "physical", "active": true, "walStatus": "reserved"})
		}
		report["replicationInfo"], report["replicationSlotsInfo"] = standbys, slots
	} else {
		report["receivedLsn"], report["replayLsn"] = s.position(0), s.position(2048)
		report["isWalReceiverActive"] = true
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}

// serveMetrics is a short CloudNativePG exporter page; counters advance with
// the process's uptime so the console's rate views have something to draw.
func (s *simulator) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/metrics" {
		http.NotFound(w, r)
		return
	}
	up := time.Since(s.start).Seconds()
	var b strings.Builder
	gauge := func(name string, v float64) { fmt.Fprintf(&b, "# TYPE %s gauge\n%s %g\n", name, name, v) }
	counter := func(name string, v float64) { fmt.Fprintf(&b, "# TYPE %s counter\n%s %.0f\n", name, name, v) }
	counter("loadtest_sim_status_served_total", float64(s.served.Load()))
	counter("loadtest_sim_status_unknown_pod_total", float64(s.unknown.Load()))
	gauge("cnpg_backends_total", 12)
	gauge("cnpg_backends_waiting_total", 0)
	gauge("cnpg_backends_max_tx_duration_seconds", 0.4)
	gauge("cnpg_pg_replication_lag", 0.002)
	counter("cnpg_pg_stat_database_xact_commit_total", up*120)
	counter("cnpg_pg_stat_database_xact_rollback_total", up*0.5)
	counter("cnpg_pg_stat_database_blks_hit_total", up*9000)
	counter("cnpg_pg_stat_database_blks_read_total", up*40)
	counter("cnpg_pg_stat_database_tup_inserted_total", up*30)
	counter("cnpg_pg_stat_database_tup_updated_total", up*20)
	counter("cnpg_collector_wal_bytes", up*16384)
	counter("cnpg_pg_stat_archiver_archived_count_total", up/60)
	counter("cnpg_pg_stat_archiver_failed_count_total", 0)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}
