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

package kube

import (
	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// The four declarative CRDs. Each declares one PostgreSQL object and
// carries the operator's report of reconciling it; none of them is read
// for database content.
var ()

const (
	databaseObjectPageSize     = 200
	maxDatabaseObjectsScan     = 2000
	maxDeclaredRoleMemberships = 32
)

// declaredStatus lifts the reconciliation spine every declarative kind
// reports. The message is bounded here, at the boundary.
func declaredStatus(observedGeneration int64, applied *bool, message string) observe.Declared {
	d := observe.Declared{ObservedGeneration: observedGeneration, Message: boundOperatorMessage(message)}
	if applied != nil {
		v := *applied
		d.Applied = &v
	}
	return d
}

func (c *Client) convertDatabase(content map[string]any) (observe.DatabaseFacts, bool, error) {
	var db cnpgv1.Database
	if err := fromUnstructured(content, &db, "database convert"); err != nil {
		return observe.DatabaseFacts{}, false, err
	}
	return observe.DatabaseFacts{
		Name: db.Name, UID: string(db.UID), Database: db.Spec.Name,
		Owner: db.Spec.Owner, Encoding: db.Spec.Encoding, Ensure: string(db.Spec.Ensure),
		CreatedAt: db.CreationTimestamp.Time.UTC(),
		Declared:  declaredStatus(db.Status.ObservedGeneration, db.Status.Applied, db.Status.Message),
	}, c.declaredForTarget(db.Namespace, db.Spec.ClusterRef.Name), nil
}

func (c *Client) convertDatabaseRole(content map[string]any) (observe.DatabaseRoleFacts, bool, error) {
	var role cnpgv1.DatabaseRole
	if err := fromUnstructured(content, &role, "database role convert"); err != nil {
		return observe.DatabaseRoleFacts{}, false, err
	}
	facts := observe.DatabaseRoleFacts{
		Name: role.Name, UID: string(role.UID), Role: role.Spec.Name,
		Superuser: role.Spec.Superuser, CreateDB: role.Spec.CreateDB, CreateRole: role.Spec.CreateRole,
		ConnectionLimit: role.Spec.ConnectionLimit,
		// The reference, never the Secret. The console holds no Secret
		// permission and nothing it displays may need one.
		HasPasswordSecret: role.Spec.PasswordSecret != nil,
		CreatedAt:         role.CreationTimestamp.Time.UTC(),
		Declared:          declaredStatus(role.Status.ObservedGeneration, role.Status.Applied, role.Status.Message),
	}
	if role.Spec.ValidUntil != nil {
		t := role.Spec.ValidUntil.Time.UTC()
		facts.ValidUntil = &t
	}
	memberships := role.Spec.InRoles
	if len(memberships) > maxDeclaredRoleMemberships {
		memberships = memberships[:maxDeclaredRoleMemberships]
	}
	facts.InRoles = append([]string(nil), memberships...)
	return facts, c.declaredForTarget(role.Namespace, role.Spec.ClusterRef.Name), nil
}

func (c *Client) convertPublication(content map[string]any) (observe.PublicationFacts, bool, error) {
	var pub cnpgv1.Publication
	if err := fromUnstructured(content, &pub, "publication convert"); err != nil {
		return observe.PublicationFacts{}, false, err
	}
	return observe.PublicationFacts{
		Name: pub.Name, UID: string(pub.UID), Publication: pub.Spec.Name, Database: pub.Spec.DBName,
		// Only whether the target is the whole database. The enumerated
		// object list is a table list, which is database content and not
		// this console's to render.
		AllTables: pub.Spec.Target.AllTables,
		CreatedAt: pub.CreationTimestamp.Time.UTC(),
		Declared:  declaredStatus(pub.Status.ObservedGeneration, pub.Status.Applied, pub.Status.Message),
	}, c.declaredForTarget(pub.Namespace, pub.Spec.ClusterRef.Name), nil
}

func (c *Client) convertSubscription(content map[string]any) (observe.SubscriptionFacts, bool, error) {
	var sub cnpgv1.Subscription
	if err := fromUnstructured(content, &sub, "subscription convert"); err != nil {
		return observe.SubscriptionFacts{}, false, err
	}
	return observe.SubscriptionFacts{
		Name: sub.Name, UID: string(sub.UID), Subscription: sub.Spec.Name, Database: sub.Spec.DBName,
		Publication: sub.Spec.PublicationName, ExternalCluster: sub.Spec.ExternalClusterName,
		CreatedAt: sub.CreationTimestamp.Time.UTC(),
		Declared:  declaredStatus(sub.Status.ObservedGeneration, sub.Status.Applied, sub.Status.Message),
	}, c.declaredForTarget(sub.Namespace, sub.Spec.ClusterRef.Name), nil
}

// declaredForTarget is the membership rule every declarative kind
// shares: the exact spec.cluster.name reference, in the configured
// namespace.
func (c *Client) declaredForTarget(namespace, clusterRef string) bool {
	return namespace == c.opts.Namespace && clusterRef == c.opts.ClusterName
}

// fromUnstructured decodes into a typed CRD, categorizing the failure.
func fromUnstructured(content map[string]any, into any, op string) error {
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(content, into); err != nil {
		return redact.NewError(op, redact.CategoryInternal, err)
	}
	return nil
}
