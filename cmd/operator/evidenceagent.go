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

package main

// EvidenceAgent wiring of the operator binary. Kept in its own file so each
// controller's wiring changes without touching the others'; main.go calls
// the two functions below.

import (
	"context"
	"flag"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/evidenceagent"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// defaultEvidenceAgentImage is the agent image used when an EvidenceAgent
// names none; the flag overrides it, EVIDENCE_AGENT_IMAGE is the
// packaging's way to set it.
var (
	defaultEvidenceAgentImage string
	evidenceAgentRepositories string
)

func init() {
	utilruntime.Must(pgtoolboxv1alpha1.AddToScheme(scheme))
	flag.StringVar(&defaultEvidenceAgentImage, "default-evidence-agent-image", os.Getenv("EVIDENCE_AGENT_IMAGE"),
		"The evidence agent image an EvidenceAgent runs when its spec names none; a digest-pinned reference.")
	flag.StringVar(&evidenceAgentRepositories, "evidence-agent-image-repositories", os.Getenv("EVIDENCE_AGENT_IMAGE_REPOSITORIES"),
		"Comma-separated image repositories an EvidenceAgent's spec.image may name (exact, or a prefix ending in '/'). Empty: the default image's repository only.")
}

// evidenceAgentCacheOptions adds the EvidenceAgent controller's informer
// filters to the manager's cache options, and keeps Secret and ConfigMap
// content out of every cache (they are read through the APIReader and
// watched metadata-only).
func evidenceAgentCacheOptions(options *ctrl.Options) {
	if options.Cache.ByObject == nil {
		options.Cache.ByObject = map[client.Object]cache.ByObject{}
	}
	for object, byObject := range evidenceagent.CacheByObject() {
		options.Cache.ByObject[object] = byObject
	}
	if options.Cache.DefaultTransform == nil {
		options.Cache.DefaultTransform = cache.TransformStripManagedFields()
	}
	if options.Client.Cache == nil {
		options.Client.Cache = &client.CacheOptions{}
	}
	options.Client.Cache.DisableFor = append(options.Client.Cache.DisableFor, &corev1.Secret{}, &corev1.ConfigMap{})
}

// setupEvidenceAgent probes the optional ObjectStore, Stanza and Archive
// APIs and registers the controller; it returns the kinds the controller's
// informers hold (pgtoolbox_cached_objects). A probe error other than "not
// served / not allowed" fails the start.
func setupEvidenceAgent(ctx context.Context, mgr ctrl.Manager, watchNamespaces []string) ([]client.Object, error) {
	objectStoreAPI, stanzaAPI, archiveAPI, err := probeEvidenceAgentAPIs(ctx, mgr.GetAPIReader(), watchNamespaces, startupStepTimeout)
	if err != nil {
		return nil, err
	}
	if err := metrics.Registry.Register(evidenceagent.Collector{Reader: mgr.GetCache()}); err != nil {
		return nil, err
	}
	setupLog.Info("EvidenceAgent controller", "objectStoreAPI", objectStoreAPI.State, "objectStoreAPIReason", objectStoreAPI.Reason,
		"stanzaAPI", stanzaAPI.State, "stanzaAPIReason", stanzaAPI.Reason, "archiveAPI", archiveAPI.State, "archiveAPIReason", archiveAPI.Reason,
		"defaultImage", defaultEvidenceAgentImage)
	apis := evidenceagent.OptionalAPIs{ObjectStore: objectStoreAPI.Usable(), Stanza: stanzaAPI.Usable(), Archive: archiveAPI.Usable()}
	return evidenceagent.CachedKinds(apis), (&evidenceagent.Reconciler{
		Client:                   mgr.GetClient(),
		APIReader:                mgr.GetAPIReader(),
		Recorder:                 mgr.GetEventRecorder("evidenceagent"),
		DefaultImage:             defaultEvidenceAgentImage,
		AllowedImageRepositories: splitList(evidenceAgentRepositories),
		ObjectStoreAPI:           objectStoreAPI,
		StanzaAPI:                stanzaAPI,
		ArchiveAPI:               archiveAPI,
	}).SetupWithManager(ctx, mgr)
}

func splitList(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

// probeEvidenceAgentAPIs probes the ObjectStore, Stanza and Archive APIs
// as one start-up step (startupStep): an API server that never answers
// fails the start naming the step, before the liveness probe kills it.
func probeEvidenceAgentAPIs(ctx context.Context, reader client.Reader, watchNamespaces []string, timeout time.Duration) (
	objectStore, stanza, archive discovery.Result, err error,
) {
	err = startupStep(ctx, timeout, "probe the EvidenceAgent's optional APIs", func(ctx context.Context) error {
		var err error
		if objectStore, err = discovery.ProbeObjectStoreAPI(ctx, reader, watchNamespaces); err != nil {
			return err
		}
		if stanza, err = discovery.ProbeStanzaAPI(ctx, reader, watchNamespaces); err != nil {
			return err
		}
		archive, err = discovery.ProbeArchiveAPI(ctx, reader, watchNamespaces)
		return err
	})
	return objectStore, stanza, archive, err
}
