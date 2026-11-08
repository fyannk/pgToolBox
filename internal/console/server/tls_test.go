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

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeKeyPair writes a self-signed serving certificate for 127.0.0.1 and
// returns the file paths and a pool trusting it.
func writeKeyPair(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pgtoolbox-console"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certFile, keyFile = filepath.Join(directory, "tls.crt"), filepath.Join(directory, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(certificate)
	return certFile, keyFile, roots
}

// TestProbeListenerFollowsTLS: with TLS_CERT_FILE/TLS_KEY_FILE the probe
// listener, and so /metrics, is HTTPS with the main listener's
// certificate and refuses plain HTTP; without them both are plain HTTP.
func TestProbeListenerFollowsTLS(t *testing.T) {
	t.Parallel()
	certFile, keyFile, roots := writeKeyPair(t)
	for _, tc := range []struct {
		name string
		tls  bool
	}{{"tls", true}, {"plain", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options := Options{Auth: testAuth(t), Ready: &Readiness{}, Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "pgtoolbox_console_up 1\n")
			})}
			if tc.tls {
				options.TLSCertFile, options.TLSKeyFile = certFile, keyFile
			}
			s := New(options)
			var lc net.ListenConfig
			ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			probe, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- s.Serve(ctx, ln, probe) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()

			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			}}
			fetch := func(scheme, path string) (int, error) {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, scheme+"://"+probe.Addr().String()+path, http.NoBody)
				if err != nil {
					return 0, err
				}
				response, err := client.Do(request)
				if err != nil {
					return 0, err
				}
				defer func() { _ = response.Body.Close() }()
				_, _ = io.Copy(io.Discard, response.Body)
				return response.StatusCode, nil
			}
			secure, insecure := "https", "http"
			if !tc.tls {
				secure, insecure = "http", "https"
			}
			if status, err := fetch(secure, PathHealth); err != nil || status != http.StatusOK {
				t.Fatalf("%s %s = %d, %v", secure, PathHealth, status, err)
			}
			if status, err := fetch(secure, "/metrics"); err != nil || status != http.StatusOK {
				t.Fatalf("%s /metrics = %d, %v", secure, status, err)
			}
			if status, err := fetch(insecure, "/metrics"); err == nil && status == http.StatusOK {
				t.Fatalf("/metrics answered 200 over %s", insecure)
			}
		})
	}
}
