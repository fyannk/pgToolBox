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

package evidenceagent

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestCertificateLifecycle(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	names := servingDNSNames("orders-evidence-agent", "team-a")
	if names[2] != "orders-evidence-agent.team-a.svc" {
		t.Fatalf("names = %v", names)
	}
	ca, err := newCA("team-a/orders evidence agent CA", now)
	if err != nil {
		t.Fatal(err)
	}
	if !caUsable(ca, now) || !validPEMBundle(ca.Cert) {
		t.Fatal("a fresh CA is unusable")
	}
	leaf, err := issueLeaf(ca, names, now)
	if err != nil {
		t.Fatal(err)
	}
	renewAt, usable := leafRenewal(leaf, ca, names, now)
	if !usable || !renewAt.Equal(now.Add(leafLifetime-leafRenewBefore)) {
		t.Fatalf("fresh leaf: usable=%v renewAt=%s", usable, renewAt)
	}
	block, _ := pem.Decode(leaf.Cert)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || cert.IsCA || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("leaf = %+v, %v", cert, err)
	}

	tests := []struct {
		name  string
		leaf  keyPair
		ca    keyPair
		names []string
		now   time.Time
	}{
		{"inside the renewal window", leaf, ca, names, renewAt.Add(time.Second)},
		{"expired", leaf, ca, names, now.Add(leafLifetime + time.Hour)},
		{"another Service name", leaf, ca, servingDNSNames("billing-evidence-agent", "team-a"), now},
		{"another namespace", leaf, ca, servingDNSNames("orders-evidence-agent", "team-b"), now},
		{"signed by another CA", leaf, mustCA(t, now), names, now},
		{"key does not match", keyPair{Cert: leaf.Cert, Key: ca.Key}, ca, names, now},
		{"garbage", keyPair{Cert: []byte("x"), Key: []byte("y")}, ca, names, now},
		{"empty", keyPair{}, ca, names, now},
	}
	for _, test := range tests {
		if _, usable := leafRenewal(test.leaf, test.ca, test.names, test.now); usable {
			t.Errorf("%s: leaf judged usable", test.name)
		}
	}

	if caUsable(ca, now.Add(caLifetime-caRenewBefore+time.Hour)) {
		t.Error("a CA within a year of expiry is still used")
	}
	if caUsable(leaf, now) {
		t.Error("a leaf passes as a CA")
	}
	if caUsable(keyPair{Cert: ca.Cert, Key: leaf.Key}, now) {
		t.Error("a CA certificate with a foreign key passes")
	}
}

func TestLeafNeverOutlivesItsCA(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	ca := mustCA(t, now)
	leaf, err := issueLeaf(ca, servingDNSNames("a", "b"), now.Add(caLifetime-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	caCert, _, _ := parsePair(ca)
	leafCert, _, _ := parsePair(leaf)
	if leafCert.NotAfter.After(caCert.NotAfter) {
		t.Fatal("the leaf outlives its CA")
	}
}

func TestValidPEMBundle(t *testing.T) {
	ca := mustCA(t, time.Now())
	for name, bundle := range map[string][]byte{
		"empty": nil, "spaces": []byte("  \n"), "key": ca.Key, "trailing garbage": append(append([]byte{}, ca.Cert...), []byte("garbage")...),
	} {
		if validPEMBundle(bundle) {
			t.Errorf("%s accepted", name)
		}
	}
	if !validPEMBundle(append(append([]byte{}, ca.Cert...), mustCA(t, time.Now()).Cert...)) {
		t.Error("a two-certificate bundle rejected")
	}
}

func mustCA(t *testing.T, now time.Time) keyPair {
	t.Helper()
	ca, err := newCA("test", now)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}
