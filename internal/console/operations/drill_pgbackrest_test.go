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

package operations

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

const drillLabel = "20261029-030004F_20261029-090002I"

// pgbackrestDrillFixture is orders backing up with a pgBackRest plugin,
// its newest Backup object the one that took the window's newest label.
func pgbackrestDrillFixture(t *testing.T, drillNS, plugin string, params map[string]string, mutate func(*DrillOptions)) *drillFixture {
	t.Helper()
	stopped := now.Add(-3 * time.Hour)
	f := newDrillFixture(t, drillNS, func(d *DrillOptions) {
		d.State.(*fakeDrillState).target = PgBackRestTarget{Label: drillLabel, Stanza: "orders", StoppedAt: &stopped}
		if mutate != nil {
			mutate(d)
		}
	})
	st := f.svc.opts.State.(*fakeState).st
	t2 := true
	st.Cluster.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: plugin, IsWALArchiver: &t2, Parameters: params}}
	took := completed("orders-pgbr", stopped)
	took.Status.BackupID, took.Status.BackupName = drillLabel, drillLabel
	st.Backups = []*cnpgv1.Backup{completed("orders-old", now.Add(-30*time.Hour)), took}
	f.svc.opts.State = &fakeState{st: st}
	f.w.c = st.Cluster
	return f
}

func TestPgBackRestDrillRequest(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name, plugin string
		params, want map[string]string
		source       string
	}{
		{
			"Dalibo", ops.PgBackRestDaliboPlugin,
			map[string]string{"stanzaRef": "orders-stanza", "pluginConfigRef": "orders-config", "replicaStanzaRef": "x"},
			map[string]string{"stanzaRef": "orders-stanza", "pluginConfigRef": "orders-config"},
			"Stanza orders-stanza (pgBackRest, Dalibo)",
		},
		{
			"Opera with the default stanza", ops.PgBackRestOperaPlugin,
			map[string]string{"pgbackrestObjectName": "orders-archive"},
			map[string]string{"pgbackrestObjectName": "orders-archive", "stanza": "orders"},
			"Archive orders-archive (pgBackRest, Opera), stanza orders",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := pgbackrestDrillFixture(t, "", tt.plugin, tt.params, nil)
			v, err := f.svc.Offers(ctx, principal(), authz.Capabilities{}, "team-a", "orders")
			if err != nil {
				t.Fatal(err)
			}
			o := drillOfferOf(t, v)
			if len(o.Blockers) != 0 || o.Drill.BackupLabel != drillLabel || o.Drill.Backup != "orders-pgbr" || o.Drill.RecoverySource != tt.source {
				t.Fatalf("offer = %+v, blockers %v", o.Drill, reasons(o.Blockers))
			}
			if _, err := f.svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationRestoreDrill, Confirmation: "orders"}); err != nil {
				t.Fatal(err)
			}
			d := f.w.drills[0]
			ext := d.Spec.ExternalClusters[0].PluginConfiguration
			if len(d.Spec.Plugins) != 0 || ext.Name != tt.plugin || !maps.Equal(ext.Parameters, tt.want) ||
				d.Spec.Bootstrap.Recovery.RecoveryTarget.BackupID != drillLabel {
				t.Errorf("drill spec: plugins %v, source %+v, target %+v", d.Spec.Plugins, ext, d.Spec.Bootstrap.Recovery.RecoveryTarget)
			}
			d.CreationTimestamp = metav1.NewTime(now.Add(time.Second))
			meta, err := ops.VerifyDrill(d, "team-a", f.svc.opts.Drills.Keys)
			if err != nil || meta.BackupLabel != drillLabel || meta.Backup != "orders-pgbr" {
				t.Errorf("sealed record = %+v, %v", meta, err)
			}
		})
	}
}

func TestPgBackRestDrillBlockers(t *testing.T) {
	ctx := context.Background()
	dalibo := map[string]string{"stanzaRef": "orders-stanza"}
	for _, tt := range []struct {
		name, ns, plugin string
		params           map[string]string
		mutate           func(*DrillOptions)
		want             string
	}{
		{"another drill namespace", "drills", ops.PgBackRestDaliboPlugin, dalibo, nil, "PgBackRestOtherNamespace"},
		{"no recovery window", "", ops.PgBackRestDaliboPlugin, dalibo, func(d *DrillOptions) {
			st := d.State.(*fakeDrillState)
			st.target, st.targetWhy = PgBackRestTarget{}, "the evidence is stale"
		}, "NoRecoveryWindow"},
		{"the agent read another stanza", "", ops.PgBackRestOperaPlugin, map[string]string{"pgbackrestObjectName": "a", "stanza": "orders-v2"}, nil, "StanzaMismatch"},
		{"a Dalibo agent read another Stanza", "", ops.PgBackRestDaliboPlugin, dalibo, func(d *DrillOptions) {
			st := d.State.(*fakeDrillState)
			st.target.Plugin, st.target.Object = ops.PgBackRestDaliboPlugin, "orders-old-stanza"
		}, "StanzaObjectMismatch"},
		{"an agent of the other plugin", "", ops.PgBackRestDaliboPlugin, dalibo, func(d *DrillOptions) {
			st := d.State.(*fakeDrillState)
			st.target.Plugin, st.target.Object = ops.PgBackRestOperaPlugin, "orders-stanza"
		}, "StanzaObjectMismatch"},
		{"an Opera agent read another Archive", "", ops.PgBackRestOperaPlugin, map[string]string{"pgbackrestObjectName": "orders-archive"}, func(d *DrillOptions) {
			st := d.State.(*fakeDrillState)
			st.target.Plugin, st.target.Object = ops.PgBackRestOperaPlugin, "elsewhere"
		}, "StanzaObjectMismatch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := pgbackrestDrillFixture(t, tt.ns, tt.plugin, tt.params, tt.mutate)
			v, err := f.svc.Offers(ctx, principal(), authz.Capabilities{}, "team-a", "orders")
			if err != nil {
				t.Fatal(err)
			}
			o := drillOfferOf(t, v)
			if got := strings.Join(reasons(o.Blockers), ","); !strings.Contains(got, tt.want) || strings.Contains(got, "NoCompletedBackup") {
				t.Errorf("blockers %s, want %s", got, tt.want)
			}
			if _, err := f.svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationRestoreDrill, Confirmation: "orders"}); err == nil || len(f.w.drills) != 0 {
				t.Errorf("drill created despite %s: %v", tt.want, err)
			}
		})
	}
	// The agent reporting the object the Cluster names is no blocker.
	f := pgbackrestDrillFixture(t, "", ops.PgBackRestDaliboPlugin, dalibo, func(d *DrillOptions) {
		st := d.State.(*fakeDrillState)
		st.target.Plugin, st.target.Object = ops.PgBackRestDaliboPlugin, "orders-stanza"
	})
	if v, _ := f.svc.Offers(ctx, principal(), authz.Capabilities{}, "team-a", "orders"); len(drillOfferOf(t, v).Blockers) != 0 {
		t.Errorf("the agent's own object blocked the drill: %v", reasons(drillOfferOf(t, v).Blockers))
	}
	// A window whose backup no Backup object took (taken outside CNPG)
	// still drills: the label is what is restored.
	f = pgbackrestDrillFixture(t, "", ops.PgBackRestDaliboPlugin, dalibo, nil)
	st := f.svc.opts.State.(*fakeState).st
	st.Backups = nil
	f.svc.opts.State = &fakeState{st: st}
	v, _ := f.svc.Offers(ctx, principal(), authz.Capabilities{}, "team-a", "orders")
	if o := drillOfferOf(t, v); len(o.Blockers) != 0 || o.Drill.Backup != drillLabel {
		t.Errorf("label without Backup object: %+v %v", o.Drill, reasons(o.Blockers))
	}
}
