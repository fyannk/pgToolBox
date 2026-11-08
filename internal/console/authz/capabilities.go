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

package authz

import (
	"context"
	"errors"
)

// API groups and resources the console asks about.
const (
	CNPGGroup      = "postgresql.cnpg.io"
	PgToolBoxGroup = "pgtoolbox.fyannk.dev"
)

// Capabilities are the derived capabilities for one Cluster, each one
// API-server question:
//
//	View       = get clusters/<name>
//	Logs       = get pods/log in the namespace
//	Operations = patch clusters/<name> ∧ patch clusters/<name>/status ∧ create backups
//
// A capability whose review could not be completed is false and listed
// in Undecided, so the UI can say "could not check" instead of hiding a
// button silently.
type Capabilities struct {
	View       bool `json:"view" doc:"May get the Cluster (the console shows nothing about a Cluster without it)."`
	Logs       bool `json:"logs" doc:"May read pod logs in the Cluster's namespace."`
	Operations bool `json:"operations" doc:"May patch the Cluster and its status and create Backups: promote, restart, reload, backup."`
	// The per-operation answers behind Operations, so a caller granted
	// only part of the verbs sees exactly what they may do.
	Backup    bool     `json:"backup" doc:"May create Backups in the namespace."`
	Reload    bool     `json:"reload" doc:"May patch the Cluster (annotation cnpg.io/reloadedAt)."`
	Restart   bool     `json:"restart" doc:"May patch the Cluster (annotation kubectl.kubernetes.io/restartedAt)."`
	Promote   bool     `json:"promote" doc:"May patch the Cluster's status subresource."`
	Undecided []string `json:"undecided,omitempty" doc:"Capabilities the API server could not be asked about; they are false until it answers." enum:"view,logs,operations,backup,reload,restart,promote"`
}

// ClusterView is the question that decides whether a Cluster is shown.
func ClusterView(namespace, name string) Attributes {
	return Attributes{Namespace: namespace, Verb: "get", Group: CNPGGroup, Resource: "clusters", Name: name}
}

// ClusterCapabilities asks the five questions behind Capabilities in
// parallel.
func (a *Authorizer) ClusterCapabilities(ctx context.Context, u User, namespace, name string) Capabilities {
	ds := a.Check(ctx, u, []Attributes{
		ClusterView(namespace, name),
		{Namespace: namespace, Verb: "get", Resource: "pods", Subresource: "log"},
		{Namespace: namespace, Verb: "patch", Group: CNPGGroup, Resource: "clusters", Name: name},
		{Namespace: namespace, Verb: "patch", Group: CNPGGroup, Resource: "clusters", Subresource: "status", Name: name},
		{Namespace: namespace, Verb: "create", Group: CNPGGroup, Resource: "backups"},
	})
	var c Capabilities
	c.View = ds[0].Allowed
	if ds[0].Err != nil {
		c.Undecided = append(c.Undecided, "view")
	}
	c.Logs = ds[1].Allowed
	if ds[1].Err != nil {
		c.Undecided = append(c.Undecided, "logs")
	}
	c.Operations = ds[2].Allowed && ds[3].Allowed && ds[4].Allowed
	if !c.Operations && (ds[2].Err != nil || ds[3].Err != nil || ds[4].Err != nil) {
		c.Undecided = append(c.Undecided, "operations")
	}
	c.Backup, c.Reload, c.Restart, c.Promote = ds[4].Allowed, ds[2].Allowed, ds[2].Allowed, ds[3].Allowed
	for _, op := range []struct {
		name string
		d    Decision
	}{{"backup", ds[4]}, {"reload", ds[2]}, {"restart", ds[2]}, {"promote", ds[3]}} {
		if op.d.Err != nil {
			c.Undecided = append(c.Undecided, op.name)
		}
	}
	return c
}

// PgAdminAccess asks whether u may open the pgAdmin of PgAdmin <name>
// (get pgadmins/<name>; dba-only by the chart's ClusterRoles).
func (a *Authorizer) PgAdminAccess(ctx context.Context, u User, namespace, name string) Decision {
	return a.Allowed(ctx, u, Attributes{Namespace: namespace, Verb: "get", Group: PgToolBoxGroup, Resource: "pgadmins", Name: name})
}

// ObjectRef names one namespaced object.
type ObjectRef struct {
	Namespace string
	Name      string
}

// VisibleClusters returns, in input order, the Clusters u may get, and
// the ones whose review failed (shown by the UI as "could not check",
// never as data).
func (a *Authorizer) VisibleClusters(ctx context.Context, u User, refs []ObjectRef) (visible, undecided []ObjectRef, err error) {
	ats := make([]Attributes, len(refs))
	for i, r := range refs {
		ats[i] = ClusterView(r.Namespace, r.Name)
	}
	var errs []error
	for i, d := range a.Check(ctx, u, ats) {
		switch {
		case d.Allowed:
			visible = append(visible, refs[i])
		case d.Err != nil:
			undecided = append(undecided, refs[i])
			errs = append(errs, d.Err)
		}
	}
	if len(errs) > 0 {
		err = errors.Join(errs...)
	}
	return visible, undecided, err
}
