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

package passfilesync

import (
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/fyannk/pgToolBox/internal/operator/adminsync"
	"github.com/fyannk/pgToolBox/internal/operator/cnpgcred"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/declared"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

// The declared mode: pgAdmin installed by the pgtools chart,
// without the operator. Nobody works out the offered set; the chart renders
// the servers a person declared, each naming the Secret its credential is
// in. Before using a named Secret the sidecar checks it against what the
// Cluster or the DatabaseRole says right now, with CloudNativePG's own
// rules (cnpgcred), so a Secret that does not belong to the server it is
// declared for is never written into libpq's files:
//
//   - application / superuser: the Cluster must designate that Secret (and
//     enable superuser access, for the superuser);
//   - databaseRole: the DatabaseRole must name it, and CloudNativePG must
//     report the role applied at its current generation with that Secret
//     at the version it applied;
//   - every kind: the Secret's username, when it has one, must be the
//     declared or derived user.
//
// A server's CA is its CA Secret's ca.crt, once that Secret is checked to
// be the Cluster's server CA, or the ca.crt of its caConfigMap, mounted
// into the sidecar and not checked against anything live (the Pod has no
// right to read the Cluster's CA Secret): libpq's verify-full decides.
//
// A server whose credential fails a check keeps its service section when
// its user and CA are known, without a password: pgAdmin asks for one, and
// the sidecar logs why, by name. The line is written as soon as the state
// allows it. A server whose Cluster cannot serve its endpoint (an ro server
// on a one-instance Cluster, an ro or r server whose Service the Cluster
// disabled) is withheld instead: no section, and the registration leaves
// it out of pgAdmin (declared.Withheld) until the Cluster can serve it.

// Live is what the sidecar has seen of the objects the list names, by
// name; an absent entry is an object that is absent or not readable.
type Live struct {
	Clusters      map[string]*cnpgv1.Cluster
	DatabaseRoles map[string]*cnpgv1.DatabaseRole
	Poolers       map[string]*cnpgv1.Pooler
	Secrets       map[string]*corev1.Secret
	// CAConfigMaps holds the ca.crt read from each mounted caConfigMap; an
	// absent entry is a file that is not there (the ConfigMap is absent or
	// has no ca.crt) or could not be read.
	CAConfigMaps map[string][]byte
}

// Connection is one declared server resolved against Live.
type Connection struct {
	// Name and Service are the pgAdmin name and the service-file section.
	Name, Service string
	// Host and Port are where the endpoint listens.
	Host string
	Port int32
	// User and Database are the declared values, else what the Cluster or
	// the DatabaseRole say ("" while unknown).
	User, Database string
	// CAKey names the CA file; CA is its PEM ("" when not usable).
	CAKey, CA string
	// Password is the verified password ("" when none is offered).
	Password string
	// NoSection says why the server has no service section at all ("" when
	// it has one); NoPassword why its section carries no password ("" when
	// it does, or when the server declares credentials: none).
	NoSection, NoPassword string
	// Withheld is set when the Cluster cannot serve the endpoint
	// (cnpgcred.EndpointNotServed; NoSection says why): the server has no
	// section and the registration does not register it.
	Withheld bool
}

// Reason is why the connection is not fully offered, "" when it is.
func (c Connection) Reason() string {
	if c.NoSection != "" {
		return c.NoSection
	}
	return c.NoPassword
}

// Resolve checks every declared server against live state. It is pure: the
// same list and state give the same connections.
func Resolve(doc declared.Document, namespace string, live Live) []Connection {
	services := doc.Services()
	out := make([]Connection, 0, len(doc.Servers))
	for i, s := range doc.Servers {
		c := Connection{
			Name: s.DisplayName(), Service: services[i], Host: s.Host(namespace), Port: declared.Port,
			User: s.User, Database: s.Database, CAKey: s.Cluster,
		}
		resolve(&c, s, live)
		out = append(out, c)
	}
	return out
}

func resolve(c *Connection, s declared.Server, live Live) {
	cluster := live.Clusters[s.Cluster]
	if cluster == nil {
		c.NoSection = "Cluster " + s.Cluster + " is absent or not readable"
		return
	}
	// A Service that refuses every connection or does not exist: the
	// server is withheld from pgAdmin, not registered broken. An absent
	// Cluster above withholds nothing: unknown is not "cannot serve".
	if why := cnpgcred.EndpointNotServed(cluster, s.Endpoint); why != "" {
		c.NoSection, c.Withheld = why, true
		return
	}
	if pooler, ok := s.PoolerName(); ok {
		switch p := live.Poolers[pooler]; {
		case p == nil:
			c.NoSection = "Pooler " + pooler + " is absent or not readable"
			return
		case p.Spec.Cluster.Name != s.Cluster:
			c.NoSection = "Pooler " + pooler + " belongs to Cluster " + p.Spec.Cluster.Name + ", not " + s.Cluster
			return
		}
	}
	// libpq verifies the server (verify-full): without the Cluster's own
	// CA there is nothing to connect to.
	if s.CAConfigMap != "" {
		// A ConfigMap holding only ca.crt: nothing live to compare it
		// with (the Pod may not read the Cluster's CA Secret, which is
		// the point), so libpq's verify-full is the check.
		ca, ok := live.CAConfigMaps[s.CAConfigMap]
		if !ok {
			c.NoSection = "CA ConfigMap " + s.CAConfigMap + " is absent or has no " + declared.CAConfigMapKey
			return
		}
		pem, why := caPEM(ca)
		if why != "" {
			c.NoSection = "CA ConfigMap " + s.CAConfigMap + " " + why
			return
		}
		c.CA = pem
	} else if !resolveCASecret(c, s, cluster, live) {
		return
	}
	resolveCredentials(c, s, cluster, live)
}

// resolveCASecret takes the CA from the server's CA Secret, once checked
// to be the Cluster's; false (NoSection set) when it cannot.
func resolveCASecret(c *Connection, s declared.Server, cluster *cnpgv1.Cluster, live Live) bool {
	if want := cnpgcred.CASecretName(cluster); want != s.CASecretName() {
		c.NoSection = "the Cluster's server CA is in Secret " + want + ", not " + s.CASecretName() + " (set caSecret)"
		return false
	}
	ca, why := caOf(live.Secrets[s.CASecretName()])
	if why != "" {
		c.NoSection = "CA Secret " + s.CASecretName() + " " + why
		return false
	}
	c.CA = ca
	return true
}

// resolveCredentials checks the server's credential source against the
// Cluster or the DatabaseRole, and takes its password when it matches.
func resolveCredentials(c *Connection, s declared.Server, cluster *cnpgv1.Cluster, live Live) {
	check := offer.Server{User: c.User}
	switch credentials := s.Credentials; credentials.Kind {
	case declared.KindNone:
		return
	case declared.KindSecret:
		// Nothing live names this Secret: only its username is checked.
	case declared.KindApplication:
		c.User = or(c.User, cnpgcred.ApplicationOwner(cluster))
		c.Database = or(c.Database, cnpgcred.ApplicationDatabase(cluster))
		if want := cnpgcred.ApplicationSecretName(cluster); want != credentials.Secret {
			c.NoPassword = "the Cluster's application Secret is " + want + ", not " + credentials.Secret
			return
		}
	case declared.KindSuperuser:
		c.User = or(c.User, cnpgcred.SuperuserRole)
		c.Database = or(c.Database, cnpgcred.DefaultDatabase)
		if !cnpgcred.SuperuserEnabled(cluster) {
			c.NoPassword = "the Cluster does not enable superuser access"
			return
		}
		if want := cnpgcred.SuperuserSecretName(cluster); want != credentials.Secret {
			c.NoPassword = "the Cluster's superuser Secret is " + want + ", not " + credentials.Secret
			return
		}
	case declared.KindDatabaseRole:
		c.Database = or(c.Database, cnpgcred.DefaultDatabase)
		role := live.DatabaseRoles[credentials.Role]
		if role == nil {
			why := "DatabaseRole " + credentials.Role + " is absent or not readable"
			if c.User == "" {
				// The user comes from the role: nothing to write yet.
				c.NoSection = why
			} else {
				c.NoPassword = why
			}
			return
		}
		if role.Spec.ClusterRef.Name != s.Cluster {
			c.NoSection = "DatabaseRole " + credentials.Role + " belongs to Cluster " + role.Spec.ClusterRef.Name + ", not " + s.Cluster
			return
		}
		c.User = or(c.User, cnpgcred.DatabaseRoleName(role))
		if want := cnpgcred.PasswordSecretName(role); want != credentials.Secret {
			if want == "" {
				want = "none"
			}
			c.NoPassword = "DatabaseRole " + credentials.Role + "'s password Secret is " + want + ", not " + credentials.Secret
			return
		}
		if why := cnpgcred.RoleNotReady(role); why != "" {
			c.NoPassword = "DatabaseRole " + credentials.Role + ": " + why
			return
		}
		check.SecretResourceVersion = role.Status.SecretResourceVersion
	}
	check.User = c.User
	password, why := declaredPassword(live.Secrets[s.Credentials.Secret], check, s.Credentials.Kind == declared.KindSecret)
	if why != "" {
		c.NoPassword = "Secret " + s.Credentials.Secret + " " + why
		return
	}
	c.Password = password
}

// declaredPassword is passwordOf, and for a Secret the team manages, the
// password of the first pgpass line naming the user when the Secret has no
// password key (a pgpass-format Secret).
func declaredPassword(secret *corev1.Secret, server offer.Server, pgpassAllowed bool) (string, string) {
	password, why := passwordOf(secret, server)
	if why != "has no password" || !pgpassAllowed {
		return password, why
	}
	for _, line := range strings.Split(string(secret.Data["pgpass"]), "\n") {
		fields := adminsync.SplitPgpassLine(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(line, "#") || len(fields) != 5 || fields[4] == "" {
			continue
		}
		if fields[3] == server.User || fields[3] == adminsync.PgpassWildcard {
			return fields[4], ""
		}
	}
	return "", why
}

// RenderDeclared writes the files for the resolved connections: a section
// for each connection with one, its password when verified, and the CA of
// each Cluster those sections name.
func RenderDeclared(files Files, connections []Connection) (Outcome, error) {
	var outcome Outcome
	cas := map[string]string{}
	var servers []adminsync.Server
	for _, c := range connections {
		if c.Reason() != "" {
			outcome.Refused = append(outcome.Refused, c.Name+": "+c.Reason())
		}
		if c.NoSection != "" {
			continue
		}
		cas[c.CAKey] = c.CA
		servers = append(servers, adminsync.Server{
			Name:          c.Name,
			Group:         offer.ManagedGroup,
			Host:          c.Host,
			Port:          c.Port,
			MaintenanceDB: c.Database,
			Username:      c.User,
			SSLMode:       offer.SSLMode,
			Password:      c.Password,
			Service:       c.Service,
			SSLRootCert:   offer.CAFile(files.Dir, c.CAKey),
		})
		if c.Password != "" {
			outcome.Offered++
		}
	}
	_, refused, err := writeFiles(files, servers, cas)
	if refused != "" {
		outcome.Refused = append(outcome.Refused, refused)
	}
	return outcome, err
}

// WithheldOf returns the connections the registration must not register.
func WithheldOf(connections []Connection) declared.Withheld {
	var w declared.Withheld
	for _, c := range connections {
		if c.Withheld {
			w.Servers = append(w.Servers, declared.WithheldServer{Name: c.Name, Reason: c.NoSection})
		}
	}
	return w
}

func or(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
