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
	"net"
	"net/url"
	"slices"
	"strings"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Policy is what the agent's author allowed (the EvidenceAgent spec, or the
// pgtools chart's evidence.agents entry). The Barman configuration is
// written by whoever may edit the Cluster or the ObjectStore, a wider set
// than the agent's author: "only named ones" (as for pgAdmin) means the
// live configuration is checked against these choices before anything is
// read or rendered, so a Cluster edit cannot point the agent at another
// Secret, a clear-text endpoint or the node's cloud identity.
type Policy struct {
	// AllowedSecrets are the Secrets the author named. Unlisted says the
	// author named none (an EvidenceAgent without spec.allowedSecrets).
	AllowedSecrets []string
	Unlisted       bool
	// AllowInsecureEndpoint admits a plain-http endpoint, over which the
	// credentials cross the network in clear text.
	AllowInsecureEndpoint bool
	// WorkloadIdentity admits a configuration that uses the Pod's own cloud
	// identity (inheritFromIAMRole, inheritFromAzureAD,
	// useDefaultAzureCredentials, gkeEnvironment).
	WorkloadIdentity bool
	// PgBackRestEndpoint pins the endpoint of a pgBackRest repository
	// (spec.pgbackrest.endpoint, pgtools evidence.agents[].pgbackrest.endpoint):
	// a host[:port] the endpoint the agent sends its requests to must match.
	// The Stanza or the Archive is written by whoever may edit it, and
	// whoever chooses its endpoint receives the agent's credentials; the pin
	// keeps that choice with the agent's author. Empty: no pin.
	PgBackRestEndpoint string
}

// unlistedObjectStore is an EvidenceAgent without spec.allowedSecrets on a
// plugin ObjectStore: every Secret the ObjectStore names is admitted (a
// deprecated, permissive profile). Steering it needs ObjectStore write;
// the in-tree configuration (Cluster write) never gets it.
func (p Policy) unlistedObjectStore(origin Origin) bool {
	return p.Unlisted && origin.ObjectStore != ""
}

// Admit checks the composition against the author's choices. notAllowed
// lists the referenced Secrets outside AllowedSecrets (the caller names
// them in its own words); a Problem is any other refusal. Neither names a
// value.
func (c *Composition) Admit(policy Policy) (notAllowed []string, whyNot *Problem) {
	if whyNot := c.sharedCredentialSlot(); whyNot != nil {
		return nil, whyNot
	}
	switch {
	case policy.unlistedObjectStore(c.Origin):
	case policy.Unlisted:
		if referenced := c.ReferencedSecrets(); len(referenced) > 0 {
			return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
				"%s names Secret %s, which only an EvidenceAgent's spec.allowedSecrets may admit: list it there", c.Origin, strings.Join(referenced, ", "))
		}
	default:
		for _, name := range c.ReferencedSecrets() {
			if !slices.Contains(policy.AllowedSecrets, name) {
				notAllowed = append(notAllowed, name)
			}
		}
		if len(notAllowed) > 0 {
			return notAllowed, nil
		}
	}
	if c.Credential == CredentialAmbient && !policy.WorkloadIdentity {
		return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
			"%s uses the Pod's cloud identity, which the agent's author has not enabled: "+
				"set spec.workloadIdentity (pgtools: evidence.agents[].workloadIdentity)", c.Origin)
	}
	if c.insecureEndpoint() && !policy.AllowInsecureEndpoint && !policy.unlistedObjectStore(c.Origin) {
		return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
			"%s names a plain-http endpoint (credentials in clear text): "+
				"use https, or set spec.allowInsecureEndpoint (pgtools: evidence.agents[].allowInsecureEndpoint)", c.Origin)
	}
	pinned := c.Format == FormatPgBackRest && policy.PgBackRestEndpoint != ""
	if pinned {
		if whyNot := c.matchEndpointPin(policy.PgBackRestEndpoint); whyNot != nil {
			return nil, whyNot
		}
	}
	if c.azureKeyOffCloud() && !pinned {
		return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
			"%s sends an Azure storage key or SAS token to %s, which is not an Azure Blob host: "+
				"pin that endpoint in spec.pgbackrest.endpoint (pgtools: evidence.agents[].pgbackrest.endpoint) to read an emulator", c.Origin, c.requestHost())
	}
	return nil, nil
}

// requestHost is the host[:port] the agent sends its requests to: the S3
// endpoint (the agent may prefix the bucket to it, host-style), or the
// Azure account's Blob service host (<account>.<endpoint> host-style, the
// endpoint itself path-style). Empty for S3 without an endpoint (AWS).
func (c *Composition) requestHost() string {
	origin := c.EndpointURL
	if c.Provider == evidencev1alpha1.ProviderAzure {
		origin = evidencev1alpha1.AzureServiceURL(c.EndpointURL, c.AzureAccount)
	}
	if origin == "" {
		return ""
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// matchEndpointPin refuses a repository whose request host is not pin. The
// host compares without case; a pin without a port matches the scheme's
// default port only.
func (c *Composition) matchEndpointPin(pin string) *Problem {
	pinHost, pinPort, ok := ParseEndpointPin(pin)
	if !ok {
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore, "the pgBackRest endpoint pin is not a host[:port]")
	}
	refused := func(what string) *Problem {
		return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
			"%s: the repository's endpoint %s is not the pinned endpoint %s (spec.pgbackrest.endpoint; pgtools: evidence.agents[].pgbackrest.endpoint): "+
				"the agent does not send its credentials there", c.Origin, what, pin)
	}
	host := c.requestHost()
	if host == "" {
		return refused("(AWS's default, the plugin's object names none)")
	}
	parsed, err := url.Parse("//" + host)
	if err != nil {
		return refused(host)
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
		if c.insecureEndpoint() {
			port = "80"
		}
	}
	if pinPort == "" {
		pinPort = "443"
		if c.insecureEndpoint() {
			pinPort = "80"
		}
	}
	if !strings.EqualFold(parsed.Hostname(), pinHost) || port != pinPort {
		return refused(host)
	}
	return nil
}

// ParseEndpointPin splits a host[:port] endpoint pin: a DNS name, an IPv4
// address or a bracketed IPv6 address, and an optional port from 1 to
// 65535. The host is lower-cased; the port is empty when the pin has none.
func ParseEndpointPin(pin string) (host, port string, ok bool) {
	if pin == "" || len(pin) > 261 || strings.HasSuffix(pin, ":") || strings.ContainsAny(pin, "/?#@%\\ ") {
		return "", "", false
	}
	parsed, err := url.Parse("https://" + pin)
	if err != nil || parsed.Host != pin || parsed.Hostname() == "" {
		return "", "", false
	}
	host, port = strings.ToLower(parsed.Hostname()), parsed.Port()
	if strings.HasPrefix(parsed.Host, "[") {
		if net.ParseIP(host) == nil {
			return "", "", false
		}
	} else if !validPinName(host) {
		return "", "", false
	}
	if port != "" {
		number := 0
		for _, digit := range port {
			number = number*10 + int(digit-'0')
			if number > 65535 {
				return "", "", false
			}
		}
		if number == 0 || len(port) > 5 {
			return "", "", false
		}
	}
	return host, port, true
}

// validPinName accepts a DNS name (labels of letters, digits and inner
// hyphens) or an IPv4 address.
func validPinName(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-') {
				return false
			}
		}
	}
	return true
}

// azureKeyOffCloud: an Azure storage key or SAS token would go to a host
// outside the public and national Azure Blob domains (an emulator,
// path-style). A shared-key signature or a SAS token is replayable by
// whoever receives it, so only the agent's author can send one there.
func (c *Composition) azureKeyOffCloud() bool {
	if c.Provider != evidencev1alpha1.ProviderAzure || (c.Credential != CredentialStorageKey && c.Credential != CredentialSASToken) {
		return false
	}
	return evidencev1alpha1.AzureDestination{Endpoint: c.EndpointURL, Account: c.AzureAccount}.Emulator()
}

// insecureEndpoint: the S3 endpoint or the Azure emulator origin is http.
func (c *Composition) insecureEndpoint() bool {
	if c.EndpointURL == "" {
		return false
	}
	parsed, err := url.Parse(c.EndpointURL)
	return err != nil || !strings.EqualFold(parsed.Scheme, "https")
}

// sharedCredentialSlot refuses two credential settings that name the same
// Secret key: the agent sends the access key, the session token and the
// region in clear (in the request's signature header), and the SSE-C key
// in a request header, and keeps only the secret key to itself, so an
// access key that names the secret key's Secret key would publish it to
// the endpoint.
func (c *Composition) sharedCredentialSlot() *Problem {
	slots := []struct {
		name      string
		reference *SecretKeyRef
	}{
		{"accessKeyId", c.AccessKeyID},
		{"secretAccessKey", c.SecretAccessKey},
		{"sessionToken", c.SessionToken},
		{"region", c.Region.SecretRef},
		{"sseCustomerKey", c.SSECustomerKey},
		{"endpointCA", c.EndpointCA},
		{"azureCredentials", c.AzureSecret},
		{"cipherPass", c.CipherPass},
	}
	for i := range slots {
		for j := i + 1; j < len(slots); j++ {
			a, b := slots[i].reference, slots[j].reference
			if a != nil && b != nil && *a == *b {
				return problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
					"%s names Secret %s key %s for both %s and %s", c.Origin, a.Name, a.Key, slots[i].name, slots[j].name)
			}
		}
	}
	return nil
}
