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

package evidence

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
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	testNamespace = "team-a"
	testService   = "orders-evidence"
	testCluster   = "orders"
	testAgent     = "orders"
	testServer    = "orders"
	testEpoch     = 7
	// testToken is a syntactically valid compact JWS; a canary for the
	// no-token-in-logs assertion.
	testToken = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJjYW5hcnkifQ.c2lnbmF0dXJlLWNhbmFyeQ"
)

var testNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// testCA is a CA and a serving certificate it signed for some DNS names.
type testCA struct {
	pem  []byte
	cert tls.Certificate
}

// newTestCA generates a CA and a serving certificate for names.
func newTestCA(t *testing.T, names ...string) testCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test evidence CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{
		pem:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		cert: tls.Certificate{Certificate: [][]byte{serverDER}, PrivateKey: serverKey},
	}
}

// fixtureSnapshot is the contract's wire golden snapshot, bound to the
// test agent.
func fixtureSnapshot(t *testing.T) evidencev1alpha1.RepositoryEvidenceSnapshot {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "evidence", "v1alpha1", "testdata", "wire", "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Identity.Cluster.Namespace = testNamespace
	snapshot.ProducerEpoch = testEpoch
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// fixtureBackup is the contract's wire golden backup item.
func fixtureBackup(t *testing.T, id string) evidencev1alpha1.BarmanBackup {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "evidence", "v1alpha1", "testdata", "wire", "backups.json"))
	if err != nil {
		t.Fatal(err)
	}
	var page evidencev1alpha1.BarmanBackupPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	item := page.Items[0]
	item.BackupID = id
	return item
}

// fixtureRecovery is the contract's wire golden recovery-path page,
// bound to the snapshot's publication.
func fixtureRecovery(t *testing.T, snapshot evidencev1alpha1.RepositoryEvidenceSnapshot) *evidencev1alpha1.BarmanRecoveryPathPage {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "evidence", "v1alpha1", "testdata", "wire", "recovery-paths.json"))
	if err != nil {
		t.Fatal(err)
	}
	var page evidencev1alpha1.BarmanRecoveryPathPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	page.Revision, page.EvidenceGeneration = snapshot.Revision, snapshot.EvidenceGeneration
	return &page
}

// backupPages splits items into pages of size, chained by cursors "c1",
// "c2", ...
func backupPages(snapshot evidencev1alpha1.RepositoryEvidenceSnapshot, items []evidencev1alpha1.BarmanBackup, size int) []evidencev1alpha1.BarmanBackupPage {
	total := uint64(len(items))
	var pages []evidencev1alpha1.BarmanBackupPage
	for start := 0; start < len(items) || start == 0; start += size {
		end := min(start+size, len(items))
		page := evidencev1alpha1.BarmanBackupPage{
			PageHeader: evidencev1alpha1.PageHeader{
				APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.BarmanBackupPageKind,
				Revision: snapshot.Revision, EvidenceGeneration: snapshot.EvidenceGeneration, TotalItems: &total,
			},
			Items: append([]evidencev1alpha1.BarmanBackup{}, items[start:end]...),
		}
		if end < len(items) {
			cursor := "c" + itoa(len(pages)+1)
			page.NextCursor = &cursor
		}
		pages = append(pages, page)
		if end >= len(items) {
			break
		}
	}
	return pages
}

func itoa(value int) string { return strconv.Itoa(value) }

// fakeAgent is a scripted evidence agent behind an httptest TLS server.
type fakeAgent struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// snapshot is served on SnapshotPath unless snapshotBody is set.
	snapshot     evidencev1alpha1.RepositoryEvidenceSnapshot
	snapshotBody []byte
	// pages are served on BackupsPath: the first without a cursor, then
	// page i+1 at cursor "c<i+1>".
	pages []evidencev1alpha1.BarmanBackupPage
	// status, when non-zero, answers every evidence route with this status
	// and an error body of errorCode.
	status    int
	errorCode evidencev1alpha1.ErrorCode
	// conflictOnce answers the first backups request with 409.
	conflictOnce bool
	// recovery is served on RecoveryPathsPath (one page); nil answers 404
	// like an agent without the collection. recoveryConflictOnce answers
	// the first recovery request with 409; recoveryRequests counts them.
	recovery             *evidencev1alpha1.BarmanRecoveryPathPage
	recoveryConflictOnce bool
	// pgbackrestPages and pgbackrestRecovery replace pages and recovery
	// for an agent reading a pgBackRest stanza.
	pgbackrestPages    []evidencev1alpha1.PGBackRestBackupPage
	pgbackrestRecovery *evidencev1alpha1.PGBackRestRecoveryPathPage
	recoveryRequests   int
	contentType        string
	redirect           bool
	// requests counts evidence requests; snapshots and backups by route.
	snapshots, backupRequests int
	// sawToken is false when a request came without the expected token.
	badAuth bool
	// token is the bearer token the agent expects; testToken when empty.
	token string
}

func newFakeAgent(t *testing.T, ca testCA) *fakeAgent {
	t.Helper()
	agent := &fakeAgent{t: t, contentType: evidencev1alpha1.MediaType}
	server := httptest.NewUnstartedServer(http.HandlerFunc(agent.serve))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{ca.cert}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	agent.server = server
	return agent
}

func (a *fakeAgent) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, a.server.Listener.Addr().String())
}

func (a *fakeAgent) writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		a.t.Error(err)
	}
	w.Header().Set("Content-Type", a.contentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (a *fakeAgent) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	expected := a.token
	if expected == "" {
		expected = testToken
	}
	if r.Header.Get("Authorization") != "Bearer "+expected || r.Header.Get("Accept") != evidencev1alpha1.MediaType {
		a.badAuth = true
	}
	if a.redirect {
		http.Redirect(w, r, "https://elsewhere.invalid/", http.StatusFound)
		return
	}
	if a.status != 0 {
		a.writeJSON(w, a.status, evidencev1alpha1.EvidenceAPIError{
			APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ErrorKind, Code: a.errorCode, Message: "refused",
		})
		return
	}
	switch r.URL.Path {
	case evidencev1alpha1.SnapshotPath:
		a.snapshots++
		if a.snapshotBody != nil {
			w.Header().Set("Content-Type", a.contentType)
			_, _ = w.Write(a.snapshotBody)
			return
		}
		a.writeJSON(w, http.StatusOK, a.snapshot)
	case evidencev1alpha1.BackupsPath:
		a.backupRequests++
		if a.conflictOnce {
			a.conflictOnce = false
			a.writeJSON(w, http.StatusConflict, evidencev1alpha1.EvidenceAPIError{
				APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ErrorKind, Code: evidencev1alpha1.ErrorPublicationChanged, Message: "changed",
			})
			return
		}
		query := r.URL.Query()
		if query.Get("revision") != itoa(int(a.snapshot.Revision)) {
			a.writeJSON(w, http.StatusConflict, evidencev1alpha1.EvidenceAPIError{
				APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ErrorKind, Code: evidencev1alpha1.ErrorPublicationChanged, Message: "changed",
			})
			return
		}
		index := 0
		if cursor := query.Get("cursor"); cursor != "" {
			for i := range a.pages {
				if a.pages[i].NextCursor != nil && *a.pages[i].NextCursor == cursor {
					index = i + 1
				}
			}
		}
		if a.pgbackrestPages != nil {
			if cursor := query.Get("cursor"); cursor != "" {
				for i := range a.pgbackrestPages {
					if a.pgbackrestPages[i].NextCursor != nil && *a.pgbackrestPages[i].NextCursor == cursor {
						index = i + 1
					}
				}
			}
			if index < len(a.pgbackrestPages) {
				a.writeJSON(w, http.StatusOK, a.pgbackrestPages[index])
				return
			}
		}
		if index >= len(a.pages) {
			a.writeJSON(w, http.StatusBadRequest, evidencev1alpha1.EvidenceAPIError{
				APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ErrorKind, Code: evidencev1alpha1.ErrorInvalidRequest, Message: "bad",
			})
			return
		}
		a.writeJSON(w, http.StatusOK, a.pages[index])
	case evidencev1alpha1.RecoveryPathsPath:
		a.recoveryRequests++
		if a.pgbackrestRecovery != nil {
			a.writeJSON(w, http.StatusOK, a.pgbackrestRecovery)
			return
		}
		if a.recovery == nil {
			http.NotFound(w, r)
			return
		}
		if a.recoveryConflictOnce {
			a.recoveryConflictOnce = false
			a.writeJSON(w, http.StatusConflict, evidencev1alpha1.EvidenceAPIError{
				APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ErrorKind, Code: evidencev1alpha1.ErrorPublicationChanged, Message: "changed",
			})
			return
		}
		a.writeJSON(w, http.StatusOK, a.recovery)
	default:
		http.NotFound(w, r)
	}
}

func (a *fakeAgent) set(change func(*fakeAgent)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	change(a)
}

// testEvidenceAgent is the EvidenceAgent of the fake agent.
func testEvidenceAgent(name string, ca testCA, snapshot evidencev1alpha1.RepositoryEvidenceSnapshot, created time.Time) *pgtoolboxv1alpha1.EvidenceAgent {
	return &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace, UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(created),
			ManagedFields:     operatorManagedFields(OperatorStatusManager),
		},
		Spec: pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: testCluster}},
		Status: pgtoolboxv1alpha1.EvidenceAgentStatus{
			Endpoint:      evidencev1alpha1.Endpoint(testService, testNamespace),
			CABundle:      ca.pem,
			Fingerprint:   snapshot.Identity.Repository.DestinationFingerprint,
			ProducerEpoch: testEpoch,
			ServerName:    testServer,
		},
	}
}

// writeToken writes testNamespace's projected token file and returns the
// token directory (<dir>/<namespace>/token).
func writeToken(t *testing.T, content string) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, testNamespace), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, testNamespace, "token"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return directory
}

// operatorManagedFields is what the API server records once the operator
// published status.endpoint and status.caBundle with its status field
// manager (a merge patch on the status subresource).
func operatorManagedFields(manager string) []metav1.ManagedFieldsEntry {
	return []metav1.ManagedFieldsEntry{
		{
			Manager: "pgtoolbox-operator", Operation: metav1.ManagedFieldsOperationApply, APIVersion: "pgtoolbox.fyannk.dev/v1alpha1",
			FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:clusterRef":{}}}`),
		},
		{
			Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "pgtoolbox.fyannk.dev/v1alpha1", Subresource: "status",
			FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(`{"f:status":{"f:caBundle":{},"f:endpoint":{},"f:fingerprint":{},"f:producerEpoch":{}}}`),
		},
	}
}

// newFakeLister is the controller-runtime fake client with the v1alpha1
// scheme, holding objects.
func newFakeLister(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := pgtoolboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithReturnManagedFields().
		WithStatusSubresource(&pgtoolboxv1alpha1.EvidenceAgent{}).Build()
}

// newTestRunner builds a runner over lister, dialing agent.
func newTestRunner(t *testing.T, lister AgentLister, agent *fakeAgent, tokenDirectory string, opts ...func(*Options)) *Runner {
	t.Helper()
	options := Options{
		Lister: lister, Namespaces: []string{testNamespace}, TokenDirectory: tokenDirectory,
		Now: func() time.Time { return testNow },
	}
	for _, opt := range opts {
		opt(&options)
	}
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if agent != nil {
		runner.dial = agent.dial
	}
	return runner
}
