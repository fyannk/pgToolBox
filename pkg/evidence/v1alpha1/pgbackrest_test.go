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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readWire[T any](t *testing.T, name string) T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "wire", name))
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestPGBackRestSnapshotValidation(t *testing.T) {
	base := readWire[RepositoryEvidenceSnapshot](t, "pgbackrest-snapshot.json")
	if err := base.Validate(); err != nil {
		t.Fatalf("golden: %v", err)
	}
	if base.Details.PGBackRest == nil || base.Details.Unknown() {
		t.Fatal("the pgBackRest payload was not decoded")
	}
	clone := func() RepositoryEvidenceSnapshot {
		value := readWire[RepositoryEvidenceSnapshot](t, "pgbackrest-snapshot.json")
		return value
	}
	tests := map[string]func(*RepositoryEvidenceSnapshot){
		"Barman scope kind":           func(s *RepositoryEvidenceSnapshot) { s.Identity.Repository.Scope.Kind = ScopeKindBarmanServer },
		"stanza kind under Barman":    func(s *RepositoryEvidenceSnapshot) { s.Identity.Repository.Format = FormatBarmanCloud },
		"unknown format":              func(s *RepositoryEvidenceSnapshot) { s.Identity.Repository.Format = "wal-g" },
		"stanza with a slash":         func(s *RepositoryEvidenceSnapshot) { s.Identity.Repository.Scope.Name = "a/b" },
		"details of another stanza":   func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.Stanza = "other" },
		"Barman tag on pgBackRest":    func(s *RepositoryEvidenceSnapshot) { s.Details.Type = BarmanDetailsType },
		"missing payload":             func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest = nil },
		"both payloads":               func(s *RepositoryEvidenceSnapshot) { s.Details.BarmanCloud = &BarmanCloudSummary{} },
		"repository zero":             func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.Repository = 0 },
		"repository 257":              func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.Repository = 257 },
		"nil archive ids":             func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.ArchiveIDs = nil },
		"bad archive id":              func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.ArchiveIDs = []string{"17-0"} },
		"duplicate archive id":        func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.ArchiveIDs = []string{"17-1", "17-1"} },
		"segment size without source": func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.WALSegmentSizeSource = nil },
		"odd segment size":            func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.WALSegmentSizeBytes = new(uint64(3 << 20)) },
		"unknown size source":         func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.WALSegmentSizeSource = new("guessed") },
		"state counts do not add up":  func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.BackupStates.Unknown++ },
		"more usable than visible":    func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.UsableBackups = new(uint64(3)) },
		"truncated WAL not unknown":   func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.RangesTruncated = true },
		"nil findings":                func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.Findings = nil },
		"unsorted findings": func(s *RepositoryEvidenceSnapshot) {
			findings := s.Details.PGBackRest.Findings
			findings[0], findings[1] = findings[1], findings[0]
		},
		"lower-case finding code": func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.Findings[0].Code = "checksumMismatch" },
		"unknown severity":        func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.Findings[0].Severity = "info" },
		"control in subject":      func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.Findings[0].Subject = "a\nb" },
		"window ends before start": func(s *RepositoryEvidenceSnapshot) {
			s.Details.PGBackRest.RecoveryWindow.NewestStopAt = s.Details.PGBackRest.RecoveryWindow.StartAt.Add(-time.Second)
		},
		"window with a bad label": func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.RecoveryWindow.OldestBackup = "latest" },
		"window counts too many":  func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.RecoveryWindow.UsableBackups = 2 },
		"window time not in UTC": func(s *RepositoryEvidenceSnapshot) {
			s.Details.PGBackRest.RecoveryWindow.StartAt = time.Now().In(time.FixedZone("x", 3600))
		},
		"bad database version":     func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.Database.Version = "17beta" },
		"database system id":       func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.Database.SystemID = "-1" },
		"repository format 0":      func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.RepositoryFormat = new(uint64(0)) },
		"incomplete with counts":   func(s *RepositoryEvidenceSnapshot) { s.Completeness, s.State = Incomplete, Unknown },
		"unbounded catalog reason": func(s *RepositoryEvidenceSnapshot) { s.Details.PGBackRest.Catalog.Reason.Code = "Catalog" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := clone()
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("invalid pgBackRest snapshot accepted")
			}
		})
	}
}

func TestPGBackRestDetailsRoundTripAndUnknownTag(t *testing.T) {
	base := readWire[RepositoryEvidenceSnapshot](t, "pgbackrest-snapshot.json")
	encoded, err := json.Marshal(base.Details)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"pgbackrest":{`) || strings.Contains(string(encoded), "barman_cloud") {
		t.Fatalf("pgBackRest details encoding = %s", encoded)
	}
	var future Details
	if err := json.Unmarshal([]byte(`{"type":"pgbackrest/v1alpha9","pgbackrest":{"stanza":"x"}}`), &future); err != nil {
		t.Fatal(err)
	}
	if !future.Unknown() || future.PGBackRest != nil || future.validate() != nil {
		t.Fatalf("a future pgBackRest tag kept its payload: %#v", future)
	}
	// A Barman identity carrying a pgBackRest payload is refused.
	barman := readWire[RepositoryEvidenceSnapshot](t, "snapshot.json")
	barman.Details = base.Details
	if err := barman.Validate(); err == nil {
		t.Fatal("pgBackRest details under a Barman identity accepted")
	}
}

func TestPGBackRestPagesValidation(t *testing.T) {
	backups := func() PGBackRestBackupPage { return readWire[PGBackRestBackupPage](t, "pgbackrest-backups.json") }
	ranges := func() PGBackRestWALRangePage {
		return readWire[PGBackRestWALRangePage](t, "pgbackrest-wal-ranges.json")
	}
	gaps := func() PGBackRestWALGapPage { return readWire[PGBackRestWALGapPage](t, "pgbackrest-wal-gaps.json") }
	paths := func() PGBackRestRecoveryPathPage {
		return readWire[PGBackRestRecoveryPathPage](t, "pgbackrest-recovery-paths.json")
	}
	tests := map[string]func() error{
		"Barman page kind": func() error { page := backups(); page.Kind = BarmanBackupPageKind; return page.Validate() },
		"unsorted backups": func() error {
			page := backups()
			page.Items[0], page.Items[1] = page.Items[1], page.Items[0]
			return page.Validate()
		},
		"label of no type":          func() error { page := backups(); page.Items[0].Label = "20261107-081054"; return page.Validate() },
		"type disagrees with label": func() error { page := backups(); page.Items[0].Type = PGBackRestDifferential; return page.Validate() },
		"full with a prior":         func() error { page := backups(); page.Items[0].Prior = new("20261106-081054F"); return page.Validate() },
		"incremental without prior": func() error { page := backups(); page.Items[1].Prior = nil; return page.Validate() },
		"prior of another full": func() error {
			page := backups()
			page.Items[1].Prior = new("20261106-081054F")
			return page.Validate()
		},
		"usable with reasons": func() error {
			page := backups()
			page.Items[0].UnusableReasons = []string{UnusableBackupError}
			return page.Validate()
		},
		"unusable without reasons": func() error { page := backups(); page.Items[1].UnusableReasons = []string{}; return page.Validate() },
		"usable but unhealthy":     func() error { page := backups(); page.Items[0].State = Warning; return page.Validate() },
		"nil references":           func() error { page := backups(); page.Items[0].References = nil; return page.Validate() },
		"nil annotations":          func() error { page := backups(); page.Items[0].Annotations = nil; return page.Validate() },
		"unsorted annotations": func() error {
			page := backups()
			page.Items[0].Annotations = []PGBackRestAnnotation{{Key: "z", Value: "1"}, {Key: "a", Value: "2"}}
			return page.Validate()
		},
		"stop before start": func() error {
			page := backups()
			stop := page.Items[0].StartAt.Add(-time.Second)
			page.Items[0].StopAt = &stop
			return page.Validate()
		},
		"lower-case LSN":       func() error { page := backups(); page.Items[0].StopLSN = new("0/4000a00"); return page.Validate() },
		"range count":          func() error { page := ranges(); page.Items[0].SegmentCount++; return page.Validate() },
		"range archive id":     func() error { page := ranges(); page.Items[0].ArchiveID = "x"; return page.Validate() },
		"candidate seen twice": func() error { page := gaps(); page.Items[0].LastObservedGeneration++; return page.Validate() },
		"gap of a Barman kind": func() error { page := gaps(); page.Kind = BarmanWALGapPageKind; return page.Validate() },
		"healthy path off target": func() error {
			page := paths()
			page.Items[0].State, page.Items[0].Stop, page.Items[0].OffTarget = Healthy, CoverageFrontier, true
			return page.Validate()
		},
		"timeline zero in path": func() error { page := paths(); page.Items[0].Timelines = []uint64{0}; return page.Validate() },
		"half a frontier":       func() error { page := paths(); page.Items[0].FrontierWAL = nil; return page.Validate() },
		"unsorted assumptions": func() error {
			page := paths()
			page.Items[0].AssumptionCodes = []string{"b", "a"}
			return page.Validate()
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			if err := run(); err == nil {
				t.Fatal("invalid pgBackRest page accepted")
			}
		})
	}
}

func TestPGBackRestLabelGrammar(t *testing.T) {
	for _, test := range []struct {
		label, kind, full string
	}{
		{"20261107-081054F", PGBackRestFull, "20261107-081054F"},
		{"20261107-081054F_20261107-081103D", PGBackRestDifferential, "20261107-081054F"},
		{"20261107-081054F_20261107-081108I", PGBackRestIncremental, "20261107-081054F"},
		{"20261107-081054D", "", ""},
		{"20261107-081054F_20261107-081108F", "", ""},
		{"2026110-0810540F", "", ""},
		{"", "", ""},
	} {
		kind, full, ok := pgBackRestLabelType(test.label)
		if kind != test.kind || full != test.full || ok != (test.kind != "") {
			t.Fatalf("pgBackRestLabelType(%q) = %q, %q, %v", test.label, kind, full, ok)
		}
	}
	for value, want := range map[string]bool{"17-1": true, "9.6-12": true, "17-0": false, "017-1": false, "17": false, "9.10-1": false, "17-1a": false} {
		if validArchiveID(value) != want {
			t.Fatalf("validArchiveID(%q) != %v", value, want)
		}
	}
}

// TestPGBackRestFingerprints: the fingerprint accepts the stanza profile on
// every provider, separates it from a Barman server of the same name, and
// refuses a crossed profile.
func TestPGBackRestFingerprints(t *testing.T) {
	stanza := func(provider string) RepositoryFingerprintInput {
		input := RepositoryFingerprintInput{Provider: provider, Bucket: "backups", Prefix: "/pgbr/orders/", Format: FormatPGBackRest, ScopeKind: ScopeKindStanza, ScopeName: "orders"}
		if provider == ProviderAzure {
			input.Account = "account"
		}
		return input
	}
	for _, provider := range []string{ProviderS3, ProviderAzure, ProviderGCS} {
		pgbackrest, err := FingerprintRepository(stanza(provider))
		if err != nil || !validFingerprint(pgbackrest) {
			t.Fatalf("%s pgBackRest fingerprint = %q, %v", provider, pgbackrest, err)
		}
		barman := stanza(provider)
		barman.Format, barman.ScopeKind = FormatBarmanCloud, ScopeKindBarmanServer
		other, err := FingerprintRepository(barman)
		if err != nil || other == pgbackrest {
			t.Fatalf("%s Barman and pgBackRest fingerprints collide: %q, %v", provider, other, err)
		}
		crossed := stanza(provider)
		crossed.ScopeKind = ScopeKindBarmanServer
		if _, err := FingerprintRepository(crossed); err == nil {
			t.Fatalf("%s crossed profile accepted", provider)
		}
		canonical, err := CanonicalRepositoryFingerprintInput(stanza(provider))
		if err != nil || canonical.Prefix != "pgbr/orders" {
			t.Fatalf("%s canonical prefix = %q, %v", provider, canonical.Prefix, err)
		}
	}
	long := stanza(ProviderS3)
	long.ScopeName = strings.Repeat("s", 129)
	if _, err := FingerprintRepository(long); err == nil {
		t.Fatal("a stanza name over 128 bytes accepted")
	}
	s3, err := FingerprintS3(S3FingerprintInput{Bucket: "backups", Prefix: "pgbr/orders", Format: FormatPGBackRest, ScopeKind: ScopeKindStanza, ScopeName: "orders"})
	if want, _ := FingerprintRepository(stanza(ProviderS3)); err != nil || s3 != want {
		t.Fatalf("FingerprintS3 = %q, %v; FingerprintRepository = %q", s3, err, want)
	}
}

// TestValidPGBackRestStanza is the one stanza rule the operator, the agent
// and this contract share (the operator accepted 256
// bytes, the agent 128).
func TestValidPGBackRestStanza(t *testing.T) {
	for value, want := range map[string]bool{
		"main":       true,
		"orders-wal": true,
		strings.Repeat("s", MaxPGBackRestStanzaBytes):   true,
		strings.Repeat("é", MaxPGBackRestStanzaBytes/2): true,
		strings.Repeat("s", MaxPGBackRestStanzaBytes+1): false,
		"":       false,
		".":      false,
		"..":     false,
		"a/b":    false,
		`a\b`:    false,
		" main":  false,
		"main ":  false,
		"ma\nin": false,
		"\xff":   false,
	} {
		if got := ValidPGBackRestStanza(value); got != want {
			t.Errorf("ValidPGBackRestStanza(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestValidatePGBackRestRetention(t *testing.T) {
	for _, tt := range []struct {
		full, diff, archive   int
		fullType, archiveType string
		option                string
	}{
		{0, 0, 0, "", "", ""},
		{MaxPGBackRestRetention, 1, 1, RetentionFullTime, PGBackRestIncremental, ""},
		{-1, 0, 0, "", "", "full"},
		{0, MaxPGBackRestRetention + 1, 0, "", "", "diff"},
		{0, 0, -1, "", "", "archive"},
		{1, 0, 0, "days", "", "full-type"},
		{1, 0, 0, RetentionFullCount, "log", "archive-type"},
	} {
		err := ValidatePGBackRestRetention(tt.full, tt.fullType, tt.diff, tt.archive, tt.archiveType)
		var refused *PGBackRestRetentionError
		errors.As(err, &refused)
		switch {
		case tt.option == "" && err != nil:
			t.Errorf("%+v refused: %v", tt, err)
		case tt.option != "" && (refused == nil || refused.Option != tt.option):
			t.Errorf("%+v: %v, want option %s refused", tt, err, tt.option)
		case tt.option == "full" && !strings.Contains(err.Error(), "0 (unset) to 9999999"):
			t.Errorf("message %q does not say the bound", err)
		}
	}
}
