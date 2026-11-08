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

package status

// The compare screen: two Clusters' declared
// configuration and operator-reported state, row by row. "Why is staging
// fine and prod not?" starts with what differs.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/pgconf"
)

// maxCompareParameters bounds the parameter rows: a Cluster declares a
// few dozen, and a hostile spec must not grow the response unbounded.
const maxCompareParameters = 400

// set is a value read in full: rows compare full values and shorten
// them for display only afterwards (shown), so two values that differ
// past the display bound never read as the same.
func set(v string) viewmodel.CompareValue {
	if v == "" {
		return viewmodel.CompareValue{State: "unset"}
	}
	return viewmodel.CompareValue{State: "set", Value: v}
}

func shown(v viewmodel.CompareValue) viewmodel.CompareValue {
	v.Value = bound(v.Value)
	return v
}

func unknownValue() viewmodel.CompareValue { return viewmodel.CompareValue{State: "unknown"} }

func intValue(v int) viewmodel.CompareValue { return set(strconv.Itoa(v)) }

func boolPtr(v *bool) viewmodel.CompareValue {
	if v == nil {
		return set("")
	}
	return set(strconv.FormatBool(*v))
}

// field is one row read from either side.
type field struct {
	key  string
	read func(g *gathered) viewmodel.CompareValue
}

// Compare is the compare screen of two Clusters, before authorization
// (the API checks both).
func (s *Service) Compare(ctx context.Context, leftNS, leftName, rightNS, rightName string) (viewmodel.Comparison, error) {
	l, err := s.gather(ctx, cache.ClusterKey{Namespace: leftNS, Name: leftName}, false)
	if err != nil {
		return viewmodel.Comparison{}, err
	}
	r, err := s.gather(ctx, cache.ClusterKey{Namespace: rightNS, Name: rightName}, false)
	if err != nil {
		return viewmodel.Comparison{}, err
	}
	now := s.now().UTC()
	out := viewmodel.Comparison{
		GeneratedAt: now, Left: s.compared(l, now), Right: s.compared(r, now), Sections: []viewmodel.CompareSection{},
		BlindSpots: []viewmodel.BlindSpot{{
			ID: "compare.declared", Panel: "compare",
			Text: "Parameters and resources are what each Cluster declares; what PostgreSQL applied is not reported (a change waiting for a restart reads as applied).",
		}},
	}
	spec := func(id, source string, fields []field) {
		out.Sections = append(out.Sections, section(id, viewmodel.OriginOperator, source, l, r, fields))
	}
	spec("version", "Cluster.status.image, pgDataImageInfo; Cluster.spec.imageName, imageCatalogRef; detected operator version", versionFields)
	spec("instances", "Cluster.spec, Cluster.status.readyInstances", instanceFields)
	out.Sections = append(out.Sections, parameterSection(l, r))
	out.Sections = append(out.Sections, section("resources", viewmodel.OriginOperator, "Cluster.spec.resources (the postgres container)", l, r, resourceFields(l, r)))
	spec("storage", "Cluster.spec.storage, walStorage, tablespaces", storageFields)
	spec("backup", "Cluster.spec.backup, spec.plugins, ObjectStore.spec, ScheduledBackups", backupFields)
	spec("topology", "Cluster.spec.postgresql.synchronous, spec.affinity, Poolers", topologyFields)
	for _, sec := range out.Sections {
		out.Differences += sec.Differences
	}
	if dropped := parametersBeyondBound(l, r); dropped > 0 {
		out.BlindSpots = append(out.BlindSpots, viewmodel.BlindSpot{
			ID: "compare.parametersBounded", Panel: "compare",
			Text: fmt.Sprintf("%d parameters beyond the first %d (in name order) are not compared.", dropped, maxCompareParameters),
		})
	}
	return out, nil
}

func (s *Service) compared(g *gathered, now time.Time) viewmodel.ComparedCluster {
	row := s.row(g)
	return viewmodel.ComparedCluster{
		Ref: ref(g.cluster), Health: row.Verdict.Health, Phase: bound(g.cluster.Status.Phase),
		Provenance: s.provenance(g.key.Namespace, cache.SourceClusters, viewmodel.OriginOperator, "Cluster", now),
	}
}

func section(id string, origin viewmodel.Origin, source string, l, r *gathered, fields []field) viewmodel.CompareSection {
	sec := viewmodel.CompareSection{ID: id, Origin: origin, Source: source, Rows: []viewmodel.CompareRow{}}
	for _, f := range fields {
		sectionAdd(&sec, f.key, f.read(l), f.read(r), "")
	}
	return sec
}

// sectionAdd is CompareSection.add: Same is equal and known.
func sectionAdd(sec *viewmodel.CompareSection, key string, a, b viewmodel.CompareValue, note string) {
	row := viewmodel.CompareRow{Key: key, Left: shown(a), Right: shown(b), Note: note}
	row.Same = a.State != "unknown" && b.State != "unknown" && a.State == b.State && a.Value == b.Value
	if !row.Same {
		sec.Differences++
	}
	sec.Rows = append(sec.Rows, row)
}

var versionFields = []field{
	{"PostgreSQL major", func(g *gathered) viewmodel.CompareValue {
		if info := g.cluster.Status.PGDataImageInfo; info != nil && info.MajorVersion > 0 {
			return intValue(info.MajorVersion)
		}
		return unknownValue()
	}},
	{"status.image", func(g *gathered) viewmodel.CompareValue {
		if g.cluster.Status.Image == "" {
			return unknownValue()
		}
		return set(g.cluster.Status.Image)
	}},
	{"spec.imageName", func(g *gathered) viewmodel.CompareValue { return set(g.cluster.Spec.ImageName) }},
	{"spec.imageCatalogRef", func(g *gathered) viewmodel.CompareValue {
		ref := g.cluster.Spec.ImageCatalogRef
		if ref == nil {
			return set("")
		}
		return set(fmt.Sprintf("%s/%s major %d", ref.Kind, ref.Name, ref.Major))
	}},
	{"CloudNativePG (detected)", func(g *gathered) viewmodel.CompareValue {
		if g.version.Version == "" {
			return unknownValue()
		}
		return set(g.version.Version)
	}},
}

var instanceFields = []field{
	{"spec.instances", func(g *gathered) viewmodel.CompareValue { return intValue(g.cluster.Spec.Instances) }},
	{"status.readyInstances", func(g *gathered) viewmodel.CompareValue { return intValue(g.cluster.Status.ReadyInstances) }},
	{"spec.primaryUpdateStrategy", func(g *gathered) viewmodel.CompareValue { return set(string(g.cluster.Spec.PrimaryUpdateStrategy)) }},
	{"spec.primaryUpdateMethod", func(g *gathered) viewmodel.CompareValue { return set(string(g.cluster.Spec.PrimaryUpdateMethod)) }},
	{"spec.failoverDelay", func(g *gathered) viewmodel.CompareValue { return int32Value(g.cluster.Spec.FailoverDelay) }},
	{"spec.switchoverDelay", func(g *gathered) viewmodel.CompareValue { return int32Value(g.cluster.Spec.MaxSwitchoverDelay) }},
	{"spec.replica.enabled", func(g *gathered) viewmodel.CompareValue {
		if g.cluster.Spec.ReplicaCluster == nil {
			return set("")
		}
		return boolPtr(g.cluster.Spec.ReplicaCluster.Enabled)
	}},
}

func int32Value(v int32) viewmodel.CompareValue {
	if v == 0 {
		return set("")
	}
	return set(strconv.Itoa(int(v)))
}

// declaredParameters is a Cluster's parameters by lower-case name, as
// PostgreSQL matches them; of two spellings of one name the later in
// sorted order wins, as the later line of postgresql.conf does.
func declaredParameters(g *gathered) map[string][2]string {
	params := g.cluster.Spec.PostgresConfiguration.Parameters
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string][2]string, len(keys))
	for _, k := range keys {
		out[strings.ToLower(k)] = [2]string{k, params[k]}
	}
	return out
}

// parameterNames is the sorted union of both sides' parameter names.
func parameterNames(lp, rp map[string][2]string) []string {
	names := make([]string, 0, len(lp)+len(rp))
	for k := range lp {
		names = append(names, k)
	}
	for k := range rp {
		if _, ok := lp[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

// parametersBeyondBound counts the parameter rows maxCompareParameters
// leaves out.
func parametersBeyondBound(l, r *gathered) int {
	return max(0, len(parameterNames(declaredParameters(l), declaredParameters(r)))-maxCompareParameters)
}

// parameterSection is the union of both Clusters' parameters, by name
// case-insensitively (PostgreSQL's names are), sorted, plus
// shared_preload_libraries. A memory parameter spelled differently but
// meaning the same bytes is the same, with a note.
func parameterSection(l, r *gathered) viewmodel.CompareSection {
	sec := viewmodel.CompareSection{
		ID: "parameters", Origin: viewmodel.OriginOperator, Source: "Cluster.spec.postgresql.parameters, shared_preload_libraries",
		Rows: []viewmodel.CompareRow{},
	}
	lp, rp := declaredParameters(l), declaredParameters(r)
	names := parameterNames(lp, rp)
	if len(names) > maxCompareParameters {
		names = names[:maxCompareParameters]
	}
	for _, name := range names {
		a, aok := lp[name]
		b, bok := rp[name]
		key := a[0]
		if !aok {
			key = b[0]
		}
		var note string
		if aok && bok && a[0] != b[0] {
			note = "declared as " + a[0] + " and " + b[0] + "; PostgreSQL reads names case-insensitively"
		}
		av, bv := set(a[1]), set(b[1])
		if aok && bok && a[1] != b[1] && pgconf.Equivalent(name, a[1], b[1]) {
			// Equal bytes: shown as the same, with both spellings kept.
			sec.Rows = append(sec.Rows, viewmodel.CompareRow{
				Key: key, Left: shown(av), Right: shown(bv), Same: true,
				Note: strings.TrimPrefix(note+"; the same value, spelled differently", "; "),
			})
			continue
		}
		sectionAdd(&sec, key, av, bv, note)
	}
	libs := func(g *gathered) viewmodel.CompareValue {
		v := append([]string(nil), g.cluster.Spec.PostgresConfiguration.AdditionalLibraries...)
		sort.Strings(v)
		return set(strings.Join(v, ", "))
	}
	sectionAdd(&sec, "shared_preload_libraries", libs(l), libs(r), "")
	return sec
}

// resourceFields are requests and limits of every resource either side
// names, cpu and memory first.
func resourceFields(l, r *gathered) []field {
	names := map[string]bool{"cpu": true, "memory": true}
	for _, g := range []*gathered{l, r} {
		for n := range g.cluster.Spec.Resources.Requests {
			names[string(n)] = true
		}
		for n := range g.cluster.Spec.Resources.Limits {
			names[string(n)] = true
		}
	}
	keys := make([]string, 0, len(names))
	for n := range names {
		keys = append(keys, n)
	}
	sort.Slice(keys, func(i, j int) bool {
		rank := func(s string) int {
			switch s {
			case "cpu":
				return 0
			case "memory":
				return 1
			}
			return 2
		}
		if rank(keys[i]) != rank(keys[j]) {
			return rank(keys[i]) < rank(keys[j])
		}
		return keys[i] < keys[j]
	})
	out := make([]field, 0, 2*len(keys))
	for _, n := range keys {
		name := corev1.ResourceName(n)
		out = append(out,
			field{"requests." + n, func(g *gathered) viewmodel.CompareValue { return quantity(g.cluster.Spec.Resources.Requests, name) }},
			field{"limits." + n, func(g *gathered) viewmodel.CompareValue { return quantity(g.cluster.Spec.Resources.Limits, name) }},
		)
	}
	return out
}

// quantity renders a resource quantity canonically, so "1Gi" and
// "1024Mi" compare the same.
func quantity(list corev1.ResourceList, name corev1.ResourceName) viewmodel.CompareValue {
	q, ok := list[name]
	if !ok {
		return set("")
	}
	return set(q.String())
}

var storageFields = []field{
	{"storage.size", func(g *gathered) viewmodel.CompareValue { return set(g.cluster.Spec.StorageConfiguration.Size) }},
	{"storage.storageClass", func(g *gathered) viewmodel.CompareValue {
		return strPtr(g.cluster.Spec.StorageConfiguration.StorageClass)
	}},
	{"walStorage.size", func(g *gathered) viewmodel.CompareValue {
		if w := g.cluster.Spec.WalStorage; w != nil {
			return set(w.Size)
		}
		return set("")
	}},
	{"walStorage.storageClass", func(g *gathered) viewmodel.CompareValue {
		if w := g.cluster.Spec.WalStorage; w != nil {
			return strPtr(w.StorageClass)
		}
		return set("")
	}},
	{"tablespaces", func(g *gathered) viewmodel.CompareValue {
		parts := make([]string, 0, len(g.cluster.Spec.Tablespaces))
		for _, t := range g.cluster.Spec.Tablespaces {
			parts = append(parts, t.Name+" "+t.Storage.Size)
		}
		sort.Strings(parts)
		return set(strings.Join(parts, ", "))
	}},
}

func strPtr(v *string) viewmodel.CompareValue {
	if v == nil {
		return set("")
	}
	return set(*v)
}

var backupFields = []field{
	{"method", func(g *gathered) viewmodel.CompareValue {
		cfg := cnpgstatus.Backups(g.cluster)
		var methods []string
		if cfg.PluginBased {
			methods = append(methods, "plugin "+cfg.PluginName)
		}
		if b := g.cluster.Spec.Backup; b != nil && b.BarmanObjectStore != nil {
			methods = append(methods, "in-tree Barman Cloud")
		}
		if b := g.cluster.Spec.Backup; b != nil && b.VolumeSnapshot != nil {
			methods = append(methods, "volume snapshots")
		}
		if len(methods) == 0 {
			return set("none")
		}
		return set(strings.Join(methods, ", "))
	}},
	{"objectStore", func(g *gathered) viewmodel.CompareValue { return set(objectStoreName(g.cluster)) }},
	{"retentionPolicy", func(g *gathered) viewmodel.CompareValue {
		if name := objectStoreName(g.cluster); name != "" {
			if g.store == nil {
				// The ObjectStore holds it; not readable or absent.
				return unknownValue()
			}
			return set(nestedString(g.store, "spec", "retentionPolicy"))
		}
		if b := g.cluster.Spec.Backup; b != nil {
			return set(b.RetentionPolicy)
		}
		return set("")
	}},
	{"backup.target", func(g *gathered) viewmodel.CompareValue {
		if b := g.cluster.Spec.Backup; b != nil {
			return set(string(b.Target))
		}
		return set("")
	}},
	{"schedules", func(g *gathered) viewmodel.CompareValue {
		if !g.listed(cache.SourceScheduledBackups) {
			return unknownValue()
		}
		var parts []string
		for _, sb := range g.scheduled {
			p := sb.Spec.Schedule
			if sb.Spec.Suspend != nil && *sb.Spec.Suspend {
				p += " (suspended)"
			}
			parts = append(parts, p)
		}
		sort.Strings(parts)
		return set(strings.Join(parts, "; "))
	}},
}

var topologyFields = []field{
	{"synchronous", func(g *gathered) viewmodel.CompareValue {
		sync := cnpgstatus.SyncConfig(g.minor, g.cluster)
		switch sync.Mode {
		case cnpgstatus.SyncModeExplicit:
			v := fmt.Sprintf("%s %d, dataDurability %s", sync.Method, sync.Number, sync.DataDurability)
			if sync.FailoverQuorum {
				v += ", failoverQuorum"
			}
			return set(v)
		case cnpgstatus.SyncModeLegacy:
			return set(fmt.Sprintf("legacy, min %d max %d", sync.MinSync, sync.MaxSync))
		}
		return set("none")
	}},
	{"affinity.enablePodAntiAffinity", func(g *gathered) viewmodel.CompareValue {
		return boolPtr(g.cluster.Spec.Affinity.EnablePodAntiAffinity)
	}},
	{"affinity.podAntiAffinityType", func(g *gathered) viewmodel.CompareValue { return set(g.cluster.Spec.Affinity.PodAntiAffinityType) }},
	{"affinity.topologyKey", func(g *gathered) viewmodel.CompareValue { return set(g.cluster.Spec.Affinity.TopologyKey) }},
	{"affinity.nodeSelector", func(g *gathered) viewmodel.CompareValue {
		parts := make([]string, 0, len(g.cluster.Spec.Affinity.NodeSelector))
		for k, v := range g.cluster.Spec.Affinity.NodeSelector {
			parts = append(parts, k+"="+v)
		}
		sort.Strings(parts)
		return set(strings.Join(parts, ", "))
	}},
	{"affinity.tolerations", func(g *gathered) viewmodel.CompareValue {
		if n := len(g.cluster.Spec.Affinity.Tolerations); n > 0 {
			return intValue(n)
		}
		return set("")
	}},
	{"poolers", func(g *gathered) viewmodel.CompareValue {
		if !g.listed(cache.SourcePoolers) {
			return unknownValue()
		}
		var parts []string
		for _, p := range g.poolers {
			v := string(p.Spec.Type)
			if p.Spec.Instances != nil {
				v += " ×" + strconv.Itoa(int(*p.Spec.Instances))
			}
			parts = append(parts, v)
		}
		sort.Strings(parts)
		return set(strings.Join(parts, ", "))
	}},
}
