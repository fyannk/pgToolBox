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

//go:build integration

package s3

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/provider/providertest"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/web"
)

func TestS3MinIOJourney(t *testing.T) {
	endpointValue := os.Getenv("EVIDENCE_AGENT_S3_INTEGRATION_ENDPOINT")
	accessKey := os.Getenv("EVIDENCE_AGENT_S3_INTEGRATION_ACCESS_KEY")
	secretKey := os.Getenv("EVIDENCE_AGENT_S3_INTEGRATION_SECRET_KEY")
	if endpointValue == "" || accessKey == "" || secretKey == "" {
		t.Skip("MinIO integration environment is not configured")
	}
	endpoint, err := url.Parse(endpointValue)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const bucket = "evidence-agent-proof"
	adapter, err := New(ctx, Options{
		Bucket: bucket, Prefix: "repository", Endpoint: endpoint, Region: "us-east-1",
		AccessKeyID: []byte(accessKey), SecretAccessKey: []byte(secretKey), RequestTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	normalized := providertest.BarmanCatalogJourney(t, adapter)
	providertest.WriteJourneyResult(t, normalized)
	format := barmancloud.New()
	cache, err := inventory.NewCache(inventory.Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	probe := readiness.New(true, time.Minute, time.Now)
	scanner, err := inventory.NewScanner(inventory.ScannerOptions{
		Store: adapter, Format: format, Cache: cache, Readiness: probe,
		RefreshInterval: time.Minute, MaxObjects: 1000, PageSize: 2, RecentLimit: 20,
		AnalyzeBarmanCatalog: true,
		Now:                  time.Now, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if category := scanner.Probe(ctx); category != fault.Unknown || !probe.Result().Ready {
		t.Fatalf("MinIO probe = %s, readiness = %#v", category, probe.Result())
	}
	if err := scanner.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Load()
	if !snapshot.TotalsKnown || snapshot.ObjectCount < 11 || snapshot.PagesExamined < 2 || len(snapshot.Scopes) != 1 || snapshot.Scopes[0].Name != "alpha" || !snapshot.Scopes[0].Recognized {
		t.Fatalf("MinIO inventory = %#v", snapshot)
	}
	foundGeneratedWAL := false
	for _, recent := range snapshot.RecentObjects {
		if strings.Contains(recent.Key, "/wals/") && strings.HasSuffix(recent.Key, "000000010000000000000001") {
			foundGeneratedWAL = true
		}
	}
	if !foundGeneratedWAL {
		t.Fatalf("Barman-generated WAL was not inventoried: %#v", snapshot.RecentObjects)
	}
	wantStates := map[string]evidence.State{
		"started": evidence.Warning, "failed": evidence.Unhealthy,
		"malformed": evidence.Unknown, "missing-artifact": evidence.Unhealthy,
		"missing-info": evidence.Unknown,
	}
	healthyID := ""
	for _, backup := range snapshot.BarmanCatalog.Backups {
		if backup.State == evidence.Healthy {
			healthyID = backup.ID
		}
		if want, exists := wantStates[backup.ID]; exists {
			if backup.State != want {
				t.Fatalf("backup %s state = %s, want %s: %#v", backup.ID, backup.State, want, backup)
			}
			delete(wantStates, backup.ID)
		}
	}
	if healthyID == "" || len(wantStates) != 0 {
		t.Fatalf("Barman catalog did not contain generated/mutated matrix: healthy=%q missing=%v catalog=%#v", healthyID, wantStates, snapshot.BarmanCatalog)
	}
	if snapshot.Evidence.State != evidence.Unknown || snapshot.Evidence.Completeness != evidence.Complete {
		t.Fatalf("mixed Barman catalog rollup = %#v", snapshot.Evidence)
	}
	infoKey := "alpha/base/" + healthyID + "/backup.info"
	object, err := adapter.Stat(ctx, store.StatRequest{Key: infoKey})
	if err != nil {
		t.Fatal(err)
	}
	if object.Key != infoKey || object.Size < 100 || strings.Contains(object.Key+object.ETag, "private-canary") {
		t.Fatalf("allowlisted Stat result = %#v", object)
	}
	reader, err := adapter.Open(ctx, store.OpenRequest{Key: infoKey, MaxBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || !bytes.Contains(content, []byte("backup_label=")) {
		t.Fatalf("bounded Open content = %q, read error = %v, close error = %v", content, err, closeErr)
	}
	handler, err := web.New(web.Options{
		Logger: slog.New(slog.DiscardHandler), Provider: "s3",
		Format: format.Descriptor(), Inventory: cache.Load, Readiness: probe.Result,
		RequestID: func() string { return "s3-proof" }, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.Routes().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody))
	if response.Code != http.StatusOK {
		t.Fatalf("render status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{healthyID, "started", "failed", "malformed", "missing-artifact", "missing-info", ">healthy<", ">warning<", ">unhealthy<", ">unknown<", "A structurally usable backup"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("rendered catalog does not contain %q: %s", expected, body)
		}
	}
	viewerConfig, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	viewerClient := awss3.NewFromConfig(viewerConfig, func(options *awss3.Options) {
		options.BaseEndpoint = aws.String(endpoint.String())
		options.UsePathStyle = true
	})
	_, err = viewerClient.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("repository/forbidden"), Body: strings.NewReader("forbidden"),
	})
	if fault.Categorize(safeError(ctx, "write-proof", err)) != fault.Authorization {
		t.Fatalf("MinIO proof credential did not deny writes: %v", err)
	}
}
