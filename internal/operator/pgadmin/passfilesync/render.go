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

// Package passfilesync is the sidecar of the pgAdmin Pod that turns the
// offered set into libpq's service file and password file. It
// runs the operator binary, copied into the Pod by an init container, from
// pgAdmin's image, so it runs under pgAdmin's own UID and the 0600 files it
// writes on the memory-backed volume are readable by pgAdmin alone. It
// reads the offer ConfigMap and exactly the Secrets the offer names through
// the Pod's name-scoped Role (get and watch by name; it cannot list), and
// rewrites the files atomically on every change, so a rotated password
// reaches libpq on the next connection without a rollout or a setup.py run.
//
// It never logs, prints or reports a Secret's content; only names.
package passfilesync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/fyannk/pgToolBox/internal/operator/adminsync"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

// MaxSecretBytes bounds what one Secret may contribute; a larger one is
// refused rather than copied onto the memory-backed volume.
const MaxSecretBytes = 64 << 10

// Files are the credential files under one directory.
type Files struct {
	Dir string
}

// ServiceFile is libpq's PGSERVICEFILE.
func (f Files) ServiceFile() string { return filepath.Join(f.Dir, "pg_service.conf") }

// PassFile is libpq's PGPASSFILE.
func (f Files) PassFile() string { return filepath.Join(f.Dir, "pgpass") }

// Outcome says what a render offered and what it refused (names only).
type Outcome struct {
	Offered int
	Refused []string
}

// Render writes the CA files, then the service and password files, for doc
// from the Secrets it has seen (by offer.SecretRef.Key: the name, or
// namespace/name for a Secret of a granted namespace; absent when not
// readable). The settings database connection, when the document has one,
// is written like a server. Each
// server is offered only when its password Secret is present at the
// version the offer requires, names the offered role (when it names one),
// and its Cluster's CA is present: a server libpq could not verify is not
// offered. CA files of Clusters no longer offered are removed.
func Render(files Files, doc offer.Document, secrets map[string]*corev1.Secret) (Outcome, error) {
	var outcome Outcome
	refuse := func(server offer.Server, why string) {
		outcome.Refused = append(outcome.Refused, fmt.Sprintf("%s: %s", server.Name, why))
	}

	caDir := filepath.Dir(offer.CAFile(files.Dir, "x"))
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		return outcome, fmt.Errorf("create CA directory: %w", err)
	}
	cas := map[string]string{} // CA key -> PEM
	var servers []adminsync.Server
	for _, server := range doc.Connections() {
		ca, why := caOf(secrets[server.CASecret().Key()])
		if why != "" {
			refuse(server, "CA Secret "+server.CASecret().Key()+" "+why)
			continue
		}
		password, why := passwordOf(secrets[server.PasswordSecret().Key()], server)
		if why != "" {
			refuse(server, "Secret "+server.PasswordSecret().Key()+" "+why)
			continue
		}
		cas[server.CAKey()] = ca
		servers = append(servers, adminsync.Server{
			Name:          server.Name,
			Group:         offer.ManagedGroup,
			Host:          server.Host,
			Port:          server.Port,
			MaintenanceDB: server.DBName,
			Username:      server.User,
			SSLMode:       offer.SSLMode,
			Password:      password,
			Service:       server.Service,
			SSLRootCert:   offer.CAFile(files.Dir, server.CAKey()),
		})
	}

	offered, refused, err := writeFiles(files, servers, cas)
	if refused != "" {
		outcome.Refused = append(outcome.Refused, refused)
	}
	outcome.Offered = offered
	return outcome, err
}

// writeFiles writes the CA files (cas: CA key -> PEM) and their bundle,
// then the service and password files for servers, and removes the CA
// files of Clusters no longer present. offered counts the servers the
// files carry; refused names those the files cannot carry exactly ("" when
// none).
func writeFiles(files Files, servers []adminsync.Server, cas map[string]string) (int, string, error) {
	caDir := filepath.Dir(offer.CAFile(files.Dir, "x"))
	// CA files first: a service section must never name a CA file that is
	// not there yet.
	clusters := make([]string, 0, len(cas))
	for cluster := range cas {
		clusters = append(clusters, cluster)
	}
	slices.Sort(clusters)
	var bundle strings.Builder
	for _, cluster := range clusters {
		if err := adminsync.WriteFile(offer.CAFile(files.Dir, cluster), cas[cluster]); err != nil {
			return 0, "", fmt.Errorf("write CA of %s: %w", cluster, err)
		}
		bundle.WriteString(strings.TrimRight(cas[cluster], "\n") + "\n")
	}
	if err := adminsync.WriteFile(offer.CABundleFile(files.Dir), bundle.String()); err != nil {
		return 0, "", fmt.Errorf("write CA bundle: %w", err)
	}

	offered, refused, err := adminsync.WriteCredentialFiles(files.ServiceFile(), files.PassFile(), servers)
	message := ""
	if refused != nil {
		message = refused.Error()
	}
	if err != nil {
		return 0, message, err
	}
	return len(offered), message, removeStaleCAs(caDir, clusters)
}

// caOf returns a CA Secret's ca.crt (only that key: a CNPG <cluster>-ca
// Secret also holds the signing key, which is never copied).
func caOf(secret *corev1.Secret) (string, string) {
	if secret == nil {
		return "", "is absent or not readable"
	}
	return caPEM(secret.Data["ca.crt"])
}

// caPEM checks a ca.crt, from a CA Secret or a mounted CA ConfigMap.
func caPEM(ca []byte) (string, string) {
	switch {
	case len(ca) == 0:
		return "", "has no ca.crt"
	case len(ca) > MaxSecretBytes:
		return "", "ca.crt is too large"
	case !strings.Contains(string(ca), "-----BEGIN CERTIFICATE-----"):
		return "", "ca.crt is not PEM"
	}
	return string(ca), ""
}

// passwordOf returns the password a Secret holds for server, or why it is
// not offered.
func passwordOf(secret *corev1.Secret, server offer.Server) (string, string) {
	switch {
	case secret == nil:
		return "", "is absent or not readable"
	case server.SecretResourceVersion != "" && secret.ResourceVersion != server.SecretResourceVersion:
		return "", "is not at the version CloudNativePG applied to the role"
	}
	size := 0
	for _, value := range secret.Data {
		size += len(value)
	}
	if size > MaxSecretBytes {
		return "", "is too large"
	}
	if username, ok := secret.Data["username"]; ok && string(username) != server.User {
		return "", "names another role than " + server.User
	}
	password := string(secret.Data["password"])
	if password == "" {
		return "", "has no password"
	}
	return password, ""
}

func removeStaleCAs(dir string, keep []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var problems []error
	for _, entry := range entries {
		name := entry.Name()
		cluster, ok := strings.CutSuffix(name, ".crt")
		if !ok || name == filepath.Base(offer.CABundleFile("")) || slices.Contains(keep, cluster) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}
