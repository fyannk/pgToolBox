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

// Package pgbackrest holds the part of the two CloudNativePG pgBackRest
// plugins' CRDs that the resolver reads: Dalibo's Stanza
// (pgbackrest.dalibo.com/v1, cnpg-i-pgbackrest v0.0.3) and Opera's Archive
// (pgbackrest.cnpg.opera.com/v1, cnpg-plugin-pgbackrest v0.9.0). The
// structs copy only the fields the resolver uses, with their JSON tags
// verbatim, so neither plugin's Go module is a dependency: both are pre-1.0
// and pull a CloudNativePG operator module of their own. The objects are
// read unstructured and decoded here; a field the plugins add later is
// ignored, and one they rename is caught by the decode test of their
// example manifests at the pinned tag (testdata/).
package pgbackrest

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// The plugins' registration names and their Cluster parameters.
const (
	// (pkg/cnpgstatus is the source of truth.)
	// DaliboPluginName is cnpg-i-pgbackrest's (internal/metadata).
	DaliboPluginName = cnpgstatus.PgBackRestDaliboPlugin
	// DaliboStanzaParameter names the Stanza (internal/config: stanzaRef).
	DaliboStanzaParameter = cnpgstatus.PgBackRestDaliboObject
	// OperaPluginName is cnpg-plugin-pgbackrest's (internal/cnpgi/metadata).
	OperaPluginName = cnpgstatus.PgBackRestOperaPlugin
	// OperaObjectParameter names the Archive (pgbackrestObjectName).
	OperaObjectParameter = cnpgstatus.PgBackRestOperaObject
	// OperaStanzaParameter is the backup stanza; the Cluster's name when
	// absent (internal/cnpgi/operator/config NewFromCluster).
	OperaStanzaParameter = cnpgstatus.PgBackRestOperaStanza
)

// SecretKeySelector is machinery's api.SecretKeySelector: a key of a
// Secret in the object's namespace.
type SecretKeySelector struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// Stanza is Dalibo's Stanza (api/v1/stanza_types.go).
type Stanza struct {
	Spec StanzaSpec `json:"spec"`
}

// StanzaSpec is the Stanza's spec.
type StanzaSpec struct {
	Configuration StanzaConfiguration `json:"stanzaConfiguration"`
}

// StanzaConfiguration is one stanza and its repositories. The plugin
// numbers the S3 repositories repo1..N, then the Azure ones after them
// (internal/config GetEnvVarConfig).
type StanzaConfiguration struct {
	S3Repositories    []S3Repository    `json:"s3Repositories"`
	AzureRepositories []AzureRepository `json:"azureRepositories"`
	Name              string            `json:"name"`
	// CustomEnvVar is passed to pgBackRest before the plugin's own
	// variables (StanzaConfiguration.ToEnv).
	CustomEnvVar map[string]string `json:"customEnvVar,omitempty,omitzero"`
}

// S3Repository is one S3 repository of a Stanza.
type S3Repository struct {
	Bucket   string `json:"bucket"`
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	UriStyle string `json:"uriStyle"` //nolint:revive // the plugin's field name, verbatim.
	// VerifyTLS is a plain bool the plugin always emits: verification is
	// off unless set. The agent does not follow it: it never reads a
	// repository whose server it cannot authenticate.
	VerifyTLS bool          `json:"verifyTLS,omitempty,omitzero"`
	SecretRef *S3SecretRef  `json:"secretRef,omitempty"`
	RepoPath  string        `json:"repoPath"`
	Cipher    *CipherConfig `json:"cipherConfig"`
	// RetentionPolicy is passed as PGBACKREST_REPO<n>_RETENTION_*.
	RetentionPolicy Retention `json:"retentionPolicy"`
}

// Retention is a repository's retention: Dalibo's Retention
// (retentionPolicy) and Opera's PgbackrestRetention (retention) have the
// same fields and tags. A zero count is an unset option, which both
// plugins omit; Opera passes history=0 (a pointer), Dalibo cannot (an
// int32 with omitzero). History governs backup.history only, which the
// agent does not read.
type Retention struct {
	Archive     int32  `json:"archive,omitempty"`
	ArchiveType string `json:"archiveType,omitempty"`
	Full        int32  `json:"full,omitempty"`
	FullType    string `json:"fullType,omitempty"`
	Diff        int32  `json:"diff,omitempty"`
	History     *int32 `json:"history,omitempty"`
}

// S3SecretRef is the S3 key pair.
type S3SecretRef struct {
	AccessKeyIDReference     *SecretKeySelector `json:"accessKeyId,omitempty"`
	SecretAccessKeyReference *SecretKeySelector `json:"secretAccessKey,omitempty"`
}

// CipherConfig is the repository encryption of an S3 repository (the only
// kind the plugin encrypts).
type CipherConfig struct {
	PassReference *SecretKeySelector `json:"encryptionPass,omitempty"`
	Type          string             `json:"type,omitempty,omitzero"`
}

// AzureRepository is one Azure Blob repository of a Stanza.
type AzureRepository struct {
	Account   string          `json:"account"`
	Container string          `json:"container"`
	Endpoint  string          `json:"endpoint"`
	SecretRef *AzureSecretRef `json:"secretRef,omitempty"`
	// KeyType is shared, sas or auto (pgBackRest repo-azure-key-type).
	KeyType   string `json:"keyType"`
	UriStyle  string `json:"uriStyle"` //nolint:revive // the plugin's field name, verbatim.
	VerifyTLS *bool  `json:"verifyTLS"`
	RepoPath  string `json:"repoPath"`
	// RetentionPolicy is as on an S3 repository.
	RetentionPolicy Retention `json:"retentionPolicy"`
}

// AzureSecretRef is the shared key or the SAS token, as KeyType says.
type AzureSecretRef struct {
	KeyReference *SecretKeySelector `json:"keyReference,omitempty"`
}

// Archive is Opera's Archive (api/v1/archive_types.go).
type Archive struct {
	Spec ArchiveSpec `json:"spec"`
}

// ArchiveSpec is the Archive's spec.
type ArchiveSpec struct {
	Configuration                ArchiveConfiguration         `json:"configuration"`
	InstanceSidecarConfiguration InstanceSidecarConfiguration `json:"instanceSidecarConfiguration,omitempty"`
}

// InstanceSidecarConfiguration is the plugin sidecar's; its env reaches
// every pgBackRest command the sidecar runs.
type InstanceSidecarConfiguration struct {
	Env []corev1.EnvVar `json:"env,omitempty"`
}

// ArchiveConfiguration is internal/pgbackrest/api PgbackrestConfiguration.
type ArchiveConfiguration struct {
	Repositories []Repository              `json:"repositories"`
	Wal          *WalBackupConfiguration   `json:"wal,omitempty"`
	Data         *DataBackupConfiguration  `json:"data,omitempty"`
	Restore      *DataRestoreConfiguration `json:"restore,omitempty"`
	// Stanza is the WAL stanza (archive-push and archive-get); the backup
	// stanza when empty.
	Stanza string `json:"stanza,omitempty"`
}

// Repository is one repository (S3 only), numbered repo1..N in order.
type Repository struct {
	AWS           *S3Credentials     `json:"s3Credentials,omitempty"`
	Encryption    string             `json:"encryption,omitempty"`
	EncryptionKey *SecretKeySelector `json:"encryptionKey,omitempty"`
	// EndpointURL is --repo<n>-s3-endpoint as written (a host[:port]).
	EndpointURL      string             `json:"endpointURL,omitempty"`
	EndpointCA       *SecretKeySelector `json:"endpointCA,omitempty"`
	DisableVerifyTLS bool               `json:"disableVerifyTLS,omitempty"`
	// DestinationPath is --repo<n>-path.
	DestinationPath string `json:"destinationPath"`
	Bucket          string `json:"bucket"`
	// Retention is passed as --repo<n>-retention-*; nil sets none.
	Retention *Retention `json:"retention,omitempty"`
}

// S3Credentials is a repository's S3 identity.
type S3Credentials struct {
	// KeyType is shared (the default), auto or web-id.
	KeyType                  string             `json:"keyType,omitempty"`
	AccessKeyIDReference     *SecretKeySelector `json:"accessKeyId,omitempty"`
	SecretAccessKeyReference *SecretKeySelector `json:"secretAccessKey,omitempty"`
	// Region is the region itself, not a Secret reference.
	Region   string `json:"region,omitempty"`
	URIStyle string `json:"uriStyle,omitempty"`
}

// WalBackupConfiguration holds the WAL commands' extra arguments.
type WalBackupConfiguration struct {
	ArchiveAdditionalCommandArgs []string `json:"archiveAdditionalCommandArgs,omitempty"`
	RestoreAdditionalCommandArgs []string `json:"restoreAdditionalCommandArgs,omitempty"`
}

// DataBackupConfiguration holds the backup command's extra arguments.
type DataBackupConfiguration struct {
	AdditionalCommandArgs []string `json:"additionalCommandArgs,omitempty"`
}

// DataRestoreConfiguration holds the restore command's extra arguments.
type DataRestoreConfiguration struct {
	AdditionalCommandArgs []string `json:"additionalCommandArgs,omitempty"`
}

// DecodeStanza decodes an unstructured Stanza. Unknown fields are ignored.
func DecodeStanza(object *unstructured.Unstructured) (*Stanza, error) {
	stanza := &Stanza{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, stanza); err != nil {
		return nil, err
	}
	return stanza, nil
}

// DecodeArchive decodes an unstructured Archive. Unknown fields are ignored.
func DecodeArchive(object *unstructured.Unstructured) (*Archive, error) {
	archive := &Archive{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, archive); err != nil {
		return nil, err
	}
	return archive, nil
}
