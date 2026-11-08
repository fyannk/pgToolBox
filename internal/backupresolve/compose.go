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
	"net/url"
	"slices"

	barmanapi "github.com/cloudnative-pg/barman-cloud/pkg/api"
	machineryapi "github.com/cloudnative-pg/machinery/pkg/api"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// RegionSource is where the S3 signing region comes from: the
// configuration's region Secret reference or a value derived from an AWS
// endpoint hostname; the zero value leaves it to the agent's default.
type RegionSource struct {
	SecretRef *SecretKeyRef
	Value     string
}

// Credential is how the agent authenticates to the object store.
type Credential string

const (
	// CredentialStaticKeys is an S3 access key pair (and optional session
	// token) from Secrets.
	CredentialStaticKeys Credential = "static-keys"
	// CredentialConnectionString is an Azure connection string from a Secret.
	CredentialConnectionString Credential = "connection-string"
	// CredentialStorageKey is an Azure storage account key from a Secret.
	CredentialStorageKey Credential = "storage-key"
	// CredentialSASToken is an Azure shared access signature from a Secret.
	CredentialSASToken Credential = "sas-token"
	// CredentialJSONKey is a Google service account JSON key from a Secret.
	CredentialJSONKey Credential = "json-key"
	// CredentialAmbient is the Pod's own cloud identity (workload identity:
	// IRSA or EKS Pod Identity, Azure workload identity or managed
	// identity, GKE workload identity), through the SDK's default chain;
	// no Secret.
	CredentialAmbient Credential = "ambient"
)

// Composition is everything the agent needs to read one repository,
// resolved from the Cluster and its Barman configuration. Plain values:
// only this package sees the Barman types. It holds Secret references,
// never values.
type Composition struct {
	ClusterName string
	ClusterUID  string
	// Format is FormatBarmanCloud or FormatPgBackRest.
	Format string
	// Origin is where the Barman configuration was read; its ObjectStore
	// is empty for the in-tree spec.backup.barmanObjectStore.
	Origin          Origin
	ObjectStoreName string
	ServerName      string
	// Provider is evidencev1alpha1.ProviderS3, ProviderAzure or ProviderGCS.
	Provider string
	// DestinationPath is the Barman destinationPath as configured.
	DestinationPath string
	// AgentDestination is the same destination in the agent's grammar
	// (DESTINATION_PATH): s3://bucket/prefix, azure://container/prefix or
	// gs://bucket/prefix.
	AgentDestination string
	// EndpointURL is the S3-compatible origin, or the Azure emulator origin
	// taken from the destination; empty otherwise.
	EndpointURL string
	// AzureAccount is the Azure storage account, from the destination.
	AzureAccount string
	EndpointCA   *SecretKeyRef
	Credential   Credential
	// S3 static keys (CredentialStaticKeys).
	AccessKeyID     *SecretKeyRef
	SecretAccessKey *SecretKeyRef
	SessionToken    *SecretKeyRef
	Region          RegionSource
	// SSECustomerKey is the S3 server-side encryption key with customer
	// keys (SSE-C, s3Credentials.sseCustomerKey): a base64-encoded 256-bit
	// AES key that every object read must send, whatever the credential.
	SSECustomerKey *SecretKeyRef
	// AzureSecret is the connection string, storage key or SAS token,
	// as Credential says.
	AzureSecret *SecretKeyRef
	// GoogleCredentials is the service account JSON key (CredentialJSONKey).
	GoogleCredentials *SecretKeyRef
	// Fingerprint is the repository identity (the contract's
	// FingerprintRepository of the format's profile).
	Fingerprint string

	// pgBackRest (Format FormatPgBackRest): the plugin and its object, the
	// backup stanza, the WAL stanza when it differs (Opera's split
	// stanza), the repository number (repo<n>), the URI style as the
	// plugin declares it, and the repository cipher pass.
	PgBackRestPlugin string `json:",omitempty"`
	PgBackRestKind   string `json:",omitempty"`
	PgBackRestObject string `json:",omitempty"`
	Stanza           string `json:",omitempty"`
	ArchiveStanza    string `json:",omitempty"`
	Repository       int    `json:",omitempty"`
	URIStyle         string `json:",omitempty"`
	CipherPass       *SecretKeyRef
	// Retention is the selected repository's retention, which the agent
	// compares the stanza with; always set for pgBackRest.
	Retention *PgBackRestRetention `json:",omitempty"`
}

// ReferencedSecrets lists the Secret names the composition reads, sorted
// and unique.
func (c *Composition) ReferencedSecrets() []string {
	var names []string
	for _, reference := range []*SecretKeyRef{
		c.AccessKeyID, c.SecretAccessKey, c.SessionToken, c.SSECustomerKey, c.EndpointCA, c.Region.SecretRef, c.AzureSecret, c.GoogleCredentials,
		c.CipherPass,
	} {
		if reference != nil {
			names = append(names, reference.Name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func reference(selector *machineryapi.SecretKeySelector) *SecretKeyRef {
	if selector == nil {
		return nil
	}
	return &SecretKeyRef{Name: selector.Name, Key: selector.Key}
}

// Compose maps the Barman configuration onto the agent's profile: one
// server of a barman-cloud repository on S3, Azure Blob Storage or Google
// Cloud Storage. Every shape outside it is a named Problem; nothing
// defaults to "probably fine". Both install paths (the operator and the
// self-resolving agent) read exactly what this returns.
func Compose(origin Origin, serverName string, configuration *barmanapi.BarmanObjectStoreConfiguration) (*Composition, *Problem) {
	providers := 0
	for _, set := range []bool{configuration.AWS != nil, configuration.Azure != nil, configuration.Google != nil} {
		if set {
			providers++
		}
	}
	switch {
	case providers == 0:
		return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s declares no s3Credentials, azureCredentials or googleCredentials", origin)
	case providers > 1:
		return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s declares credentials for more than one provider", origin)
	}
	resolved := &Composition{
		Format: FormatBarmanCloud, Origin: origin, ObjectStoreName: origin.ObjectStore, ServerName: serverName,
		DestinationPath: configuration.DestinationPath,
	}
	var whyNot *Problem
	switch {
	case configuration.AWS != nil:
		whyNot = composeS3(resolved, origin, configuration)
	case configuration.Azure != nil:
		whyNot = composeAzure(resolved, origin, configuration)
	default:
		whyNot = composeGCS(resolved, origin, configuration)
	}
	if whyNot != nil {
		return nil, whyNot
	}
	return resolved, nil
}

// fingerprint completes the composition's identity, or names why the
// contract rejects it.
func (c *Composition) fingerprint(origin Origin, bucket, prefix string) *Problem {
	fingerprint, err := evidencev1alpha1.FingerprintRepository(evidencev1alpha1.RepositoryFingerprintInput{
		Provider: c.Provider, Endpoint: c.EndpointURL, Account: c.AzureAccount, Bucket: bucket, Prefix: prefix,
		Format: FormatBarmanCloud, ScopeKind: evidencev1alpha1.ScopeKindBarmanServer, ScopeName: c.ServerName,
	})
	if err != nil {
		return problem(pgtoolboxv1alpha1.ReasonDestinationInvalid, "%s: the endpoint or the server name %q is rejected by the evidence contract", origin, c.ServerName)
	}
	c.Fingerprint = fingerprint
	return nil
}

func composeS3(resolved *Composition, origin Origin, configuration *barmanapi.BarmanObjectStoreConfiguration) *Problem {
	aws := configuration.AWS
	if !aws.InheritFromIAMRole && (aws.AccessKeyIDReference == nil || aws.SecretAccessKeyReference == nil) {
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s names no static access key pair", origin)
	}
	// The parse error is fixed and value-free: a destinationPath may carry
	// userinfo or a presigned query, which must not reach status.
	bucket, prefix, err := evidencev1alpha1.ParseS3Destination(configuration.DestinationPath)
	if err != nil {
		return problem(pgtoolboxv1alpha1.ReasonDestinationInvalid, "%s: destinationPath is not a credential-free s3://bucket/prefix URI", origin)
	}
	resolved.Provider = evidencev1alpha1.ProviderS3
	resolved.AgentDestination = configuration.DestinationPath
	resolved.EndpointURL = configuration.EndpointURL
	if whyNot := resolved.fingerprint(origin, bucket, prefix); whyNot != nil {
		return whyNot
	}
	if aws.InheritFromIAMRole {
		// The Pod's own AWS identity (IRSA or EKS Pod Identity): the SDK's
		// default chain, with the variables the identity webhook injects.
		resolved.Credential = CredentialAmbient
	} else {
		resolved.Credential = CredentialStaticKeys
		resolved.AccessKeyID, resolved.SecretAccessKey = reference(aws.AccessKeyIDReference), reference(aws.SecretAccessKeyReference)
		resolved.SessionToken = reference(aws.SessionToken)
	}
	resolved.EndpointCA = reference(configuration.EndpointCA)
	// SSE-C is independent of the credential, as barman-cloud has it: the
	// objects were written with this key, and barman-cloud sends it on
	// every GET and HEAD (never on a list), so the agent does the same.
	resolved.SSECustomerKey = reference(aws.SSECustomerKey)
	switch {
	case aws.RegionReference != nil:
		resolved.Region.SecretRef = reference(aws.RegionReference)
	case configuration.EndpointURL == "" && aws.InheritFromIAMRole:
		// Left to the Pod's AWS_REGION, which the EKS identity webhook
		// injects (as barman-cloud's boto3 relies on); the agent names
		// AWS_REGION if it is missing.
	case configuration.EndpointURL == "":
		return problem(pgtoolboxv1alpha1.ReasonRegionRequired, "%s: AWS S3 without endpointURL needs s3Credentials.region", origin)
	default:
		resolved.Region.Value = RegionFromEndpoint(configuration.EndpointURL)
	}
	return nil
}

// composeAzure: Barman addresses the account from the destination (the
// public or a national cloud's host, or an emulator's first path segment),
// so the
// storageAccount Secret is not read; the key or SAS token must be that
// account's. endpointURL is not used for Azure by barman-cloud and is not
// read here either. endpointCA is: barman-cloud trusts it for Azure through
// REQUESTS_CA_BUNDLE (Azurite over TLS, Azure Stack), and so does the agent.
func composeAzure(resolved *Composition, origin Origin, configuration *barmanapi.BarmanObjectStoreConfiguration) *Problem {
	azure := configuration.Azure
	destination, err := evidencev1alpha1.ParseAzureDestination(configuration.DestinationPath)
	if err != nil {
		return problem(pgtoolboxv1alpha1.ReasonDestinationInvalid,
			"%s: destinationPath is not a credential-free https://<account>.blob.core.windows.net/<container>/<prefix> (or emulator http(s)://<host>/<account>/<container>/<prefix>) URL", origin)
	}
	resolved.Provider = evidencev1alpha1.ProviderAzure
	resolved.AzureAccount, resolved.EndpointURL = destination.Account, destination.Endpoint
	resolved.EndpointCA = reference(configuration.EndpointCA)
	resolved.AgentDestination = agentDestination("azure", destination.Container, destination.Prefix)
	if whyNot := resolved.fingerprint(origin, destination.Container, destination.Prefix); whyNot != nil {
		return whyNot
	}
	switch {
	case azure.ConnectionString != nil:
		resolved.Credential, resolved.AzureSecret = CredentialConnectionString, reference(azure.ConnectionString)
	case destination.Emulator():
		// Barman reaches an emulator only through a connection string.
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s: an Azure emulator destination needs azureCredentials.connectionString", origin)
	case azure.StorageKey != nil:
		resolved.Credential, resolved.AzureSecret = CredentialStorageKey, reference(azure.StorageKey)
	case azure.StorageSasToken != nil:
		resolved.Credential, resolved.AzureSecret = CredentialSASToken, reference(azure.StorageSasToken)
	case azure.InheritFromAzureAD || azure.UseDefaultAzureCredentials:
		// The Pod's Azure identity (workload identity or managed identity)
		// through DefaultAzureCredential.
		resolved.Credential = CredentialAmbient
	default:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s names no Azure connection string, storage key or SAS token", origin)
	}
	return nil
}

// composeGCS: barman-cloud reaches Google Cloud Storage at its public
// endpoint; endpointURL and endpointCA do not apply. barman ignores
// --endpoint-url for google-cloud-storage, and neither barman-cloud nor
// CloudNativePG hands a CA to it (AWS_CA_BUNDLE is S3's,
// REQUESTS_CA_BUNDLE Azure's). A configuration that names an endpointCA is
// therefore read as barman-cloud reads it: at the public endpoint with the
// system roots, and the CA Secret is neither referenced nor read (so it
// need not be allow-listed).
func composeGCS(resolved *Composition, origin Origin, configuration *barmanapi.BarmanObjectStoreConfiguration) *Problem {
	google := configuration.Google
	bucket, prefix, err := evidencev1alpha1.ParseGCSDestination(configuration.DestinationPath)
	if err != nil {
		return problem(pgtoolboxv1alpha1.ReasonDestinationInvalid, "%s: destinationPath is not a credential-free gs://bucket/prefix URI", origin)
	}
	resolved.Provider = evidencev1alpha1.ProviderGCS
	resolved.AgentDestination = agentDestination("gs", bucket, prefix)
	if whyNot := resolved.fingerprint(origin, bucket, prefix); whyNot != nil {
		return whyNot
	}
	switch {
	case google.ApplicationCredentials != nil:
		resolved.Credential, resolved.GoogleCredentials = CredentialJSONKey, reference(google.ApplicationCredentials)
	case google.GKEEnvironment:
		// GKE workload identity: Application Default Credentials.
		resolved.Credential = CredentialAmbient
	default:
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "%s names no applicationCredentials", origin)
	}
	return nil
}

// agentDestination spells a parsed destination in the agent's grammar; the
// agent parses the prefix back to the same decoded value.
func agentDestination(scheme, bucket, prefix string) string {
	destination := url.URL{Scheme: scheme, Host: bucket}
	if prefix != "" {
		destination.Path = "/" + prefix
	}
	return destination.String()
}
