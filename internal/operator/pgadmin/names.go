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

package pgadmin

import (
	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/metadata"
)

// Components (app.kubernetes.io/component) of the generated objects.
const (
	componentPgAdmin      = "pgadmin"
	componentRegistration = "registration"
	componentBackup       = "upgrade-backup"
)

// Fixed paths and ports of the pgAdmin Pod.
const (
	// pgAdminPort is where pgAdmin listens. 8080 because the restricted
	// security context (no privilege escalation) cannot bind below 1024.
	pgAdminPort int32 = 8080
	// proxyPort is where the OpenShift oauth-proxy listens (TLS).
	proxyPort int32 = 8443

	settingsVolume    = "settings"
	settingsMountPath = "/var/lib/pgadmin"
	settingsDBPath    = settingsMountPath + "/pgadmin4.db"
	// backupDir holds the pre-upgrade backups, under DATA_DIR and outside
	// STORAGE_DIR, so the file manager never serves them.
	backupDir = settingsMountPath + "/.pgtoolbox/backups"
	// lockFile serializes the Jobs that run setup.py.
	lockFile = settingsMountPath + "/.pgtoolbox/setup.lock"

	tmpVolume = "tmp"

	// runVolume is the pgAdmin image's only writable path outside its data
	// directory and /tmp: the generated config_distro.py and, for an
	// arbitrary UID, its passwd entry.
	runVolume        = "run"
	runMountPath     = "/run/pgadmin"
	configDistroPath = runMountPath + "/config_distro.py"

	credentialsVolume    = "credentials"
	credentialsMountPath = "/run/pgadmin/credentials" // #nosec G101 -- mount path.
	serviceFilePath      = credentialsMountPath + "/pg_service.conf"
	passFilePath         = credentialsMountPath + "/pgpass"

	binVolume    = "pgtoolbox-bin"
	binMountPath = "/opt/pgtoolbox/bin"
	binPath      = binMountPath + "/manager"

	bootstrapVolume    = "bootstrap"
	bootstrapMountPath = "/run/secrets/pgadmin-bootstrap" // #nosec G101 -- mount path.
	bootstrapKey       = "password"
	// bootstrapEmail is pgAdmin's internal account, which owns the shared
	// servers. Nobody signs in as it: the only authentication source is
	// SSO. pgAdmin validates it as an e-mail address and rejects the
	// reserved test domains, so it uses the API group's own domain.
	bootstrapEmail = "pgtoolbox@pgtoolbox.fyannk.dev"

	oauth2Volume     = "oauth2-client"
	oauth2MountPath  = "/run/pgadmin/oauth2" // #nosec G101 -- mount path.
	oauth2SecretFile = "client-secret"

	apiAccessVolume    = "kube-api-access"
	apiAccessMountPath = "/var/run/secrets/kubernetes.io/serviceaccount" // #nosec G101 -- mount path.

	offerVolume    = "offer"
	offerMountPath = "/run/pgtoolbox/offer"

	proxyTLSVolume    = "proxy-tls"
	proxyTLSMountPath = "/etc/tls/private"
	proxyCookieVolume = "proxy-cookie"
	proxyCookiePath   = "/etc/proxy/cookie"

	// fsGroup makes the settings volume writable by the image's user
	// where the platform assigns no group (not OpenShift). It is a group,
	// not a user: no UID is fixed on the Pod.
	fsGroup int64 = 5050
)

// names are the generated object names of one PgAdmin.
type names struct {
	base, offer, credentials, bootstrap, proxyCookie, proxyTLS string
}

func namesOf(p *pgtoolboxv1alpha1.PgAdmin) names {
	base := metadata.BoundedName(p.Name, "-pgadmin")
	return names{
		base:        base,
		offer:       metadata.BoundedName(p.Name, "-pgadmin-offer"),
		credentials: metadata.BoundedName(p.Name, "-pgadmin-credentials"),
		bootstrap:   metadata.BoundedName(p.Name, "-pgadmin-bootstrap"),
		proxyCookie: metadata.BoundedName(p.Name, "-pgadmin-proxy-cookie"),
		proxyTLS:    metadata.BoundedName(p.Name, "-pgadmin-tls"),
	}
}

// registrationJobName is the Job of one offered set: its hash is in the
// name, so the Job of the current set is found by name and a changed set
// starts a new one.
func registrationJobName(p *pgtoolboxv1alpha1.PgAdmin, hash string) string {
	return metadata.BoundedName(p.Name, "-pgadmin-sync-"+hash[:8])
}

// backupJobName is the pre-upgrade backup Job of one image change.
func backupJobName(p *pgtoolboxv1alpha1.PgAdmin, hash string) string {
	return metadata.BoundedName(p.Name, "-pgadmin-bk-"+hash[:8])
}

func identity(p *pgtoolboxv1alpha1.PgAdmin) metadata.Identity {
	return metadata.Identity{Application: "pgadmin", Instance: p.Name, InstanceLabelKey: pgtoolboxv1alpha1.PgAdminLabelKey}
}
