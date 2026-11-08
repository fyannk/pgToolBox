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

package application

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/fyannk/pgToolBox/internal/evidence/agent"
	"github.com/fyannk/pgToolBox/internal/evidence/config"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/redact"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	agentConsoleToken   = "eyJhbGciOiJSUzI1NiJ9.Y29uc29sZQ.c2ln"
	agentAPIAudToken    = "eyJhbGciOiJSUzI1NiJ9.YXBp.c2ln"
	agentServerName     = "orders-evidence.database-team.svc"
	agentConsoleSubject = "system:serviceaccount:database-team:pgtoolbox-console"
)

// apiServerStub plays the API server's two reviews: the console token carries
// the agent's audience, the other one only the API server's.
type apiServerStub struct {
	reviews atomic.Int64
}

func (s *apiServerStub) ReviewToken(_ context.Context, token string, audiences []string) (agent.User, []string, bool, error) {
	s.reviews.Add(1)
	tokenAudiences := map[string][]string{
		agentConsoleToken: {"pgtoolbox-evidence-agent/database-team"},
		agentAPIAudToken:  {"https://kubernetes.default.svc"},
	}[token]
	var matched []string
	for _, audience := range audiences {
		if slices.Contains(tokenAudiences, audience) {
			matched = append(matched, audience)
		}
	}
	if len(matched) == 0 {
		return agent.User{}, nil, false, nil
	}
	return agent.User{Username: agentConsoleSubject}, matched, true, nil
}

func (s *apiServerStub) Allowed(_ context.Context, user agent.User, attributes authorizationv1.ResourceAttributes) (bool, error) {
	return user.Username == agentConsoleSubject && attributes.Name == "orders" && attributes.Namespace == "database-team" &&
		attributes.Resource == "evidenceagents" && attributes.Verb == "get", nil
}

func writeServingPair(t *testing.T, directory string) (*x509.CertPool, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: agentServerName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{agentServerName}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(directory, "tls.crt"), filepath.Join(directory, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return pool, certFile, keyFile
}

func agentTestConfig(t *testing.T, directory, certFile, keyFile string, overrides map[string]string) config.Config {
	t.Helper()
	accessPath, secretPath := filepath.Join(directory, "access"), filepath.Join(directory, "secret")
	for _, path := range []string{accessPath, secretPath} {
		if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	environment := map[string]string{
		"RUNTIME_MODE": "agent", "REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3",
		"DESTINATION_PATH": "s3://synthetic-bucket/cluster", "BARMAN_SERVER_NAMES": "orders",
		"CNPG_CLUSTER_NAMESPACE": "database-team", "CNPG_CLUSTER_UID": "2f12b7d1-7e8d-4c37-a68f-233efc5f3191",
		"CNPG_CLUSTER_NAME": "orders", "STORE_CREDENTIAL_MODE": "static-files",
		"AWS_ACCESS_KEY_ID_FILE": accessPath, "AWS_SECRET_ACCESS_KEY_FILE": secretPath, "AWS_REGION": "eu-west-1",
		"TLS_CERT_FILE": certFile, "TLS_KEY_FILE": keyFile, "AGENT_NAME": "orders", "AGENT_NAMESPACE": "database-team",
		"PRODUCER_EPOCH": "3",
	}
	for key, value := range overrides {
		environment[key] = value
	}
	cfg, err := config.Load(func(key string) string { return environment[key] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type agentClient struct {
	client  *http.Client
	address string
}

func (c agentClient) get(t *testing.T, path, token string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+c.address+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

// TestAgentRuntimeServesBoundTokenClientsOverHTTPS is the unit-level form of
// the kind scenario: a console-style client verifying the agent's CA and
// presenting a bound token with the agent audience reads the channel; a
// token minted for the API server is refused; a failed refresh is served
// stale and unknown, never as fresh evidence.
func TestAgentRuntimeServesBoundTokenClientsOverHTTPS(t *testing.T) {
	directory := t.TempDir()
	pool, certFile, keyFile := writeServingPair(t, directory)
	cfg := agentTestConfig(t, directory, certFile, keyFile, nil)
	var failList atomic.Bool
	var badPrefix atomic.Bool
	factory := func(context.Context, config.Config, *redact.Redactor) (store.Reader, error) {
		return &storetest.Fake{ListFunc: func(_ context.Context, request store.ListRequest) (store.Page, error) {
			if request.Prefix != "orders/" {
				badPrefix.Store(true)
			}
			if failList.Load() {
				return store.Page{}, context.DeadlineExceeded
			}
			return store.Page{}, nil
		}}, nil
	}
	stub := &apiServerStub{}
	app, err := newWithFactoryVersion(context.Background(), cfg, slog.New(slog.DiscardHandler), factory, "1.2.3", WithReviewer(stub))
	if err != nil {
		t.Fatal(err)
	}
	if app.server != nil || app.agentHandler == nil {
		t.Fatal("agent mode must expose only the HTTPS evidence handler")
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.ServeAgent(ctx, listener) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := agentClient{
		client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: pool, ServerName: agentServerName, MinVersion: tls.VersionTLS13,
		}}},
		address: listener.Addr().String(),
	}

	var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, body := client.get(t, evidencev1alpha1.SnapshotPath, agentConsoleToken)
		if status == http.StatusOK && json.Unmarshal(body, &snapshot) == nil && snapshot.EvidenceGeneration > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no evidence published: %d %s", status, body)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := snapshot.Validate(); err != nil || snapshot.ProducerEpoch != 3 || snapshot.Producer.Version != "1.2.3" ||
		snapshot.Identity.Cluster.Namespace != "database-team" || snapshot.Identity.Repository.Scope.Name != "orders" {
		t.Fatalf("agent snapshot = %+v (%v)", snapshot, err)
	}
	if badPrefix.Load() {
		t.Fatal("the agent listed outside its Barman server prefix")
	}
	status, _ := client.get(t, evidencev1alpha1.BackupsPath+"?revision="+strconv.FormatUint(snapshot.Revision, 10), agentConsoleToken)
	if status != http.StatusOK {
		t.Fatalf("backups = %d", status)
	}

	for _, test := range []struct {
		name, token string
		status      int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"API-server audience", agentAPIAudToken, http.StatusUnauthorized},
	} {
		status, body := client.get(t, evidencev1alpha1.SnapshotPath, test.token)
		var refusal evidencev1alpha1.EvidenceAPIError
		if status != test.status || json.Unmarshal(body, &refusal) != nil || refusal.Code != evidencev1alpha1.ErrorUnauthenticated {
			t.Fatalf("%s: %d %s", test.name, status, body)
		}
		if strings.Contains(string(body), "synthetic-bucket") {
			t.Fatalf("%s: refusal disclosed repository data", test.name)
		}
	}
	if status, _ := client.get(t, evidencev1alpha1.HealthPath, ""); status != http.StatusOK {
		t.Fatalf("unauthenticated health = %d", status)
	}

	scanner, ok := app.worker.(*inventory.Scanner)
	if !ok {
		t.Fatalf("worker = %T", app.worker)
	}
	failList.Store(true)
	if err := scanner.Refresh(context.Background()); err == nil {
		t.Fatal("failed refresh reported success")
	}
	_, body := client.get(t, evidencev1alpha1.SnapshotPath, agentConsoleToken)
	var stale evidencev1alpha1.RepositoryEvidenceSnapshot
	if json.Unmarshal(body, &stale) != nil || !stale.Stale || stale.State != evidencev1alpha1.Unknown ||
		stale.EvidenceGeneration != snapshot.EvidenceGeneration || stale.ProducerInstance != snapshot.ProducerInstance || stale.ProducerEpoch != 3 {
		t.Fatalf("failed refresh served = %s", body)
	}
}

func TestAgentRuntimeServesUnknownEvidenceOnRepositoryError(t *testing.T) {
	directory := t.TempDir()
	pool, certFile, keyFile := writeServingPair(t, directory)
	cfg := agentTestConfig(t, directory, certFile, keyFile, map[string]string{"DESTINATION_PATH": "s3://bucket/x?secret=canary"})
	if cfg.RepositoryError == nil {
		t.Fatal("the bad destination was not deferred")
	}
	factory := func(context.Context, config.Config, *redact.Redactor) (store.Reader, error) {
		return nil, errors.New("no store may be built")
	}
	app, err := newWithFactoryVersion(context.Background(), cfg, slog.New(slog.DiscardHandler), factory, "1.2.3", WithReviewer(&apiServerStub{}))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.ServeAgent(ctx, listener) }()
	defer func() { cancel(); <-done }()
	client := agentClient{
		client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: pool, ServerName: agentServerName, MinVersion: tls.VersionTLS13,
		}}},
		address: listener.Addr().String(),
	}
	status, body := client.get(t, evidencev1alpha1.SnapshotPath, agentConsoleToken)
	var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	if status != http.StatusOK || json.Unmarshal(body, &snapshot) != nil || snapshot.Validate() != nil {
		t.Fatalf("snapshot = %d %s", status, body)
	}
	if snapshot.State != evidencev1alpha1.Unknown || !snapshot.Stale || strings.Contains(string(body), "canary") {
		t.Fatalf("repository error served as %s", body)
	}
}

func TestAgentRuntimeRefusesToStartWithoutItsCertificate(t *testing.T) {
	directory := t.TempDir()
	cfg := agentTestConfig(t, directory, filepath.Join(directory, "missing.crt"), filepath.Join(directory, "missing.key"), nil)
	_, err := newWithFactoryVersion(context.Background(), cfg, slog.New(slog.DiscardHandler), fakeReaderFactory, "1.2.3", WithReviewer(&apiServerStub{}))
	if !errors.Is(err, agent.ErrInvalidCertificate) {
		t.Fatalf("start without a serving certificate = %v", err)
	}
}

type fixedKeys struct{}

func (fixedKeys) Keys(context.Context) ([]byte, []byte, []byte, error) {
	return []byte("synthetic"), []byte("synthetic"), nil, nil
}

// TestTeamAgentServesSharedTokenClientsOverPlainHTTP is the pgtools profile:
// AGENT_RESOLVE=cluster (the repository applied from a resolution), a shared
// token from a file and plain HTTP. No reviewer exists: a nil one would make
// the operator profile reach for the in-cluster API and fail here.
func TestTeamAgentServesSharedTokenClientsOverPlainHTTP(t *testing.T) {
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "token")
	const shared = "Zx81QpL0aVt7Kc2mN4bR9sWe3Yh6Ju5Gd0Fo1Ti8Lk2Mn7Bv"
	if err := os.WriteFile(tokenFile, []byte(shared+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"RUNTIME_MODE": "agent", "AGENT_AUTH": "sharedtoken", "AGENT_TOKEN_FILE": tokenFile, "AGENT_PLAIN_HTTP": "true",
		"AGENT_RESOLVE": "cluster", "AGENT_NAME": "pgc-evidence-orders", "AGENT_NAMESPACE": "database-team",
		"CNPG_CLUSTER_NAME": "orders", "AGENT_ALLOWED_SECRETS": "minio",
	}
	cfg, err := config.Load(func(key string) string { return environment[key] })
	if err != nil {
		t.Fatal(err)
	}
	cfg = config.ApplyResolved(cfg, config.ResolvedRepository{
		ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders", DestinationPath: "s3://synthetic-bucket/cluster",
		EndpointURL: "http://minio.storage.svc:9000", Keys: fixedKeys{},
	}, 1730000000, nil)
	var built config.Config
	factory := func(_ context.Context, cfg config.Config, _ *redact.Redactor) (store.Reader, error) {
		built = cfg
		return &storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) { return store.Page{}, nil }}, nil
	}
	app, err := newWithFactoryVersion(context.Background(), cfg, slog.New(slog.DiscardHandler), factory, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if app.certificates != nil || !app.plainHTTP || built.Credentials.AWSKeySource == nil {
		t.Fatalf("team agent assembled with certificates=%v plain=%v keys=%v", app.certificates, app.plainHTTP, built.Credentials.AWSKeySource)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.ServeAgent(ctx, listener) }()
	defer func() { cancel(); <-done }()

	get := func(token string) (int, []byte) {
		request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+listener.Addr().String()+evidencev1alpha1.SnapshotPath, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, body
	}
	var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, body := get(shared)
		if status == http.StatusOK && json.Unmarshal(body, &snapshot) == nil && snapshot.EvidenceGeneration > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no evidence published: %d %s", status, body)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snapshot.ProducerEpoch != 1730000000 || snapshot.Identity.Cluster.Name == nil || *snapshot.Identity.Cluster.Name != "orders" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	for _, token := range []string{"", "Qa5Ws1Ed9Rf3Tg7Yh2Uj6Ik0Ol4Pz8Xc5Vb1Nm9Mq3Wr7Et2", agentConsoleToken} {
		if status, _ := get(token); status != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", token, status)
		}
	}
}

// TestBoundedReason: a refusal longer than the contract's reason bound is
// published cut (on a rune boundary), not refused by the snapshot's
// validation, which would stop the Pod.
func TestBoundedReason(t *testing.T) {
	long := strings.Repeat("é", 200) + "\nend"
	for name, reason := range map[string]string{"short": "refused", "long": long, "blank": " \n"} {
		got := boundedReason(reason)
		if got == "" || len(got) > maxReason || !utf8.ValidString(got) || strings.ContainsAny(got, "\n\r\t") {
			t.Fatalf("%s: boundedReason = %q (%d bytes)", name, got, len(got))
		}
	}
	if got := boundedReason("refused"); got != "refused" {
		t.Fatalf("a short reason changed: %q", got)
	}
}

// TestTeamAgentServesALongRefusal is the lab finding: a team agent whose
// resolution was refused with a long reason crash-looped instead of
// publishing unknown evidence.
func TestTeamAgentServesALongRefusal(t *testing.T) {
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte("Zx81QpL0aVt7Kc2mN4bR9sWe3Yh6Ju5Gd0Fo1Ti8Lk2Mn7Bv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"RUNTIME_MODE": "agent", "AGENT_AUTH": "sharedtoken", "AGENT_TOKEN_FILE": tokenFile, "AGENT_PLAIN_HTTP": "true",
		"AGENT_RESOLVE": "cluster", "AGENT_NAME": "pgc-evidence-orders", "AGENT_NAMESPACE": "database-team",
		"CNPG_CLUSTER_NAME": "orders", "AGENT_ALLOWED_SECRETS": "minio",
	}
	cfg, err := config.Load(func(key string) string { return environment[key] })
	if err != nil {
		t.Fatal(err)
	}
	cfg = config.ApplyResolved(cfg, config.ResolvedRepository{ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191"},
		1730000000, errors.New(strings.Repeat("a refusal that names many objects ", 20)))
	factory := func(context.Context, config.Config, *redact.Redactor) (store.Reader, error) {
		return nil, errors.New("no store may be built")
	}
	if _, err := newWithFactoryVersion(context.Background(), cfg, slog.New(slog.DiscardHandler), factory, "1.2.3"); err != nil {
		t.Fatalf("a long refusal stopped the agent: %v", err)
	}
}
