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
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	serverHeaderBytes = 16 * 1024
	serverIdleTimeout = 60 * time.Second
	serverShutdown    = 15 * time.Second
	// certificateRecheck bounds how often a handshake stats the key pair
	// files; a rotated pair (the operator re-issued the leaf, or the service
	// CA rotated it) is served within this delay of the kubelet's update.
	certificateRecheck = 10 * time.Second
)

// ErrInvalidCertificate is returned when the serving key pair cannot be
// loaded. It never names the files' content.
var ErrInvalidCertificate = errors.New("agent serving certificate is invalid")

// CertificateReloader serves the key pair from two files and reloads it when
// either file changes. A pair that fails to load after a change keeps the
// previous one serving: a half-written rotation never takes the listener down.
type CertificateReloader struct {
	certFile, keyFile string
	now               func() time.Time

	mu          sync.Mutex
	certificate *tls.Certificate
	stamp       fileStamp
	checked     time.Time
}

type fileStamp struct {
	certModified, keyModified time.Time
	certSize, keySize         int64
}

// NewCertificateReloader loads the pair once; a failure is a start failure.
func NewCertificateReloader(certFile, keyFile string) (*CertificateReloader, error) {
	reloader := &CertificateReloader{certFile: certFile, keyFile: keyFile, now: time.Now}
	stamp, err := reloader.stampFiles()
	if err != nil {
		return nil, err
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, ErrInvalidCertificate
	}
	reloader.certificate, reloader.stamp, reloader.checked = &certificate, stamp, reloader.now()
	return reloader, nil
}

// GetCertificate implements tls.Config.GetCertificate.
func (r *CertificateReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Sub(r.checked) < certificateRecheck {
		return r.certificate, nil
	}
	r.checked = now
	stamp, err := r.stampFiles()
	if err != nil || stamp == r.stamp {
		return r.certificate, nil
	}
	certificate, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return r.certificate, nil
	}
	r.certificate, r.stamp = &certificate, stamp
	return r.certificate, nil
}

func (r *CertificateReloader) stampFiles() (fileStamp, error) {
	cert, err := os.Stat(r.certFile)
	if err != nil {
		return fileStamp{}, ErrInvalidCertificate
	}
	key, err := os.Stat(r.keyFile)
	if err != nil {
		return fileStamp{}, ErrInvalidCertificate
	}
	return fileStamp{certModified: cert.ModTime(), keyModified: key.ModTime(), certSize: cert.Size(), keySize: key.Size()}, nil
}

// TLSConfig is the agent's server TLS configuration: TLS 1.3 only, the
// reloading certificate (net/http adds the ALPN protocols).
func TLSConfig(certificates *CertificateReloader) *tls.Config {
	return &tls.Config{
		MinVersion:     evidencev1alpha1.MinimumTLSVersion,
		GetCertificate: certificates.GetCertificate,
	}
}

// Serve serves handler over TLS on listener until ctx is canceled, then
// shuts down gracefully.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler, certificates *CertificateReloader) error {
	if certificates == nil {
		return errors.New("agent server configuration is invalid")
	}
	return serve(ctx, listener, handler, certificates)
}

// ServePlain serves handler over plain HTTP (AGENT_PLAIN_HTTP=true, the
// pgtools chart's evidence.tls.mode=none). Inside one namespace TLS adds
// little: whoever can tamper with the path (edit the Service, start a pod
// with the console's labels, read the namespace's Secrets) can already read
// the object-store keys the agent holds, and what travels is backup
// metadata and a token, never backup data or keys.
func ServePlain(ctx context.Context, listener net.Listener, handler http.Handler) error {
	return serve(ctx, listener, handler, nil)
}

// serve runs the server, over TLS when certificates is set.
func serve(ctx context.Context, listener net.Listener, handler http.Handler, certificates *CertificateReloader) error {
	if ctx == nil || listener == nil || handler == nil {
		return errors.New("agent server configuration is invalid")
	}
	runtimeCtx, cancelRuntime := context.WithCancel(ctx)
	defer cancelRuntime()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: evidencev1alpha1.RequestTimeout,
		ReadTimeout:       evidencev1alpha1.RequestTimeout,
		WriteTimeout:      2 * evidencev1alpha1.RequestTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    serverHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return runtimeCtx },
		// Handshake noise from probes and scanners is not an application
		// event; request outcomes are logged by the handler.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	serveErrors := make(chan error, 1)
	if certificates != nil {
		server.TLSConfig = TLSConfig(certificates)
		go func() { serveErrors <- server.ServeTLS(listener, "", "") }()
	} else {
		go func() { serveErrors <- server.Serve(listener) }()
	}
	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serverShutdown)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		if err := <-serveErrors; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
