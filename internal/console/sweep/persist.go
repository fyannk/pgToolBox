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

package sweep

import (
	"sort"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
)

// The sweep's part of the state snapshot (internal/console/statesnap):
// per Cluster, the instances' state, "since" and last good reading, the
// rings, the metrics window and the insight histories. Everything is
// keyed by the Cluster's UID and each instance by its pod's UID: a
// Cluster or a pod recreated under the same name while the console was
// down inherits nothing. What a snapshot restores waits until its
// Cluster is swept, because only the roster says which UID the name now
// carries.

// SnapshotVersion gates the layout of PersistedState.
const SnapshotVersion = 1

// MaxSnapshotBytes bounds the sweep's snapshot. At the defaults (30 s
// sweeps, one hour of rings and metrics) a swept instance takes about
// 100 KiB, so this holds some 5,000 instances: more than the console is
// sized for (docs: console sizing).
const MaxSnapshotBytes = 512 << 20

// restoreKeep is how long restored state waits for its Cluster: past
// the longest history any part keeps (the statement snapshots, 26 h),
// nothing of it would be kept anyway.
const restoreKeep = 26 * time.Hour

// PersistedState is the sweep's snapshot.
type PersistedState struct {
	Clusters []PersistedCluster
}

// PersistedCluster is one Cluster's swept state.
type PersistedCluster struct {
	UID, Namespace, Name string
	Instances            []PersistedInstance
	Rings                []PersistedRing
	Metrics              *metrics.PersistedState
	Insight              *insight.PersistedState
}

// PersistedInstance is one instance's state.
type PersistedInstance struct {
	Name, PodUID string
	State        string
	Reason       string
	At, Since    int64
	Reading      *instancestatus.Reading
	// Fenced carries Reading.IsFenced: 0 not reported, 1 false, 2 true.
	// A gob encoder omits a *bool holding false, which would read back
	// as "not reported".
	Fenced int8
}

// PersistedRing is one instance's series; Has[i] false is a gap.
type PersistedRing struct {
	Instance, Series string
	At               []int64
	Values           []float64
	Has              []bool
}

// clusterCopy is what Export copies under the sweep's lock.
type clusterCopy struct {
	key     Key
	uid     string
	snap    Snapshot
	rings   []PersistedRing
	metrics *metrics.Store
	insight *insight.Tracker
}

// Export copies the sweep out for a snapshot. The sweep's lock is held
// per Cluster while its rings and instances are copied; the metrics and
// insight stores are copied under their own locks, so a large fleet
// never blocks a sweep for the whole export.
func (s *Sweeper) Export(now time.Time) PersistedState {
	s.mu.RLock()
	keys := make([]Key, 0, len(s.clusters))
	for key := range s.clusters {
		keys = append(keys, key)
	}
	s.mu.RUnlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Namespace != keys[j].Namespace {
			return keys[i].Namespace < keys[j].Namespace
		}
		return keys[i].Name < keys[j].Name
	})
	out := PersistedState{Clusters: make([]PersistedCluster, 0, len(keys))}
	for _, key := range keys {
		c, ok := s.copyCluster(key)
		if !ok || c.uid == "" {
			continue
		}
		p := PersistedCluster{UID: c.uid, Namespace: key.Namespace, Name: key.Name, Rings: c.rings}
		names := make([]string, 0, len(c.snap.Instances))
		for name := range c.snap.Instances {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			inst := c.snap.Instances[name]
			pi := PersistedInstance{
				Name: name, PodUID: inst.uid, State: string(inst.State), Reason: inst.Reason,
				At: inst.At.UnixNano(), Since: inst.Since.UnixNano(), Reading: inst.Reading,
			}
			if inst.Reading != nil && inst.Reading.IsFenced != nil {
				pi.Fenced = 1
				if *inst.Reading.IsFenced {
					pi.Fenced = 2
				}
			}
			p.Instances = append(p.Instances, pi)
		}
		if c.metrics != nil {
			m := c.metrics.Export(now)
			p.Metrics = &m
		}
		if c.insight != nil {
			h := c.insight.Export()
			p.Insight = &h
		}
		out.Clusters = append(out.Clusters, p)
	}
	return out
}

func (s *Sweeper) copyCluster(key Key) (clusterCopy, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs, ok := s.clusters[key]
	if !ok {
		return clusterCopy{}, false
	}
	c := clusterCopy{key: key, uid: cs.target.ClusterUID, snap: cs.snap, metrics: cs.metrics, insight: cs.insight}
	instances := make([]string, 0, len(cs.rings))
	for name := range cs.rings {
		instances = append(instances, name)
	}
	sort.Strings(instances)
	for _, name := range instances {
		series := make([]string, 0, len(cs.rings[name]))
		for key := range cs.rings[name] {
			series = append(series, key)
		}
		sort.Strings(series)
		for _, key := range series {
			r := cs.rings[name][key]
			pr := PersistedRing{
				Instance: name, Series: key, At: make([]int64, 0, len(r.samples)),
				Values: make([]float64, 0, len(r.samples)), Has: make([]bool, 0, len(r.samples)),
			}
			for _, sm := range r.samples {
				pr.At = append(pr.At, sm.at.UnixNano())
				if sm.value != nil {
					pr.Values, pr.Has = append(pr.Values, *sm.value), append(pr.Has, true)
				} else {
					pr.Values, pr.Has = append(pr.Values, 0), append(pr.Has, false)
				}
			}
			c.rings = append(c.rings, pr)
		}
	}
	return c, true
}

// Import holds a snapshot until its Clusters are swept; it must be
// called before Run. It reports how many Clusters it holds of how many
// the snapshot named: one without a UID or a name, or named twice, is
// dropped.
func (s *Sweeper) Import(state PersistedState, now time.Time) (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = make(map[Key]PersistedCluster, len(state.Clusters))
	s.pendingAt = now
	for _, c := range state.Clusters {
		key := Key{Namespace: c.Namespace, Name: c.Name}
		if c.UID == "" || key.Namespace == "" || key.Name == "" {
			continue
		}
		if _, dup := s.pending[key]; dup {
			delete(s.pending, key)
			continue
		}
		s.pending[key] = c
	}
	return len(s.pending), len(state.Clusters)
}

// expirePendingLocked forgets restored state nobody claimed in time:
// a Cluster deleted while the console was down.
func (s *Sweeper) expirePendingLocked(at time.Time) {
	if len(s.pending) > 0 && at.Sub(s.pendingAt) > restoreKeep {
		s.pending = nil
	}
}

// restoreLocked seeds a new Cluster's state from the snapshot when the
// Cluster still carries the UID it was saved under, and the instances
// whose pods still carry theirs. Samples past the retention are dropped,
// and the restart gap is written into each ring as a gap, so a sparkline
// shows the hole instead of a line across it.
func (s *Sweeper) restoreLocked(cs *clusterState, target Target, at time.Time) {
	p, ok := s.pending[target.Key]
	if !ok {
		return
	}
	delete(s.pending, target.Key)
	if p.UID != target.ClusterUID {
		return
	}
	pods := make(map[string]string, len(target.Pods))
	for _, pod := range target.Pods {
		pods[pod.Name] = pod.UID
	}
	from := at.Add(-s.opts.Retention)
	same := map[string]bool{}
	cs.snap = Snapshot{Interval: s.opts.Interval, Instances: map[string]Instance{}}
	for _, pi := range p.Instances {
		if pi.PodUID == "" || pods[pi.Name] != pi.PodUID {
			continue
		}
		same[pi.Name] = true
		inst := Instance{
			State: viewmodel.StatusReadState(pi.State), Reason: pi.Reason,
			At: time.Unix(0, pi.At).UTC(), Since: time.Unix(0, pi.Since).UTC(), uid: pi.PodUID,
		}
		if inst.Since.After(at) {
			inst.Since = at
		}
		// The last good report, unless it is older than the rings: past
		// the retention it describes a pod nobody has read for too long
		// to stand in for a failed read.
		if r := pi.Reading; r != nil && r.PodUID == pi.PodUID && !r.ObservedAt.Before(from) && !r.ObservedAt.After(at) {
			reading := *r
			switch pi.Fenced {
			case 1, 2:
				fenced := pi.Fenced == 2
				reading.IsFenced = &fenced
			default:
				reading.IsFenced = nil
			}
			inst.Reading = &reading
		}
		cs.snap.Instances[pi.Name] = inst
	}
	for _, pr := range p.Rings {
		if !same[pr.Instance] || len(pr.At) != len(pr.Values) || len(pr.At) != len(pr.Has) {
			continue
		}
		r := &ring{}
		var last time.Time
		for i, ns := range pr.At {
			t := time.Unix(0, ns).UTC()
			if t.Before(from) || t.After(at) || (!last.IsZero() && !t.After(last)) {
				continue
			}
			sm := sample{at: t}
			if pr.Has[i] {
				v := pr.Values[i]
				sm.value = &v
			}
			r.samples = append(r.samples, sm)
			last = t
		}
		if len(r.samples) == 0 {
			continue
		}
		if gap := last.Add(s.opts.Interval); gap.Before(at) {
			r.samples = append(r.samples, sample{at: gap})
		}
		if cs.rings[pr.Instance] == nil {
			cs.rings[pr.Instance] = map[string]*ring{}
		}
		cs.rings[pr.Instance][pr.Series] = r
	}
	if cs.metrics != nil && p.Metrics != nil {
		kept := metrics.PersistedState{Version: p.Metrics.Version, SavedAt: p.Metrics.SavedAt}
		for _, ins := range p.Metrics.Instruments {
			if same[ins.Instance] {
				kept.Instruments = append(kept.Instruments, ins)
			}
		}
		cs.metrics.Import(kept, at)
	}
	if cs.insight != nil && p.Insight != nil {
		cs.insight.Import(*p.Insight, at)
	}
}
