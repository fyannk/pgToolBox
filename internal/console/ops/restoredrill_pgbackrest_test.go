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

package ops

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"sigs.k8s.io/yaml"
)

const pgbrTarget = "20261103-020004F_20261103-120001I"

// pgbackrestCluster is pluginSource backing up with a pgBackRest plugin.
func pgbackrestCluster(plugin string, params map[string]string) *cnpgv1.Cluster {
	c := pluginSource()
	c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: plugin, IsWALArchiver: new(true), Parameters: params}}
	return c
}

func daliboSource() *cnpgv1.Cluster {
	return pgbackrestCluster(PgBackRestDaliboPlugin, map[string]string{
		"stanzaRef": "orders-stanza", "pluginConfigRef": "orders-pluginconfig", "replicaStanzaRef": "orders-replica",
	})
}

func operaSource() *cnpgv1.Cluster {
	return pgbackrestCluster(PgBackRestOperaPlugin, map[string]string{"pgbackrestObjectName": "orders-archive", "stanza": "orders-v2"})
}

func pgbackrestDrillRequest(t *testing.T, src *cnpgv1.Cluster) DrillRequest {
	t.Helper()
	r := drillRequest(t, src)
	r.Recovery.PgBackRest.Target = pgbrTarget
	r.Meta.BackupLabel = pgbrTarget
	return r
}

func TestPgBackRestDrillClusterGolden(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]*cnpgv1.Cluster{"pgbackrest-dalibo": daliboSource(), "pgbackrest-opera": operaSource()} {
		c, err := DrillCluster(pgbackrestDrillRequest(t, src), [][]byte{drillKey})
		if err != nil {
			t.Fatal(name, err)
		}
		got, err := yaml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "restoredrill-"+name+".golden.yaml")
		if *updateGolden {
			if err := os.WriteFile(path, got, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s drill differs from %s (go test -update):\n%s", name, path, got)
		}
	}
}

func TestPgBackRestDrillRecoverySource(t *testing.T) {
	t.Parallel()
	rs, err := DrillRecoverySource(daliboSource())
	if err != nil || rs.Plugin || rs.PgBackRest == nil || rs.PgBackRest.Object != "orders-stanza" || rs.PgBackRest.PluginConfig != "orders-pluginconfig" || rs.PgBackRest.Stanza != "" {
		t.Errorf("Dalibo = %+v, %v", rs.PgBackRest, err)
	}
	rs, err = DrillRecoverySource(operaSource())
	if err != nil || rs.PgBackRest == nil || rs.PgBackRest.Object != "orders-archive" || rs.PgBackRest.Stanza != "orders-v2" {
		t.Errorf("Opera = %+v, %v", rs.PgBackRest, err)
	}
	// Opera's default stanza is the source's name, written out: on the
	// drill the default would be the drill's own name.
	src := pgbackrestCluster(PgBackRestOperaPlugin, map[string]string{"pgbackrestObjectName": "orders-archive"})
	if rs, _ := DrillRecoverySource(src); rs.PgBackRest == nil || rs.PgBackRest.Stanza != "orders" || rs.PgBackRest.Parameters()["stanza"] != "orders" {
		t.Errorf("Opera default stanza = %+v", rs.PgBackRest)
	}
	for name, params := range map[string]map[string]string{"Dalibo without stanzaRef": {"pluginConfigRef": "x"}, "Opera without its Archive": {"stanza": "x"}} {
		plugin := PgBackRestDaliboPlugin
		if strings.HasPrefix(name, "Opera") {
			plugin = PgBackRestOperaPlugin
		}
		if _, err := DrillRecoverySource(pgbackrestCluster(plugin, params)); !errors.Is(err, ErrNoRecoverySource) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := DrillCluster(drillRequest(t, daliboSource()), [][]byte{drillKey}); err == nil {
		t.Error("a pgBackRest drill without a target label was built")
	}
}

// A pgBackRest drill can never archive into the source's stanza: it has
// no spec.plugins, its recovery source carries only the parameters that
// name where to read, and every way back is refused by the invariant.
func TestPgBackRestDrillNeverArchivesIntoTheSource(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]*cnpgv1.Cluster{"Dalibo": daliboSource(), "Opera": operaSource()} {
		c, err := DrillCluster(pgbackrestDrillRequest(t, src), [][]byte{drillKey})
		if err != nil {
			t.Fatal(name, err)
		}
		if len(c.Spec.Plugins) != 0 || c.Spec.Backup != nil {
			t.Errorf("%s: drill spec = %+v", name, c.Spec)
		}
		if ext := c.Spec.ExternalClusters[0].PluginConfiguration; ext.IsWALArchiver != nil || ext.Parameters["replicaStanzaRef"] != "" {
			t.Errorf("%s: recovery source = %+v", name, ext)
		}
	}
	tests := []struct {
		name   string
		src    func() *cnpgv1.Cluster
		mutate func(*cnpgv1.Cluster)
		why    string
	}{
		{"Dalibo archiving into the source's Stanza", daliboSource, func(c *cnpgv1.Cluster) {
			c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: PgBackRestDaliboPlugin, Parameters: map[string]string{"stanzaRef": "orders-stanza"}}}
		}, "would archive into the source's stanza"},
		{"Opera archiving into the source's Archive", operaSource, func(c *cnpgv1.Cluster) {
			c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: PgBackRestOperaPlugin, Parameters: map[string]string{"pgbackrestObjectName": "orders-archive", "stanza": "orders-v2"}}}
		}, "would archive into the source's stanza"},
		{"Opera archiving elsewhere", operaSource, func(c *cnpgv1.Cluster) {
			c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: PgBackRestOperaPlugin, Parameters: map[string]string{"pgbackrestObjectName": "drill-archive"}}}
		}, "spec.plugins is set"},
		{"the recovery source is the WAL archiver", daliboSource, func(c *cnpgv1.Cluster) {
			c.Spec.ExternalClusters[0].PluginConfiguration.IsWALArchiver = new(true)
		}, "marked as WAL archiver"},
		{"an extra Dalibo parameter", daliboSource, func(c *cnpgv1.Cluster) {
			c.Spec.ExternalClusters[0].PluginConfiguration.Parameters["replicaStanzaRef"] = "orders-stanza"
		}, "carries parameter replicaStanzaRef"},
		{"Opera without an explicit stanza", operaSource, func(c *cnpgv1.Cluster) {
			delete(c.Spec.ExternalClusters[0].PluginConfiguration.Parameters, "stanza")
		}, "both pgbackrestObjectName and stanza"},
		{"Dalibo without stanzaRef", daliboSource, func(c *cnpgv1.Cluster) {
			delete(c.Spec.ExternalClusters[0].PluginConfiguration.Parameters, "stanzaRef")
		}, "names no stanzaRef"},
		{"no backup label", operaSource, func(c *cnpgv1.Cluster) { c.Spec.Bootstrap.Recovery.RecoveryTarget = nil }, "recoveryTarget.backupID"},
	}
	for _, tt := range tests {
		c, err := DrillCluster(pgbackrestDrillRequest(t, tt.src()), [][]byte{drillKey})
		if err != nil {
			t.Fatal(tt.name, err)
		}
		tt.mutate(c)
		err = CheckDrillCluster(c)
		if !errors.Is(err, ErrDrillWrites) || !strings.Contains(err.Error(), tt.why) {
			t.Errorf("%s: CheckDrillCluster = %v, want %q", tt.name, err, tt.why)
		}
	}
}
