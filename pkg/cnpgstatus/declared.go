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

package cnpgstatus

import (
	"sort"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
)

// Declarative-object states.
const (
	DeclaredApplied           = "applied"
	DeclaredPendingGeneration = "pendingGeneration"
	DeclaredFailed            = "failed"
	DeclaredWaitingForPrimary = "waitingForPrimary"
	DeclaredNotReconciled     = "notReconciled"
	DeclaredUnknown           = "unknown"
)

// waitingForPrimaryMessage is errClusterIsReplica
// (internal/management/controller/common.go v1.30.0:48,
// main:48), written with applied=nil on a replica cluster.
const waitingForPrimaryMessage = "waiting for the cluster to become primary"

// DeclaredReading is the state of a Database, Publication, Subscription
// or DatabaseRole (operator claim written by the primary's instance
// manager).
type DeclaredReading struct {
	Class Class  `json:"class"`
	State string `json:"state"`
	// Message is status.message: the PostgreSQL (or admission) error.
	Message string `json:"message,omitempty"`
	// FailedObjects are a Database's schemas/extensions/fdws/servers
	// whose own applied is false, "kind/name: message".
	FailedObjects []string `json:"failedObjects,omitempty"`
}

// Declared is ok only when applied && observedGeneration == generation:
// an applied flag from an older generation says nothing about the current
// spec. The status setters (api/v1/database_funcs.go
// v1.30.0:28-44, same in publication_funcs.go and subscription_funcs.go
// at v1.30.0 and main; databaserole_funcs.go v1.30.0:29-42,
// main:33-46) are:
//
//   - SetAsReady: applied=true, message cleared, observedGeneration =
//     generation. applied true with an older observedGeneration is a new
//     spec not processed yet: progressing.
//   - SetAsFailed: applied=false, message = the error; observedGeneration
//     is NOT advanced. Warning: the object is not what the spec says.
//   - SetAsUnknown: applied=nil, message = the error; with
//     "waiting for the cluster to become primary" on a replica cluster:
//     inactive; with any other message: unknown.
//   - applied=nil and no message: not reconciled yet, unknown.
func Declared(applied *bool, observedGeneration, generation int64, message string) DeclaredReading {
	reading := DeclaredReading{Message: bound(message, maxMessage)}
	switch {
	case applied == nil && message == waitingForPrimaryMessage:
		reading.Class, reading.State = ClassInactive, DeclaredWaitingForPrimary
	case applied == nil && message != "":
		reading.Class, reading.State = ClassUnknown, DeclaredUnknown
	case applied == nil:
		reading.Class, reading.State = ClassUnknown, DeclaredNotReconciled
	case !*applied:
		reading.Class, reading.State = ClassWarning, DeclaredFailed
	case observedGeneration != generation:
		reading.Class, reading.State = ClassProgressing, DeclaredPendingGeneration
	default:
		reading.Class, reading.State = ClassOK, DeclaredApplied
	}
	return reading
}

// DeclaredDatabase is Declared plus the per-object status of a Database
// (DatabaseStatus schemas/extensions/fdws/servers, api/v1/database_types.go
// v1.30.0:328-371, same lines at main): a failed object makes
// an otherwise applied Database a warning. A nil Database is unknown.
func DeclaredDatabase(d *cnpgv1.Database) DeclaredReading {
	if d == nil {
		return DeclaredReading{Class: ClassUnknown, State: DeclaredUnknown}
	}
	reading := Declared(d.Status.Applied, d.Status.ObservedGeneration, d.Generation, d.Status.Message)
	groups := []struct {
		kind    string
		objects []cnpgv1.DatabaseObjectStatus
	}{
		{"schema", d.Status.Schemas},
		{"extension", d.Status.Extensions},
		{"fdw", d.Status.FDWs},
		{"server", d.Status.Servers},
	}
	for _, g := range groups {
		for _, object := range g.objects {
			if object.Applied {
				continue
			}
			if len(reading.FailedObjects) >= maxFencedInstances {
				break
			}
			reading.FailedObjects = append(reading.FailedObjects,
				g.kind+"/"+bound(object.Name, maxAnnotationValue)+": "+bound(object.Message, maxMessage))
		}
	}
	if len(reading.FailedObjects) > 0 {
		reading.Class = Worst(reading.Class, ClassWarning)
	}
	return reading
}

// DeclaredPublication reads a Publication (publication_types.go
// PublicationStatus v1.30.0:122-135). A nil Publication is unknown.
func DeclaredPublication(p *cnpgv1.Publication) DeclaredReading {
	if p == nil {
		return DeclaredReading{Class: ClassUnknown, State: DeclaredUnknown}
	}
	return Declared(p.Status.Applied, p.Status.ObservedGeneration, p.Generation, p.Status.Message)
}

// DeclaredSubscription reads a Subscription (subscription_types.go
// SubscriptionStatus v1.30.0:84-97). A nil Subscription is unknown.
func DeclaredSubscription(s *cnpgv1.Subscription) DeclaredReading {
	if s == nil {
		return DeclaredReading{Class: ClassUnknown, State: DeclaredUnknown}
	}
	return Declared(s.Status.Applied, s.Status.ObservedGeneration, s.Generation, s.Status.Message)
}

// DatabaseRoleReading is a DatabaseRole: Declared, plus its conditions
// rendered generically by the caller.
type DatabaseRoleReading struct {
	DeclaredReading
	// Applicable is true on every supported minor: DatabaseRole ships with
	// 1.30, the support floor (api/v1/databaserole_types.go). An install
	// whose CRDs lack the kind has no objects to read, so nothing to apply.
	Applicable bool `json:"applicable"`
	// SecretResourceVersion is status.secretResourceVersion
	// (databaserole_types.go v1.30.0:129): the password Secret version
	// last applied.
	SecretResourceVersion string `json:"secretResourceVersion,omitempty"`
	// HasClientCertificate reports status.clientCertificate
	// (v1.30.0:134). status.password (generated passwords) exists on
	// main only (main:327) and cannot be read with the v1.30.0 API types.
	HasClientCertificate bool `json:"hasClientCertificate"`
}

// DeclaredRole reads a DatabaseRole (databaserole_types.go
// DatabaseRoleStatus v1.30.0:112-139, main:300-332). Every supported
// minor (1.30 and later) serves the kind and reads alike; the minor
// stays in the signature for the next one that does not. A nil role is
// unknown.
func DeclaredRole(_ Minor, r *cnpgv1.DatabaseRole) DatabaseRoleReading {
	if r == nil {
		return DatabaseRoleReading{Applicable: true, DeclaredReading: DeclaredReading{Class: ClassUnknown, State: DeclaredUnknown}}
	}
	return DatabaseRoleReading{
		DeclaredReading:       Declared(r.Status.Applied, r.Status.ObservedGeneration, r.Generation, r.Status.Message),
		Applicable:            true,
		SecretResourceVersion: bound(r.Status.SecretResourceVersion, maxAnnotationValue),
		HasClientCertificate:  r.Status.ClientCertificate != nil,
	}
}

// Managed-role states: the byStatus keys (api/v1/cluster_types.go
// RoleStatus v1.30.0:819-825, main:830-836), plus the
// console's "unreported" for a spec role in none of them.
const (
	RoleReconciled            = "reconciled"
	RoleNotManaged            = "not-managed"
	RolePendingReconciliation = "pending-reconciliation"
	RoleReserved              = "reserved"
	RoleUnreported            = "unreported"
)

var roleStatusClass = map[string]Class{
	RoleReconciled:            ClassOK,
	RoleNotManaged:            ClassInfo,
	RolePendingReconciliation: ClassProgressing,
	RoleReserved:              ClassInfo,
}

// ManagedRoleRow is one role of status.managedRolesStatus.
type ManagedRoleRow struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Class  Class  `json:"class"`
	// Declared is true for a role of spec.managed.roles.
	Declared bool `json:"declared"`
	// Errors are cannotReconcile[name] (bounded).
	Errors []string `json:"errors,omitempty"`
	// PasswordTransactionID is passwordStatus[name].transactionID, the
	// last PostgreSQL transaction that changed the role; 0 when absent.
	PasswordTransactionID int64 `json:"passwordTransactionID,omitempty"`
}

// ManagedRolesReading is status.managedRolesStatus, written by the
// primary's instance manager (internal/management/controller/roles/
// reconciler.go v1.30.0:84 byStatus; runnable.go v1.30.0:171-172
// passwordStatus and cannotReconcile).
type ManagedRolesReading struct {
	Class Class            `json:"class"`
	Roles []ManagedRoleRow `json:"roles,omitempty"`
}

// ManagedRoles reads status.managedRolesStatus (api/v1/cluster_types.go
// ManagedRoles v1.30.0:839-854, main:850-865). A role
// in cannotReconcile is a warning whatever its byStatus key; a declared
// role in no byStatus list is unknown; an undeclared byStatus key is
// shown with class unknown. No declared role and no status: info. A nil
// Cluster is unknown.
func ManagedRoles(c *cnpgv1.Cluster) ManagedRolesReading {
	if c == nil {
		return ManagedRolesReading{Class: ClassUnknown}
	}
	status := c.Status.ManagedRolesStatus
	rows := map[string]*ManagedRoleRow{}
	row := func(name string) *ManagedRoleRow {
		name = bound(name, maxAnnotationValue)
		if r, ok := rows[name]; ok {
			return r
		}
		r := &ManagedRoleRow{Name: name, Status: RoleUnreported, Class: ClassUnknown}
		rows[name] = r
		return r
	}
	if c.Spec.Managed != nil {
		for _, role := range c.Spec.Managed.Roles {
			row(role.Name).Declared = true
		}
	}
	for key, names := range status.ByStatus {
		class, known := roleStatusClass[string(key)]
		if !known {
			class = ClassUnknown
		}
		for _, name := range names {
			r := row(name)
			r.Status = bound(string(key), maxAnnotationValue)
			r.Class = class
		}
	}
	for name, errs := range status.CannotReconcile {
		r := row(name)
		for _, e := range errs {
			if len(r.Errors) >= maxFencedInstances {
				break
			}
			r.Errors = append(r.Errors, bound(e, maxMessage))
		}
		r.Class = Worst(r.Class, ClassWarning)
	}
	for name, password := range status.PasswordStatus {
		if r, ok := rows[bound(name, maxAnnotationValue)]; ok {
			r.PasswordTransactionID = password.TransactionID
		}
	}
	reading := ManagedRolesReading{}
	if len(rows) == 0 {
		reading.Class = ClassInfo
		return reading
	}
	classes := make([]Class, 0, len(rows))
	for _, r := range rows {
		reading.Roles = append(reading.Roles, *r)
		classes = append(classes, r.Class)
	}
	sort.Slice(reading.Roles, func(i, j int) bool { return reading.Roles[i].Name < reading.Roles[j].Name })
	reading.Class = Worst(classes...)
	return reading
}

// Tablespace states (api/v1/cluster_types.go TablespaceStatus
// v1.30.0:878-881, main:889-892), plus "unreported".
const (
	TablespaceReconciled = "reconciled"
	TablespacePending    = "pending"
	TablespaceUnreported = "unreported"
)

// TablespaceRow is one tablespace.
type TablespaceRow struct {
	Name      string `json:"name"`
	Owner     string `json:"owner,omitempty"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	Class     Class  `json:"class"`
	Declared  bool   `json:"declared"`
	Temporary bool   `json:"temporary,omitempty"`
}

// TablespacesReading is status.tablespacesStatus, written by the
// primary's instance manager (internal/management/controller/
// tablespaces/reconciler.go v1.30.0:144).
type TablespacesReading struct {
	Class       Class           `json:"class"`
	Tablespaces []TablespaceRow `json:"tablespaces,omitempty"`
}

// Tablespaces reads status.tablespacesStatus (TablespaceState
// v1.30.0:857-871, main:868-882) against
// spec.tablespaces: reconciled without error ok; pending progressing;
// any error warning; a declared tablespace absent from the status
// unknown; an unknown state unknown. None declared and none reported:
// info. A nil Cluster is unknown.
func Tablespaces(c *cnpgv1.Cluster) TablespacesReading {
	if c == nil {
		return TablespacesReading{Class: ClassUnknown}
	}
	reported := map[string]cnpgv1.TablespaceState{}
	for _, state := range c.Status.TablespacesStatus {
		reported[state.Name] = state
	}
	var rows []TablespaceRow
	seen := map[string]bool{}
	add := func(state cnpgv1.TablespaceState, declared, temporary bool) {
		r := TablespaceRow{
			Name: bound(state.Name, maxAnnotationValue), Owner: bound(state.Owner, maxAnnotationValue),
			State: bound(string(state.State), maxAnnotationValue), Error: bound(state.Error, maxMessage),
			Declared: declared, Temporary: temporary,
		}
		switch r.State {
		case TablespaceReconciled:
			r.Class = ClassOK
		case TablespacePending:
			r.Class = ClassProgressing
		default:
			r.Class = ClassUnknown
		}
		if r.Error != "" {
			r.Class = Worst(r.Class, ClassWarning)
		}
		rows = append(rows, r)
	}
	for _, tbs := range c.Spec.Tablespaces {
		seen[tbs.Name] = true
		state, ok := reported[tbs.Name]
		if !ok {
			rows = append(rows, TablespaceRow{
				Name: bound(tbs.Name, maxAnnotationValue), State: TablespaceUnreported, Class: ClassUnknown,
				Declared: true, Temporary: tbs.Temporary,
			})
			continue
		}
		add(state, true, tbs.Temporary)
	}
	for _, state := range c.Status.TablespacesStatus {
		if !seen[state.Name] {
			add(state, false, false)
		}
	}
	reading := TablespacesReading{Tablespaces: rows}
	if len(rows) == 0 {
		reading.Class = ClassInfo
		return reading
	}
	classes := make([]Class, 0, len(rows))
	for _, r := range rows {
		classes = append(classes, r.Class)
	}
	reading.Class = Worst(classes...)
	return reading
}

// Pooler phases (api/v1/pooler_types.go PoolerPhase v1.30.0:31-51,
// main:31-51).
const (
	PoolerPhaseActive   = "active"
	PoolerPhasePaused   = "paused"
	PoolerPhaseInactive = "inactive"
	PoolerPhaseFailed   = "failed"
)

var poolerPhaseClass = map[string]Class{
	PoolerPhaseActive:   ClassOK,
	PoolerPhasePaused:   ClassInactive,
	PoolerPhaseInactive: ClassWarning,
	PoolerPhaseFailed:   ClassCritical,
}

// PoolerReading is one Pooler.
type PoolerReading struct {
	Class Class `json:"class"`
	// Type is spec.type (rw default, ro, r: pooler_types.go v1.30.0:59-66,
	// :97); PoolMode spec.pgbouncer.poolMode (session default,
	// transaction: :81-84, :235); Paused spec.pgbouncer.paused (:291).
	Type     string `json:"type,omitempty"`
	PoolMode string `json:"poolMode,omitempty"`
	Paused   bool   `json:"paused"`
	// DesiredInstances is spec.instances (default 1, :102); Instances is
	// status.instances = the Deployment's status.replicas, Pods created,
	// not Pods ready (internal/controller/pooler_status.go
	// v1.30.0:93, main:93).
	DesiredInstances int32 `json:"desiredInstances"`
	Instances        int32 `json:"instances"`
	// Phase, PhaseReason, Image and Error (pooler_types.go
	// v1.30.0:317-334); PhaseKnown is false for a phase outside the table.
	Phase       string `json:"phase,omitempty"`
	PhaseKnown  bool   `json:"phaseKnown"`
	PhaseReason string `json:"phaseReason,omitempty"`
	Image       string `json:"image,omitempty"`
	Error       string `json:"error,omitempty"`
	Text        string `json:"text,omitempty"`
}

// PoolerState reads a Pooler. The class comes from
// status.phase (pooler_status.go v1.30.0:95-113: failed when the image
// cannot be resolved, paused while spec.pgbouncer.paused, active
// otherwise; inactive from pooler_controller.go v1.30.0:347 when a
// prerequisite is missing) and status.error (admission) is a warning;
// a Pooler without a phase is unknown unless paused (inactive). On every
// minor,
// status.instances below spec.instances lowers ok to progressing:
// "active" does not mean the Pods are ready, the Pods' readiness
// (cnpg.io/podRole=pooler) is the evidence. Every supported minor
// reads alike; the minor stays in the signature for the next one that
// does not. A nil Pooler is unknown.
func PoolerState(_ Minor, p *cnpgv1.Pooler) PoolerReading {
	if p == nil {
		return PoolerReading{Class: ClassUnknown}
	}
	reading := PoolerReading{
		Type:             bound(string(p.Spec.Type), maxAnnotationValue),
		DesiredInstances: 1,
		Instances:        p.Status.Instances,
		Phase:            bound(string(p.Status.Phase), maxAnnotationValue),
		PhaseReason:      bound(p.Status.PhaseReason, maxMessage),
		Image:            bound(p.Status.Image, maxAnnotationValue),
		Error:            bound(p.Status.Error, maxMessage),
	}
	if reading.Type == "" {
		reading.Type = "rw"
	}
	if p.Spec.Instances != nil {
		reading.DesiredInstances = *p.Spec.Instances
	}
	if p.Spec.PgBouncer != nil {
		reading.PoolMode = bound(string(p.Spec.PgBouncer.PoolMode), maxAnnotationValue)
		reading.Paused = p.Spec.PgBouncer.Paused != nil && *p.Spec.PgBouncer.Paused
	}
	if reading.PoolMode == "" {
		reading.PoolMode = "session"
	}
	switch {
	case reading.Phase != "":
		class, known := poolerPhaseClass[reading.Phase]
		reading.PhaseKnown = known
		if !known {
			class = ClassWarning
		}
		reading.Class = class
	case reading.Paused:
		reading.Class = ClassInactive
		reading.Text = "pgbouncer is paused by spec.pgbouncer.paused"
	default:
		reading.Class = ClassUnknown
		reading.Text = "the operator has not reported a phase"
	}
	if reading.Error != "" {
		reading.Class = Worst(reading.Class, ClassWarning)
	}
	if reading.Class == ClassOK && reading.Instances < reading.DesiredInstances {
		reading.Class = ClassProgressing
		reading.Text = "fewer Pods than spec.instances"
	}
	if reading.DesiredInstances == 0 && reading.Class == ClassOK {
		reading.Class = ClassInactive
		reading.Text = "scaled to zero instances"
	}
	return reading
}
