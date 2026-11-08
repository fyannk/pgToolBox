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
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

func TestIDRoundTrip(t *testing.T) {
	for _, id := range []ID{
		{Kind: viewmodel.OperationBackup, Namespace: "team-a", Cluster: "orders", Created: "ondemand-20261029-abcde", AcceptedAt: 1793275200},
		{Kind: viewmodel.OperationPromote, Namespace: "team-a", Cluster: "orders", Instance: "orders-2", Marker: "2026-10-29T12:00:00.000000Z", AcceptedAt: 1},
		{Kind: viewmodel.OperationRestart, Namespace: "a", Cluster: "b.c", Marker: "2026-10-29T12:00:00Z", AcceptedAt: 5},
		{Kind: viewmodel.OperationRestoreDrill, Namespace: "team-a", Cluster: "orders", Created: "drill-orders-2611031415", CreatedNamespace: "drills", AcceptedAt: 9},
	} {
		got, err := ParseID(id.Encode())
		if err != nil || got != id {
			t.Errorf("round trip of %+v: %+v, %v", id, got, err)
		}
	}
}

func TestParseIDRefuses(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, raw := range map[string]string{
		"empty":              "",
		"not base64":         "!!!",
		"too long":           strings.Repeat("A", MaxIDLength+1),
		"wrong version":      enc(`{"v":2,"k":"backup","n":"a","c":"b","t":1}`),
		"unknown field":      enc(`{"v":1,"k":"backup","n":"a","c":"b","t":1,"x":1}`),
		"unknown kind":       enc(`{"v":1,"k":"delete","n":"a","c":"b","t":1}`),
		"bad namespace":      enc(`{"v":1,"k":"backup","n":"A/../b","c":"b","t":1}`),
		"promote no inst":    enc(`{"v":1,"k":"promote","n":"a","c":"b","t":1}`),
		"inst not promote":   enc(`{"v":1,"k":"reload","n":"a","c":"b","i":"x","t":1}`),
		"created on promote": enc(`{"v":1,"k":"promote","n":"a","c":"b","i":"x","b":"y","t":1}`),
		"no time":            enc(`{"v":1,"k":"backup","n":"a","c":"b"}`),
		"trailing data":      enc(`{"v":1,"k":"backup","n":"a","c":"b","t":1}{}`),
		"bad marker":         enc(`{"v":1,"k":"restart","n":"a","c":"b","m":"<script>","t":1}`),
		"drill no namespace": enc(`{"v":1,"k":"restoreDrill","n":"a","c":"b","b":"d","t":1}`),
		"drill no cluster":   enc(`{"v":1,"k":"restoreDrill","n":"a","c":"b","d":"x","t":1}`),
		"drill bad ns":       enc(`{"v":1,"k":"restoreDrill","n":"a","c":"b","b":"d","d":"X_Y","t":1}`),
		"ns not drill":       enc(`{"v":1,"k":"backup","n":"a","c":"b","b":"d","d":"x","t":1}`),
	} {
		if _, err := ParseID(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheckConfirmation(t *testing.T) {
	tests := []struct {
		name string
		req  viewmodel.OperationRequest
		ok   bool
	}{
		{"backup typed", viewmodel.OperationRequest{Kind: viewmodel.OperationBackup, Confirmation: "orders"}, true},
		{"backup mistyped", viewmodel.OperationRequest{Kind: viewmodel.OperationBackup, Confirmation: "order"}, false},
		{"restart with target", viewmodel.OperationRequest{Kind: viewmodel.OperationRestart, Target: "orders-1", Confirmation: "orders"}, false},
		{"promote typed", viewmodel.OperationRequest{Kind: viewmodel.OperationPromote, Target: "orders-2", Confirmation: "orders-2"}, true},
		{"promote cluster name", viewmodel.OperationRequest{Kind: viewmodel.OperationPromote, Target: "orders-2", Confirmation: "orders"}, false},
		{"promote no target", viewmodel.OperationRequest{Kind: viewmodel.OperationPromote, Confirmation: ""}, false},
		{"unknown", viewmodel.OperationRequest{Kind: "fence", Confirmation: "orders"}, false},
	}
	for _, tt := range tests {
		err := CheckConfirmation("orders", tt.req)
		if (err == nil) != tt.ok {
			t.Errorf("%s: %v", tt.name, err)
		}
		var r *Rejected
		if err != nil && (!errors.As(err, &r) || r.HTTPStatus() != http.StatusUnprocessableEntity) {
			t.Errorf("%s: %T %v", tt.name, err, err)
		}
	}
}

// FuzzParseID: whatever the input, an accepted handle names a
// well-formed Cluster and re-encodes to a handle that parses to itself.
func FuzzParseID(f *testing.F) {
	f.Add(ID{Kind: viewmodel.OperationBackup, Namespace: "team-a", Cluster: "orders", Created: "x-1", AcceptedAt: 1}.Encode())
	f.Add(ID{Kind: viewmodel.OperationPromote, Namespace: "team-a", Cluster: "orders", Instance: "orders-2", Marker: "m", AcceptedAt: 2}.Encode())
	f.Add("")
	f.Add("eyJ2IjoxfQ")
	f.Fuzz(func(t *testing.T, raw string) {
		id, err := ParseID(raw)
		if err != nil {
			return
		}
		if !dnsLabel.MatchString(id.Namespace) || !dnsSubdomain.MatchString(id.Cluster) || id.AcceptedAt <= 0 || !validKind(id.Kind) {
			t.Fatalf("accepted a malformed handle: %+v", id)
		}
		again, err := ParseID(id.Encode())
		if err != nil || again != id {
			t.Fatalf("re-encoding changed the handle: %+v -> %+v, %v", id, again, err)
		}
	})
}
