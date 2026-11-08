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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/config"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest/pgbackrestfixture"
	"github.com/fyannk/pgToolBox/internal/evidence/redact"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func TestMultiPrefixReaderWalksOnlyItsPrefixes(t *testing.T) {
	t.Parallel()
	objects := []store.Object{
		{Key: "archive/demo/17-1/0000000100000000/000000010000000000000001-aa.gz"},
		{Key: "archive/demo/archive.info"},
		{Key: "archive/other/archive.info"},
		{Key: "backup/demo/backup.info"},
		{Key: "backup/other/backup.info"},
	}
	var mu sync.Mutex
	var prefixes []string
	inner := &storetest.Fake{ListFunc: func(_ context.Context, request store.ListRequest) (store.Page, error) {
		mu.Lock()
		prefixes = append(prefixes, request.Prefix)
		mu.Unlock()
		page := store.Page{}
		for _, object := range objects {
			if !strings.HasPrefix(object.Key, request.Prefix) || request.Cursor != "" && object.Key <= request.Cursor {
				continue
			}
			if len(page.Objects) == request.Limit {
				page.NextCursor = page.Objects[len(page.Objects)-1].Key
				break
			}
			page.Objects = append(page.Objects, object)
		}
		return page, nil
	}}
	reader, err := newScopeConfinedReader(inner, []string{"backup/demo/", "archive/demo/"})
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	cursor := ""
	for range 10 {
		page, err := reader.List(context.Background(), store.ListRequest{Cursor: cursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range page.Objects {
			listed = append(listed, object.Key)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{objects[0].Key, objects[1].Key, objects[3].Key}
	if !slices.Equal(listed, want) {
		t.Fatalf("listed %v, want %v", listed, want)
	}
	for _, prefix := range prefixes {
		if prefix != "archive/demo/" && prefix != "backup/demo/" {
			t.Fatalf("the provider was asked for %q", prefix)
		}
	}
	for name, call := range map[string]func() error{
		"list another stanza": func() error {
			_, err := reader.List(context.Background(), store.ListRequest{Prefix: "backup/other/", Limit: 1})
			return err
		},
		"open another stanza": func() error {
			_, err := reader.Open(context.Background(), store.OpenRequest{Key: "backup/other/backup.info", MaxBytes: 1})
			return err
		},
		"stat another stanza": func() error {
			_, err := reader.Stat(context.Background(), store.StatRequest{Key: "archive/other/archive.info"})
			return err
		},
		"a forged cursor": func() error {
			_, err := reader.List(context.Background(), store.ListRequest{Cursor: "7|x", Limit: 1})
			return err
		},
	} {
		if err := call(); !errors.Is(err, store.ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	malicious, err := newScopeConfinedReader(&storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		return store.Page{Objects: []store.Object{{Key: "backup/other/backup.info"}}}, nil
	}}, []string{"archive/demo/", "backup/demo/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := malicious.List(context.Background(), store.ListRequest{Limit: 1}); fault.Categorize(err) != fault.SafetyLimit {
		t.Fatalf("an out-of-scope provider answer: %v", err)
	}
	if _, err := newScopeConfinedReader(inner, []string{"backup/demo/", "backup/demo/x/"}); err == nil {
		t.Fatal("overlapping prefixes accepted")
	}
}

// TestAgentRuntimeServesAPGBackRestStanza runs the agent over the encrypted
// B1 fixture beside another repository's objects: it lists only its two stanza
// directories, opens the encrypted info files with the configured
// passphrase, and serves the pgBackRest profile over HTTPS without the
// passphrase ever reaching a response or a log line.
func TestAgentRuntimeServesAPGBackRestStanza(t *testing.T) {
	directory := t.TempDir()
	pool, certFile, keyFile := writeServingPair(t, directory)
	cipherPath := filepath.Join(directory, "cipher")
	if err := os.WriteFile(cipherPath, []byte(pgbackrestfixture.Passphrase), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := agentTestConfig(t, directory, certFile, keyFile, map[string]string{
		"REPOSITORY_FORMAT": "pgbackrest", "BARMAN_SERVER_NAMES": "", "PGBACKREST_STANZA": "demo",
		"PGBACKREST_CIPHER_PASS_FILE": cipherPath, "DESTINATION_PATH": "s3://synthetic-bucket/pgbr/orders",
	})
	repository := pgbackrestfixture.Merge(pgbackrestfixture.Load(t, "encrypted"), pgbackrestfixture.Load(t, "plain").Prefixed("other/"))
	inner := repository.Reader(t)
	var mu sync.Mutex
	var prefixes []string
	factory := func(context.Context, config.Config, *redact.Redactor) (store.Reader, error) {
		return &storetest.Fake{
			ListFunc: func(ctx context.Context, request store.ListRequest) (store.Page, error) {
				mu.Lock()
				prefixes = append(prefixes, request.Prefix)
				mu.Unlock()
				return inner.List(ctx, request)
			},
			OpenFunc: func(ctx context.Context, request store.OpenRequest) (io.ReadCloser, error) {
				return inner.Open(ctx, request)
			},
		}, nil
	}
	logs := &bytes.Buffer{}
	var logMu sync.Mutex
	logger := slog.New(slog.NewJSONHandler(&lockedWriter{mu: &logMu, writer: logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	app, err := newWithFactoryVersion(context.Background(), cfg, logger, factory, "1.2.3", WithReviewer(&apiServerStub{}))
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
	var body []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status int
		status, body = client.get(t, evidencev1alpha1.SnapshotPath, agentConsoleToken)
		if status == http.StatusOK && json.Unmarshal(body, &snapshot) == nil && snapshot.EvidenceGeneration > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no evidence published: %d %s", status, body)
		}
		time.Sleep(5 * time.Millisecond)
	}
	details := snapshot.Details.PGBackRest
	if err := snapshot.Validate(); err != nil || details == nil || details.Stanza != "demo" || details.Encrypted == nil || !*details.Encrypted || snapshot.State != evidencev1alpha1.Warning {
		t.Fatalf("snapshot %s (%v)", body, err)
	}
	if snapshot.Identity.Repository.Scope != (evidencev1alpha1.ScopeIdentity{Kind: "stanza", Name: "demo"}) || snapshot.Identity.Repository.Format != "pgbackrest" {
		t.Fatalf("identity %+v", snapshot.Identity)
	}
	status, page := client.get(t, evidencev1alpha1.BackupsPath+"?revision="+strconv.FormatUint(snapshot.Revision, 10), agentConsoleToken)
	var backups evidencev1alpha1.PGBackRestBackupPage
	if status != http.StatusOK || json.Unmarshal(page, &backups) != nil || backups.Validate() != nil || len(backups.Items) != 4 {
		t.Fatalf("backups %d %s", status, page)
	}
	mu.Lock()
	for _, prefix := range prefixes {
		if prefix != "archive/demo/" && prefix != "backup/demo/" {
			t.Errorf("the agent listed %q", prefix)
		}
	}
	mu.Unlock()
	logMu.Lock()
	defer logMu.Unlock()
	for _, text := range []string{string(body), string(page), logs.String()} {
		if strings.Contains(text, pgbackrestfixture.Passphrase) {
			t.Fatal("the passphrase reached a response or a log")
		}
	}
}

type lockedWriter struct {
	mu     *sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}
