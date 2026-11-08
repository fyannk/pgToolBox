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

// Package declared is the list of pgAdmin servers the pgtools chart
// renders from pgadmin.servers: pgAdmin without the
// operator. Where the operator works out the offered set from the live
// Clusters (package offer), here a person declares every server and where
// its credential comes from, and nothing is guessed. Two programs in the
// pgAdmin Pod read the list from a ConfigMap volume: the passfile-sync
// sidecar, which checks each named Secret against the live Cluster or
// DatabaseRole before writing it into libpq's files, and the registration,
// which turns the list into pgAdmin's shared servers.
//
// The list holds names only, never a password. Both readers still treat it
// as untrusted input: whoever may edit ConfigMaps in the namespace can
// rewrite the file. What such an edit can reach is bounded by the Pod's
// Role (get and watch on the Secrets the chart named, nothing else) and by
// the live checks.
//
// A server's CA comes from a Secret the sidecar reads through the API
// (caSecret, else CloudNativePG's <cluster>-ca, which also holds the CA's
// signing key) or from a ConfigMap holding only ca.crt (caConfigMap),
// mounted into the sidecar: with it the Role names no CA Secret at all.
package declared

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/fyannk/pgToolBox/internal/operator/adminsync"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

// FileKey is the key of the list in the chart's ConfigMap, and its file
// name in the Pod.
const FileKey = "servers.json"

// Version is the format version.
const Version = 1

// CAConfigMapKey is the key a caConfigMap holds the server CA under, and
// its file name in the mounted directory.
const CAConfigMapKey = "ca.crt"

// Port is where every endpoint listens: the Cluster's Services and a
// Pooler's Service both serve PostgreSQL on 5432.
const Port int32 = 5432

// MaxServers bounds a list, as it bounds an operator offer.
const MaxServers = offer.MaxServers

// MaxDocumentBytes bounds the encoded list.
const MaxDocumentBytes = offer.MaxDocumentBytes

// Kind is where a server's credential comes from.
type Kind string

const (
	// KindApplication is the Cluster's application user: the Secret must
	// be the one the Cluster designates (cnpgcred.ApplicationSecretName).
	KindApplication Kind = "application"
	// KindSuperuser is the Cluster's superuser, only with
	// enableSuperuserAccess, from the Secret the Cluster designates.
	KindSuperuser Kind = "superuser"
	// KindDatabaseRole is a DatabaseRole's password: the role must name
	// the Secret and CloudNativePG must have applied it at its current
	// generation, at the Secret's current version.
	KindDatabaseRole Kind = "databaseRole"
	// KindSecret is a Secret the team manages; nothing live to check it
	// against beyond its username.
	KindSecret Kind = "secret"
	// KindNone writes no password: pgAdmin asks for it.
	KindNone Kind = "none"
)

// Document is the declared list.
type Document struct {
	Version int      `json:"version"`
	Servers []Server `json:"servers"`
}

// Server is one declared connection.
type Server struct {
	// Name is pgAdmin's display name; empty selects DisplayName's default.
	Name string `json:"name,omitempty"`
	// Cluster is a CloudNativePG Cluster of the release namespace.
	Cluster string `json:"cluster"`
	// Endpoint is rw, ro, r or pooler:<name>.
	Endpoint string `json:"endpoint"`
	// Database and User, when set, win over what the Cluster or the
	// DatabaseRole say. KindSecret and KindNone need both.
	Database string `json:"database,omitempty"`
	User     string `json:"user,omitempty"`
	// CASecret holds the Cluster's server CA (key ca.crt). Empty, without
	// CAConfigMap, is CloudNativePG's default, <cluster>-ca: a name the
	// values do not state, deprecated (DerivedCASecret). The sidecar checks
	// it against the Cluster.
	CASecret string `json:"caSecret,omitempty"`
	// CAConfigMap holds the Cluster's server CA (key ca.crt only), mounted
	// into the sidecar (CAConfigMapFile) instead of read through the API.
	// Nothing live says what it must contain: libpq's verify-full is the
	// check. Exclusive with CASecret; every server of one Cluster names the
	// same one, since the Pod keeps one CA file per Cluster.
	CAConfigMap string      `json:"caConfigMap,omitempty"`
	Credentials Credentials `json:"credentials"`
}

// Credentials is where a server's credential comes from. In values and in
// the file it is the string "none", or an object with exactly one key:
// {application: {secret}}, {superuser: {secret}}, {databaseRole: {name,
// secret}} or {secret: <name>}. There is no default: an
// implicit none looks like a broken setup, an implicit Secret is a guess.
type Credentials struct {
	Kind Kind
	// Secret names the Secret holding the password (every kind but none).
	Secret string
	// Role names the DatabaseRole (KindDatabaseRole).
	Role string
}

type secretRef struct {
	Secret string `json:"secret"`
}

type roleRef struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

type credentialsObject struct {
	Application  *secretRef `json:"application,omitempty"`
	Superuser    *secretRef `json:"superuser,omitempty"`
	DatabaseRole *roleRef   `json:"databaseRole,omitempty"`
	Secret       *string    `json:"secret,omitempty"`
}

// UnmarshalJSON reads "none" or a one-key object.
func (c *Credentials) UnmarshalJSON(raw []byte) error {
	if string(bytes.TrimSpace(raw)) == "null" {
		// Left unset: Validate says the credential is missing.
		*c = Credentials{}
		return nil
	}
	var word string
	if err := json.Unmarshal(raw, &word); err == nil {
		if Kind(word) != KindNone {
			return fmt.Errorf("credentials %q: the only string form is %q", word, KindNone)
		}
		*c = Credentials{Kind: KindNone}
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var o credentialsObject
	if err := decoder.Decode(&o); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	var kinds []Credentials
	if o.Application != nil {
		kinds = append(kinds, Credentials{Kind: KindApplication, Secret: o.Application.Secret})
	}
	if o.Superuser != nil {
		kinds = append(kinds, Credentials{Kind: KindSuperuser, Secret: o.Superuser.Secret})
	}
	if o.DatabaseRole != nil {
		kinds = append(kinds, Credentials{Kind: KindDatabaseRole, Secret: o.DatabaseRole.Secret, Role: o.DatabaseRole.Name})
	}
	if o.Secret != nil {
		kinds = append(kinds, Credentials{Kind: KindSecret, Secret: *o.Secret})
	}
	if len(kinds) != 1 {
		return errors.New("credentials: exactly one of application, superuser, databaseRole, secret or \"none\"")
	}
	*c = kinds[0]
	return nil
}

// MarshalJSON writes the form UnmarshalJSON reads.
func (c Credentials) MarshalJSON() ([]byte, error) {
	switch c.Kind {
	case KindNone:
		return json.Marshal(string(KindNone))
	case KindApplication:
		return json.Marshal(credentialsObject{Application: &secretRef{Secret: c.Secret}})
	case KindSuperuser:
		return json.Marshal(credentialsObject{Superuser: &secretRef{Secret: c.Secret}})
	case KindDatabaseRole:
		return json.Marshal(credentialsObject{DatabaseRole: &roleRef{Name: c.Role, Secret: c.Secret}})
	case KindSecret:
		return json.Marshal(credentialsObject{Secret: &c.Secret})
	}
	return nil, fmt.Errorf("credentials: unknown kind %q", c.Kind)
}

var (
	dnsName    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	secretName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	endpoint   = regexp.MustCompile(`^(rw|ro|r|pooler:[a-z0-9]([-a-z0-9]*[a-z0-9])?)$`)
	// roleName: what a PostgreSQL role or database name may be in a libpq
	// file and a pgAdmin definition: printable, no line breaks.
	roleName = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,63}$`)
	// displayName: pgAdmin's server name, printable.
	displayName = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,200}$`)
)

// Load reads and validates the list from a file.
func Load(path string) (Document, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path from the container's arguments.
	if err != nil {
		return Document{}, fmt.Errorf("read the declared servers: %w", err)
	}
	return Parse(raw)
}

// Parse decodes and validates a list. Unknown fields are refused, so a
// newer chart is noticed rather than half-understood.
func Parse(raw []byte) (Document, error) {
	if len(raw) > MaxDocumentBytes {
		return Document{}, fmt.Errorf("declared servers of %d bytes exceed %d", len(raw), MaxDocumentBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var doc Document
	if err := decoder.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("decode the declared servers: %w", err)
	}
	if decoder.More() {
		return Document{}, errors.New("decode the declared servers: trailing data")
	}
	if doc.Version != Version {
		return Document{}, fmt.Errorf("declared servers version %d, this binary reads %d", doc.Version, Version)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// Validate checks every field against what the Kubernetes API, the libpq
// files and pgAdmin accept, and that display names are unique.
func (d Document) Validate() error {
	if len(d.Servers) > MaxServers {
		return fmt.Errorf("%d declared servers, over %d", len(d.Servers), MaxServers)
	}
	var problems []error
	names := map[string]bool{}
	// The caConfigMap of each Cluster's first server ("" for a Secret).
	caSource := map[string]string{}
	for i, s := range d.Servers {
		check := func(ok bool, field string) {
			if !ok {
				problems = append(problems, fmt.Errorf("server %d: invalid %s", i, field))
			}
		}
		check(dnsName.MatchString(s.Cluster) && len(s.Cluster) <= 50, "cluster")
		check(endpoint.MatchString(s.Endpoint) && len(s.Endpoint) <= 63+len("pooler:"), "endpoint")
		check(s.Name == "" || displayName.MatchString(s.Name), "name")
		check(s.Database == "" || roleName.MatchString(s.Database), "database")
		check(s.User == "" || roleName.MatchString(s.User), "user")
		check(s.CASecret == "" || (secretName.MatchString(s.CASecret) && len(s.CASecret) <= 253), "caSecret")
		check(s.CAConfigMap == "" || (secretName.MatchString(s.CAConfigMap) && len(s.CAConfigMap) <= 253), "caConfigMap")
		if s.CASecret != "" && s.CAConfigMap != "" {
			problems = append(problems, fmt.Errorf("server %d: caSecret and caConfigMap are exclusive", i))
		}
		if first, seen := caSource[s.Cluster]; !seen {
			caSource[s.Cluster] = s.CAConfigMap
		} else if first != s.CAConfigMap {
			problems = append(problems, fmt.Errorf("server %d: every server of Cluster %s must name the same caConfigMap", i, s.Cluster))
		}
		switch s.Credentials.Kind {
		case KindNone:
		case KindApplication, KindSuperuser, KindSecret:
			check(secretName.MatchString(s.Credentials.Secret) && len(s.Credentials.Secret) <= 253, "credentials secret")
		case KindDatabaseRole:
			check(secretName.MatchString(s.Credentials.Secret) && len(s.Credentials.Secret) <= 253, "credentials secret")
			check(secretName.MatchString(s.Credentials.Role) && len(s.Credentials.Role) <= 253, "credentials databaseRole name")
		default:
			problems = append(problems, fmt.Errorf("server %d: credentials missing (no default: name one)", i))
		}
		if (s.Credentials.Kind == KindSecret || s.Credentials.Kind == KindNone) && (s.User == "" || s.Database == "") {
			problems = append(problems, fmt.Errorf("server %d: credentials %s need user and database", i, s.Credentials.Kind))
		}
		name := s.DisplayName()
		if names[name] {
			problems = append(problems, fmt.Errorf("server %d: duplicate name %q", i, name))
		}
		names[name] = true
	}
	return errors.Join(problems...)
}

// DisplayName is the server's name in pgAdmin: the declared one, else
// "<cluster> <endpoint> (<who>)", who being the declared user, else the
// credential's source.
func (s Server) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	who := s.User
	if who == "" {
		switch s.Credentials.Kind {
		case KindDatabaseRole:
			who = s.Credentials.Role
		default:
			who = string(s.Credentials.Kind)
		}
	}
	return fmt.Sprintf("%s %s (%s)", s.Cluster, s.Endpoint, who)
}

// CASecretName is the Secret the server's CA is read from; "" when it
// comes from a ConfigMap.
func (s Server) CASecretName() string {
	if s.CAConfigMap != "" {
		return ""
	}
	if s.CASecret != "" {
		return s.CASecret
	}
	return s.Cluster + "-ca"
}

// DerivedCASecret reports whether the server's CA Secret is the derived
// <cluster>-ca default rather than a name the values state.
func (s Server) DerivedCASecret() bool {
	return s.CASecret == "" && s.CAConfigMap == ""
}

// CAConfigMapFile is where a caConfigMap's CA is mounted under dir,
// <dir>/<configmap>/ca.crt: the layout of the chart's projected volume.
func CAConfigMapFile(dir, configMap string) string {
	return filepath.Join(dir, configMap, CAConfigMapKey)
}

// CAConfigMaps returns the ConfigMaps the list mounts, sorted and unique.
func (d Document) CAConfigMaps() []string {
	var out []string
	for _, s := range d.Servers {
		if s.CAConfigMap != "" && !slices.Contains(out, s.CAConfigMap) {
			out = append(out, s.CAConfigMap)
		}
	}
	slices.Sort(out)
	return out
}

// PoolerName is the Pooler of a pooler:<name> endpoint.
func (s Server) PoolerName() (string, bool) {
	return strings.CutPrefix(s.Endpoint, "pooler:")
}

// Host is the Service the endpoint names, in namespace: the Cluster's
// <cluster>-rw, -ro or -r Service, or the Pooler's own Service (a Pooler's
// Service carries the Pooler's name).
func (s Server) Host(namespace string) string {
	if pooler, ok := s.PoolerName(); ok {
		return pooler + "." + namespace + ".svc"
	}
	return s.Cluster + "-" + s.Endpoint + "." + namespace + ".svc"
}

// Services returns each server's service-file section name, in order:
// the same derivation as the operator's offer (adminsync.ServiceNames).
func (d Document) Services() []string {
	servers := make([]adminsync.Server, 0, len(d.Servers))
	for _, s := range d.Servers {
		servers = append(servers, adminsync.Server{Name: s.DisplayName()})
	}
	return adminsync.ServiceNames(servers)
}

// Names are the objects a list makes the sidecar read, by kind: exactly
// what the chart's Role grants (get and watch by name). A caConfigMap is
// mounted, not read through the API, so it is not among them.
type Names struct {
	Clusters, DatabaseRoles, Poolers, Secrets []string
}

// Names returns the objects the list names, each kind sorted and unique.
func (d Document) Names() Names {
	var n Names
	add := func(list *[]string, name string) {
		if name != "" && !slices.Contains(*list, name) {
			*list = append(*list, name)
		}
	}
	for _, s := range d.Servers {
		add(&n.Clusters, s.Cluster)
		add(&n.Secrets, s.CASecretName())
		add(&n.Secrets, s.Credentials.Secret)
		add(&n.DatabaseRoles, s.Credentials.Role)
		if pooler, ok := s.PoolerName(); ok {
			add(&n.Poolers, pooler)
		}
	}
	for _, list := range []*[]string{&n.Clusters, &n.DatabaseRoles, &n.Poolers, &n.Secrets} {
		slices.Sort(*list)
	}
	return n
}
