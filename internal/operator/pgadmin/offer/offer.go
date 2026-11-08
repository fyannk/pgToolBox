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

// Package offer is the contract between the PgAdmin reconciler and the two
// programs it starts in the namespace: the passfile-sync sidecar (which
// turns it into the libpq service and password files) and the registration
// Job (which turns it into pgAdmin's shared servers). The reconciler writes
// it into a Secret only the operator writes (no password in it: names of
// Secrets and roles), so editing ConfigMaps in the namespace cannot steer
// the sidecar or the registration.
//
// Both readers still treat the document as untrusted input: whoever may
// update Secrets in the namespace can rewrite it. What such an edit can do
// is bounded by the sidecar's name-scoped Role (it reads only the Secrets
// the reconciler granted) and by the checks below.
package offer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// SecretKey is the key of the document in the offer Secret.
const SecretKey = "offer.json"

// Version is the document format version.
const Version = 1

// MaxServers bounds a document (and with it the credential files and the
// registration). It equals the file writer's bound.
const MaxServers = 64

// MaxDocumentBytes bounds the encoded document.
const MaxDocumentBytes = 256 << 10

// ManagedGroup is the pgAdmin server group the registration owns; it
// replaces that group and nothing else.
const ManagedGroup = "PgToolBox"

// SSLMode of every offered server: the Cluster must prove the Service name
// with a certificate from its CA.
const SSLMode = "verify-full"

// Document is the offered set of one PgAdmin.
type Document struct {
	Version int      `json:"version"`
	Servers []Server `json:"servers"`
	// ConfigDatabase, when pgAdmin's settings database is on a Cluster
	// (spec.configDatabase), is that connection: the sidecar writes it into
	// the credential files like a server, the registration never
	// registers it.
	ConfigDatabase *Server `json:"configDatabase,omitempty"`
}

// Server is one offered connection.
type Server struct {
	// Name is pgAdmin's display name.
	Name string `json:"name"`
	// Cluster and Endpoint say where it points.
	Cluster  string `json:"cluster"`
	Endpoint string `json:"endpoint"`
	// Host, Port, DBName and User are the libpq parameters.
	Host   string `json:"host"`
	Port   int32  `json:"port"`
	DBName string `json:"dbname"`
	User   string `json:"user"`
	// Service is the section name in the service file.
	Service string `json:"service"`
	// SecretName holds the password (key "password"); its "username", when
	// present, must equal User.
	SecretName string `json:"secretName"`
	// SecretResourceVersion, when set, is the only version of the Secret
	// that may be offered: the one CloudNativePG recorded applying to a
	// DatabaseRole.
	SecretResourceVersion string `json:"secretResourceVersion,omitempty"`
	// CASecretName holds the Cluster's server CA (key "ca.crt" only).
	CASecretName string `json:"caSecretName"`
	// Namespace is the namespace of the Cluster and of both Secrets when
	// it is not the PgAdmin's own (a Cluster a PgAdminAccess grants).
	Namespace string `json:"namespace,omitempty"`
	// Owner is the pgAdmin user a personal server belongs to; empty for a
	// shared server.
	Owner string `json:"owner,omitempty"`
}

// CAKey names the Cluster's CA file: the Cluster, qualified by its
// namespace when it is not the PgAdmin's (names cannot contain a dot, so
// the two forms never collide).
func (s Server) CAKey() string {
	if s.Namespace == "" {
		return s.Cluster
	}
	return s.Namespace + "." + s.Cluster
}

// SecretRef names one Secret: Namespace is empty for the PgAdmin's own.
type SecretRef struct {
	Namespace, Name string
}

// Key is the SecretRef as one string ("name" or "namespace/name").
func (r SecretRef) Key() string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "/" + r.Name
}

// PasswordSecret is the server's password Secret.
func (s Server) PasswordSecret() SecretRef {
	return SecretRef{Namespace: s.Namespace, Name: s.SecretName}
}

// CASecret is the server's CA Secret.
func (s Server) CASecret() SecretRef {
	return SecretRef{Namespace: s.Namespace, Name: s.CASecretName}
}

// Connections returns the servers, then the settings database connection
// when there is one: everything the credential files carry.
func (d Document) Connections() []Server {
	out := slices.Clone(d.Servers)
	if d.ConfigDatabase != nil {
		out = append(out, *d.ConfigDatabase)
	}
	return out
}

var (
	dnsName     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	secretName  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	hostName    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	serviceName = regexp.MustCompile(`^pgtoolbox-[a-z0-9-]{1,100}$`)
	endpoint    = regexp.MustCompile(`^(rw|ro|r|pooler:[a-z0-9]([-a-z0-9]*[a-z0-9])?)$`)
	// roleName: what a PostgreSQL role name may be in a libpq file and a
	// pgAdmin definition: printable, no line breaks.
	roleName = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,63}$`)
	// ownerName: a pgAdmin username as spec.admins accepts it.
	ownerName = regexp.MustCompile(`^[^-\s'"\\\x00-\x1f\x7f][^\s'"\\\x00-\x1f\x7f]*$`)
	// displayName: pgAdmin's server name, printable.
	displayName = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,200}$`)
)

// Encode returns the canonical encoding (stable for a stable document, so
// its hash drives the registration).
func Encode(doc Document) ([]byte, error) {
	doc.Version = Version
	if doc.Servers == nil {
		doc.Servers = []Server{}
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxDocumentBytes {
		return nil, fmt.Errorf("offer document of %d bytes exceeds %d", len(raw), MaxDocumentBytes)
	}
	return raw, nil
}

// Parse decodes and validates a document. Unknown fields are refused, so a
// newer writer is noticed rather than half-understood.
func Parse(raw []byte) (Document, error) {
	if len(raw) > MaxDocumentBytes {
		return Document{}, fmt.Errorf("offer document of %d bytes exceeds %d", len(raw), MaxDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("decode offer document: %w", err)
	}
	if decoder.More() {
		return Document{}, errors.New("decode offer document: trailing data")
	}
	if doc.Version != Version {
		return Document{}, fmt.Errorf("offer document version %d, this binary reads %d", doc.Version, Version)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Validate checks every field against what the libpq files, the Kubernetes
// API and pgAdmin accept, and that names are unique.
func (d Document) Validate() error {
	if len(d.Servers) > MaxServers {
		return fmt.Errorf("offer document has %d servers, over %d", len(d.Servers), MaxServers)
	}
	services := map[string]bool{}
	names := map[string]bool{}
	var problems []error
	for i, s := range d.Connections() {
		check := func(ok bool, field string) {
			if !ok {
				problems = append(problems, fmt.Errorf("server %d: invalid %s", i, field))
			}
		}
		check(displayName.MatchString(s.Name), "name")
		check(dnsName.MatchString(s.Cluster) && len(s.Cluster) <= 253, "cluster")
		check(endpoint.MatchString(s.Endpoint), "endpoint")
		check(hostName.MatchString(s.Host) && len(s.Host) <= 253, "host")
		check(s.Port > 0 && s.Port < 65536, "port")
		check(roleName.MatchString(s.DBName), "dbname")
		check(roleName.MatchString(s.User), "user")
		check(serviceName.MatchString(s.Service), "service")
		check(secretName.MatchString(s.SecretName) && len(s.SecretName) <= 253, "secretName")
		check(secretName.MatchString(s.CASecretName) && len(s.CASecretName) <= 253, "caSecretName")
		check(len(s.SecretResourceVersion) <= 64 && !strings.ContainsAny(s.SecretResourceVersion, "\x00\n\r"), "secretResourceVersion")
		check(s.Namespace == "" || (dnsName.MatchString(s.Namespace) && len(s.Namespace) <= 63), "namespace")
		check(s.Owner == "" || (ownerName.MatchString(s.Owner) && len(s.Owner) <= 254), "owner")
		if services[s.Service] {
			problems = append(problems, fmt.Errorf("server %d: duplicate service %q", i, s.Service))
		}
		if names[s.Name] {
			problems = append(problems, fmt.Errorf("server %d: duplicate name %q", i, s.Name))
		}
		services[s.Service], names[s.Name] = true, true
	}
	return errors.Join(problems...)
}

// CAFile is where the sidecar writes a Cluster's CA, under dir; key is
// Server.CAKey.
func CAFile(dir, key string) string {
	return path.Join(dir, "ca", key+".crt")
}

// CABundleFile is the concatenation of every offered Cluster's CA, the
// PGSSLROOTCERT of libpq clients that name no service.
func CABundleFile(dir string) string {
	return path.Join(dir, "ca", "bundle.crt")
}

// SecretNames returns every Secret of namespace the document names
// (passwords and CAs; "" is the PgAdmin's own namespace), sorted and
// unique: exactly what the sidecar's Role there may read.
func (d Document) SecretNames(namespace string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range d.Connections() {
		if s.Namespace != namespace {
			continue
		}
		for _, name := range []string{s.SecretName, s.CASecretName} {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// SecretRefs returns every Secret the document names, in every namespace,
// sorted and unique: what the sidecar watches.
func (d Document) SecretRefs() []SecretRef {
	seen := map[SecretRef]bool{}
	var out []SecretRef
	for _, s := range d.Connections() {
		for _, ref := range []SecretRef{s.PasswordSecret(), s.CASecret()} {
			if !seen[ref] {
				seen[ref] = true
				out = append(out, ref)
			}
		}
	}
	slices.SortFunc(out, func(a, b SecretRef) int { return strings.Compare(a.Key(), b.Key()) })
	return out
}

// Owners returns the pgAdmin users of the personal servers, sorted.
func (d Document) Owners() []string {
	var out []string
	for _, s := range d.Servers {
		if s.Owner != "" && !slices.Contains(out, s.Owner) {
			out = append(out, s.Owner)
		}
	}
	slices.Sort(out)
	return out
}

// Namespaces returns the other namespaces the document names, sorted.
func (d Document) Namespaces() []string {
	var out []string
	for _, s := range d.Connections() {
		if s.Namespace != "" && !slices.Contains(out, s.Namespace) {
			out = append(out, s.Namespace)
		}
	}
	slices.Sort(out)
	return out
}
