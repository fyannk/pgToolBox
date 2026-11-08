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
	"slices"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// PVC groups: the five lists of Cluster.status
// (api/v1/cluster_types.go danglingPVC..unusablePVC
// v1.30.0:973-989, main:984-1000), filled by EnrichStatus
// (pkg/reconciler/persistentvolumeclaim/status.go v1.30.0:236-241).
// PVCUnlisted is the console's name for a PVC in none of them: the
// operator ignores PVCs that are deleting, not Pending/Bound, carry an
// unknown pvcStatus annotation or no serial (status.go v1.30.0:195-212,
// :253), so "unlisted" is unknown, never healthy.
const (
	PVCHealthy      = "healthy"
	PVCInitializing = "initializing"
	PVCResizing     = "resizing"
	PVCDangling     = "dangling"
	PVCUnusable     = "unusable"
	PVCUnlisted     = "unlisted"
)

// PVCGroupReading is the operator's classification of one PVC.
type PVCGroupReading struct {
	Group string `json:"group"`
	Class Class  `json:"class"`
	Text  string `json:"text"`
}

// PVCGroup finds pvcName in the Cluster's PVC lists. Classes:
// healthy ok; initializing and resizing progressing; dangling warning
// (no Pod and no running Job: the operator recreates the Pod, a
// persistent dangling PVC is an instance that does not come back);
// unusable critical (its instance's PVC group is incomplete, the
// instance cannot start); unlisted unknown. The initializing text
// differs on 1.31 (main), where a Pod whose bootstrap-instance init
// container has not completed also counts (status.go main:62-64,
// :273-275 and v1.30.0). A nil Cluster is unknown.
func PVCGroup(minor Minor, c *cnpgv1.Cluster, pvcName string) PVCGroupReading {
	if c == nil {
		return PVCGroupReading{Group: PVCUnlisted, Class: ClassUnknown, Text: "the Cluster could not be read"}
	}
	status := c.Status
	initializing := "being initialized: a bootstrap Job uses it and no Pod does yet"
	if minor.atLeast(Minor131) {
		initializing = "being initialized: a bootstrap Job uses it, or its Pod's bootstrap init container has not completed"
	}
	groups := []struct {
		group string
		names []string
		class Class
		text  string
	}{
		// status.go v1.30.0:266-271: a PVC with a Pod and the Resizing
		// condition is resizing, otherwise healthy.
		{PVCHealthy, status.HealthyPVC, ClassOK, "attached to its instance Pod"},
		{PVCResizing, status.ResizingPVC, ClassProgressing, "being resized while attached to its Pod"},
		{PVCInitializing, status.InitializingPVC, ClassProgressing, initializing},
		// status.go v1.30.0:283-296: podless resizing PVCs are dangling too.
		{PVCDangling, status.DanglingPVC, ClassWarning, "not attached to a Pod or a running Job: the operator should recreate its instance Pod"},
		// status.go v1.30.0:260-263.
		{PVCUnusable, status.UnusablePVC, ClassCritical, "part of an incomplete PVC group: its instance cannot start"},
	}
	for _, g := range groups {
		if slices.Contains(g.names, pvcName) {
			return PVCGroupReading{Group: g.group, Class: g.class, Text: g.text}
		}
	}
	return PVCGroupReading{
		Group: PVCUnlisted, Class: ClassUnknown,
		Text: "not classified by the operator (deleting, not Pending/Bound, or not one of this cluster's instance PVCs)",
	}
}

// PVC roles, labels and annotations (pkg/utils/labels_annotations.go
// v1.30.0 :86-95 and :359-363, main :86-95 and :366-370).
const (
	LabelPVCRole        = "cnpg.io/pvcRole"
	LabelInstanceName   = "cnpg.io/instanceName"
	LabelTablespaceName = "cnpg.io/tablespaceName"
	LabelCluster        = "cnpg.io/cluster"
	LabelPodRole        = "cnpg.io/podRole"
	PVCRolePgData       = "PG_DATA"
	PVCRolePgWal        = "PG_WAL"
	PVCRolePgTablespace = "PG_TABLESPACE"
	PodRoleInstance     = "instance"
	PodRolePooler       = "pooler"
)

// Fork volume names in /pg/status.volumeUsages[].volume
// (FORK origin/dev/64 931cda924 pkg/postgres/status.go:118-128). Upstream
// v1.30.0 and main report no volume usage at all.
const (
	ForkVolumeUsageBranch  = "origin/dev/64"
	forkVolumePgData       = "pgdata"
	forkVolumeWal          = "wal"
	forkVolumeTablespacePr = "tbs-"
)

// PVCRoleReading is a PVC's role from its labels.
type PVCRoleReading struct {
	// Role is PG_DATA, PG_WAL, PG_TABLESPACE, or the raw label value.
	Role  string `json:"role,omitempty"`
	Known bool   `json:"known"`
	// Instance is cnpg.io/instanceName, Tablespace cnpg.io/tablespaceName
	// (PG_TABLESPACE only).
	Instance   string `json:"instance,omitempty"`
	Tablespace string `json:"tablespace,omitempty"`
	// VolumeUsageName is the matching volumeUsages[].volume of the
	// fork's /pg/status (pgdata, wal, tbs-<name>); empty when unknown.
	VolumeUsageName string `json:"volumeUsageName,omitempty"`
}

// PVCRole reads a PVC's role labels. The operator sets them at creation
// (pkg/reconciler/persistentvolumeclaim/calculator.go v1.30.0:123-132,
// :194-199, :257-262) and repairs them (metadata.go v1.30.0:269-295). A missing
// or foreign role is not known.
func PVCRole(labels map[string]string) PVCRoleReading {
	reading := PVCRoleReading{
		Role:     bound(labels[LabelPVCRole], maxAnnotationValue),
		Instance: bound(labels[LabelInstanceName], maxAnnotationValue),
	}
	switch reading.Role {
	case PVCRolePgData:
		reading.Known, reading.VolumeUsageName = true, forkVolumePgData
	case PVCRolePgWal:
		reading.Known, reading.VolumeUsageName = true, forkVolumeWal
	case PVCRolePgTablespace:
		reading.Known = true
		reading.Tablespace = bound(labels[LabelTablespaceName], maxAnnotationValue)
		if reading.Tablespace != "" {
			reading.VolumeUsageName = forkVolumeTablespacePr + reading.Tablespace
		}
	}
	return reading
}

// VolumeRequest is the storage the Cluster asks for, per role.
type VolumeRequest struct {
	Role       string `json:"role"`
	Tablespace string `json:"tablespace,omitempty"`
	// Size is storage.size, or the pvcTemplate's storage request when
	// size is empty (the operator applies size over the template).
	Size string `json:"size,omitempty"`
	// StorageClass is storage.storageClass, or the template's
	// storageClassName; empty means the namespace default class.
	StorageClass       string `json:"storageClass,omitempty"`
	ResizeInUseVolumes *bool  `json:"resizeInUseVolumes,omitempty"`
	Temporary          bool   `json:"temporary,omitempty"`
}

// StorageRequests lists spec.storage, spec.walStorage and every
// spec.tablespaces[].storage (api/v1/cluster_types.go
// StorageConfiguration v1.30.0:2249-2271, storage :328, walStorage
// :348, tablespaces :506 and TablespaceConfiguration :2275-2293; same
// fields at main). A nil Cluster gives nil.
func StorageRequests(c *cnpgv1.Cluster) []VolumeRequest {
	if c == nil {
		return nil
	}
	requests := []VolumeRequest{volumeRequest(PVCRolePgData, "", &c.Spec.StorageConfiguration)}
	if c.Spec.WalStorage != nil {
		requests = append(requests, volumeRequest(PVCRolePgWal, "", c.Spec.WalStorage))
	}
	for i := range c.Spec.Tablespaces {
		tbs := &c.Spec.Tablespaces[i]
		request := volumeRequest(PVCRolePgTablespace, bound(tbs.Name, maxAnnotationValue), &tbs.Storage)
		request.Temporary = tbs.Temporary
		requests = append(requests, request)
	}
	return requests
}

func volumeRequest(role, tablespace string, s *cnpgv1.StorageConfiguration) VolumeRequest {
	request := VolumeRequest{Role: role, Tablespace: tablespace, Size: bound(s.Size, maxAnnotationValue), ResizeInUseVolumes: s.ResizeInUseVolumes}
	if s.StorageClass != nil {
		request.StorageClass = bound(*s.StorageClass, maxAnnotationValue)
	}
	if template := s.PersistentVolumeClaimTemplate; template != nil {
		if request.Size == "" {
			if quantity, ok := template.Resources.Requests[corev1.ResourceStorage]; ok {
				request.Size = quantity.String()
			}
		}
		if request.StorageClass == "" && template.StorageClassName != nil {
			request.StorageClass = bound(*template.StorageClassName, maxAnnotationValue)
		}
	}
	return request
}

// AutoGrow is the fork's autoGrow stanza of one storage section
// (FORK origin/dev/64 931cda924 api/v1/cluster_types.go
// AutoGrowConfiguration): the operator grows the volume's PVCs by Step
// when any instance's filesystem is above Threshold percent used, never
// beyond MaxSize, without changing the spec. Upstream CloudNativePG has
// no such field.
type AutoGrow struct {
	// MaxSize is verbatim; MaxBytes its value, nil when it does not
	// parse as a quantity.
	MaxSize  string  `json:"maxSize,omitempty"`
	MaxBytes *uint64 `json:"maxBytes,omitempty"`
	// Threshold is the percentage used that triggers a growth (the CRD
	// defaults it to 80); Step is a percentage or a quantity ("20%").
	Threshold int    `json:"threshold,omitempty"`
	Step      string `json:"step,omitempty"`
}

// ForkAutoGrow reads the autoGrow stanzas of a raw Cluster object, keyed
// by the volume name of /pg/status volumeUsages: pgdata
// (spec.storage), wal (spec.walStorage), tbs-<name>
// (spec.tablespaces[].storage). A Cluster without the fork's field
// yields an empty map: the data decides, never the build.
func ForkAutoGrow(object map[string]any) map[string]AutoGrow {
	out := map[string]AutoGrow{}
	spec, _ := object["spec"].(map[string]any)
	read := func(section any) (AutoGrow, bool) {
		storage, _ := section.(map[string]any)
		raw, ok := storage["autoGrow"].(map[string]any)
		if !ok {
			return AutoGrow{}, false
		}
		grow := AutoGrow{}
		if maxSize, ok := raw["maxSize"].(string); ok {
			grow.MaxSize = bound(maxSize, maxAnnotationValue)
			if q, err := resource.ParseQuantity(maxSize); err == nil && q.Sign() > 0 {
				if v, ok := q.AsInt64(); ok && v > 0 {
					bytes := uint64(v) //nolint:gosec // v > 0: the conversion cannot wrap.
					grow.MaxBytes = &bytes
				}
			}
		}
		switch threshold := raw["threshold"].(type) {
		case int64:
			grow.Threshold = int(threshold)
		case float64:
			grow.Threshold = int(threshold)
		}
		if step, ok := raw["step"].(string); ok {
			grow.Step = bound(step, maxAnnotationValue)
		}
		return grow, true
	}
	if grow, ok := read(spec["storage"]); ok {
		out[forkVolumePgData] = grow
	}
	if grow, ok := read(spec["walStorage"]); ok {
		out[forkVolumeWal] = grow
	}
	tablespaces, _ := spec["tablespaces"].([]any)
	for _, item := range tablespaces {
		tbs, _ := item.(map[string]any)
		name, _ := tbs["name"].(string)
		if name == "" {
			continue
		}
		if grow, ok := read(tbs["storage"]); ok {
			out[forkVolumeTablespacePr+bound(name, maxAnnotationValue)] = grow
		}
	}
	return out
}

// VolumeName is the /pg/status volumeUsages name of a declared volume.
func (r VolumeRequest) VolumeName() string {
	switch r.Role {
	case PVCRolePgData:
		return forkVolumePgData
	case PVCRolePgWal:
		return forkVolumeWal
	case PVCRolePgTablespace:
		if r.Tablespace != "" {
			return forkVolumeTablespacePr + r.Tablespace
		}
	}
	return ""
}
