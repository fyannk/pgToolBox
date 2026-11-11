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

package evidenceagent

import (
	"context"
	"fmt"
	"strings"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/backupresolve"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// The resolution itself (Cluster → plugin stanza → ObjectStore → credential
// references) lives in internal/backupresolve, shared with the agent's own
// AGENT_RESOLVE=cluster mode, so both install paths read the same shapes.
// These names keep the builders and the tests of this package unchanged.
type (
	secretKeyRef = backupresolve.SecretKeyRef
	regionSource = backupresolve.RegionSource
	composition  = backupresolve.Composition
)

const (
	barmanPluginName      = backupresolve.BarmanPluginName
	barmanObjectParameter = backupresolve.BarmanObjectParameter
	barmanServerParameter = backupresolve.BarmanServerParameter
)

// configurationProblem is a cross-object check the CRD cannot express:
// ConfigurationValid=False with a catalogue reason. An expected outcome,
// not an error: it is published, not retried.
type configurationProblem struct {
	reason  pgtoolboxv1alpha1.Reason
	message string
}

func problem(reason pgtoolboxv1alpha1.Reason, format string, args ...any) *configurationProblem {
	return &configurationProblem{reason: reason, message: fmt.Sprintf(format, args...)}
}

func fromResolver(p *backupresolve.Problem) *configurationProblem {
	if p == nil {
		return nil
	}
	return &configurationProblem{reason: p.Reason, message: p.Message}
}

// resolve walks Cluster → plugin stanza → ObjectStore (or the in-tree
// spec.backup.barmanObjectStore, or a pgBackRest plugin's Stanza or
// Archive) → credential references and computes the repository
// fingerprint. Foreign objects are read through the APIReader (their
// watches are metadata-only), so neither a Cluster spec nor an ObjectStore,
// a Stanza or an Archive is ever held in the operator's cache.
func (r *Reconciler) resolve(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent) (*composition, *configurationProblem, error) {
	request := backupresolve.Request{
		Namespace: agent.Namespace, ClusterName: agent.Spec.ClusterRef.Name, ServerName: agent.Spec.ServerName,
		PgBackRest: pgBackRestSettings(agent.Spec.PgBackRest),
	}
	if agent.Spec.ObjectStoreRef != nil {
		request.ObjectStoreName = agent.Spec.ObjectStoreRef.Name
	}
	located, whyNot, err := backupresolve.Locate(ctx, r.APIReader, request)
	if err != nil {
		return nil, nil, err
	}
	if located != nil {
		// Recorded before the ObjectStore, Stanza or Archive is read, so
		// its watch can find this agent when it appears later.
		agent.Status.ObjectStoreName = located.ObjectStoreName
		agent.Status.PgBackRest = nil
		if location := located.PgBackRest; location != nil {
			agent.Status.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestStatus{
				Plugin: location.Plugin, Kind: location.Kind, ObjectName: location.ObjectName,
			}
		}
	}
	if whyNot != nil {
		return nil, fromResolver(whyNot), nil
	}
	var resolved *composition
	if located.PgBackRest != nil {
		resolved, whyNot, err = r.composePgBackRest(ctx, located, request.PgBackRest)
	} else {
		resolved, whyNot, err = r.composeBarman(ctx, located)
	}
	if err != nil || whyNot != nil {
		return nil, fromResolver(whyNot), err
	}
	resolved.ClusterName = located.Cluster.Name
	resolved.ClusterUID = string(located.Cluster.UID)
	notAllowed, whyNot := resolved.Admit(policyOf(agent))
	if whyNot != nil {
		return nil, fromResolver(whyNot), nil
	}
	if len(notAllowed) > 0 {
		return nil, problem(pgtoolboxv1alpha1.ReasonUnsupportedObjectStore,
			"%s names Secret %s, which spec.allowedSecrets does not list: the agent reads no other Secret",
			resolved.Origin, strings.Join(notAllowed, ", ")), nil
	}
	return resolved, nil, nil
}

// composeBarman reads the Barman configuration (in-tree, or the
// ObjectStore when its API is usable) and composes it.
func (r *Reconciler) composeBarman(ctx context.Context, located *backupresolve.Located) (*composition, *backupresolve.Problem, error) {
	// Each namespace's RBAC decides there alone: the probe's answer in the
	// Cluster's namespace, not the answer anywhere.
	if probe := r.ObjectStoreAPI.In(located.Cluster.Namespace); located.InTree == nil && !probe.Usable() {
		return nil, unavailable(probe, "the barmancloud.cnpg.io/v1 ObjectStore API"), nil
	}
	configuration, whyNot, err := backupresolve.Configuration(ctx, r.APIReader, located)
	if err != nil || whyNot != nil {
		return nil, whyNot, err
	}
	resolved, whyNot := backupresolve.Compose(located.Origin(), located.ServerName, configuration)
	return resolved, whyNot, nil
}

// composePgBackRest reads the plugin's Stanza or Archive (when its API is
// usable) and composes the selected repository.
func (r *Reconciler) composePgBackRest(ctx context.Context, located *backupresolve.Located, settings *backupresolve.PgBackRestSettings) (*composition, *backupresolve.Problem, error) {
	probe, api := r.ArchiveAPI, "the pgbackrest.cnpg.opera.com/v1 Archive API"
	if located.PgBackRest.Kind == backupresolve.PgBackRestKindStanza {
		probe, api = r.StanzaAPI, "the pgbackrest.dalibo.com/v1 Stanza API"
	}
	if probe = probe.In(located.Cluster.Namespace); !probe.Usable() {
		return nil, unavailable(probe, api), nil
	}
	object, whyNot, err := backupresolve.ReadPgBackRest(ctx, r.APIReader, located.Cluster.Namespace, located.PgBackRest)
	if err != nil || whyNot != nil {
		return nil, whyNot, err
	}
	resolved, whyNot := backupresolve.ComposePgBackRest(located, object, settings)
	return resolved, whyNot, nil
}

// unavailable is the Problem of an optional API the start-time probe
// turned off.
func unavailable(probe discovery.Result, api string) *backupresolve.Problem {
	why := probe.Reason
	if why == "" {
		why = "not probed"
	}
	return &backupresolve.Problem{
		Reason:  pgtoolboxv1alpha1.ReasonObjectStoreAPIUnavailable,
		Message: fmt.Sprintf("%s is unavailable to the operator (%s)", api, why),
	}
}

// pgBackRestSettings are spec.pgbackrest in the resolver's terms.
func pgBackRestSettings(spec *pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec) *backupresolve.PgBackRestSettings {
	if spec == nil {
		return nil
	}
	settings := &backupresolve.PgBackRestSettings{Repository: int(spec.Repository)}
	if ca := spec.EndpointCA; ca != nil {
		settings.EndpointCA = &backupresolve.SecretKeyRef{Name: ca.Name, Key: ca.Key}
	}
	return settings
}

// policyOf is what the agent's author allowed: the live Barman
// configuration (Cluster or ObjectStore, edited by others) is checked
// against it before anything is rendered.
func policyOf(agent *pgtoolboxv1alpha1.EvidenceAgent) backupresolve.Policy {
	policy := backupresolve.Policy{
		AllowedSecrets: agent.Spec.AllowedSecrets, Unlisted: len(agent.Spec.AllowedSecrets) == 0,
		AllowInsecureEndpoint: agent.Spec.AllowInsecureEndpoint, WorkloadIdentity: agent.Spec.WorkloadIdentity != nil,
	}
	if agent.Spec.PgBackRest != nil {
		policy.PgBackRestEndpoint = agent.Spec.PgBackRest.Endpoint
	}
	return policy
}
