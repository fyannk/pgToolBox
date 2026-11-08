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
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"slices"
	"time"
)

// The operator issuer: one CA per EvidenceAgent, kept
// in a Secret the agent Pod never mounts, signing a renewable serving
// certificate. A CA per agent (rather than one operator-wide CA) keeps a
// leaked CA key confined to the one agent identity that trusts it, and the
// console already trusts each agent through its own status.caBundle.
const (
	caLifetime   = 10 * 365 * 24 * time.Hour
	leafLifetime = 365 * 24 * time.Hour
	// leafRenewBefore: a leaf is re-issued when less than this remains;
	// the agent reloads the mounted pair, so renewal needs no rollout.
	leafRenewBefore = 120 * 24 * time.Hour
	// caRenewBefore: a CA this close to expiry is replaced (the bundle in
	// status changes and the console follows it).
	caRenewBefore = 365 * 24 * time.Hour
	// clockSkew backdates NotBefore so a Pod on a slightly late node
	// accepts a fresh certificate.
	clockSkew = 5 * time.Minute

	caCertKey = "ca.crt"
	caKeyKey  = "ca.key"
)

var errInvalidPEM = errors.New("certificate material is invalid")

// keyPair is PEM material.
type keyPair struct {
	Cert []byte
	Key  []byte
}

// servingDNSNames are the names a console may dial: the Service name in
// every form cluster DNS resolves.
func servingDNSNames(service, namespace string) []string {
	return []string{
		service,
		service + "." + namespace,
		service + "." + namespace + ".svc",
		service + "." + namespace + ".svc.cluster.local",
	}
}

// newCA generates a P-256 CA valid caLifetime from now.
func newCA(commonName string, now time.Time) (keyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return keyPair{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return keyPair{}, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"pgtoolbox"}},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(caLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return keyPair{}, err
	}
	return encodePair(der, key)
}

// issueLeaf signs a serving certificate for dnsNames with ca.
func issueLeaf(ca keyPair, dnsNames []string, now time.Time) (keyPair, error) {
	caCert, caKey, err := parsePair(ca)
	if err != nil {
		return keyPair{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return keyPair{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return keyPair{}, err
	}
	notAfter := now.Add(leafLifetime)
	if notAfter.After(caCert.NotAfter) {
		notAfter = caCert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     slices.Clone(dnsNames),
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return keyPair{}, err
	}
	return encodePair(der, key)
}

// caUsable reports whether ca parses as a CA key pair that is not within
// caRenewBefore of expiry.
func caUsable(ca keyPair, now time.Time) bool {
	cert, _, err := parsePair(ca)
	return err == nil && cert.IsCA && now.Before(cert.NotAfter.Add(-caRenewBefore)) && !now.Before(cert.NotBefore)
}

// leafRenewal checks a serving pair against ca and dnsNames. It returns the
// time it must be renewed and whether it is usable now; an unusable pair is
// re-issued at once.
func leafRenewal(leaf, ca keyPair, dnsNames []string, now time.Time) (time.Time, bool) {
	cert, _, err := parsePair(leaf)
	if err != nil {
		return time.Time{}, false
	}
	caCert, _, err := parsePair(ca)
	if err != nil {
		return time.Time{}, false
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	for _, name := range dnsNames {
		if _, err := cert.Verify(x509.VerifyOptions{
			DNSName: name, Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			return time.Time{}, false
		}
	}
	if !slices.Equal(cert.DNSNames, dnsNames) {
		return time.Time{}, false
	}
	renewAt := cert.NotAfter.Add(-leafRenewBefore)
	return renewAt, now.Before(renewAt)
}

func parsePair(pair keyPair) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, rest := pem.Decode(pair.Cert)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, errInvalidPEM
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, errInvalidPEM
	}
	keyBlock, _ := pem.Decode(pair.Key)
	if keyBlock == nil {
		return nil, nil, errInvalidPEM
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, errInvalidPEM
	}
	public, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !public.Equal(&key.PublicKey) {
		return nil, nil, errInvalidPEM
	}
	return cert, key, nil
}

func encodePair(der []byte, key *ecdsa.PrivateKey) (keyPair, error) {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return keyPair{}, err
	}
	return keyPair{
		Cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Key:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// validPEMBundle reports whether bundle holds at least one certificate and
// nothing else, as status.caBundle must.
func validPEMBundle(bundle []byte) bool {
	found := false
	for rest := bundle; len(bytes.TrimSpace(rest)) > 0; {
		block, next := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return false
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return false
		}
		found = true
		rest = next
	}
	return found
}

// chainsTo reports whether the first certificate of leafPEM verifies against
// bundle for dnsName at now (server authentication).
func chainsTo(leafPEM, bundle []byte, dnsName string, now time.Time) bool {
	block, _ := pem.Decode(leafPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		return false
	}
	_, err = leaf.Verify(x509.VerifyOptions{DNSName: dnsName, Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err == nil
}
