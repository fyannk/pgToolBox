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

package cache

import (
	"fmt"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// journal is a fake history: every hook call, in order.
type journal struct{ calls []string }

func (j *journal) tap(_ Kind, obj client.Object, deleted bool) {
	verb := "tap"
	if deleted {
		verb = "delete"
	}
	j.calls = append(j.calls, verb+" "+obj.GetName())
}

func (j *journal) seed(kind Kind, namespaces []string, objs []client.Object) {
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, o.GetName())
	}
	j.calls = append(j.calls, fmt.Sprintf("seed %s %v %v", kind.Source, namespaces, names))
}

func pod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: name}}
}

func TestRecorderSeedsTheInitialListThenReplaysInOrder(t *testing.T) {
	j := &journal{}
	r := newRecorder(j.tap, j.seed)
	s := r.source(Kind{Source: SourcePods}, []string{"a"})
	s.deliver(pod("p1"), false, true)
	s.deliver(pod("p2"), false, true)
	// A watch event arriving before the handler saw the whole list.
	s.deliver(pod("p1"), false, false)
	s.deliver(pod("p2"), true, false)
	if len(j.calls) != 0 {
		t.Fatalf("delivered before the list completed: %v", j.calls)
	}
	s.flush()
	s.deliver(pod("p3"), false, false)
	s.flush() // a second flush is a no-op
	want := []string{"seed pods [a] [p1 p2]", "tap p1", "delete p2", "tap p3"}
	if !slices.Equal(j.calls, want) {
		t.Fatalf("calls = %v, want %v", j.calls, want)
	}
}

func TestRecorderMutedEpochDeliversNothing(t *testing.T) {
	j := &journal{}
	r := newRecorder(j.tap, j.seed)
	s := r.source(Kind{Source: SourcePods}, []string{"a"})
	s.deliver(pod("p1"), false, true)
	r.mute()
	s.flush()
	s.deliver(pod("p2"), false, false)
	if len(j.calls) != 0 {
		t.Fatalf("a muted epoch recorded %v", j.calls)
	}
}

func TestRecorderWithoutSeedTapsEverything(t *testing.T) {
	j := &journal{}
	s := newRecorder(j.tap, nil).source(Kind{Source: SourcePods}, nil)
	s.deliver(pod("p1"), false, true)
	if want := []string{"tap p1"}; !slices.Equal(j.calls, want) {
		t.Fatalf("calls = %v, want %v", j.calls, want)
	}
	var none *recorder
	if newRecorder(nil, nil) != nil || none.source(Kind{}, nil) != nil {
		t.Fatal("no hooks still built a recorder")
	}
	none.mute()
	var nothing *sourceRecorder
	nothing.deliver(pod("p"), false, true)
	nothing.flush()
}
