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

package sharedstate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
)

func hash16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// fillToTheBounds fills every document to its bound: every grant, every
// code, the sessions document full of replacements, a year of daily
// digests on several channels, many "since" times, and the drill results
// drillsAt makes. worst makes each grant its costliest (client name,
// scope, revocation); otherwise it is what a real client declares.
func fillToTheBounds(s *Store, worst bool, drillsAt func(i int) DrillResult) {
	name, scope, revoked := "Claude Code", "read", int64(0)
	if worst {
		name, scope, revoked = TruncateClientName(strings.Repeat("<& \"\\\x01é", 100)), strings.Repeat("s", maxScope), t0.Unix()
	}
	for i := range MaxGrants {
		s.grants[grantID(i)] = grantEntry{
			User: uh(fmt.Sprint("user", i)), Client: name, Scope: scope,
			Created: t0.Unix(), Used: t0.Unix(), Expiry: t0.Add(48 * time.Hour).Unix(), Gen: 999999, Revoked: revoked,
		}
	}
	for i := range maxCodes {
		s.codes[hash16(strconv.Itoa(i))] = codeEntry{Expiry: t0.Add(48 * time.Hour).Unix(), User: uh("x")}
	}
	for i := range MaxSignOuts {
		s.signedOut[hash16(fmt.Sprint("sid", i))] = signOut{
			Expiry: t0.Add(48 * time.Hour).Unix(), Recorded: t0.Unix() + int64(i), Subject: tag(fmt.Sprint("p", i/MaxSignOutsPerSubject)),
			Slot: hash16(fmt.Sprint("slot", i)), Gen: maxSignOutGen,
		}
	}
	for d := range 366 {
		for _, channel := range []string{"email", "webhook", "slack", "teams"} {
			s.sent[sentKey(t0.AddDate(0, 0, -d).Format("20060102T1504Z"), channel)] = t0.Unix()
		}
	}
	for i := range 20000 {
		s.setFactLocked(factKey(kindFinding, fmt.Sprintf("%036d", i), "finding"), fact{State: hashState(findingOn), Since: t0.UnixNano() + int64(i)})
	}
	for i := range maxDrills {
		r := drillsAt(i)
		s.drills[drillKey(r.Namespace, r.Name)] = r
	}
}

func drillAtTheBounds(i int) DrillResult {
	ns := fmt.Sprintf("%063d", i)
	return DrillResult{
		Namespace: ns, Name: strings.Repeat("n", maxDrillName-4) + fmt.Sprintf("%04d", i), SourceNamespace: ns,
		Source: strings.Repeat("c", maxDrillName), SourceUID: strings.Repeat("u", maxDrillUID),
		Backup: strings.Repeat("b", maxDrillName), BackupLabel: strings.Repeat("l", maxDrillLabel),
		Result: "failed", Reason: strings.Repeat("\x01", maxDrillReason),
		StartedAt: t0.Unix(), Deadline: t0.Unix(), HealthyAt: t0.Unix(), FinishedAt: t0.Unix() + int64(i),
	}
}

// Every document at its bound used to add up to some 1.7 MB (round 9):
// the API server then refuses every write, the store fails, and MCP
// grants are refused. The ConfigMap's data now stays within MaxData: the
// oldest drill results go (counted, logged once), never a sign-out, a
// grant, a code or a delivery mark, and the write is accepted.
func TestEveryDocumentAtItsBoundFitsTheConfigMap(t *testing.T) {
	api := newAPIServer()
	api.maxBytes = 1 << 20
	var logs bytes.Buffer
	clock := &testClock{now: t0}
	s := newStore(api, clock, slog.New(slog.NewJSONHandler(&logs, nil)))
	s.mu.Lock()
	s.mergeLocked(api.cm.DeepCopy())
	s.setModeLocked(modeLive, "")
	fillToTheBounds(s, true, drillAtTheBounds)
	sessions, grants, codes, sent := len(s.signedOut), len(s.grants), len(s.codes), len(s.sent)
	before := testutil.ToFloat64(sharedDropped.WithLabelValues(KeyDrills))
	s.changedLocked(false)
	s.mu.Unlock()
	if err := s.flush(t.Context()); err != nil {
		t.Fatalf("the ConfigMap at every bound was refused: %v", err)
	}
	size := 0
	for k, v := range api.cm.Data {
		size += len(k) + len(v)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Logf("ConfigMap data %d bytes; %d drill results of %d kept", size, len(s.drills), maxDrills)
	if size > MaxData {
		t.Errorf("the ConfigMap's data is %d bytes, past %d", size, MaxData)
	}
	if len(s.signedOut) != sessions || len(s.grants) != grants || len(s.codes) != codes || len(s.sent) != sent {
		t.Error("a sign-out, a grant, a code or a delivery mark was dropped for the size")
	}
	dropped := maxDrills - len(s.drills)
	if dropped == 0 || len(s.drills) == 0 {
		t.Fatalf("%d drill results dropped of %d", dropped, maxDrills)
	}
	if got := testutil.ToFloat64(sharedDropped.WithLabelValues(KeyDrills)) - before; got != float64(dropped) {
		t.Errorf("drill drops counted: %v, want %d", got, dropped)
	}
	// The oldest go first.
	for i := range dropped {
		r := drillAtTheBounds(i)
		if _, ok := s.drills[drillKey(r.Namespace, r.Name)]; ok {
			t.Fatalf("drill %d kept while a newer one was dropped", i)
		}
	}
	if n := strings.Count(logs.String(), "oldest restore drill results were dropped"); n != 1 {
		t.Errorf("the drop was logged %d times, want once", n)
	}
	if got := testutil.ToFloat64(sharedBytes.WithLabelValues("total")); int(got) != size {
		t.Errorf("total bytes gauge %v, want %d", got, size)
	}
}

// Realistic drill results at their count bound, beside every other
// document at its bound with what real clients declare, fit without
// dropping any.
func TestRealisticDrillsAtTheirBoundAreKept(t *testing.T) {
	s := New(Options{Name: "console-state", Now: (&testClock{now: t0}).Now})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeLocked(&corev1.ConfigMap{})
	fillToTheBounds(s, false, realisticDrill)
	s.pruneLocked()
	_, data := s.sizesLocked()
	t.Logf("ConfigMap data %d bytes", data)
	if len(s.drills) != maxDrills || data > MaxData {
		t.Errorf("%d drill results kept, data %d bytes", len(s.drills), data)
	}
}

// realisticDrill is a drill result as a real console writes it.
func realisticDrill(i int) DrillResult {
	return DrillResult{
		Namespace: "team-payments-prod", Name: fmt.Sprintf("drill-pg-main-%d", i), SourceNamespace: "team-payments-prod",
		Source: fmt.Sprint("pg-main-", i%10), SourceUID: "0f0e0d0c-0b0a-0908-0706-050403020100", Backup: "pg-main-20261111010203",
		BackupLabel: "20261111-010203F_20261111-020304I", Result: "failed", Reason: strings.Repeat("r", 120),
		StartedAt: t0.Unix(), Deadline: t0.Unix(), HealthyAt: t0.Unix(), FinishedAt: t0.Unix() + int64(i),
	}
}

// The review's measure (round 10): with every other document at its
// bound and realistic grants and drills, the documents took 965,088
// bytes against the 786,432 of the budget, and every "since" time was
// dropped while some 67 KB were left under MaxData. The since times now
// take the room the ConfigMap leaves, the newest kept, and give way to
// nothing else.
func TestSinceTimesKeepTheRoomLeftWhenEverythingElseIsFull(t *testing.T) {
	s := New(Options{Name: "console-state", Now: (&testClock{now: t0}).Now})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeLocked(&corev1.ConfigMap{})
	fillToTheBounds(s, false, realisticDrill)
	sessions, grants, codes, sent := len(s.signedOut), len(s.grants), len(s.codes), len(s.sent)
	docs := s.pruneLocked()
	_, data := s.measureLocked(docs)
	t.Logf("ConfigMap data %d bytes, %d since times kept of 20000, since document %d bytes", data, len(s.facts), len(docs[KeySince]))
	if data > MaxData {
		t.Fatalf("data %d bytes, past %d", data, MaxData)
	}
	if len(s.drills) != maxDrills || len(s.signedOut) != sessions || len(s.grants) != grants || len(s.codes) != codes || len(s.sent) != sent {
		t.Fatal("a drill result, a sign-out, a grant, a code or a delivery mark gave way to since times")
	}
	if len(s.facts) == 0 {
		t.Fatalf("every since time was dropped with %d bytes left under MaxData", MaxData-data)
	}
	if room := MaxData - data; room > 256 {
		t.Errorf("%d bytes left under MaxData while since times were dropped", room)
	}
	// The newest are kept.
	if _, newest := s.facts[factKey(kindFinding, fmt.Sprintf("%036d", 19999), "finding")]; !newest {
		t.Error("the newest since time was dropped")
	}
	if _, oldest := s.facts[factKey(kindFinding, fmt.Sprintf("%036d", 0), "finding")]; oldest {
		t.Error("the oldest since time was kept")
	}
	// What is kept is what is written.
	if got := docs[KeySince]; got != s.encodeSinceLocked() {
		t.Error("the since document returned is not the state's")
	}
}

// A field longer than Kubernetes allows is not one this console wrote.
func TestDrillFieldsAtKubernetesBounds(t *testing.T) {
	ok := passed("drill", t0)
	for _, tc := range []struct {
		name string
		edit func(*DrillResult)
		want bool
	}{
		{"as written", func(*DrillResult) {}, true},
		{"namespace at 63", func(r *DrillResult) { r.Namespace = strings.Repeat("a", 63) }, true},
		{"namespace at 64", func(r *DrillResult) { r.Namespace = strings.Repeat("a", 64) }, false},
		{"source namespace at 64", func(r *DrillResult) { r.SourceNamespace = strings.Repeat("a", 64) }, false},
		{"name at 253", func(r *DrillResult) { r.Name = strings.Repeat("a", 253) }, true},
		{"UID past a UUID", func(r *DrillResult) { r.SourceUID = strings.Repeat("u", 37) }, false},
		{"label past its bound", func(r *DrillResult) { r.BackupLabel = strings.Repeat("l", maxDrillLabel+1) }, false},
	} {
		r := ok
		tc.edit(&r)
		if got := validDrill(drillKey(r.Namespace, r.Name), r); got != tc.want {
			t.Errorf("%s: valid = %v, want %v", tc.name, got, tc.want)
		}
	}
}
