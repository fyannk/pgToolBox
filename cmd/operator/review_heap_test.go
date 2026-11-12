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

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// hostileReviewShapes are the rules reviews that decode to the most heap
// per byte on the wire: one rule whose verbs list has thousands of
// one-letter entries (one Role of a huge verbs list bound many times), and
// thousands of minimal rules (one Role of tens of thousands of rules bound
// many times; every field RBAC requires, one letter each).
var hostileReviewShapes = map[string]func(n int) []authorizationv1.ResourceRule{
	"one rule, many verbs": func(n int) []authorizationv1.ResourceRule {
		verbs := make([]string, n)
		for i := range verbs {
			verbs[i] = "a"
		}
		return []authorizationv1.ResourceRule{{APIGroups: []string{""}, Resources: []string{"a"}, Verbs: verbs}}
	},
	"many minimal rules": func(n int) []authorizationv1.ResourceRule {
		rules := make([]authorizationv1.ResourceRule, n)
		for i := range rules {
			rules[i] = authorizationv1.ResourceRule{APIGroups: []string{""}, Resources: []string{"a"}, Verbs: []string{"a"}}
		}
		return rules
	},
}

// nearCapReview is the encoding (by encode) of a rules review of shape
// just under managerrole.ReviewResponseLimit.
func nearCapReview(t *testing.T, encode func(*authorizationv1.SelfSubjectRulesReview) []byte, shape func(int) []authorizationv1.ResourceRule) []byte {
	t.Helper()
	review := func(n int) []byte {
		r := &authorizationv1.SelfSubjectRulesReview{}
		r.APIVersion, r.Kind = "authorization.k8s.io/v1", "SelfSubjectRulesReview"
		r.Status.ResourceRules = shape(n)
		return encode(r)
	}
	small, large := len(review(1_000)), len(review(2_000))
	perEntry := float64(large-small) / 1_000
	n := 1_000 + int(float64(managerrole.ReviewResponseLimit-small-64)/perEntry)
	for {
		body := review(n)
		if len(body) <= managerrole.ReviewResponseLimit {
			return body
		}
		n -= 16
	}
}

// TestStartupReviewsHeapIsBounded: round 18, K8S P3-1, measured (it
// replaces round 17's arithmetic). Every review in flight
// (discovery.ProbeConcurrency) answers just under
// managerrole.ReviewResponseLimit with a shape that decodes to the most
// heap, all at once through the real start-up client: the heap grows by
// well under the soft memory limit of the chart's 256Mi container
// (memlimit.Fraction of it, 230 MiB). At 1 MiB they grew it by 240-340 MiB,
// and at 256 KiB of protobuf by 90-140 MiB; at 256 KiB of JSON, by 20-40.
func TestStartupReviewsHeapIsBounded(t *testing.T) {
	const budget = 64 << 20
	for name, shape := range hostileReviewShapes {
		t.Run(name, func(t *testing.T) {
			codecs := serializer.NewCodecFactory(scheme)
			var mu sync.Mutex
			bodies := map[string][]byte{}
			body := func(contentType string) ([]byte, bool) {
				mu.Lock()
				defer mu.Unlock()
				if b, ok := bodies[contentType]; ok {
					return b, true
				}
				info, ok := k8sruntime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), contentType)
				if !ok {
					return nil, false
				}
				encoder := codecs.EncoderForVersion(info.Serializer, authorizationv1.SchemeGroupVersion)
				b := nearCapReview(t, func(r *authorizationv1.SelfSubjectRulesReview) []byte {
					var buf bytes.Buffer
					if err := encoder.Encode(r, &buf); err != nil {
						t.Error(err)
					}
					return buf.Bytes()
				}, shape)
				bodies[contentType] = b
				return b, true
			}
			// Every response waits until all are being answered, so their
			// bodies are read and decoded together.
			var arrived atomic.Int32
			var notJSON atomic.Bool
			all := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				// Round 19, OPERATOR P3-3: the response's media type is
				// the one Accept asks for first, as an API server picks
				// it, whatever the request's own Content-Type: a client
				// that sends JSON but accepts protobuf gets protobuf.
				accept := r.Header.Get("Accept")
				contentType, _, _ := strings.Cut(accept, ",")
				contentType = strings.TrimSpace(contentType)
				if accept != "application/json" || r.Header.Get("Content-Type") != "application/json" {
					notJSON.Store(true)
				}
				b, ok := body(contentType)
				if !ok {
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				if arrived.Add(1) == discovery.ProbeConcurrency {
					close(all)
				}
				select {
				case <-all:
				case <-time.After(5 * time.Second):
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(b)
			}))
			defer server.Close()
			mapper := meta.NewDefaultRESTMapper(nil)
			mapper.Add(authorizationv1.SchemeGroupVersion.WithKind("SelfSubjectRulesReview"), meta.RESTScopeRoot)
			c, err := client.New(startupConfig(&rest.Config{Host: server.URL}), client.Options{Scheme: scheme, Mapper: mapper})
			if err != nil {
				t.Fatal(err)
			}
			namespaces := make([]string, discovery.ProbeConcurrency)
			for i := range namespaces {
				namespaces[i] = fmt.Sprintf("team-%02d", i)
			}
			// Encode both media types first (the server's bodies are part
			// of the baseline), then measure from a collected heap. A low
			// GOGC keeps garbage from passing for heap in use: the
			// operator runs under a soft memory limit that collects it.
			for _, mediaType := range []string{"application/json", "application/vnd.kubernetes.protobuf"} {
				if _, ok := body(mediaType); !ok {
					t.Fatalf("no %s serializer", mediaType)
				}
			}
			defer debug.SetGCPercent(debug.SetGCPercent(10))
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			var peak atomic.Uint64
			stop := make(chan struct{})
			sampled := make(chan struct{})
			go func() {
				defer close(sampled)
				var m runtime.MemStats
				for {
					runtime.ReadMemStats(&m)
					if m.HeapAlloc > peak.Load() {
						peak.Store(m.HeapAlloc)
					}
					select {
					case <-stop:
						return
					case <-time.After(time.Millisecond):
					}
				}
			}()
			required := []managerrole.Permission{{Group: "apps", Resource: "deployments", Verbs: []string{"list"}}}
			review, err := managerrole.PermittedNamespaces(t.Context(), c, required, namespaces, 0)
			close(stop)
			<-sampled
			if err != nil {
				t.Fatal(err)
			}
			if notJSON.Load() {
				t.Error("the start-up client sent a review in, or accepted its answer in, another media type than JSON alone: protobuf decodes to several times more heap per byte")
			}
			if len(review.Unreviewed) != 0 {
				t.Fatalf("a near-cap review was refused: %+v", review.Unreviewed[0])
			}
			grown := int64(peak.Load()) - int64(before.HeapAlloc)
			t.Logf("%d reviews of %d bytes in flight: the heap grew by at most %d MiB", len(namespaces), len(bodies["application/json"]), grown>>20)
			if grown > budget {
				t.Errorf("%d near-cap reviews in flight grew the heap by %d MiB, over the %d MiB budget", len(namespaces), grown>>20, budget>>20)
			}
		})
	}
}
