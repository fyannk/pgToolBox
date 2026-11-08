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

// Package operations is the console's operation service: the offers a caller sees, the pessimistic request that
// writes as the caller through internal/console/ops, and the tracker
// that follows the operator's reaction in the read model.
//
// The service keeps no state a second replica would need: an operation
// ID is a self-describing handle (kind, Cluster, target, the marker the
// write left) that any replica resolves against its own informers, so a
// tracker survives a reload landing on the other replica. The handle
// carries nothing secret and grants nothing: resolving it re-authorizes
// the caller on the Cluster it names, and what it shows is the Cluster's
// public state.
package operations

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// MaxIDLength bounds an operation ID on input.
const MaxIDLength = 512

// ErrBadID is returned for a handle that does not parse.
var ErrBadID = errors.New("not an operation id")

// ID is the decoded handle.
type ID struct {
	Kind      viewmodel.OperationKind `json:"k"`
	Namespace string                  `json:"n"`
	Cluster   string                  `json:"c"`
	// Instance is the promote target.
	Instance string `json:"i,omitempty"`
	// Created is the Backup or drill Cluster the request created.
	Created string `json:"b,omitempty"`
	// CreatedNamespace is the drill Cluster's namespace (restoreDrill).
	CreatedNamespace string `json:"d,omitempty"`
	// Marker is the value the write left: the annotation value of a
	// restart or reload, the targetPrimaryTimestamp of a promote.
	Marker string `json:"m,omitempty"`
	// AcceptedAt is the acceptance instant, Unix seconds. Who asked is
	// deliberately not part of the handle: that is the API server's audit
	// log's to say, and a handle must not be a forgeable claim of it.
	AcceptedAt int64 `json:"t"`
}

var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	markerRE     = regexp.MustCompile(`^[0-9A-Za-z:.+\-]{1,64}$`)
)

// Encode returns the handle's opaque form.
func (id ID) Encode() string {
	b, _ := json.Marshal(struct {
		V int `json:"v"`
		ID
	}{1, id})
	return base64.RawURLEncoding.EncodeToString(b)
}

// Accepted is the acceptance instant.
func (id ID) Accepted() time.Time { return time.Unix(id.AcceptedAt, 0).UTC() }

// Ref is the Cluster the operation targets.
func (id ID) Ref() viewmodel.ClusterRef {
	return viewmodel.ClusterRef{Namespace: id.Namespace, Name: id.Cluster}
}

// ParseID decodes and validates a handle. Every field is checked against
// the shape the console writes, so a crafted handle can name only a
// well-formed Cluster in a namespace, which the caller is then
// authorized on before anything is read.
func ParseID(raw string) (ID, error) {
	if raw == "" || len(raw) > MaxIDLength {
		return ID{}, ErrBadID
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return ID{}, ErrBadID
	}
	var wire struct {
		V int `json:"v"`
		ID
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil || dec.More() || wire.V != 1 {
		return ID{}, ErrBadID
	}
	id := wire.ID
	switch {
	case !validKind(id.Kind),
		len(id.Namespace) > 63 || !dnsLabel.MatchString(id.Namespace),
		len(id.Cluster) > 253 || !dnsSubdomain.MatchString(id.Cluster),
		id.Instance != "" && (len(id.Instance) > 253 || !dnsSubdomain.MatchString(id.Instance)),
		id.Created != "" && (len(id.Created) > 253 || !dnsSubdomain.MatchString(id.Created)),
		id.Marker != "" && !markerRE.MatchString(id.Marker),
		id.AcceptedAt <= 0,
		(id.Kind == viewmodel.OperationPromote) != (id.Instance != ""),
		id.Created != "" && id.Kind != viewmodel.OperationBackup && id.Kind != viewmodel.OperationRestoreDrill,
		(id.Kind == viewmodel.OperationRestoreDrill) != (id.CreatedNamespace != ""),
		id.Kind == viewmodel.OperationRestoreDrill && id.Created == "",
		id.CreatedNamespace != "" && (len(id.CreatedNamespace) > 63 || !dnsLabel.MatchString(id.CreatedNamespace)):
		return ID{}, ErrBadID
	}
	return id, nil
}

func validKind(k viewmodel.OperationKind) bool {
	switch k {
	case viewmodel.OperationBackup, viewmodel.OperationReload, viewmodel.OperationRestart, viewmodel.OperationPromote,
		viewmodel.OperationRestoreDrill:
		return true
	default:
		return false
	}
}
