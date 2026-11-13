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

package mcpoauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// crowded is the leader's index loaded from a ConfigMap past four
// fifths of its grants, a hundred people holding thirteen each (the
// mean), key holding four: key is then past the fair share of new work.
// A load is not new work: nothing refuses what the ConfigMap holds.
func crowded(t *testing.T, c *clock, key string) *sharedstate.Store {
	t.Helper()
	at := c.Now().Unix()
	grants := map[string]map[string]any{}
	n := 0
	add := func(user string) {
		n++
		grants[fmt.Sprintf("grant-%032x", 1<<40+n)] = map[string]any{"u": user, "c": "other", "s": "mcp:read", "t": at, "l": at, "e": at + 3600}
	}
	for p := range 100 {
		sum := sha256.Sum256([]byte(fmt.Sprint("crowd", p)))
		for range 13 {
			add(hex.EncodeToString(sum[:16]))
		}
	}
	for range 4 {
		add(key)
	}
	raw, err := json.Marshal(map[string]any{"version": 2, "grants": grants, "codes": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "console-state", Namespace: "team-a", ResourceVersion: "1"},
		Data:       map[string]string{sharedstate.KeyGrants: string(raw)},
	})
	store := sharedstate.New(sharedstate.Options{Client: cs.CoreV1().ConfigMaps("team-a"), Name: "console-state", Now: c.Now})
	store.SetLeader(func() bool { return true })
	if err := store.LoadGrants(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !store.GrantsUsable() {
		t.Fatal("the crowded index is not usable")
	}
	return store
}

// A replayed code's revocation is recorded even when its person is past
// the fair share (round-11 review): the placeholder a later record of the
// grant merges into is not new work.
func TestAReplayedCodesRevocationIsRecordedPastTheFairShare(t *testing.T) {
	c := &clock{now: t0}
	f := newSessions(t, c, secretA)
	key := newReplica(t, f, memoryStore(c), c, nil).s.userKey("alice")
	store := crowded(t, c, key)
	r := newReplica(t, f, store, c, nil)
	cid := r.clientID(t)
	code := r.consent(t, authorizeQuery(cid, loopback), "approve").Get("code")
	// The first redemption spends the code; its grant is refused (new
	// work past the fair share).
	if w, _, e := r.token(t, codeForm(cid, code)); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Fatalf("first redemption: %d %s", w.Code, w.Body)
	}
	if w, _, e := r.token(t, codeForm(cid, code)); w.Code != http.StatusBadRequest || e != "invalid_grant" {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	if !store.GrantRevoked(grantOfCode(t, r, code)) {
		t.Error("the replayed code's revocation was not recorded past the fair share")
	}
}
