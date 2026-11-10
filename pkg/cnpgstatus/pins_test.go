//go:build catalogpins

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

package cnpgstatus

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// operatorModule is the CloudNativePG operator module the tables were
// read from (the same corpus pkg/diagnose/catalog's pin test uses).
const operatorModule = "github.com/cloudnative-pg/cloudnative-pg"

// verifiedReleases maps each released minor to the tag its table was
// read at. 1.31 is planned (upstream main) and has no release to fetch.
var verifiedReleases = map[Minor]string{
	Minor130: "1.30.0",
}

// belowFloorRelease is the newest release below the support floor
// (1.29 is unsupported since 2026-11-06). It has no table; it is
// fetched only to prove that the 1.30 feature floors DetectVersion
// relies on are absent there, so a 1.29 operator is never read as 1.30.
const belowFloorRelease = "1.29.2"

// TestTablePinsExistInVerifiedReleases greps every literal the tables
// and readers rely on in the operator source of each verified release.
// A reworded phase, a renamed condition or annotation fails here
// instead of silently never matching. Run with -tags catalogpins (it
// needs the module cache or the network).
func TestTablePinsExistInVerifiedReleases(t *testing.T) {
	for minor, release := range verifiedReleases {
		corpus := operatorSources(t, release)
		var literals []string
		for _, row := range PhaseTable(minor) {
			literals = append(literals, `"`+row.Phase+`"`)
		}
		for _, rule := range ConditionTable(minor, false) {
			literals = append(literals, `"`+rule.Type+`"`)
			if rule.Reason != anyReason {
				literals = append(literals, `"`+rule.Reason+`"`)
			}
		}
		literals = append(literals,
			// annotations.go: keys are MetadataNamespace/AlphaMetadataNamespace + suffix.
			`"/fencedInstances"`, `"/hibernation"`, `"/reconciliationLoop"`, `"/validation"`,
			`"/podPatch"`, `"/reconcilePodSpec"`, `"/skipWalArchiving"`, `"/skipEmptyWalArchiveCheck"`,
			`"/unrecoverable"`, `"cnpg.io"`, `"alpha.cnpg.io"`, `FenceAllInstances = "*"`,
			`"disabled"`, `"enabled"`, `HibernationAnnotationValueOn HibernationAnnotationValue = "on"`,
			`HibernationAnnotationValueOff HibernationAnnotationValue = "off"`,
			"WrongAnnotationValue", "WaitingForHealthy", "DeletingPods", "WaitingPodsDeletion", "Hibernated",
			// statusport.go
			`PostgresContainerName = "postgres"`, `"--status-port-tls"`, `func GetStatusSchemeFromPod`,
			`ServiceReadWriteSuffix = "-rw"`, `ServiceReadSuffix = "-r"`, `ServiceReadOnlySuffix = "-ro"`,
			// primary.go
			`PendingFailoverMarker = "pending"`, `json:"currentPrimaryFailingSinceTimestamp,omitempty"`,
			`json:"targetPrimaryTimestamp,omitempty"`, `json:"currentPrimaryTimestamp,omitempty"`,
			// version.go
			`BootstrapControllerContainerName = "bootstrap-controller"`, `json:"instanceManagerVersion"`,
			`json:"cloudNativePGCommitHash,omitempty"`, "type FailoverQuorum struct",
			`OperatorVersionAnnotationName = MetadataNamespace + "/operatorVersion"`,
			"utils.SetOperatorVersion(&meta, versions.Version)", "cluster.GetFixedInheritedAnnotations(), configuration.Current)",
			// backups.go and blindspots.go
			"Deprecated: the field is not set for backup plugins",
			`json:"firstRecoverabilityPoint,omitempty"`, `json:"firstRecoverabilityPointByMethod,omitempty"`,
			`json:"lastSuccessfulBackup,omitempty"`, `json:"lastSuccessfulBackupByMethod,omitempty"`,
			`json:"lastFailedBackup,omitempty"`,
			`"first_recoverability_point"`, `"last_available_backup_timestamp"`, `"last_failed_backup_timestamp"`,
			`json:"isArchivingWAL,omitempty"`, `json:"lastFailedWAL,omitempty"`, `json:"readyWalFiles,omitempty"`,
			"AS is_archiving", "WHERE application_name ~ $1 AND usename = $2",
		)
		if minor.atLeast(Minor130) {
			literals = append(literals, `json:"operatorCertificateFingerprint,omitempty"`, "func (r *ClusterReconciler) reconcilePrimaryLease", "type DatabaseRoleSpec struct")
		}
		for _, literal := range literals {
			if !bytes.Contains(corpus, []byte(literal)) {
				t.Errorf("v%s: %q is not in the operator source", release, literal)
			}
		}
		t.Logf("v%s: %d literals verified against %d bytes", release, len(literals), len(corpus))
	}
}

// TestFeatureFloorsAreAbsentBelowTheFloor: the 1.30 feature floors
// must really be absent from the newest release below the support
// floor, or DetectVersion would read a 1.29 operator as a supported 1.30.
func TestFeatureFloorsAreAbsentBelowTheFloor(t *testing.T) {
	corpus := operatorSources(t, belowFloorRelease)
	for _, absent := range []string{
		`json:"operatorCertificateFingerprint,omitempty"`, "type DatabaseRoleSpec struct",
		"reconcilePrimaryLease", `ClusterConditionType = "Initialized"`, `"Invalid cluster definition"`,
	} {
		if bytes.Contains(corpus, []byte(absent)) {
			t.Errorf("v%s already contains %q: the 1.30 floor is wrong", belowFloorRelease, absent)
		}
	}
}

// TestEveryUpstreamPhaseIsInTheTable is the inverse: every phase
// constant of api/v1/cluster_types.go at a verified release has a row.
func TestEveryUpstreamPhaseIsInTheTable(t *testing.T) {
	constant := regexp.MustCompile(`(?m)^\s*Phase\w+\s*=\s*"([^"]+)"`)
	conditionType := regexp.MustCompile(`(?m)^\s*Condition\w+\s+ClusterConditionType\s*=\s*"([^"]+)"`)
	for minor, release := range verifiedReleases {
		types, err := os.ReadFile(filepath.Join(moduleDir(t, release), "api", "v1", "cluster_types.go"))
		if err != nil {
			t.Fatal(err)
		}
		known := map[string]bool{}
		for _, row := range PhaseTable(minor) {
			known[row.Phase] = true
		}
		for _, m := range constant.FindAllSubmatch(types, -1) {
			if !known[string(m[1])] {
				t.Errorf("v%s phase %q has no row in the %s table", release, m[1], minor)
			}
		}
		knownTypes := map[string]bool{}
		for _, rule := range ConditionTable(minor, false) {
			knownTypes[rule.Type] = true
		}
		for _, m := range conditionType.FindAllSubmatch(types, -1) {
			if !knownTypes[string(m[1])] {
				t.Errorf("v%s condition %q has no row in the %s table", release, m[1], minor)
			}
		}
	}
}

func moduleDir(t *testing.T, release string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "mod", "download", "-json", operatorModule+"@v"+release)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "GOFLAGS=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("download %s@v%s: %v\n%s", operatorModule, release, err, out)
	}
	var info struct{ Dir, Error string }
	if err := json.Unmarshal(out, &info); err != nil {
		t.Fatalf("parse download result: %v\n%s", err, out)
	}
	if info.Error != "" || info.Dir == "" {
		t.Fatalf("download %s@v%s: %s", operatorModule, release, info.Error)
	}
	return info.Dir
}

func operatorSources(t *testing.T, release string) []byte {
	t.Helper()
	var corpus bytes.Buffer
	err := filepath.WalkDir(moduleDir(t, release), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		corpus.Write(content)
		corpus.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("read v%s sources: %v", release, err)
	}
	return corpus.Bytes()
}
