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

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/volumetrend"
)

// rawClusterReader answers the raw Cluster view with the fork's autoGrow
// stanzas, which the typed Cluster drops.
type rawClusterReader struct {
	client.Reader
	spec map[string]any
}

func (r rawClusterReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Cluster" {
		u.Object["metadata"] = map[string]any{"namespace": key.Namespace, "name": key.Name}
		u.Object["spec"] = r.spec
		return nil
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

type readerCache struct {
	Cache
	reader client.Reader
}

func (c readerCache) Reader() client.Reader { return c.reader }

func TestStorageForecastsReadAutoGrowAndArchiving(t *testing.T) {
	ctx := context.Background()
	svc := newScreenService(t)
	svc.cache = readerCache{Cache: svc.cache, reader: rawClusterReader{Reader: svc.cache.Reader(), spec: map[string]any{
		"storage": map[string]any{"size": "20Gi", "autoGrow": map[string]any{"maxSize": "50Gi", "threshold": int64(85), "step": "5Gi"}},
	}}}
	// orders' primary stops archiving with a backlog.
	snaps := replicationSnaps()
	key := sweep.Key{Namespace: "team-a", Name: "orders"}
	snap := snaps[key]
	instances := map[string]sweep.Instance{}
	for name, inst := range snap.Instances {
		if name == "orders-1" {
			r := *inst.Reading
			r.IsArchivingWAL, r.LastFailedWAL, r.ReadyWALFiles = false, "000000010000000000000042", 40
			inst.Reading = &r
		}
		instances[name] = inst
	}
	snap.Instances = instances
	snaps[key] = snap
	svc.SetSweep(fakeSweep{snaps: snaps})

	got, err := svc.Storage(ctx, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Requests) == 0 || got.Requests[0].AutoGrow == nil || got.Requests[0].AutoGrow.MaxSize != "50Gi" {
		t.Fatalf("requests = %+v", got.Requests)
	}
	for _, b := range got.BlindSpots {
		if b.ID == cnpgstatus.BlindSpotVolumeUsageNotReported {
			t.Error("the volumes are reported, yet the blind spot stays")
		}
	}
	var data, wal bool
	for _, v := range got.Forecasts.Volumes {
		switch v.Volume {
		case "pgdata":
			data = true
			if v.AutoGrow == nil || v.BytesForecast.Target != "filesystem" || !v.BytesForecast.AutoGrowSteps {
				t.Errorf("pgdata filesystem forecast: %+v", v.BytesForecast)
			}
			if g := v.AutoGrowForecast; g == nil || g.Target != "autoGrowMaxSize" || g.Capacity != 50*(1<<30) {
				t.Errorf("pgdata forecast ignores autoGrow: %+v", g)
			}
			if v.ArchivingFailing {
				t.Error("archiving flagged on pgdata although a WAL volume exists")
			}
		case "wal":
			wal = true
			if !v.ArchivingFailing || v.ArchivingText == "" {
				t.Errorf("WAL volume not flagged while archiving fails: %+v", v)
			}
			if v.AutoGrow != nil {
				t.Error("walStorage has no autoGrow here")
			}
		}
	}
	if !data || !wal {
		t.Errorf("volumes = %+v", got.Forecasts.Volumes)
	}
}

func TestStorageForecastsSayWhyTheyAreUnavailable(t *testing.T) {
	svc := newScreenService(t)
	got, err := svc.Storage(context.Background(), "team-a", "billing")
	if err != nil {
		t.Fatal(err)
	}
	if got.Forecasts.State != forecastsUnavailable || got.Forecasts.Reason == "" || len(got.Forecasts.Volumes) != 0 {
		t.Errorf("forecasts = %+v", got.Forecasts)
	}
	found := false
	for _, b := range got.BlindSpots {
		found = found || b.ID == cnpgstatus.BlindSpotVolumeUsageNotReported
	}
	if !found {
		t.Error("stock CloudNativePG lost its blind spot")
	}
	svc.opts.Trends = nil
	if got, _ := svc.Storage(context.Background(), "team-a", "billing"); got.Forecasts.State != forecastsDisabled {
		t.Errorf("no trend store: state = %q", got.Forecasts.State)
	}
}

func TestTheTrendStoreAtItsCapIsABlindSpot(t *testing.T) {
	svc := newScreenService(t)
	store := volumetrend.NewStore(volumetrend.Limits{MaxSeries: 1})
	reading := volumetrend.Reading{At: now, TotalBytes: 10 * 1 << 30, AvailableBytes: 5 * 1 << 30}
	store.Observe(volumetrend.Key{Namespace: "team-a", Cluster: "orders", Instance: "orders-1", Volume: "pgdata"}, reading)
	store.Observe(volumetrend.Key{Namespace: "team-a", Cluster: "billing", Instance: "billing-1", Volume: "pgdata"}, reading)
	svc.opts.Trends = store
	has := func(cluster string) bool {
		got, err := svc.Storage(context.Background(), "team-a", cluster)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range got.BlindSpots {
			if b.ID == BlindSpotVolumeTrendsAtCapacity {
				return true
			}
		}
		return false
	}
	if !has("billing") {
		t.Error("a refused cluster shows no blind spot")
	}
	if has("orders") {
		t.Error("a trended cluster shows the capacity blind spot")
	}
}
