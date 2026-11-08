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
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/config"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/redact"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func fakeReaderFactory(context.Context, config.Config, *redact.Redactor) (store.Reader, error) {
	return &storetest.Fake{}, nil
}

func TestServeStopsGracefullyWhenContextIsCanceled(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(func(key string) string {
		return map[string]string{
			"REPOSITORY_FORMAT": "barman-cloud",
			"PROVIDER":          "s3",
			"DESTINATION_PATH":  "s3://backups/prefix",
			"AWS_REGION":        "eu-west-1",
		}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := newWithFactory(context.Background(), cfg, slog.New(slog.DiscardHandler), fakeReaderFactory)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, listener) }()

	response, err := http.DefaultClient.Do(getRequest(t, "http://"+listener.Addr().String()+"/healthz"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve() did not stop after cancellation")
	}
}

func TestApplicationNeverRendersOrLogsConfiguredSecrets(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	accessCanary := "access-canary-never-cross-boundary"
	secretCanary := "secret-canary-never-cross-boundary"
	accessPath := filepath.Join(directory, "access")
	secretPath := filepath.Join(directory, "secret")
	if err := os.WriteFile(accessPath, []byte(accessCanary), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte(secretCanary), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(func(key string) string {
		return map[string]string{
			"REPOSITORY_FORMAT":          "barman-cloud",
			"PROVIDER":                   "s3",
			"DESTINATION_PATH":           "s3://topology-canary/private-prefix",
			"AWS_REGION":                 "eu-west-1",
			"AWS_ACCESS_KEY_ID_FILE":     accessPath,
			"AWS_SECRET_ACCESS_KEY_FILE": secretPath,
		}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	app, err := newWithFactory(context.Background(), cfg, slog.New(slog.NewJSONHandler(logs, nil)), fakeReaderFactory)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, listener) }()

	response, err := http.DefaultClient.Do(getRequest(t, "http://"+listener.Addr().String()+"/"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	output := string(body) + logs.String()
	for _, forbidden := range []string{accessCanary, secretCanary, accessPath, secretPath, "topology-canary", "private-prefix"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("application boundary disclosed %q", forbidden)
		}
	}
}

func TestServeDrainsInflightRequestOnCancellation(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(func(key string) string {
		return map[string]string{
			"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3", "DESTINATION_PATH": "s3://backups/prefix", "AWS_REGION": "eu-west-1",
		}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := newWithFactory(context.Background(), cfg, slog.New(slog.DiscardHandler), fakeReaderFactory)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	app.server.Handler = http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		writer.WriteHeader(http.StatusNoContent)
	})
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- app.Serve(ctx, listener) }()
	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(getRequest(t, "http://"+listener.Addr().String()+"/"))
		if requestErr == nil {
			_ = response.Body.Close()
		}
		requestDone <- requestErr
	}()
	<-started
	cancel()
	select {
	case err := <-serveDone:
		t.Fatalf("Serve returned before the in-flight request drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatalf("in-flight request failed during drain: %v", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
}

func TestBrowserUsesCompletedCacheAndPerformsNoStoreCalls(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(func(key string) string {
		return map[string]string{
			"REPOSITORY_FORMAT": "barman-cloud", "PROVIDER": "s3", "DESTINATION_PATH": "s3://backups/prefix", "AWS_REGION": "eu-west-1",
		}[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	var listCalls atomic.Int64
	factory := func(context.Context, config.Config, *redact.Redactor) (store.Reader, error) {
		return &storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
			listCalls.Add(1)
			return store.Page{Objects: []store.Object{{Key: "alpha/base/20260727/backup.info", Size: 21}}}, nil
		}}, nil
	}
	app, err := newWithFactory(context.Background(), cfg, slog.New(slog.DiscardHandler), factory)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, listener) }()
	baseURL := "http://" + listener.Addr().String()
	deadline := time.Now().Add(2 * time.Second)
	for {
		response, requestErr := http.DefaultClient.Do(getRequest(t, baseURL+"/readyz"))
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("background inventory never became ready")
		}
		time.Sleep(time.Millisecond)
	}
	for {
		response, requestErr := http.DefaultClient.Do(getRequest(t, baseURL+"/"))
		if requestErr == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && strings.Contains(string(body), "alpha") {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("completed inventory never reached the cache")
		}
		time.Sleep(time.Millisecond)
	}
	before := listCalls.Load()
	for _, path := range []string{"/", "/", "/wals", "/wals?class=gap"} {
		response, requestErr := http.DefaultClient.Do(getRequest(t, baseURL+path))
		if requestErr != nil {
			cancel()
			t.Fatal(requestErr)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			cancel()
			t.Fatal(readErr)
		}
		if !strings.Contains(string(body), "<dd>1</dd>") || (path == "/" && !strings.Contains(string(body), "alpha")) || (path != "/" && !strings.Contains(string(body), "Barman WAL evidence")) {
			cancel()
			t.Fatalf("cached inventory missing: %s", body)
		}
	}
	if after := listCalls.Load(); after != before {
		cancel()
		t.Fatalf("browser requests performed store calls: before=%d after=%d", before, after)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAgentRuntimePublishesScannerEvidenceWithoutRequestStoreCalls(t *testing.T) {
	directory := t.TempDir()
	_, certFile, keyFile := writeServingPair(t, directory)
	cfg := agentTestConfig(t, directory, certFile, keyFile, nil)
	var listCalls atomic.Int64
	var badListPrefix atomic.Bool
	var failList atomic.Bool
	factory := func(context.Context, config.Config, *redact.Redactor) (store.Reader, error) {
		return &storetest.Fake{ListFunc: func(_ context.Context, request store.ListRequest) (store.Page, error) {
			if request.Prefix != "orders/" {
				badListPrefix.Store(true)
			}
			listCalls.Add(1)
			if failList.Load() {
				return store.Page{}, context.DeadlineExceeded
			}
			return store.Page{}, nil
		}}, nil
	}
	app, err := newWithFactoryVersion(context.Background(), cfg, slog.New(slog.DiscardHandler), factory, "1.2.3", WithReviewer(&apiServerStub{}))
	if err != nil {
		t.Fatal(err)
	}
	if app.server != nil || app.agentHandler == nil {
		t.Fatalf("agent surfaces = server:%v evidence:%v", app.server != nil, app.agentHandler != nil)
	}
	scanner, ok := app.worker.(*inventory.Scanner)
	if !ok {
		t.Fatalf("agent worker = %T", app.worker)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		scanner.Run(ctx)
		done <- nil
	}()
	handler := app.agentHandler

	var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	deadline := time.Now().Add(2 * time.Second)
	for {
		response := agentHandlerRequest(handler, evidencev1alpha1.SnapshotPath)
		if response.Code == http.StatusOK && json.Unmarshal(response.Body.Bytes(), &snapshot) == nil && snapshot.Revision > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("scanner publication did not reach engine: %s", response.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	if snapshot.Producer.Version != "1.2.3" || snapshot.Identity.Cluster.Namespace != "database-team" || snapshot.Identity.Cluster.UID != cfg.CNPGClusterUID || snapshot.Identity.Repository.Scope.Name != "orders" {
		t.Fatalf("agent identity = %#v", snapshot)
	}
	if badListPrefix.Load() {
		t.Fatal("agent scanner listed outside the configured Barman server prefix")
	}
	before := listCalls.Load()
	for _, path := range []string{
		evidencev1alpha1.HealthPath, evidencev1alpha1.ReadinessPath, evidencev1alpha1.SnapshotPath,
		evidencev1alpha1.BackupsPath + "?revision=" + strconv.FormatUint(snapshot.Revision, 10),
	} {
		response := agentHandlerRequest(handler, path)
		if response.Code != http.StatusOK {
			cancel()
			t.Fatalf("%s status = %d: %s", path, response.Code, response.Body.String())
		}
	}
	if after := listCalls.Load(); after != before {
		cancel()
		t.Fatalf("evidence requests performed store calls: before=%d after=%d", before, after)
	}
	failList.Store(true)
	if err := scanner.Refresh(context.Background()); err == nil {
		cancel()
		t.Fatal("failed refresh error = nil")
	}
	afterFailure := listCalls.Load()
	response := agentHandlerRequest(handler, evidencev1alpha1.SnapshotPath)
	var retained evidencev1alpha1.RepositoryEvidenceSnapshot
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &retained) != nil {
		cancel()
		t.Fatalf("retained snapshot response = %d: %s", response.Code, response.Body.String())
	}
	if retained.Revision <= snapshot.Revision || retained.EvidenceGeneration != snapshot.EvidenceGeneration || !retained.Stale || retained.State != evidencev1alpha1.Unknown {
		cancel()
		t.Fatalf("retained failed-refresh evidence = %#v", retained)
	}
	if listCalls.Load() != afterFailure {
		cancel()
		t.Fatal("failed-refresh evidence request performed a store call")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConfinedReaderConfinesEveryOperationToConfiguredScope(t *testing.T) {
	var listCalls, openCalls, statCalls atomic.Int64
	reader, err := newPrefixConfinedReader(&storetest.Fake{
		ListFunc: func(_ context.Context, request store.ListRequest) (store.Page, error) {
			listCalls.Add(1)
			if request.Prefix != "orders/" {
				t.Fatalf("forwarded list prefix = %q", request.Prefix)
			}
			return store.Page{Objects: []store.Object{{Key: "orders/base/backup/data.tar"}}}, nil
		},
		OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
			openCalls.Add(1)
			return io.NopCloser(strings.NewReader(request.Key)), nil
		},
		StatFunc: func(_ context.Context, key string) (store.Object, error) {
			statCalls.Add(1)
			return store.Object{Key: key}, nil
		},
	}, "orders/")
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.List(context.Background(), store.ListRequest{Limit: 1})
	if err != nil || len(page.Objects) != 1 || page.Objects[0].Key != "orders/base/backup/data.tar" {
		t.Fatalf("confined List() = %#v, %v", page, err)
	}
	body, err := reader.Open(context.Background(), store.OpenRequest{Key: "orders/base/backup/backup.info", MaxBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	if _, err := reader.Stat(context.Background(), store.StatRequest{Key: "orders/base/backup/data.tar"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.List(context.Background(), store.ListRequest{Prefix: "archive/", Limit: 1}); !errors.Is(err, store.ErrInvalidRequest) {
		t.Fatalf("outside List() error = %v", err)
	}
	if _, err := reader.Open(context.Background(), store.OpenRequest{Key: "archive/base/backup.info", MaxBytes: 64}); !errors.Is(err, store.ErrInvalidRequest) {
		t.Fatalf("outside Open() error = %v", err)
	}
	if _, err := reader.Stat(context.Background(), store.StatRequest{Key: "archive/base/data.tar"}); !errors.Is(err, store.ErrInvalidRequest) {
		t.Fatalf("outside Stat() error = %v", err)
	}
	if listCalls.Load() != 1 || openCalls.Load() != 1 || statCalls.Load() != 1 {
		t.Fatalf("provider calls list=%d open=%d stat=%d", listCalls.Load(), openCalls.Load(), statCalls.Load())
	}

	malicious, err := newPrefixConfinedReader(&storetest.Fake{ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
		return store.Page{Objects: []store.Object{{Key: "archive/base/backup.info"}}}, nil
	}}, "orders/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := malicious.List(context.Background(), store.ListRequest{Limit: 1}); !errors.Is(err, store.ErrInvalidRequest) || fault.Categorize(err) != fault.SafetyLimit {
		t.Fatalf("out-of-prefix provider response error = %v", err)
	}
}

// agentHandlerRequest calls the agent's handler in-process with the
// console token apiServerStub accepts.
func agentHandlerRequest(handler http.Handler, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+agentConsoleToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// A repository-coordinate gap in the agent's configuration (no region, no
// usable destination) must not crash-loop the agent: the process starts,
// serves the evidence API, and publishes unknown evidence that names the
// invalid variable without any value.
func TestAgentServesUnknownEvidenceOnRepositoryConfigurationError(t *testing.T) {
	directory := t.TempDir()
	_, certFile, keyFile := writeServingPair(t, directory)
	accessPath := filepath.Join(directory, "access")
	validFingerprint, err := evidencev1alpha1.FingerprintS3(evidencev1alpha1.S3FingerprintInput{
		Bucket: "synthetic-bucket", Prefix: "cluster", Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders",
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name            string
		changes         map[string]string
		variable        string
		realFingerprint bool
	}{
		{name: "missing region", variable: "AWS_REGION", realFingerprint: true},
		{name: "unparsable destination", changes: map[string]string{"DESTINATION_PATH": "s3://synthetic-bucket/cluster?canary=secret", "AWS_REGION": "eu-west-1"}, variable: "DESTINATION_PATH"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			overrides := map[string]string{"AWS_REGION": ""}
			for key, value := range test.changes {
				overrides[key] = value
			}
			cfg := agentTestConfig(t, directory, certFile, keyFile, overrides)
			factoryCalled := false
			factory := func(context.Context, config.Config, *redact.Redactor) (store.Reader, error) {
				factoryCalled = true
				return nil, errors.New("must not build a store")
			}
			logs := &bytes.Buffer{}
			app, err := newWithFactoryVersion(context.Background(), cfg, slog.New(slog.NewJSONHandler(logs, nil)), factory, "1.2.3", WithReviewer(&apiServerStub{}))
			if err != nil || factoryCalled || app.agentHandler == nil || app.worker != nil {
				t.Fatalf("app = %v, factory called %v", err, factoryCalled)
			}
			response := agentHandlerRequest(app.agentHandler, evidencev1alpha1.SnapshotPath)
			var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || snapshot.Validate() != nil {
				t.Fatalf("snapshot = %d %s", response.Code, response.Body.String())
			}
			if snapshot.State != evidencev1alpha1.Unknown || !snapshot.Stale || snapshot.Inventory.LastFailureCategory == nil ||
				*snapshot.Inventory.LastFailureCategory != evidencev1alpha1.FailureInvalidConfig || !strings.Contains(snapshot.Reason.Message, test.variable) {
				t.Fatalf("snapshot evidence = %#v", snapshot)
			}
			if (snapshot.Identity.Repository.DestinationFingerprint == validFingerprint) != test.realFingerprint {
				t.Fatalf("fingerprint %s; real identity expected %v", snapshot.Identity.Repository.DestinationFingerprint, test.realFingerprint)
			}
			output := response.Body.String() + logs.String()
			if strings.Contains(output, "canary") || strings.Contains(output, accessPath) || !strings.Contains(logs.String(), "serving unknown evidence") {
				t.Fatalf("output disclosed a value or missed the startup error: %s", output)
			}
		})
	}
}

// The engine path of a rejected publication: a complete scan whose snapshot
// the evidence projection refuses (here a Barman backup ID longer than the
// wire contract allows) must leave the previous data in place, published as
// stale and unknown under a newer revision, never frozen as fresh.
func TestAgentRejectedPublicationServesPreviousDataStale(t *testing.T) {
	directory := t.TempDir()
	_, certFile, keyFile := writeServingPair(t, directory)
	cfg := agentTestConfig(t, directory, certFile, keyFile, map[string]string{"CNPG_CLUSTER_NAME": ""})
	now := time.Now().UTC()
	var poison atomic.Bool
	factory := func(context.Context, config.Config, *redact.Redactor) (store.Reader, error) {
		return &storetest.Fake{
			ListFunc: func(context.Context, store.ListRequest) (store.Page, error) {
				objects := []store.Object{{Key: "orders/wals/0000000100000000/000000010000000000000001", Size: 16 << 20, LastModified: now}}
				if poison.Load() {
					objects = append(objects, store.Object{Key: "orders/base/" + strings.Repeat("9", 300) + "/data.tar", Size: 1, LastModified: now})
				}
				return store.Page{Objects: objects}, nil
			},
			OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
				return nil, errors.New("not used")
			},
			StatFunc: func(_ context.Context, key string) (store.Object, error) { return store.Object{Key: key, Size: 1}, nil },
		}, nil
	}
	app, err := newWithFactoryVersion(context.Background(), cfg, slog.New(slog.DiscardHandler), factory, "1.2.3", WithReviewer(&apiServerStub{}))
	if err != nil {
		t.Fatal(err)
	}
	scanner, ok := app.worker.(*inventory.Scanner)
	if !ok {
		t.Fatalf("worker = %T", app.worker)
	}
	snapshot := func() evidencev1alpha1.RepositoryEvidenceSnapshot {
		t.Helper()
		response := agentHandlerRequest(app.agentHandler, evidencev1alpha1.SnapshotPath)
		var value evidencev1alpha1.RepositoryEvidenceSnapshot
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &value) != nil || value.Validate() != nil {
			t.Fatalf("snapshot = %d %s", response.Code, response.Body.String())
		}
		return value
	}
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	good := snapshot()
	if good.Stale || good.EvidenceGeneration == 0 || good.Inventory.ObjectCount == nil || *good.Inventory.ObjectCount != 1 {
		t.Fatalf("first publication = %#v", good)
	}

	poison.Store(true)
	if err := scanner.Refresh(context.Background()); err == nil {
		t.Fatal("poisoned refresh reported success")
	}
	stale := snapshot()
	if !stale.Stale || stale.State != evidencev1alpha1.Unknown || stale.Revision <= good.Revision ||
		stale.EvidenceGeneration != good.EvidenceGeneration || stale.Inventory.ObjectCount == nil || *stale.Inventory.ObjectCount != 1 ||
		stale.Inventory.LastFailureCategory == nil || *stale.Inventory.LastFailureCategory != evidencev1alpha1.FailureIncompatibleFormat ||
		stale.ProducerInstance != good.ProducerInstance {
		t.Fatalf("rejected publication served = revision %d->%d generation %d->%d stale=%v state=%s failure=%v",
			good.Revision, stale.Revision, good.EvidenceGeneration, stale.EvidenceGeneration, stale.Stale, stale.State, stale.Inventory.LastFailureCategory)
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
