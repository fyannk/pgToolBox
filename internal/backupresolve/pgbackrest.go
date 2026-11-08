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

package backupresolve

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/backupresolve/pgbackrest"
	"github.com/fyannk/pgToolBox/pkg/discovery"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// A Cluster that archives through one of the two pgBackRest
// plugins is resolved from the plugin's own object (Dalibo's Stanza through
// stanzaRef, Opera's Archive through pgbackrestObjectName). Only what the
// plugins write in that object is read; anything that could put the files
// elsewhere than the object says (an environment or argument override) is
// refused by name, and so is every shape the agent cannot account for.

const (
	// FormatBarmanCloud is Composition.Format of a Barman configuration.
	FormatBarmanCloud = "barman-cloud"
	// FormatPgBackRest is Composition.Format of a pgBackRest plugin's.
	FormatPgBackRest = "pgbackrest"

	// PgBackRestKindStanza is Dalibo's object kind (PgBackRestLocation.Kind).
	PgBackRestKindStanza = "Stanza"
	// PgBackRestKindArchive is Opera's object kind.
	PgBackRestKindArchive = "Archive"

	// cipherAES256CBC is the only cipher either plugin writes.
	cipherAES256CBC = "aes-256-cbc"
)

// PgBackRestPluginNames are the CNPG-I plugins that archive with
// pgBackRest, in the order Locate prefers them when a Cluster enables both.
var PgBackRestPluginNames = []string{pgbackrest.DaliboPluginName, pgbackrest.OperaPluginName}

// PgBackRestSettings are the agent author's pgBackRest choices
// (EvidenceAgent spec.pgbackrest, pgtools evidence.agents[].pgbackrest).
type PgBackRestSettings struct {
	// Repository is the pgBackRest repository number to read (repo<n>);
	// zero reads repo1, where both plugins send a backup that names none.
	Repository int
	// EndpointCA is the author's own CA for the repository endpoint, a key
	// of an allow-listed Secret. It wins over Opera's endpointCA.
	EndpointCA *SecretKeyRef
}

// repository is the number Settings select.
func (s *PgBackRestSettings) repository() int {
	if s == nil || s.Repository == 0 {
		return 1
	}
	return s.Repository
}

// PgBackRestLocation is the pgBackRest plugin a Cluster archives with and
// the object of that plugin it names.
type PgBackRestLocation struct {
	// Plugin is pgbackrest.dalibo.com or pgbackrest.cnpg.opera.com.
	Plugin string
	// Kind is Stanza (Dalibo) or Archive (Opera); ObjectName its name in
	// the Cluster's namespace.
	Kind       string
	ObjectName string
	// StanzaParameter is Opera's stanza parameter; empty when the Cluster
	// sets none (the stanza is then the Cluster's name).
	StanzaParameter string
}

// GVK is the kind of the plugin's object.
func (l *PgBackRestLocation) GVK() schema.GroupVersionKind {
	if l.Kind == PgBackRestKindArchive {
		return discovery.ArchiveGVK
	}
	return discovery.StanzaGVK
}

// PgBackRestObject is a plugin object read and decoded: exactly one is set.
type PgBackRestObject struct {
	Stanza  *pgbackrest.Stanza
	Archive *pgbackrest.Archive
}

// enabledPgBackRestPlugin returns the Cluster's enabled pgBackRest plugin
// entry, or nil.
func enabledPgBackRestPlugin(plugins []pgBackRestPluginEntry) *pgBackRestPluginEntry {
	for _, name := range PgBackRestPluginNames {
		for i := range plugins {
			if plugins[i].name == name && plugins[i].enabled {
				return &plugins[i]
			}
		}
	}
	return nil
}

// pgBackRestPluginEntry is the part of a Cluster's spec.plugins[] entry
// Locate reads.
type pgBackRestPluginEntry struct {
	name       string
	enabled    bool
	parameters map[string]string
}

// locatePgBackRest completes a Located for a Cluster archiving through a
// pgBackRest plugin, or names why it cannot be read.
func locatePgBackRest(located *Located, request Request, entry *pgBackRestPluginEntry) (*Located, *Problem) {
	cluster := located.Cluster.Name
	if request.ServerName != "" {
		return located, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
			"spec.serverName names a Barman server, but Cluster %s archives with the pgBackRest plugin %s, whose stanza comes from the plugin: remove spec.serverName",
			cluster, entry.name)
	}
	location := &PgBackRestLocation{Plugin: entry.name, Kind: PgBackRestKindStanza}
	parameter := pgbackrest.DaliboStanzaParameter
	if entry.name == pgbackrest.OperaPluginName {
		location.Kind, parameter = PgBackRestKindArchive, pgbackrest.OperaObjectParameter
		location.StanzaParameter = entry.parameters[pgbackrest.OperaStanzaParameter]
	}
	location.ObjectName = entry.parameters[parameter]
	located.ServerName = ""
	if location.ObjectName == "" {
		return located, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
			"Cluster %s enables the pgBackRest plugin %s without the %s parameter, which names its %s", cluster, entry.name, parameter, location.Kind)
	}
	located.PgBackRest = location
	return located, nil
}

// ReadPgBackRest reads the plugin object a Located names, unstructured
// (neither caller registers a scheme for an optional CRD), and decodes the
// fields the resolver uses. NotFound, an unserved kind and an undecodable
// object are Problems; anything else is an error.
func ReadPgBackRest(ctx context.Context, reader client.Reader, namespace string, location *PgBackRestLocation) (*PgBackRestObject, *Problem, error) {
	gvk := location.GVK()
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(gvk)
	err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: location.ObjectName}, object)
	switch {
	case apierrors.IsNotFound(err):
		return nil, problem(pgtoolboxv1alpha1.ReasonObjectStoreNotFound, "%s %s was not found in namespace %s", location.Kind, location.ObjectName, namespace), nil
	case meta.IsNoMatchError(err):
		return nil, problem(pgtoolboxv1alpha1.ReasonObjectStoreAPIUnavailable, "the %s %s API is not served (plugin %s)", gvk.GroupVersion(), gvk.Kind, location.Plugin), nil
	case err != nil:
		return nil, nil, fmt.Errorf("read %s %s: %w", location.Kind, location.ObjectName, err)
	}
	decoded := &PgBackRestObject{}
	if location.Kind == PgBackRestKindArchive {
		decoded.Archive, err = pgbackrest.DecodeArchive(object)
	} else {
		decoded.Stanza, err = pgbackrest.DecodeStanza(object)
	}
	if err != nil {
		// The decoder's error may quote a value; it is not repeated.
		return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s %s: spec does not decode as the plugin %s writes it", location.Kind, location.ObjectName, location.Plugin), nil
	}
	return decoded, nil, nil
}

// ComposePgBackRest maps the plugin object onto the agent's profile: one
// stanza of one pgBackRest repository on S3 or Azure Blob Storage. Every
// shape outside it is a named Problem. It reuses the provider settings of a
// barman-cloud composition (Provider, AgentDestination, EndpointURL, the
// credential references) and adds the stanza, the repository number and
// the cipher pass.
func ComposePgBackRest(located *Located, object *PgBackRestObject, settings *PgBackRestSettings) (*Composition, *Problem) {
	location := located.PgBackRest
	origin := located.Origin()
	number := settings.repository()
	if number < 1 || number > evidencev1alpha1.MaxPGBackRestRepository {
		return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "repository %d is not a pgBackRest repository number (1 to %d)", number, evidencev1alpha1.MaxPGBackRestRepository)
	}
	resolved := &Composition{
		Origin: origin, Format: FormatPgBackRest, Repository: number,
		PgBackRestPlugin: location.Plugin, PgBackRestKind: location.Kind, PgBackRestObject: location.ObjectName,
	}
	var whyNot *Problem
	switch {
	case object.Stanza != nil:
		whyNot = composeDalibo(resolved, origin, object.Stanza, settings)
	case object.Archive != nil:
		whyNot = composeOpera(resolved, origin, located.Cluster.Name, location.StanzaParameter, object.Archive, settings)
	default:
		whyNot = problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s was not read", origin)
	}
	if whyNot != nil {
		return nil, whyNot
	}
	return resolved, nil
}

func composeDalibo(resolved *Composition, origin Origin, stanza *pgbackrest.Stanza, settings *PgBackRestSettings) *Problem {
	configuration := &stanza.Spec.Configuration
	if moved := movingVariables(mapKeys(configuration.CustomEnvVar)); len(moved) > 0 {
		return overrideProblem(origin, "spec.stanzaConfiguration.customEnvVar sets", moved)
	}
	if whyNot := resolved.setStanzas(origin, [2]string{"spec.stanzaConfiguration.name"}, configuration.Name, ""); whyNot != nil {
		return whyNot
	}
	s3, azure := len(configuration.S3Repositories), len(configuration.AzureRepositories)
	where := fmt.Sprintf("%s repo%d", origin, resolved.Repository)
	switch {
	case s3+azure == 0:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s declares no S3 or Azure repository", origin)
	case resolved.Repository > s3+azure:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
			"%s declares %d S3 and %d Azure repositories (repo1 to repo%d, S3 first): there is no repo%d", origin, s3, azure, s3+azure, resolved.Repository)
	case resolved.Repository <= s3:
		repository := &configuration.S3Repositories[resolved.Repository-1]
		// verifyTLS is not followed: the plugin leaves verification off by
		// default, and the agent, which holds read credentials to every
		// backup, never reads a repository it cannot authenticate.
		input := pgBackRestS3{
			where: where, endpoint: repository.Endpoint, bucket: repository.Bucket, path: repository.RepoPath,
			region: repository.Region, uriStyle: repository.UriStyle,
		}
		if keys := repository.SecretRef; keys != nil {
			input.accessKeyID, input.secretAccessKey = selected(keys.AccessKeyIDReference), selected(keys.SecretAccessKeyReference)
		}
		if cipher := repository.Cipher; cipher != nil {
			if cipher.Type != "" && cipher.Type != cipherAES256CBC {
				return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s cipherConfig.type %q is not aes-256-cbc", where, cipher.Type)
			}
			if cipher.PassReference == nil {
				return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s declares cipherConfig without cipherConfig.encryptionPass", where)
			}
			input.cipherPass = selected(cipher.PassReference)
		}
		input.endpointCA = settings.endpointCA(nil)
		if whyNot := resolved.setRetention(where+" retentionPolicy", repository.RetentionPolicy); whyNot != nil {
			return whyNot
		}
		return resolved.composePgBackRestS3(input)
	default:
		repository := &configuration.AzureRepositories[resolved.Repository-s3-1]
		if whyNot := resolved.setRetention(where+" retentionPolicy", repository.RetentionPolicy); whyNot != nil {
			return whyNot
		}
		return resolved.composePgBackRestAzure(where, repository, settings.endpointCA(nil))
	}
}

func composeOpera(resolved *Composition, origin Origin, clusterName, stanzaParameter string, archive *pgbackrest.Archive, settings *PgBackRestSettings) *Problem {
	configuration := &archive.Spec.Configuration
	names := make([]string, 0, len(archive.Spec.InstanceSidecarConfiguration.Env))
	for _, variable := range archive.Spec.InstanceSidecarConfiguration.Env {
		names = append(names, variable.Name)
	}
	if moved := movingVariables(names); len(moved) > 0 {
		return overrideProblem(origin, "spec.instanceSidecarConfiguration.env sets", moved)
	}
	for _, arguments := range []struct {
		field string
		args  []string
	}{
		{"spec.configuration.wal.archiveAdditionalCommandArgs", walArgs(configuration.Wal, true)},
		{"spec.configuration.wal.restoreAdditionalCommandArgs", walArgs(configuration.Wal, false)},
		{"spec.configuration.data.additionalCommandArgs", dataArgs(configuration.Data)},
		{"spec.configuration.restore.additionalCommandArgs", restoreArgs(configuration.Restore)},
	} {
		if moved := movingArguments(arguments.args); len(moved) > 0 {
			return overrideProblem(origin, arguments.field+" passes", moved)
		}
	}
	// The backup stanza is the Cluster's stanza parameter, else its name;
	// the WAL goes to configuration.stanza when set (archive-push and
	// archive-get), else to the backup stanza (cnpg-plugin-pgbackrest
	// internal/cnpgi/operator/config and internal/pgbackrest/archiver).
	backup := stanzaParameter
	if backup == "" {
		backup = clusterName
	}
	archiveStanza := ""
	if configuration.Stanza != "" && configuration.Stanza != backup {
		archiveStanza = configuration.Stanza
	}
	if whyNot := resolved.setStanzas(origin, [2]string{"the Cluster's stanza parameter (or its name)", "spec.configuration.stanza"}, backup, archiveStanza); whyNot != nil {
		return whyNot
	}
	count := len(configuration.Repositories)
	where := fmt.Sprintf("%s repo%d", origin, resolved.Repository)
	switch {
	case count == 0:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s declares no repository", origin)
	case resolved.Repository > count:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s declares %d repositories (repo1 to repo%d): there is no repo%d", origin, count, count, resolved.Repository)
	}
	repository := &configuration.Repositories[resolved.Repository-1]
	if repository.AWS == nil {
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s declares no s3Credentials", where)
	}
	// disableVerifyTLS is not followed, as Dalibo's verifyTLS is not.
	input := pgBackRestS3{
		where: where, endpoint: repository.EndpointURL, bucket: repository.Bucket, path: repository.DestinationPath,
		region: repository.AWS.Region, uriStyle: repository.AWS.URIStyle, keyType: repository.AWS.KeyType,
		accessKeyID: selected(repository.AWS.AccessKeyIDReference), secretAccessKey: selected(repository.AWS.SecretAccessKeyReference),
		endpointCA: settings.endpointCA(selected(repository.EndpointCA)),
	}
	switch repository.Encryption {
	case "":
		// encryptionKey alone encrypts nothing: the plugin passes the
		// cipher only with encryption set.
	case cipherAES256CBC:
		if repository.EncryptionKey == nil {
			return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s declares encryption aes-256-cbc without encryptionKey", where)
		}
		input.cipherPass = selected(repository.EncryptionKey)
	default:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s encryption %q is not aes-256-cbc", where, repository.Encryption)
	}
	var retention pgbackrest.Retention
	if repository.Retention != nil {
		retention = *repository.Retention
	}
	if whyNot := resolved.setRetention(where+" retention", retention); whyNot != nil {
		return whyNot
	}
	return resolved.composePgBackRestS3(input)
}

// PgBackRestRetention is a repository's retention as the agent receives it
// (PGBACKREST_RETENTION_*): pgBackRest's repo-retention-full, -full-type,
// -diff, -archive and -archive-type. A zero count is an unset option; the
// types always carry a value, pgBackRest's default (count, full) when the
// plugin sets none, so the agent tells "repo-retention-full is not set"
// from "no expectation". repo-retention-history is not carried: it governs
// backup.history, which the agent does not read.
type PgBackRestRetention struct {
	Full        int    `json:",omitempty"`
	FullType    string `json:",omitempty"`
	Diff        int    `json:",omitempty"`
	Archive     int    `json:",omitempty"`
	ArchiveType string `json:",omitempty"`
}

// Variable is one environment variable of the agent.
type Variable struct {
	Name, Value string
}

// Variables renders the retention as the agent's environment, in a fixed
// order: both types always, each count when set.
func (r *PgBackRestRetention) Variables() []Variable {
	if r == nil {
		return nil
	}
	variables := []Variable{}
	count := func(name string, value int) {
		if value > 0 {
			variables = append(variables, Variable{Name: name, Value: strconv.Itoa(value)})
		}
	}
	count("PGBACKREST_RETENTION_FULL", r.Full)
	variables = append(variables, Variable{Name: "PGBACKREST_RETENTION_FULL_TYPE", Value: r.FullType})
	count("PGBACKREST_RETENTION_DIFF", r.Diff)
	count("PGBACKREST_RETENTION_ARCHIVE", r.Archive)
	return append(variables, Variable{Name: "PGBACKREST_RETENTION_ARCHIVE_TYPE", Value: r.ArchiveType})
}

// setRetention validates and records the repository's retention; the CRDs
// enforce the same bounds, so a refusal here means an object the plugin's
// own validation did not check.
func (c *Composition) setRetention(where string, retention pgbackrest.Retention) *Problem {
	value := &PgBackRestRetention{
		Full: int(retention.Full), FullType: retention.FullType, Diff: int(retention.Diff),
		Archive: int(retention.Archive), ArchiveType: retention.ArchiveType,
	}
	if err := evidencev1alpha1.ValidatePGBackRestRetention(value.Full, value.FullType, value.Diff, value.Archive, value.ArchiveType); err != nil {
		field := map[string]string{"full-type": "fullType", "archive-type": "archiveType"}
		var refused *evidencev1alpha1.PGBackRestRetentionError
		if !errors.As(err, &refused) {
			return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s: %v", where, err)
		}
		name := refused.Option
		if renamed, ok := field[name]; ok {
			name = renamed
		}
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s.%s %s", where, name, refused.Problem)
	}
	if value.FullType == "" {
		value.FullType = evidencev1alpha1.RetentionFullCount
	}
	if value.ArchiveType == "" {
		value.ArchiveType = evidencev1alpha1.PGBackRestFull
	}
	c.Retention = value
	return nil
}

// endpointCA is the author's own CA when set, else the plugin's.
func (s *PgBackRestSettings) endpointCA(plugin *SecretKeyRef) *SecretKeyRef {
	if s != nil && s.EndpointCA != nil {
		reference := *s.EndpointCA
		return &reference
	}
	return plugin
}

// setStanzas validates and records the stanzas: a stanza is a path segment
// of every key pgBackRest writes (backup/<stanza>/, archive/<stanza>/).
func (c *Composition) setStanzas(origin Origin, fields [2]string, backup, archive string) *Problem {
	for index, stanza := range []string{backup, archive} {
		if index == 1 && stanza == "" {
			// No split: the WAL is under the backup stanza.
			continue
		}
		if !evidencev1alpha1.ValidPGBackRestStanza(stanza) {
			return problem(pgtoolboxv1alpha1.ReasonDestinationInvalid,
				"%s: the stanza from %s is empty, longer than %d bytes, has surrounding spaces, or contains '/', '\\' or a control character",
				origin, fields[index], evidencev1alpha1.MaxPGBackRestStanzaBytes)
		}
	}
	c.Stanza, c.ArchiveStanza = backup, archive
	return nil
}

// pgBackRestS3 is one S3 repository, as either plugin declares it.
type pgBackRestS3 struct {
	where                        string
	endpoint, bucket, path       string
	region, uriStyle, keyType    string
	accessKeyID, secretAccessKey *SecretKeyRef
	cipherPass, endpointCA       *SecretKeyRef
}

// composePgBackRestS3 maps an S3 repository. The agent's destination is
// s3://<bucket>/<repo path without its leading slash>; pgBackRest writes
// under it backup/<stanza>/ and archive/<stanza>/ (repo-path is the root of
// both; the provider journey, where real pgBackRest 2.60.0 writes to
// MinIO, confirms the mapping against the files themselves).
func (c *Composition) composePgBackRestS3(input pgBackRestS3) *Problem {
	endpoint, ok := pgBackRestEndpoint(input.endpoint)
	if !ok {
		return problem(pgtoolboxv1alpha1.ReasonDestinationInvalid, "%s: the endpoint is not a host[:port] or an http(s) origin without credentials or a path", input.where)
	}
	prefix := strings.Trim(input.path, "/")
	destination := agentDestination("s3", input.bucket, prefix)
	bucket, parsedPrefix, err := evidencev1alpha1.ParseS3Destination(destination)
	if err != nil || bucket != input.bucket || parsedPrefix != prefix {
		return problem(pgtoolboxv1alpha1.ReasonDestinationInvalid, "%s: the bucket or the repository path is not a valid S3 bucket and key prefix", input.where)
	}
	switch input.uriStyle {
	case "", "host", "path":
	default:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s: uriStyle %q is neither host nor path", input.where, input.uriStyle)
	}
	c.Provider, c.AgentDestination, c.DestinationPath, c.EndpointURL, c.URIStyle = evidencev1alpha1.ProviderS3, destination, destination, endpoint, input.uriStyle
	switch input.keyType {
	case "", "shared":
		if input.accessKeyID == nil || input.secretAccessKey == nil {
			return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s names no static access key pair (accessKeyId and secretAccessKey)", input.where)
		}
		c.Credential, c.AccessKeyID, c.SecretAccessKey = CredentialStaticKeys, input.accessKeyID, input.secretAccessKey
	case "auto", "web-id":
		// pgBackRest's web-id reads the IRSA token, auto the instance
		// metadata; the agent never uses the node's metadata, so either
		// reads with the Pod's own identity (IRSA or EKS Pod Identity),
		// which spec.workloadIdentity must enable and grant read on the
		// bucket (Admit).
		c.Credential = CredentialAmbient
	default:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s: s3Credentials.keyType %q is not shared, auto or web-id", input.where, input.keyType)
	}
	switch {
	case input.region != "":
		c.Region.Value = input.region
	case endpoint == "" && c.Credential == CredentialAmbient:
		// Left to the Pod's AWS_REGION, as for barman-cloud.
	case endpoint == "":
		return problem(pgtoolboxv1alpha1.ReasonRegionRequired, "%s: AWS S3 without an endpoint needs a region", input.where)
	default:
		c.Region.Value = RegionFromEndpoint(endpoint)
	}
	c.CipherPass, c.EndpointCA = input.cipherPass, input.endpointCA
	return c.pgBackRestFingerprint(input.where, input.bucket, prefix)
}

// composePgBackRestAzure maps a Dalibo Azure repository. pgBackRest
// addresses the account host-style (https://<account>.<endpoint>/<container>)
// by default, or path-style (https://<endpoint>/<account>/<container>, an
// emulator such as Azurite); the agent reads the first only on a public or
// national Azure Blob host, which the contract's destination parser knows.
func (c *Composition) composePgBackRestAzure(where string, repository *pgbackrest.AzureRepository, endpointCA *SecretKeyRef) *Problem {
	host := repository.Endpoint
	if host == "" {
		host = "blob.core.windows.net"
	}
	origin, ok := pgBackRestEndpoint(host)
	if !ok {
		return problem(pgtoolboxv1alpha1.ReasonDestinationInvalid, "%s: the endpoint is not a host[:port] or an http(s) origin without credentials or a path", where)
	}
	parsedOrigin, _ := url.Parse(origin)
	prefix := strings.Trim(repository.RepoPath, "/")
	segments := []string{repository.Container}
	if prefix != "" {
		segments = append(segments, prefix)
	}
	var raw string
	switch repository.UriStyle {
	case "", "host":
		raw = parsedOrigin.Scheme + "://" + repository.Account + "." + parsedOrigin.Host + "/" + strings.Join(segments, "/")
	case "path":
		raw = origin + "/" + repository.Account + "/" + strings.Join(segments, "/")
	default:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s: uriStyle %q is neither host nor path", where, repository.UriStyle)
	}
	destination, err := evidencev1alpha1.ParseAzureDestination(raw)
	if err != nil || destination.Account != repository.Account || destination.Container != repository.Container || destination.Prefix != prefix {
		if repository.UriStyle != "path" && err == nil {
			return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
				"%s: the endpoint is not a public or national Azure Blob host, which the agent reads host-style only: an emulator is read with uriStyle path", where)
		}
		return problem(pgtoolboxv1alpha1.ReasonDestinationInvalid, "%s: the account, the container, the endpoint or the repository path is not a valid Azure Blob destination", where)
	}
	c.Provider, c.AzureAccount, c.EndpointURL, c.URIStyle = evidencev1alpha1.ProviderAzure, destination.Account, destination.Endpoint, repository.UriStyle
	c.AgentDestination = agentDestination("azure", destination.Container, destination.Prefix)
	c.DestinationPath = raw
	c.EndpointCA = endpointCA
	var key *SecretKeyRef
	if repository.SecretRef != nil {
		key = selected(repository.SecretRef.KeyReference)
	}
	switch repository.KeyType {
	case "", "shared":
		c.Credential = CredentialStorageKey
	case "sas":
		c.Credential = CredentialSASToken
	case "auto":
		// pgBackRest's auto is the Azure managed identity; the agent reads
		// with the Pod's (workload identity or managed identity).
		c.Credential = CredentialAmbient
	default:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s: keyType %q is not shared, sas or auto", where, repository.KeyType)
	}
	if c.Credential != CredentialAmbient {
		if key == nil {
			return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s: keyType %s names no secretRef.keyReference", where, keyTypeName(repository.KeyType))
		}
		c.AzureSecret = key
	}
	return c.pgBackRestFingerprint(where, destination.Container, destination.Prefix)
}

func keyTypeName(keyType string) string {
	if keyType == "" {
		return "shared"
	}
	return keyType
}

// pgBackRestEndpoint turns a plugin's endpoint into the agent's origin.
// pgBackRest takes a host[:port] and always speaks TLS; the plugins'
// examples also spell an https:// origin. An http:// origin is kept as
// written, so the plain-http opt-in (Admit) applies. Empty is AWS's
// default endpoint.
func pgBackRestEndpoint(raw string) (string, bool) {
	if raw == "" {
		return "", true
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", false
	}
	return parsed.Scheme + "://" + parsed.Host, true
}

// pgBackRestFingerprint completes the identity of a pgBackRest
// composition: format pgbackrest, scope kind stanza, the backup stanza,
// through the contract's canonicalization. A destination the contract
// rejects is refused.
func (c *Composition) pgBackRestFingerprint(where, bucket, prefix string) *Problem {
	fingerprint, err := evidencev1alpha1.FingerprintRepository(evidencev1alpha1.RepositoryFingerprintInput{
		Provider: c.Provider, Endpoint: c.EndpointURL, Account: c.AzureAccount, Bucket: bucket, Prefix: prefix,
		Format: FormatPgBackRest, ScopeKind: evidencev1alpha1.ScopeKindStanza, ScopeName: c.Stanza,
	})
	if err != nil {
		return problem(pgtoolboxv1alpha1.ReasonDestinationInvalid, "%s: the endpoint, the bucket, the path or the stanza is rejected by the evidence contract", where)
	}
	c.Fingerprint = fingerprint
	return nil
}

// movingOptions are the pgBackRest option names (without the leading --)
// that can move the repository pgBackRest writes, or the stanza or the
// configuration it reads: the stanza, the configuration
// options, and the deprecated backup-* names, which pgBackRest still
// accepts as aliases of the repo-host options (src/config/parse.auto.c.inc,
// parseRuleOptionDeprecate). Every repo* option (indexed or not, and the
// --repo selector) is a family of its own, matched by prefix.
var movingOptions = []string{
	"stanza", "config", "config-path", "config-include-path",
	"backup-cmd", "backup-config", "backup-host", "backup-ssh-port", "backup-user",
}

// movingOption reports whether a pgBackRest option name (lower case, no
// leading --, no negate or reset prefix) can move the repository.
// pgBackRest resolves a command-line option it does not know exactly to
// the single option it is a prefix of (cfgParseOption, prefixMatch), so a
// name that abbreviates a moving option moves too; an ambiguous
// abbreviation is an error for pgBackRest, so refusing it costs nothing.
func movingOption(name string) bool {
	if name == "" {
		return false
	}
	// name is repo-something, or an abbreviation of repo itself.
	if strings.HasPrefix(name, "repo") || strings.HasPrefix("repo", name) { //nolint:gocritic // both orders are meant
		return true
	}
	for _, option := range movingOptions {
		if strings.HasPrefix(option, name) || strings.HasPrefix(name, option+"-") || name == option {
			return true
		}
	}
	return false
}

// movingVariables are the environment variable names that can move the
// repository pgBackRest writes, or the stanza or the configuration it
// reads, sorted and unique. pgBackRest maps PGBACKREST_<OPTION> to the
// option by lower-casing and turning _ into -, with no abbreviation, and
// ignores (with a warning) a negate or reset form in the environment
// (src/config/parse.c, cfgParseOptionList's environment pass), so those are
// not refused here. Every other override (log level, process-max) leaves
// the files where the object says.
func movingVariables(names []string) []string {
	var moved []string
	for _, name := range names {
		upper := strings.ToUpper(strings.TrimSpace(name))
		option, ok := strings.CutPrefix(upper, "PGBACKREST_")
		if !ok {
			continue
		}
		option = strings.ReplaceAll(strings.ToLower(option), "_", "-")
		if strings.HasPrefix(option, "repo") || slices.Contains(movingOptions, option) {
			moved = append(moved, name)
		}
	}
	slices.Sort(moved)
	return slices.Compact(moved)
}

// movingArguments are the command-line arguments that can do the same:
// every --repo* (indexed options and the --repo selector), --stanza,
// --config*, the deprecated --backup-* aliases of the repo-host options,
// their --no- and --reset- forms (pgBackRest strips one of those prefixes
// before the lookup: --reset-repo1-path puts the default path back), and
// any abbreviation of them. Only the option name is kept, never its value.
func movingArguments(arguments []string) []string {
	var moved []string
	for _, argument := range arguments {
		option, _, _ := strings.Cut(strings.TrimSpace(argument), "=")
		name, ok := strings.CutPrefix(strings.ToLower(option), "--")
		if !ok {
			continue
		}
		for _, prefix := range []string{"no-", "reset-"} {
			name = strings.TrimPrefix(name, prefix)
		}
		if movingOption(name) {
			moved = append(moved, option)
		}
	}
	slices.Sort(moved)
	return slices.Compact(moved)
}

func overrideProblem(origin Origin, field string, names []string) *Problem {
	return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
		"%s %s %s, which can move the repository, the stanza or the configuration pgBackRest uses: the agent cannot follow it, remove it",
		origin, field, strings.Join(names, ", "))
}

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func walArgs(configuration *pgbackrest.WalBackupConfiguration, archive bool) []string {
	switch {
	case configuration == nil:
		return nil
	case archive:
		return configuration.ArchiveAdditionalCommandArgs
	default:
		return configuration.RestoreAdditionalCommandArgs
	}
}

func dataArgs(configuration *pgbackrest.DataBackupConfiguration) []string {
	if configuration == nil {
		return nil
	}
	return configuration.AdditionalCommandArgs
}

func restoreArgs(configuration *pgbackrest.DataRestoreConfiguration) []string {
	if configuration == nil {
		return nil
	}
	return configuration.AdditionalCommandArgs
}

func selected(selector *pgbackrest.SecretKeySelector) *SecretKeyRef {
	if selector == nil {
		return nil
	}
	return &SecretKeyRef{Name: selector.Name, Key: selector.Key}
}
