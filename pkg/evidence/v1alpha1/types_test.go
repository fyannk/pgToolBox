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

package v1alpha1

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRepositoryEvidenceSnapshotJSONRoundTrip(t *testing.T) {
	snapshot := validSnapshot(t)
	first, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded RepositoryEvidenceSnapshot
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("round trip changed deterministic JSON\nfirst:  %s\nsecond: %s", first, second)
	}
	for _, requiredNull := range []string{`"last_attempt_at":null`, `"last_failure_category":null`} {
		if !bytes.Contains(first, []byte(requiredNull)) {
			t.Fatalf("required nullable field absent from %s", first)
		}
	}
}

func TestRepositoryEvidenceSnapshotIgnoresUnknownJSONFields(t *testing.T) {
	data, err := json.Marshal(validSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"future_field":{"ignored":true}}`)...)
	var decoded RepositoryEvidenceSnapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDetailsUnknownTagDiscardsPayload(t *testing.T) {
	canary := "raw-object-key-and-secret-canary"
	var details Details
	if err := json.Unmarshal([]byte(`{"type":"future/v1","future":{"raw":"`+canary+`"}}`), &details); err != nil {
		t.Fatal(err)
	}
	if !details.Unknown() {
		t.Fatal("unknown details tag was not retained as unavailable")
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), canary) || string(encoded) != `{"type":"future/v1"}` {
		t.Fatalf("unknown payload survived: %s", encoded)
	}
}

func TestRepositoryEvidenceSnapshotRejectsUnsafePositiveClaims(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RepositoryEvidenceSnapshot)
	}{
		{name: "incomplete healthy", mutate: func(value *RepositoryEvidenceSnapshot) { value.Completeness = Incomplete }},
		{name: "stale healthy", mutate: func(value *RepositoryEvidenceSnapshot) { value.Stale = true }},
		{name: "unsupported healthy capability", mutate: func(value *RepositoryEvidenceSnapshot) { value.Capabilities[0].Support = Unsupported }},
		{name: "unknown enum", mutate: func(value *RepositoryEvidenceSnapshot) { value.State = State("future") }},
		{name: "missing generation time", mutate: func(value *RepositoryEvidenceSnapshot) { value.CompletedAt = nil }},
		{name: "unsorted capabilities", mutate: func(value *RepositoryEvidenceSnapshot) {
			value.Capabilities[0], value.Capabilities[1] = value.Capabilities[1], value.Capabilities[0]
		}},
		{name: "credential-shaped identity", mutate: func(value *RepositoryEvidenceSnapshot) {
			value.Identity.Repository.DestinationFingerprint = "s3://user:secret@example.test"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validSnapshot(t)
			test.mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("invalid positive claim was accepted")
			}
		})
	}
}

func TestBarmanPagesRejectNullUnorderedAndOversizedItems(t *testing.T) {
	reason := Reason{Code: "structural-evidence", Message: "structural evidence evaluated"}
	items := []BarmanBackup{
		{Server: "orders", BackupID: "backup-b", State: Healthy, Reason: reason},
		{Server: "orders", BackupID: "backup-a", State: Unknown, Reason: reason},
	}
	page := BarmanBackupPage{
		PageHeader: PageHeader{APIVersion: APIVersion, Kind: BarmanBackupPageKind, Revision: 1, EvidenceGeneration: 1},
		Items:      items,
	}
	if err := page.Validate(); err == nil {
		t.Fatal("unordered backup page was accepted")
	}
	page.Items = nil
	if err := page.Validate(); err == nil {
		t.Fatal("null backup page items were accepted")
	}
	page.Items = make([]BarmanBackup, 201)
	if err := page.Validate(); err == nil {
		t.Fatal("oversized backup page was accepted")
	}
}

func TestEvidenceAPIErrorValidation(t *testing.T) {
	value := EvidenceAPIError{
		APIVersion: APIVersion,
		Kind:       ErrorKind,
		Code:       ErrorPublicationChanged,
		Message:    "repository evidence changed; restart from snapshot",
	}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	value.Code = ErrorCode("provider-secret-detail")
	if err := value.Validate(); err == nil {
		t.Fatal("unknown error code was accepted")
	}
}

func validSnapshot(t *testing.T) RepositoryEvidenceSnapshot {
	t.Helper()
	started := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	completed := started.Add(3 * time.Second)
	fingerprint, err := FingerprintS3(S3FingerprintInput{
		Bucket: "synthetic-bucket", Prefix: "cluster",
		Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := StateReason{State: Healthy, Reason: Reason{Code: "evidence-evaluated", Message: "evidence evaluated"}}
	zero := uint64(0)
	return RepositoryEvidenceSnapshot{
		APIVersion: APIVersion, Kind: SnapshotKind,
		Producer:         Producer{Name: ProducerName, Version: "1.0.0"},
		ProducerInstance: "0123456789abcdef0123456789abcdef", ProducerEpoch: 3,
		Identity: Identity{
			Cluster:    ClusterIdentity{Namespace: "database-team", UID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191"},
			Repository: RepositoryIdentity{Provider: "s3", Format: "barman-cloud", DestinationFingerprint: fingerprint, Scope: ScopeIdentity{Kind: "barman-server", Name: "orders"}},
		},
		Revision: 42, EvidenceGeneration: 42, StartedAt: &started, CompletedAt: &completed,
		Completeness: Complete, State: Healthy, Reason: result.Reason,
		Capabilities: func() []Capability {
			capabilities := make([]Capability, 0, len(capabilityIDs))
			for _, id := range capabilityIDs {
				capabilities = append(capabilities, Capability{ID: id, Support: Supported, State: Healthy, Reason: result.Reason})
			}
			return capabilities
		}(),
		Inventory: InventorySummary{Known: true, ObjectCount: &zero, StoredBytes: &zero, UnscopedObjectCount: &zero},
		Details: Details{Type: BarmanDetailsType, BarmanCloud: &BarmanCloudSummary{
			WAL: result, Timeline: result, Coverage: result,
			Retention: BarmanRetentionSummary{State: Healthy, Reason: result.Reason},
		}},
	}
}

func TestSnapshotProducerInstanceAndEpochAreRequired(t *testing.T) {
	snapshot := validSnapshot(t)
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"producer_instance":"0123456789abcdef0123456789abcdef"`, `"producer_epoch":3`} {
		if !bytes.Contains(encoded, []byte(field)) {
			t.Fatalf("snapshot encoding lacks %s: %s", field, encoded)
		}
	}
	for _, invalid := range []string{"", "short", "0123456789ABCDEF0123456789ABCDEF", strings.Repeat("a", 33)} {
		snapshot := validSnapshot(t)
		snapshot.ProducerInstance = invalid
		if snapshot.Validate() == nil {
			t.Fatalf("accepted producer instance %q", invalid)
		}
	}
	snapshot.ProducerEpoch = 0
	if snapshot.Validate() == nil {
		t.Fatal("accepted a zero producer epoch")
	}
}

func TestEvidenceAPIErrorCodesAreClosed(t *testing.T) {
	want := []ErrorCode{
		"invalid-request", "unauthenticated", "not-found", "method-not-allowed", "publication-changed",
		"response-limit", "busy", "invalid-publication", "forbidden", "authentication-unavailable",
		"audience-mismatch",
	}
	got := ErrorCodes()
	if len(got) != len(want) {
		t.Fatalf("error codes = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("error code %d = %q, want %q (codes are frozen API surface)", index, got[index], want[index])
		}
		value := EvidenceAPIError{APIVersion: APIVersion, Kind: ErrorKind, Code: got[index], Message: "bounded"}
		if err := value.Validate(); err != nil {
			t.Fatalf("code %q rejected: %v", got[index], err)
		}
	}
	got[0] = "mutated"
	if ErrorCodes()[0] != ErrorInvalidRequest {
		t.Fatal("ErrorCodes exposes the internal vocabulary")
	}
}
