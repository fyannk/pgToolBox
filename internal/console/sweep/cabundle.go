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
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// maxBundleBytes bounds the CA bundle read: a PEM CA is a couple of
// kilobytes, and a team console watches a short list of Clusters.
const maxBundleBytes = 1 << 20

// Bundle is the state of the admin-filled CA bundle.
type Bundle struct {
	// Pool verifies the status port; nil when no usable certificate.
	Pool *x509.CertPool
	// Certificates counts the CA certificates loaded.
	Certificates int
	// Problem says, when Pool is nil, why every instance is "not
	// verifiable"; secret-free (a path and a fact, never content).
	Problem string
	// Digest identifies the loaded content, for change detection and
	// logs; empty when nothing loaded.
	Digest string
}

// CABundle holds the current bundle and reloads it when the file
// changes. A ConfigMap mount is a directory of symlinks the kubelet
// swaps atomically (..data), so the directory is watched, not the file.
type CABundle struct {
	path   string
	logger *slog.Logger
	mu     sync.RWMutex
	cur    Bundle
}

// NewCABundle loads path once. An empty path yields a bundle whose
// Problem says no bundle is configured.
func NewCABundle(path string, logger *slog.Logger) *CABundle {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	b := &CABundle{path: path, logger: logger}
	b.reload()
	return b
}

// Current returns the bundle in force.
func (b *CABundle) Current() Bundle {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cur
}

// reload reads the file and swaps the bundle; it logs only on change.
func (b *CABundle) reload() {
	next := loadBundle(b.path)
	b.mu.Lock()
	changed := next.Digest != b.cur.Digest || next.Problem != b.cur.Problem
	b.cur = next
	b.mu.Unlock()
	if !changed {
		return
	}
	if next.Pool == nil {
		b.logger.Warn("instance status CA bundle unusable; every instance is shown as not verifiable", slog.String("problem", next.Problem))
		return
	}
	b.logger.Info("instance status CA bundle loaded", slog.Int("certificates", next.Certificates), slog.String("digest", next.Digest))
}

// Watch reloads the bundle on every change of its directory until ctx
// is done. Without a path there is nothing to watch.
func (b *CABundle) Watch(ctx context.Context) error {
	if b.path == "" {
		<-ctx.Done()
		return nil
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(filepath.Dir(b.path)); err != nil {
		return err
	}
	b.reload() // close the gap between the first load and the watch
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-watcher.Events:
			if !ok {
				return errors.New("CA bundle watch closed")
			}
			b.reload()
		case werr, ok := <-watcher.Errors:
			if !ok {
				return errors.New("CA bundle watch closed")
			}
			b.logger.Warn("CA bundle watch error", slog.String("error", werr.Error()))
		}
	}
}

// loadBundle parses a PEM file into a pool. Only CERTIFICATE blocks are
// accepted; any other block (a private key pasted by mistake) makes the
// whole bundle unusable, because a bundle that carries a key is a bundle
// someone filled from the <cluster>-ca Secret wholesale, and the console
// must not hold that material.
func loadBundle(path string) Bundle {
	if path == "" {
		return Bundle{Problem: "no CA bundle configured (console.instanceStatus.caBundleConfigMap)"}
	}
	f, err := os.Open(path) // #nosec G304 -- the operator-mounted bundle path, validated absolute and clean by config.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Bundle{Problem: "CA bundle file " + path + " does not exist"}
		}
		return Bundle{Problem: "CA bundle file " + path + " is not readable"}
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxBundleBytes+1))
	if err != nil {
		return Bundle{Problem: "CA bundle file " + path + " is not readable"}
	}
	if len(raw) > maxBundleBytes {
		return Bundle{Problem: "CA bundle file " + path + " exceeds 1 MiB"}
	}
	return parseBundle(path, raw)
}

func parseBundle(path string, raw []byte) Bundle {
	pool := x509.NewCertPool()
	count := 0
	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return Bundle{Problem: "CA bundle " + path + " contains a " + block.Type + " block; it must hold ca.crt certificates only"}
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return Bundle{Problem: "CA bundle " + path + " contains a certificate that does not parse"}
		}
		pool.AddCert(cert)
		count++
	}
	if count == 0 {
		return Bundle{Problem: "CA bundle " + path + " holds no certificate yet; add each watched Cluster's ca.crt"}
	}
	sum := sha256.Sum256(raw)
	return Bundle{Pool: pool, Certificates: count, Digest: hexPrefix(sum[:])}
}

func hexPrefix(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 16)
	for _, c := range b[:8] {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}
