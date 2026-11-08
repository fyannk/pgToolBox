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
	"bytes"
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// testSharedToken is a shared token of the minimum length, and a canary
// for the no-token-in-logs assertion.
const testSharedToken = "shared-canary-0123456789abcdefghijklmnop"

// newPlainFakeAgent is the scripted agent behind plain HTTP.
func newPlainFakeAgent(t *testing.T) *fakeAgent {
	t.Helper()
	agent := &fakeAgent{t: t, contentType: evidencev1alpha1.MediaType, token: testSharedToken}
	server := httptest.NewUnstartedServer(http.HandlerFunc(agent.serve))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Start()
	t.Cleanup(server.Close)
	agent.server = server
	return agent
}

// writeFile writes content to a new file and returns its path.
func writeFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newStaticRunner(t *testing.T, agent *fakeAgent, logs io.Writer, agents ...StaticAgent) *Runner {
	t.Helper()
	options := Options{Namespaces: []string{testNamespace}, Static: agents, Now: func() time.Time { return testNow }}
	if logs != nil {
		options.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
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

func TestStaticRunnerReadsAPlainAgent(t *testing.T) {
	agent := newPlainFakeAgent(t)
	snapshot := fixtureSnapshot(t)
	// No producerEpoch is compared in the static mode: an agent restarted
	// with a new epoch is still live.
	snapshot.ProducerEpoch = 99
	agent.set(func(a *fakeAgent) {
		a.snapshot = snapshot
		a.pages = backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 10)
	})
	tokenFile := writeFile(t, "token", []byte(testSharedToken+"\n"))
	var logs bytes.Buffer
	runner := newStaticRunner(t, agent, &logs, StaticAgent{
		Namespace: testNamespace, Cluster: testCluster, URL: "http://" + testService + "." + testNamespace + ".svc:8443", TokenFile: tokenFile,
	})
	if result := runner.ForCluster(testNamespace, testCluster); result.State != viewmodel.EvidencePending || result.Agent != testCluster {
		t.Fatalf("before the first pass: %+v", result)
	}
	before := testutil.ToFloat64(fetches.WithLabelValues(outcomeSuccess))
	runner.refresh(context.Background())

	result := runner.ForCluster(testNamespace, testCluster)
	if result.State != viewmodel.EvidenceLive || result.Reason != "" || !result.BackupIDs["b1"] || !result.BackupsComplete || result.Snapshot == nil {
		t.Fatalf("result = %+v", result)
	}
	if agent.badAuth {
		t.Fatal("the agent did not receive the shared token")
	}
	if got := testutil.ToFloat64(fetches.WithLabelValues(outcomeSuccess)) - before; got != 1 {
		t.Fatalf("success fetches = %v", got)
	}
	if state := runner.State(); state.State != viewmodel.SourceLive {
		t.Fatalf("source = %+v", state)
	}
	if other := runner.ForCluster(testNamespace, "billing"); other.State != viewmodel.EvidenceNoAgent || other.Reason != reasonNoStaticAgent {
		t.Fatalf("unnamed Cluster = %+v", other)
	}

	// The token is re-read every pass: a rotated Secret the agent does not
	// hold yet is refused, with the static reason.
	if err := os.WriteFile(tokenFile, []byte("rotated-0123456789abcdefghijklmnopqrstu"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent.set(func(a *fakeAgent) {
		a.status, a.errorCode = http.StatusUnauthorized, evidencev1alpha1.ErrorUnauthenticated
	})
	runner.refresh(context.Background())
	result = runner.ForCluster(testNamespace, testCluster)
	if result.State != viewmodel.EvidenceStale || result.Reason != staticReasons[failureUnauthorized] {
		t.Fatalf("rotated token: %+v", result)
	}
	if strings.Contains(logs.String(), testSharedToken) || strings.Contains(logs.String(), "rotated-0123") {
		t.Fatal("a token reached the logs")
	}
}

func TestStaticRunnerVerifiesAnHTTPSAgentAgainstItsCAFile(t *testing.T) {
	serverName := evidencev1alpha1.ServerName(testService, testNamespace)
	ca := newTestCA(t, serverName)
	agent := newFakeAgent(t, ca)
	snapshot := fixtureSnapshot(t)
	agent.set(func(a *fakeAgent) {
		a.token = testSharedToken
		a.snapshot = snapshot
		a.pages = backupPages(snapshot, nil, 10)
	})
	caFile := writeFile(t, "ca.crt", ca.pem)
	static := StaticAgent{
		Namespace: testNamespace, Cluster: testCluster, URL: "https://" + serverName + ":8443",
		TokenFile: writeFile(t, "token", []byte(testSharedToken)), CAFile: caFile,
	}
	runner := newStaticRunner(t, agent, nil, static)
	runner.refresh(context.Background())
	if result := runner.ForCluster(testNamespace, testCluster); result.State != viewmodel.EvidenceLive {
		t.Fatalf("https agent: %+v", result)
	}

	// The CA file is re-read every pass: another CA fails verification.
	if err := os.WriteFile(caFile, newTestCA(t, serverName).pem, 0o600); err != nil {
		t.Fatal(err)
	}
	runner.refresh(context.Background())
	if result := runner.ForCluster(testNamespace, testCluster); result.State != viewmodel.EvidenceStale || result.Reason != staticReasons[failureTLS] {
		t.Fatalf("other CA: %+v", result)
	}

	if err := os.Remove(caFile); err != nil {
		t.Fatal(err)
	}
	runner.refresh(context.Background())
	if result := runner.ForCluster(testNamespace, testCluster); result.Reason != staticReasons[failureCAFile] {
		t.Fatalf("missing CA file: %+v", result)
	}
}

func TestStaticRunnerFailures(t *testing.T) {
	url := "http://" + testService + "." + testNamespace + ".svc:8443"
	t.Run("missing token", func(t *testing.T) {
		agent := newPlainFakeAgent(t)
		runner := newStaticRunner(t, agent, nil, StaticAgent{
			Namespace: testNamespace, Cluster: testCluster, URL: url, TokenFile: filepath.Join(t.TempDir(), "absent"),
		})
		runner.refresh(context.Background())
		result := runner.ForCluster(testNamespace, testCluster)
		if result.State != viewmodel.EvidenceUnavailable || result.Reason != staticReasons[failureTokenMissing] {
			t.Fatalf("result = %+v", result)
		}
		if state := runner.State(); state.State != viewmodel.SourceUnavailable || state.Reason != staticReasons[failureTokenMissing] {
			t.Fatalf("source = %+v", state)
		}
		if agent.snapshots != 0 {
			t.Fatal("the agent was called without a token")
		}
	})
	t.Run("short token", func(t *testing.T) {
		runner := newStaticRunner(t, newPlainFakeAgent(t), nil, StaticAgent{
			Namespace: testNamespace, Cluster: testCluster, URL: url, TokenFile: writeFile(t, "token", []byte("short")),
		})
		runner.refresh(context.Background())
		if result := runner.ForCluster(testNamespace, testCluster); result.Reason != staticReasons[failureTokenInvalid] {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("agent does not answer", func(t *testing.T) {
		agent := newPlainFakeAgent(t)
		agent.server.Close()
		runner := newStaticRunner(t, agent, nil, StaticAgent{
			Namespace: testNamespace, Cluster: testCluster, URL: url, TokenFile: writeFile(t, "token", []byte(testSharedToken)),
		})
		runner.refresh(context.Background())
		if result := runner.ForCluster(testNamespace, testCluster); result.State != viewmodel.EvidenceUnavailable || result.Reason != staticReasons[failureUnavailable] {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("agent without its token file", func(t *testing.T) {
		agent := newPlainFakeAgent(t)
		agent.set(func(a *fakeAgent) {
			a.status, a.errorCode = http.StatusServiceUnavailable, evidencev1alpha1.ErrorAuthenticationUnavailable
		})
		runner := newStaticRunner(t, agent, nil, StaticAgent{
			Namespace: testNamespace, Cluster: testCluster, URL: url, TokenFile: writeFile(t, "token", []byte(testSharedToken)),
		})
		runner.refresh(context.Background())
		if result := runner.ForCluster(testNamespace, testCluster); result.Reason != staticReasons[failureAuthUnavailable] {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("answer about another cluster", func(t *testing.T) {
		agent := newPlainFakeAgent(t)
		snapshot := fixtureSnapshot(t)
		snapshot.Identity.Cluster.Namespace = "team-b"
		agent.set(func(a *fakeAgent) { a.snapshot = snapshot })
		runner := newStaticRunner(t, agent, nil, StaticAgent{
			Namespace: testNamespace, Cluster: testCluster, URL: url, TokenFile: writeFile(t, "token", []byte(testSharedToken)),
		})
		runner.refresh(context.Background())
		if result := runner.ForCluster(testNamespace, testCluster); result.Reason != staticReasons[failureIdentity] {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestNewRejectsInvalidStaticAgents(t *testing.T) {
	valid := StaticAgent{Namespace: testNamespace, Cluster: testCluster, URL: "http://svc." + testNamespace + ".svc:8443", TokenFile: "/var/run/token"}
	tests := map[string]func(*StaticAgent){
		"unwatched namespace":  func(a *StaticAgent) { a.Namespace = "team-b"; a.URL = "http://svc.team-b.svc" },
		"foreign service":      func(a *StaticAgent) { a.URL = "http://svc.team-b.svc" },
		"path in url":          func(a *StaticAgent) { a.URL += "/evidence" },
		"other scheme":         func(a *StaticAgent) { a.URL = "ftp://svc." + testNamespace + ".svc" },
		"relative token file":  func(a *StaticAgent) { a.TokenFile = "token" },
		"http with a CA":       func(a *StaticAgent) { a.CAFile = "/ca.crt" },
		"https without a CA":   func(a *StaticAgent) { a.URL = "https://svc." + testNamespace + ".svc" },
		"invalid cluster name": func(a *StaticAgent) { a.Cluster = "Orders" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			agent := valid
			change(&agent)
			if _, err := New(Options{Namespaces: []string{testNamespace}, Static: []StaticAgent{agent}}); err == nil {
				t.Fatal("New accepted the agent")
			}
		})
	}
	if _, err := New(Options{Namespaces: []string{testNamespace}, Static: []StaticAgent{valid, valid}}); err == nil {
		t.Fatal("New accepted two agents for one Cluster")
	}
	https := valid
	https.URL, https.CAFile = "https://svc."+testNamespace+".svc", "/ca.crt"
	if _, err := New(Options{Namespaces: []string{testNamespace}, Static: []StaticAgent{valid}}); err != nil {
		t.Fatalf("http agent: %v", err)
	}
	if _, err := New(Options{Namespaces: []string{testNamespace}, Static: []StaticAgent{https}}); err != nil {
		t.Fatalf("https agent: %v", err)
	}
}

func TestParseStaticURL(t *testing.T) {
	for raw, want := range map[string]string{
		"http://svc.team-a.svc":       "http://svc.team-a.svc",
		"http://svc.team-a.svc:8443/": "http://svc.team-a.svc:8443",
		"https://svc.team-a.svc:8443": "https://svc.team-a.svc:8443",
	} {
		scheme, origin, serverName, err := parseStaticURL(raw, "team-a")
		if err != nil || origin != want || serverName != "svc.team-a.svc" || !strings.HasPrefix(raw, scheme+"://") {
			t.Errorf("%s: %q %q %q %v", raw, scheme, origin, serverName, err)
		}
	}
	for _, raw := range []string{"", "http://user@svc.team-a.svc", "http://svc.team-a.svc?x=1", "http://svc.team-a", "http://svc.team-a.svc.cluster.local", "http://svc.team-a.svc:0"} {
		if _, _, _, err := parseStaticURL(raw, "team-a"); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}
