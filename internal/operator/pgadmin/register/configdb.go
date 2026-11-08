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

package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/passfilesync"
)

// configLocalDir holds the config_local.py setup.py imports (PYTHONPATH
// first): CONFIG_DATABASE_URI on top of the image's own configuration.
const configLocalDir = "pgtoolbox-config"

// prepareConfigDatabase makes the settings database reachable for setup.py
// the way it is for pgAdmin: the credential files of that one connection,
// written from its Secrets (get by name, through the Pod's name-scoped
// Role) by the passfile-sync renderer, and CONFIG_DATABASE_URI, which
// carries no password.
func (o *Options) prepareConfigDatabase(ctx context.Context, doc offer.Document) error {
	server := doc.ConfigDatabase
	if o.ConfigDatabaseSelfContained {
		return o.writeConfigLocal()
	}
	switch {
	case server == nil:
		return errors.New("the offer has no settings database connection; the reconciler starts this Job only once it has one")
	case o.CredentialsDir == "" || o.Secrets == nil:
		return errors.New("--credentials-dir and an API client are required with --config-database-uri")
	case strings.ContainsAny(o.ConfigDatabaseURI, "\\'\"\n\r\x00"):
		return errors.New("--config-database-uri contains a quote, a backslash or a control character")
	}
	secrets := map[string]*corev1.Secret{}
	for _, ref := range []offer.SecretRef{server.PasswordSecret(), server.CASecret()} {
		secret, err := o.Secrets(ctx, ref.Namespace, ref.Name)
		switch {
		case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
			continue // Render refuses the connection and says which.
		case err != nil:
			return fmt.Errorf("read Secret %s: %w", ref.Key(), err)
		}
		secrets[ref.Key()] = secret
	}
	files := passfilesync.Files{Dir: o.CredentialsDir}
	outcome, err := passfilesync.Render(files, offer.Document{Version: offer.Version, ConfigDatabase: server}, secrets)
	if err != nil {
		return fmt.Errorf("write the settings database credentials: %w", err)
	}
	if outcome.Offered != 1 {
		return fmt.Errorf("the settings database credentials are not usable: %s", strings.Join(outcome.Refused, "; "))
	}

	if err := o.writeConfigLocal(); err != nil {
		return err
	}
	o.Env = append(o.Env,
		"PGSERVICEFILE="+files.ServiceFile(),
		"PGPASSFILE="+files.PassFile(),
		"PGSSLMODE="+offer.SSLMode,
	)
	if o.runIsDefault {
		o.Run = EnvRunner(o.Env)
	}
	return nil
}

// writeConfigLocal writes the config_local.py setup.py imports (PYTHONPATH
// first) and points setup.py's environment at it.
func (o *Options) writeConfigLocal() error {
	if strings.ContainsAny(o.ConfigDatabaseURI, "\\'\"\n\r\x00") {
		return errors.New("the settings database URI contains a quote, a backslash or a control character (percent-encode them)")
	}
	dir := filepath.Join(o.TempDir, configLocalDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	literal, err := json.Marshal(o.ConfigDatabaseURI) // a JSON string of ASCII is a Python literal
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "config_local.py"), []byte("CONFIG_DATABASE_URI = "+string(literal)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write config_local.py: %w", err)
	}
	o.Env = append(o.Env, "PYTHONPATH="+dir+":"+o.PgAdminDir)
	if o.runIsDefault {
		o.Run = EnvRunner(o.Env)
	}
	return nil
}
