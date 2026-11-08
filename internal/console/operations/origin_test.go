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
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"Claude Code":                   "Claude Code",
		"Claude\u202e Code":             "Claude Code",
		"\u200bClaude\u200d Code\u2066": "Claude Code",
		"tab\tand\nnewline":             "tabandnewline",
		"bad\xffutf8":                   "badutf8",
		strings.Repeat("é", 100):        strings.Repeat("é", 64),
	} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpected(t *testing.T) {
	r := viewmodel.ClusterRevision{UID: "u1", Generation: 4, Phase: "Cluster in healthy state", CurrentPrimary: "orders-1"}
	for kind, want := range map[viewmodel.OperationKind]viewmodel.ClusterRevision{
		viewmodel.OperationBackup:  {UID: "u1"},
		viewmodel.OperationReload:  {UID: "u1"},
		viewmodel.OperationRestart: r,
		viewmodel.OperationPromote: {UID: "u1", CurrentPrimary: "orders-1"},
	} {
		if got := Expected(kind, r); got != want {
			t.Errorf("Expected(%s) = %+v, want %+v", kind, got, want)
		}
	}
}

// A confirmed operation lands only on the Cluster the person confirmed on.
func TestRequestRefusesAChangedCluster(t *testing.T) {
	confirmed := Expected(viewmodel.OperationRestart, RevisionOf(cluster()))
	for _, tc := range []struct {
		name   string
		kind   viewmodel.OperationKind
		target string
		live   func(*cnpgv1.Cluster)
		want   string // "" accepted
	}{
		{"unchanged", viewmodel.OperationRestart, "", func(*cnpgv1.Cluster) {}, ""},
		{"status written, nothing that matters", viewmodel.OperationRestart, "", func(c *cnpgv1.Cluster) { c.ResourceVersion = "99" }, ""},
		{"recreated", viewmodel.OperationRestart, "", func(c *cnpgv1.Cluster) { c.UID = "u2" }, "deleted and created again"},
		{"spec changed", viewmodel.OperationRestart, "", func(c *cnpgv1.Cluster) { c.Generation = 9 }, "spec was changed"},
		{"rolling out", viewmodel.OperationRestart, "", func(c *cnpgv1.Cluster) { c.Status.Phase = cnpgv1.PhaseUpgrade }, "phase is now"},
		{"switched over", viewmodel.OperationPromote, "orders-3", func(c *cnpgv1.Cluster) {
			c.Status.CurrentPrimary, c.Status.TargetPrimary = "orders-2", "orders-2"
		}, "primary is now orders-2"},
		{"a backup ignores the phase", viewmodel.OperationBackup, "", func(c *cnpgv1.Cluster) { c.Status.Phase = cnpgv1.PhaseUpgrade }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := cluster()
			tc.live(live)
			var log bytes.Buffer
			w := &fakeWriter{c: live, pods: pods()}
			svc := service(ClusterState{Cluster: cluster(), Pods: pods()}, w, &log)
			want := Expected(tc.kind, RevisionOf(cluster()))
			if tc.kind == viewmodel.OperationRestart {
				want = confirmed
			}
			var observed viewmodel.ClusterRevision
			ctx := WithOrigin(context.Background(), Origin{Via: ViaMCP, Confirmation: "0123abcd", Confirmed: &want, Observed: &observed})
			confirmation := "orders"
			if tc.target != "" {
				confirmation = tc.target
			}
			_, err := svc.Request(ctx, principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: tc.kind, Target: tc.target, Confirmation: confirmation})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if !strings.Contains(log.String(), "confirmation=0123abcd") {
					t.Errorf("the audit line lacks the confirmation: %s", log.String())
				}
				if observed != (viewmodel.ClusterRevision{}) {
					t.Errorf("an accepted write reported a live Cluster: %+v", observed)
				}
				return
			}
			var status interface{ HTTPStatus() int }
			if !errors.As(err, &status) || status.HTTPStatus() != http.StatusConflict || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a 409 with %q", err, tc.want)
			}
			if len(w.patches)+len(w.backups) != 0 {
				t.Errorf("written anyway: %v %v", w.patches, w.backups)
			}
			// The caller learns the live Cluster, to ask again on it.
			if observed != Expected(tc.kind, RevisionOf(live)) {
				t.Errorf("observed = %+v, want the live %+v", observed, Expected(tc.kind, RevisionOf(live)))
			}
		})
	}
}
