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

package agent

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
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testServerName = "orders-evidence.team-a.svc"

// testPKI is a CA and a leaf for testServerName, written as PEM files.
type testPKI struct {
	pool     *x509.CertPool
	certFile string
	keyFile  string
}

func newTestPKI(t *testing.T, directory string) testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test evidence agent CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: testServerName},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), DNSNames: []string{testServerName},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	pki := testPKI{pool: x509.NewCertPool(), certFile: filepath.Join(directory, "tls.crt"), keyFile: filepath.Join(directory, "tls.key")}
	pki.pool.AddCert(ca)
	writeFile(t, pki.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}))
	writeFile(t, pki.keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return pki
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func startTestServer(t *testing.T, reloader *CertificateReloader) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}), reloader)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return listener.Addr().String()
}

func get(t *testing.T, address string, config *tls.Config) (int, error) {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(getRequest(t, "https://"+address+"/healthz"))
	if err != nil {
		return 0, err
	}
	_ = response.Body.Close()
	return response.StatusCode, nil
}

func TestServeIsTLS13WithTheMountedCertificate(t *testing.T) {
	pki := newTestPKI(t, t.TempDir())
	reloader, err := NewCertificateReloader(pki.certFile, pki.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	address := startTestServer(t, reloader)

	if status, err := get(t, address, &tls.Config{RootCAs: pki.pool, ServerName: testServerName, MinVersion: tls.VersionTLS13}); err != nil || status != http.StatusNoContent {
		t.Fatalf("verified TLS 1.3 client: %d, %v", status, err)
	}
	if _, err := get(t, address, &tls.Config{RootCAs: pki.pool, ServerName: testServerName, MaxVersion: tls.VersionTLS12}); err == nil {
		t.Fatal("a TLS 1.2 client was served")
	}
	if _, err := get(t, address, &tls.Config{RootCAs: pki.pool, ServerName: "other.team-a.svc", MinVersion: tls.VersionTLS13}); err == nil {
		t.Fatal("the certificate verified for a foreign server name")
	}
	if _, err := get(t, address, &tls.Config{ServerName: testServerName, MinVersion: tls.VersionTLS13}); err == nil {
		t.Fatal("the certificate verified without the agent's CA bundle")
	}
}

func TestCertificateReloaderFollowsRotationAndSurvivesABadOne(t *testing.T) {
	directory := t.TempDir()
	first := newTestPKI(t, directory)
	reloader, err := NewCertificateReloader(first.certFile, first.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	now := &clock{now: time.Now()}
	reloader.now = now.Now
	address := startTestServer(t, reloader)

	// Rotation to a new pair (a new CA, so the trust check tells them apart).
	second := newTestPKI(t, directory)
	if _, err := get(t, address, &tls.Config{RootCAs: second.pool, ServerName: testServerName, MinVersion: tls.VersionTLS13}); err == nil {
		t.Fatal("the rotated pair was served before the recheck interval")
	}
	now.Advance(certificateRecheck + time.Second)
	if _, err := get(t, address, &tls.Config{RootCAs: second.pool, ServerName: testServerName, MinVersion: tls.VersionTLS13}); err != nil {
		t.Fatalf("the rotated pair is not served: %v", err)
	}

	// A half-written rotation keeps the last good pair.
	writeFile(t, second.keyFile, []byte("not a key"))
	now.Advance(certificateRecheck + time.Second)
	if _, err := get(t, address, &tls.Config{RootCAs: second.pool, ServerName: testServerName, MinVersion: tls.VersionTLS13}); err != nil {
		t.Fatalf("a broken rotation took the listener down: %v", err)
	}
}

func TestNewCertificateReloaderFailsTheStart(t *testing.T) {
	directory := t.TempDir()
	pki := newTestPKI(t, directory)
	writeFile(t, filepath.Join(directory, "garbage"), []byte("garbage"))
	for _, files := range [][2]string{
		{filepath.Join(directory, "missing.crt"), pki.keyFile},
		{pki.certFile, filepath.Join(directory, "missing.key")},
		{pki.certFile, filepath.Join(directory, "garbage")},
	} {
		if _, err := NewCertificateReloader(files[0], files[1]); !errors.Is(err, ErrInvalidCertificate) {
			t.Fatalf("NewCertificateReloader(%v) = %v", files, err)
		}
	}
}

func TestServeShutsDownOnCancel(t *testing.T) {
	pki := newTestPKI(t, t.TempDir())
	reloader, err := NewCertificateReloader(pki.certFile, pki.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, http.NotFoundHandler(), reloader) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
	if err := Serve(context.Background(), nil, http.NotFoundHandler(), reloader); err == nil {
		t.Fatal("Serve accepted a nil listener")
	}
}

func TestServePlainServesHTTPAndNoTLS(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServePlain(ctx, listener, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}))
	}()
	address := listener.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(getRequest(t, "http://"+address+"/healthz"))
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("plain GET: %v %v", response, err)
	}
	_ = response.Body.Close()
	if _, err := get(t, address, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}); err == nil { // #nosec G402 -- the test proves no TLS is offered.
		t.Fatal("the plain listener answered a TLS handshake")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServePlain after cancel = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServePlain did not stop")
	}
	if err := Serve(context.Background(), listener, http.NotFoundHandler(), nil); err == nil {
		t.Fatal("Serve accepted no certificate")
	}
}

// getRequest builds a GET bound to the test's context, so a request still in
// flight when the test ends is cancelled with it.
func getRequest(t testing.TB, url string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
